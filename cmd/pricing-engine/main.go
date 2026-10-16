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
		addr := os.Getenv("LISTEN_ADDR")
		if addr == "" || addr[0] == ':' {
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
	_ = lvl.UnmarshalText([]byte(cfg.LogLevel))
	log := slog.New(security.NewRedactingHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})))
	slog.SetDefault(log)
	log.Info("starting", "config", cfg.String())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := metrics.NewSet()
	eng := engine.New(cfg.Symbols, m)

	st := store.NewRedis(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB, cfg.StoreNS)
	defer st.Close()
	states, err := loadWithRetry(ctx, st, cfg.Symbols, 30*time.Second, log)
	if err != nil {
		// Serve from memory rather than not at all; flushes keep retrying.
		log.Error("state restore failed; starting with empty state", "err", err)
	}
	n := eng.Restore(states)
	log.Info("state restored", "symbols", len(states), "buckets", n)

	hubOpts := server.DefaultOptions()
	hubOpts.MaxClients, hubOpts.MaxFrameBytes = cfg.MaxClients, cfg.MaxFrameBytes
	hubOpts.RequireAuth, hubOpts.MaxConnsPerMerchant = cfg.WSRequireAuth, cfg.MaxConnsPerMerchant
	hub := server.NewHub(hubOpts, eng, m, log)

	// Merchant accounts: KYC, roles, API keys. Key material lives outside Redis.
	secrets, err := accounts.LoadOrCreateSecrets(cfg.SecretsDir)
	if err != nil {
		return err
	}
	acctRedis := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword, DB: cfg.RedisDB})
	defer acctRedis.Close()
	svc, err := accounts.NewService(accounts.NewStore(acctRedis), secrets, log)
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
	api := accounts.NewAPI(svc, secrets, hub, price, log, cfg.CookieSecure)
	var protect func(http.Handler) http.Handler
	if cfg.WSRequireAuth {
		protect = api.MarketAccess
	} else {
		log.Warn("WS_REQUIRE_AUTH=false: market data is served without authentication")
	}

	ticks := make(chan []model.Tick, 256)
	fc := feed.NewClient(feed.Config{
		URL: cfg.UpstreamURL, APIKey: cfg.APIKey, APISecret: cfg.APISecret, Symbols: cfg.Symbols,
		AuthTimeout: cfg.AuthTimeout, ReadTimeout: cfg.ReadTimeout,
	}, ticks, log, m)

	var pipeline sync.WaitGroup
	pipeline.Add(2)
	go func() { defer pipeline.Done(); fc.Run(ctx) }()
	go func() { defer pipeline.Done(); process(ctx, ticks, eng, hub, m) }()
	go hub.Revalidate(ctx)

	// The flusher outlives the pipeline so its final flush sees every tick.
	flushCtx, stopFlush := context.WithCancel(context.Background())
	flusher := &store.Flusher{Store: st, Source: eng.DrainDirty, Interval: cfg.FlushInterval, Log: log, Metrics: m}
	flushDone := make(chan struct{})
	go func() { defer close(flushDone); flusher.Run(flushCtx) }()

	mux := server.Routes(hub, eng, m, func(ctx context.Context) (bool, error) {
		return m.UpstreamUp.Value() == 1, st.Ping(ctx)
	}, protect)
	api.Register(mux)
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	srvErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr)
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
	pw, generated := cfg.AdminPassword, false
	if pw == "" {
		pw, generated = randomPassword(), true
	}
	created, err := svc.EnsureAdmin(ctx, cfg.AdminEmail, pw)
	if err != nil || !created {
		return err
	}
	log.Info("bootstrap admin created", "email", cfg.AdminEmail)
	if generated {
		fmt.Fprintf(os.Stderr, "\n==== BOOTSTRAP ADMIN (shown once) ====\n  email:    %s\n  password: %s\n"+
			"  Set ADMIN_PASSWORD in .env to choose your own.\n=====================================\n\n", cfg.AdminEmail, pw)
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
