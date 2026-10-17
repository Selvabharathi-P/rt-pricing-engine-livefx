// Command pricing-engine consumes the upstream market feed, computes rolling
// 24h statistics and serves them to downstream WebSocket clients.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"pricing-engine/internal/accounts"
	"pricing-engine/internal/config"
	"pricing-engine/internal/engine"
	"pricing-engine/internal/feed"
	"pricing-engine/internal/metrics"
	"pricing-engine/internal/model"
	"pricing-engine/internal/security"
	"pricing-engine/internal/server"
	"pricing-engine/internal/store"
)

func main() {
	// Container healthcheck (the distroless image has no curl/wget).
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		addr := ":8080"
		if c, err := config.Load(); err == nil && c.Server.Listen != "" { // file + LISTEN_ADDR
			addr = c.Server.Listen
		}
		if addr[0] == ':' {
			addr = "localhost" + addr
		}
		c := http.Client{Timeout: 2 * time.Second}
		resp, err := c.Get("http://" + addr + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(cfg.Logging.Level))
	log := slog.New(security.NewRedactingHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})))
	slog.SetDefault(log)
	log.Info("starting", "config", cfg.String())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := metrics.NewSet()
	m.RegisterRuntime()
	symbols := cfg.AllSymbols() // union of every enabled source
	eng := engine.New(symbols, m)

	st := store.NewRedis(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB, cfg.Persistence.Namespace)
	defer st.Close()
	states, err := loadWithRetry(ctx, st, symbols, 30*time.Second, log)
	if err != nil {
		// Serve from memory rather than not at all; flushes keep retrying.
		log.Error("state restore failed; starting with empty state", "err", err)
	}
	n := eng.Restore(states)
	log.Info("state restored", "symbols", len(states), "buckets", n)

	hubOpts := server.DefaultOptions()
	hubOpts.MaxClients, hubOpts.MaxFrameBytes = cfg.Server.MaxClients, cfg.Server.MaxFrameBytes
	hubOpts.RequireAuth, hubOpts.MaxConnsPerMerchant = cfg.Server.RequireAuth, cfg.Server.MaxConnsPerMerchant
	hub := server.NewHub(hubOpts, eng, m, log)

	// Merchant accounts: KYC, roles, API keys. Key material lives outside Redis.
	secrets, err := accounts.LoadOrCreateSecrets(cfg.Accounts.SecretsDir)
	if err != nil {
		return err
	}
	acctRedis := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB})
	defer acctRedis.Close()
	svc, err := accounts.NewService(accounts.NewStore(acctRedis, log), secrets, log)
	if err != nil {
		return err
	}
	if err := bootstrapAdmin(ctx, svc, cfg, log); err != nil {
		log.Error("bootstrap admin failed", "err", err)
	}
	svc.SetKicker(hub)
	hub.SetAuthorizer(accounts.HubAuthorizer{S: svc})
	price := func(sym string) (float64, float64, bool) {
		qs := eng.Snapshot([]string{sym})
		if len(qs) == 0 {
			return 0, 0, false
		}
		return qs[0].Buy, qs[0].Sell, true
	}
	api := accounts.NewAPI(svc, secrets, hub, price, log, cfg.Server.CookieSecure)
	var protect func(http.Handler) http.Handler
	if cfg.Server.RequireAuth {
		protect = api.MarketAccess
	} else {
		log.Warn("WS_REQUIRE_AUTH=false: market data is served without authentication")
	}

	// Market-data sources: every enabled config.yaml `sources:` entry, built
	// through the source registry (feed.Register) by its `type`.
	ticks := make(chan []model.Tick, 256)
	status := newSourceStatus(hub, m, log)
	var sources []feed.Source
	for _, sc := range cfg.EnabledSources() {
		src, err := feed.New(sc.Type, feed.Config{
			Name: sc.Name, URL: sc.URL, APIKey: sc.APIKey, APISecret: sc.APISecret, Symbols: sc.Symbols,
			AuthTimeout: sc.AuthTimeout.Duration, ReadTimeout: sc.ReadTimeout.Duration, PingInterval: sc.PingInterval.Duration,
			Retry: feed.Retry{
				MinBackoff: sc.Retry.MinBackoff.Duration, MaxBackoff: sc.Retry.MaxBackoff.Duration,
				StableAfter: sc.Retry.StableAfter.Duration, SessionInUseWait: sc.Retry.SessionInUseWait.Duration,
				SessionInUseJitter: sc.Retry.SessionInUseJitter.Duration,
			},
		}, feed.Deps{Out: ticks, Log: log, Metrics: m, OnStatus: status.reporter(sc.Name)})
		if err != nil {
			return fmt.Errorf("source %q: %w", sc.Name, err)
		}
		sources = append(sources, src)
	}

	var pipeline sync.WaitGroup
	pipeline.Add(len(sources) + 1)
	for _, src := range sources {
		go func() { defer pipeline.Done(); src.Run(ctx) }()
	}
	go func() { defer pipeline.Done(); process(ctx, ticks, eng, hub, m) }()
	go hub.Revalidate(ctx)
	go pipelineSummary(ctx, m, log)

	// The flusher outlives the pipeline so its final flush sees every tick.
	flushCtx, stopFlush := context.WithCancel(context.Background())
	flusher := &store.Flusher{Store: st, Source: eng.DrainDirty, Interval: cfg.Persistence.FlushInterval.Duration, Log: log, Metrics: m}
	flushDone := make(chan struct{})
	go func() { defer close(flushDone); flusher.Run(flushCtx) }()

	mux := server.Routes(hub, eng, m, func(ctx context.Context) (bool, error) {
		return m.UpstreamUp.Value() == 1, st.Ping(ctx)
	}, protect)
	api.Register(mux)
	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           server.AccessLog(log, m, mux),
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	srvErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Server.Listen)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err = <-srvErr:
		log.Error("http server failed", "err", err)
		stop()
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx) // WebSocket conns are hijacked; they close with the process
	pipeline.Wait()
	stopFlush()
	<-flushDone
	log.Info("stopped")
	return err
}

// bootstrapAdmin creates the first admin. Without ADMIN_PASSWORD a random
// one is generated and printed once to stderr (deliberately not via the
// logger, which redacts anything named password).
func bootstrapAdmin(ctx context.Context, svc *accounts.Service, cfg config.Config, log *slog.Logger) error {
	pw, generated := cfg.Accounts.AdminPassword, false
	if pw == "" {
		pw, generated = randomPassword(), true
	}
	created, err := svc.EnsureAdmin(ctx, cfg.Accounts.AdminEmail, pw)
	if err != nil || !created {
		return err
	}
	log.Info("bootstrap admin created", "email", cfg.Accounts.AdminEmail)
	if generated {
		fmt.Fprintf(os.Stderr, "\n==== BOOTSTRAP ADMIN (shown once) ====\n  email:    %s\n  password: %s\n"+
			"  Set ADMIN_PASSWORD in .env to choose your own.\n=====================================\n\n", cfg.Accounts.AdminEmail, pw)
	}
	return nil
}

func randomPassword() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

type srcState struct {
	up     bool
	reason string
}

// sourceStatus combines the per-source connection states into the single
// upstream status shown to users and in metrics: up only when every enabled
// source is up; otherwise the first down source's reason (config order).
type sourceStatus struct {
	mu    sync.Mutex
	order []string
	state map[string]srcState
	hub   *server.Hub
	m     *metrics.Set
	log   *slog.Logger
}

func newSourceStatus(hub *server.Hub, m *metrics.Set, log *slog.Logger) *sourceStatus {
	return &sourceStatus{hub: hub, m: m, log: log, state: map[string]srcState{}}
}

func (s *sourceStatus) reporter(name string) func(up bool, reason string) {
	s.mu.Lock()
	s.order = append(s.order, name)
	s.mu.Unlock()
	return func(up bool, reason string) {
		s.mu.Lock()
		s.state[name] = srcState{up, reason}
		allUp, why := true, "connected"
		for _, n := range s.order {
			st, ok := s.state[n]
			if !ok || !st.up {
				allUp = false
				if ok {
					why = st.reason
				} else {
					why = "connecting"
				}
				break
			}
		}
		s.mu.Unlock()
		if allUp {
			s.m.UpstreamUp.Set(1)
		} else {
			s.m.UpstreamUp.Set(0)
		}
		s.hub.SetUpstream(allUp, why)
	}
}

// pipelineSummary writes one INFO line per minute describing the price
// pipeline (deltas since the last line), so INFO logs show the system is
// alive and healthy without a line per tick.
func pipelineSummary(ctx context.Context, m *metrics.Set, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	var last [6]uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur := [6]uint64{m.UpstreamFrames.Value(), m.TicksTotal.Value(), m.DuplicatesTotal.Value(),
				m.OutOfOrderTotal.Value(), m.RejectedTotal.Value(), m.WSFramesTotal.Value()}
			log.Info("pipeline summary", "window", "1m",
				"upstream_connected", m.UpstreamUp.Value() == 1,
				"upstream_frames", cur[0]-last[0], "ticks", cur[1]-last[1], "duplicates", cur[2]-last[2],
				"out_of_order", cur[3]-last[3], "rejected", cur[4]-last[4],
				"ws_clients", m.WSClients.Value(), "ws_frames_sent", cur[5]-last[5])
			last = cur
		}
	}
}

func process(ctx context.Context, in <-chan []model.Tick, eng *engine.Engine, hub *server.Hub, m *metrics.Set) {
	for {
		select {
		case <-ctx.Done():
			return
		case batch := <-in:
			qs := eng.Apply(batch)
			hub.Publish(qs)
			if len(batch) > 0 {
				m.ProcessLatency.Observe(float64(time.Now().UnixNano()-batch[0].RecvNano) / 1e9)
			}
		}
	}
}

func loadWithRetry(ctx context.Context, st store.Store, syms []string, max time.Duration, log *slog.Logger) (map[string]model.SymbolState, error) {
	deadline := time.Now().Add(max)
	for {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		states, err := st.Load(lctx, syms)
		cancel()
		if err == nil {
			return states, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, err
		}
		log.Warn("store not ready; retrying", "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
