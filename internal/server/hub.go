// Package server is the downstream WebSocket fan-out and HTTP API.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"pricing-engine/internal/engine"
	"pricing-engine/internal/metrics"
	"pricing-engine/internal/model"
	"pricing-engine/internal/security"
)

type Options struct {
	MaxClients    int
	MaxFrameBytes int
	WriteTimeout  time.Duration // a client that can't take a frame this fast is dropped
	PongTimeout   time.Duration
	MsgRate       rate.Limit // inbound control messages per second per client
	MsgBurst      int

	RequireAuth         bool          // clients must authenticate (API key or ticket) before subscribing
	AuthTimeout         time.Duration // unauthenticated sockets are closed after this
	MaxConnsPerMerchant int
	RevalidateEvery     time.Duration // re-check each merchant's right to trade
	UpstreamDebounce    time.Duration // announce "upstream down" only if it lasts this long
}

func DefaultOptions() Options {
	return Options{MaxClients: 10000, MaxFrameBytes: 10*1024 - 256, WriteTimeout: 5 * time.Second,
		PongTimeout: 60 * time.Second, MsgRate: 10, MsgBurst: 20,
		RequireAuth: true, AuthTimeout: 10 * time.Second, MaxConnsPerMerchant: 5, RevalidateEvery: 30 * time.Second,
		UpstreamDebounce: 3 * time.Second}
}

// Identity is an authenticated downstream merchant.
type Identity struct{ ID, Email string }

// Authorizer validates WebSocket credentials against the account system.
type Authorizer interface {
	AuthenticateKey(ctx context.Context, keyID, secret string) (Identity, error)
	RedeemTicket(ctx context.Context, ticket string) (Identity, error)
	Allowed(ctx context.Context, merchantID string) bool
}

// ErrNotPermitted is returned by an Authorizer for valid credentials whose
// owner may not trade (KYC not approved, account disabled).
var ErrNotPermitted = errors.New("not permitted")

// WebSocket close codes (mirroring the upstream feed's 4001/4003 style).
const (
	CloseAuthRequired = 4001 // not authenticated / bad credentials
	CloseForbidden    = 4003 // authenticated but not permitted (KYC, disabled, key revoked)
	CloseTooMany      = 4008 // per-merchant session limit
)

// Hub routes published quotes to subscribed clients.
//
// Back-pressure model: each client holds at most one pending quote per
// symbol. A newer quote replaces an unsent older one (conflation), so a slow
// client receives the latest state instead of an ever-growing backlog, and
// memory per client is bounded by the number of symbols.
type Hub struct {
	opts Options
	eng  *engine.Engine
	m    *metrics.Set
	log  *slog.Logger

	mu         sync.RWMutex
	bySym      map[string]map[*Client]struct{}
	wild       map[*Client]struct{}
	clients    map[*Client]struct{}
	byMerchant map[string]map[*Client]struct{}
	n          atomic.Int64

	auth    Authorizer
	authLim *rate.Limiter // global cap on auth attempts (brute force across sockets)

	upMu    sync.Mutex
	upCur   UpstreamStatus // actual state, as reported by the feed client
	upPub   UpstreamStatus // last state announced to clients
	upTimer *time.Timer

	upgrader websocket.Upgrader
}

func NewHub(opts Options, eng *engine.Engine, m *metrics.Set, log *slog.Logger) *Hub {
	return &Hub{
		opts: opts, eng: eng, m: m, log: log,
		bySym:      make(map[string]map[*Client]struct{}),
		wild:       make(map[*Client]struct{}),
		clients:    make(map[*Client]struct{}),
		byMerchant: make(map[string]map[*Client]struct{}),
		authLim:    rate.NewLimiter(50, 100),
		upgrader: websocket.Upgrader{
			ReadBufferSize: 4096, WriteBufferSize: 16 << 10, EnableCompression: true,
			// Authentication is by API key message or single-use ticket, never
			// by ambient cookies, so cross-origin sockets gain nothing.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

func (h *Hub) SetAuthorizer(a Authorizer) { h.auth = a }

// UpstreamStatus is the market-data provider connection state, as shown to
// web clients ({"t":"upstream", ...} and in "welcome").
type UpstreamStatus struct {
	Up     bool   `json:"up"`
	Reason string `json:"reason"` // connecting | connected | session_in_use | auth_rejected | disconnected
	Since  int64  `json:"since"`  // unix ms of the change
}

// SetUpstream records the upstream state (wired to feed.Client.OnStatus).
// Recoveries are announced at once; outages only after UpstreamDebounce, so
// a quick reconnect never flashes a warning at users.
func (h *Hub) SetUpstream(up bool, reason string) {
	h.upMu.Lock()
	defer h.upMu.Unlock()
	if h.upCur.Up == up && h.upCur.Reason == reason && h.upCur.Since != 0 {
		return
	}
	h.upCur = UpstreamStatus{Up: up, Reason: reason, Since: time.Now().UnixMilli()}
	if h.upTimer != nil {
		h.upTimer.Stop()
		h.upTimer = nil
	}
	if up {
		go h.publishUpstream()
	} else {
		h.upTimer = time.AfterFunc(h.opts.UpstreamDebounce, h.publishUpstream)
	}
}

// Upstream returns the last state announced to clients.
func (h *Hub) Upstream() UpstreamStatus {
	h.upMu.Lock()
	defer h.upMu.Unlock()
	if h.upPub.Since == 0 {
		return UpstreamStatus{Reason: "connecting", Since: time.Now().UnixMilli()}
	}
	return h.upPub
}

func (h *Hub) publishUpstream() {
	h.upMu.Lock()
	s := h.upCur
	if s.Up == h.upPub.Up && s.Reason == h.upPub.Reason && h.upPub.Since != 0 {
		h.upMu.Unlock()
		return
	}
	first := h.upPub.Since == 0
	h.upPub = s
	h.upMu.Unlock()
	if first && s.Up {
		return // normal start-up: nothing to tell anyone
	}
	msg := map[string]any{"t": "upstream", "up": s.Up, "reason": s.Reason, "since": s.Since}
	// Copy first: sendCtrl may close a client, and closing takes h.mu.
	h.mu.RLock()
	cs := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		if c.authed.Load() {
			cs = append(cs, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range cs {
		c.sendCtrl(msg)
	}
}

// bind attaches an identity to c, enforcing the per-merchant session limit.
func (h *Hub) bind(c *Client, id Identity) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.byMerchant[id.ID]
	if len(set) >= h.opts.MaxConnsPerMerchant {
		return false
	}
	if set == nil {
		set = make(map[*Client]struct{})
		h.byMerchant[id.ID] = set
	}
	set[c] = struct{}{}
	c.merchantID, c.email = id.ID, id.Email
	c.authed.Store(true)
	if c.reqInfo != nil {
		c.reqInfo.Merchant = id.ID // shown on the /ws access-log line
	}
	return true
}

// Kick closes every live session of a merchant (KYC change, disable, key
// rotation). Implements accounts.SessionKicker.
func (h *Hub) Kick(merchantID, reason string) {
	h.mu.RLock()
	cs := make([]*Client, 0, len(h.byMerchant[merchantID]))
	for c := range h.byMerchant[merchantID] {
		cs = append(cs, c)
	}
	h.mu.RUnlock()
	for _, c := range cs {
		c.kick(CloseForbidden, reason)
	}
	if len(cs) > 0 {
		h.log.Info("merchant sessions closed", "merchant_id", merchantID, "sessions", len(cs), "reason", reason)
	}
}

// Revalidate periodically re-checks every connected merchant, so access
// ends even when nothing explicit happens (e.g. KYC approval expires).
func (h *Hub) Revalidate(ctx context.Context) {
	if h.auth == nil || h.opts.RevalidateEvery <= 0 {
		return
	}
	t := time.NewTicker(h.opts.RevalidateEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.revalidateOnce(ctx)
		}
	}
}

func (h *Hub) revalidateOnce(ctx context.Context) {
	h.mu.RLock()
	ids := make([]string, 0, len(h.byMerchant))
	for id, set := range h.byMerchant {
		if len(set) > 0 {
			ids = append(ids, id)
		}
	}
	h.mu.RUnlock()
	for _, id := range ids {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		ok := h.auth.Allowed(cctx, id)
		failed := cctx.Err() != nil
		cancel()
		if !ok && !failed {
			h.Kick(id, "access revoked")
		}
	}
}

// SessionInfo describes one live downstream connection (admin view).
type SessionInfo struct {
	MerchantID    string    `json:"merchant_id,omitempty"`
	Email         string    `json:"email,omitempty"`
	Remote        string    `json:"remote"`
	ConnectedAt   time.Time `json:"connected_at"`
	Authenticated bool      `json:"authenticated"`
	Format        string    `json:"format"`
	Symbols       []string  `json:"symbols"`
	Frames        uint64    `json:"frames"`
}

func (h *Hub) Sessions() []SessionInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]SessionInfo, 0, len(h.clients))
	for c := range h.clients {
		si := SessionInfo{MerchantID: c.merchantID, Email: c.email, Remote: c.remote, ConnectedAt: c.connectedAt,
			Authenticated: c.authed.Load(), Format: "json", Frames: c.frames.Load(), Symbols: []string{}}
		if c.binary {
			si.Format = "binary"
		}
		if c.wild {
			si.Symbols = []string{"*"}
		} else {
			for s := range c.subs {
				si.Symbols = append(si.Symbols, s)
			}
		}
		out = append(out, si)
	}
	return out
}

// Publish fans quotes out to subscribers. It never blocks on a client, and
// each client is woken once per batch so a multi-symbol update leaves as one
// frame rather than one frame per symbol.
func (h *Hub) Publish(qs []model.Quote) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.wild {
		if c.stage(qs) {
			c.wake()
		}
	}
	var touched map[*Client]struct{}
	for i := range qs {
		for c := range h.bySym[qs[i].Symbol] {
			if c.stage(qs[i : i+1]) {
				if touched == nil {
					touched = make(map[*Client]struct{})
				}
				touched[c] = struct{}{}
			}
		}
	}
	for c := range touched {
		c.wake()
	}
}

func (h *Hub) Clients() int64 { return h.n.Load() }

// subscribe adds symbols (nil = all) and returns the symbols that need a snapshot.
func (h *Hub) subscribe(c *Client, syms []string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if syms == nil {
		for s := range c.subs {
			delete(h.bySym[s], c)
		}
		clear(c.subs)
		c.wild = true
		h.wild[c] = struct{}{}
		return nil
	}
	if c.wild {
		return []string{}
	}
	for _, s := range syms {
		if _, ok := c.subs[s]; ok {
			continue
		}
		c.subs[s] = struct{}{}
		set := h.bySym[s]
		if set == nil {
			set = make(map[*Client]struct{})
			h.bySym[s] = set
		}
		set[c] = struct{}{}
	}
	return syms
}

// unsubscribe removes symbols (nil = all).
func (h *Hub) unsubscribe(c *Client, syms []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.wild {
		delete(h.wild, c)
		c.wild = false
		if syms != nil { // "all except these"
			drop := make(map[string]bool, len(syms))
			for _, s := range syms {
				drop[s] = true
			}
			for _, s := range h.eng.Symbols() {
				if !drop[s] {
					c.subs[s] = struct{}{}
					if h.bySym[s] == nil {
						h.bySym[s] = make(map[*Client]struct{})
					}
					h.bySym[s][c] = struct{}{}
				}
			}
			return
		}
	}
	if syms == nil {
		for s := range c.subs {
			syms = append(syms, s)
		}
	}
	for _, s := range syms {
		delete(c.subs, s)
		delete(h.bySym[s], c)
	}
}

func (h *Hub) remove(c *Client) {
	h.mu.Lock()
	delete(h.wild, c)
	delete(h.clients, c)
	if c.merchantID != "" {
		delete(h.byMerchant[c.merchantID], c)
		if len(h.byMerchant[c.merchantID]) == 0 {
			delete(h.byMerchant, c.merchantID)
		}
	}
	for s := range c.subs {
		delete(h.bySym[s], c)
	}
	h.mu.Unlock()
	h.m.WSClients.Set(h.n.Add(-1))
}

// ServeWS upgrades a downstream client.
//
//	/ws?format=binary&symbols=EURUSD,GBPUSD&ticket=wt_...   (all optional)
//
// With RequireAuth, the client must either present a single-use ticket
// (browser) or send {"action":"auth","api_key":"pk_...","secret":"sk_..."}
// as its first message (programmatic), like the upstream feed.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	if h.n.Add(1) > int64(h.opts.MaxClients) {
		h.n.Add(-1)
		http.Error(w, "too many clients", http.StatusServiceUnavailable)
		return
	}
	var ticketID *Identity
	if t := r.URL.Query().Get("ticket"); t != "" && h.opts.RequireAuth && h.auth != nil {
		id, err := h.auth.RedeemTicket(r.Context(), t)
		if err != nil {
			h.n.Add(-1)
			http.Error(w, "invalid or expired ticket", http.StatusUnauthorized)
			return
		}
		ticketID = &id
	}
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.n.Add(-1)
		return // Upgrade already wrote the HTTP error
	}
	h.m.WSClients.Set(h.n.Load())
	c := &Client{
		h: h, conn: conn, binary: r.URL.Query().Get("format") == "binary",
		pending: make(map[string]model.Quote), sent: make(map[string]uint64),
		// ctrl must hold more than the rate-limit burst so the limiter, not
		// queue overflow, is what stops a chatty client.
		notify: make(chan struct{}, 1), ctrl: make(chan []byte, 2*h.opts.MsgBurst+8), done: make(chan struct{}),
		subs: make(map[string]struct{}), lim: rate.NewLimiter(h.opts.MsgRate, h.opts.MsgBurst),
		remote: r.RemoteAddr, connectedAt: time.Now(), reqInfo: security.RequestInfoFrom(r.Context()),
	}
	h.log.Debug("ws connected", "remote", c.remote, "format", map[bool]string{true: "binary", false: "json"}[c.binary])
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	go c.writeLoop()
	if !h.opts.RequireAuth {
		c.authed.Store(true)
	} else if ticketID != nil {
		if !h.bind(c, *ticketID) {
			c.kick(CloseTooMany, "too many sessions for this merchant")
			c.readLoop()
			return
		}
		h.log.Info("ws authenticated", "merchant", ticketID.ID, "via", "ticket", "remote", c.remote)
	}
	c.sendCtrl(map[string]any{"t": "welcome", "fields": JSONFields, "symbols": h.eng.Symbols(),
		"format": map[bool]string{true: "binary", false: "json"}[c.binary], "auth_required": !c.authed.Load(),
		"upstream": h.Upstream()})
	if s := r.URL.Query().Get("symbols"); s != "" {
		c.deferred = strings.Split(s, ",")
	}
	if c.authed.Load() {
		c.subscribeDeferred()
	} else {
		time.AfterFunc(h.opts.AuthTimeout, func() {
			if !c.authed.Load() {
				c.kick(CloseAuthRequired, "authentication timeout")
			}
		})
	}
	c.readLoop()
}

func (c *Client) subscribeDeferred() {
	if c.deferred != nil {
		syms := c.deferred
		c.deferred = nil
		c.handle(command{Action: "subscribe", Symbols: syms})
	}
}

// kick sends a close frame with code/reason and lets the reader finish the
// handshake; the socket is force-closed after a grace period regardless.
func (c *Client) kick(code int, reason string) {
	if c.closing.Swap(true) {
		return
	}
	c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	time.AfterFunc(time.Second, c.close)
}

// Client is one downstream WebSocket connection.
type Client struct {
	h      *Hub
	conn   *websocket.Conn
	binary bool
	remote string

	mu      sync.Mutex
	pending map[string]model.Quote // latest unsent quote per symbol
	sent    map[string]uint64      // last Seq written per symbol
	notify  chan struct{}
	ctrl    chan []byte
	done    chan struct{}
	once    sync.Once
	closing atomic.Bool // close handshake in progress; reader owns teardown

	subs map[string]struct{} // guarded by h.mu
	wild bool                // guarded by h.mu
	lim  *rate.Limiter

	authed      atomic.Bool
	merchantID  string // set once by bind (under h.mu)
	email       string
	connectedAt time.Time
	frames      atomic.Uint64
	deferred    []string // ?symbols= waiting for authentication
	reqInfo     *security.RequestInfo
}

// stage queues quotes for the writer without waking it. It reports whether
// anything new was queued.
func (c *Client) stage(qs []model.Quote) bool {
	added := false
	c.mu.Lock()
	for i := range qs {
		q := &qs[i]
		if q.Seq <= c.sent[q.Symbol] {
			continue
		}
		if p, ok := c.pending[q.Symbol]; ok {
			if p.Seq >= q.Seq {
				continue
			}
			c.h.m.WSConflated.Inc()
		}
		c.pending[q.Symbol] = *q
		added = true
	}
	c.mu.Unlock()
	return added
}

func (c *Client) wake() {
	select {
	case c.notify <- struct{}{}:
	default: // writer already signalled
	}
}

func (c *Client) enqueue(q *model.Quote) {
	if c.stage([]model.Quote{*q}) {
		c.wake()
	}
}

func (c *Client) sendCtrl(v any) {
	b, _ := json.Marshal(v)
	select {
	case c.ctrl <- b:
	default:
		c.close() // a client flooding us with commands it never reads replies to
	}
}

func (c *Client) close() {
	c.once.Do(func() {
		c.h.log.Debug("ws disconnected", "merchant", c.merchantID, "remote", c.remote,
			"session_s", time.Since(c.connectedAt).Round(time.Second).Seconds(), "frames_sent", c.frames.Load())
		close(c.done)
		c.conn.Close()
		c.h.remove(c)
	})
}

func (c *Client) writeLoop() {
	ping := time.NewTicker(c.h.opts.PongTimeout / 2)
	defer ping.Stop()
	defer func() {
		if !c.closing.Load() {
			c.close()
		}
	}()
	for {
		select {
		case <-c.done:
			return
		case b := <-c.ctrl:
			if c.write(websocket.TextMessage, b) != nil {
				return
			}
		case <-c.notify:
			// Control replies first, so "welcome"/"subscribed" always precede
			// the quotes they announce (select alone picks randomly).
			for drained := false; !drained; {
				select {
				case b := <-c.ctrl:
					if c.write(websocket.TextMessage, b) != nil {
						return
					}
				default:
					drained = true
				}
			}
			if c.flush() != nil {
				return
			}
		case <-ping.C:
			if c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(c.h.opts.WriteTimeout)) != nil {
				return
			}
		}
	}
}

func (c *Client) write(kind int, b []byte) error {
	c.conn.SetWriteDeadline(time.Now().Add(c.h.opts.WriteTimeout))
	c.conn.EnableWriteCompression(len(b) > 1024) // tiny frames: deflate costs more than it saves
	err := c.conn.WriteMessage(kind, b)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		c.h.m.WSSlowDisconnect.Inc()
		c.h.log.Warn("dropping slow client", "remote", c.remote)
	}
	return err
}

func (c *Client) flush() error {
	c.mu.Lock()
	if len(c.pending) == 0 {
		c.mu.Unlock()
		return nil
	}
	batch := make([]model.Quote, 0, len(c.pending))
	for s, q := range c.pending {
		batch = append(batch, q)
		c.sent[s] = q.Seq
	}
	clear(c.pending)
	c.mu.Unlock()

	kind := websocket.TextMessage
	if c.binary {
		kind = websocket.BinaryMessage
	}
	for _, f := range EncodeFrames(batch, c.h.opts.MaxFrameBytes, c.binary) {
		if err := c.write(kind, f); err != nil {
			return err
		}
		c.h.m.WSFramesTotal.Inc()
		c.frames.Add(1)
		c.h.m.WSFrameBytes.Observe(float64(len(f)))
	}
	now := time.Now().UnixNano()
	for i := range batch {
		if r := batch[i].RecvNano; r > 0 {
			c.h.m.WSSendLatency.Observe(float64(now-r) / 1e9)
		}
	}
	return nil
}

type command struct {
	Action  string   `json:"action"`
	Symbols []string `json:"symbols"`
	APIKey  string   `json:"api_key"`
	Secret  string   `json:"secret"`
}

func (c *Client) readLoop() {
	defer c.close()
	c.conn.SetReadLimit(8 << 10)
	c.conn.SetReadDeadline(time.Now().Add(c.h.opts.PongTimeout))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(c.h.opts.PongTimeout))
	})
	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		c.conn.SetReadDeadline(time.Now().Add(c.h.opts.PongTimeout))
		if !c.lim.Allow() {
			c.closing.Store(true)
			c.conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "rate limit exceeded"),
				time.Now().Add(time.Second))
			// Complete the close handshake so the client sees the reason
			// instead of a TCP reset from unread data.
			c.conn.SetReadDeadline(time.Now().Add(time.Second))
			for {
				if _, _, err := c.conn.ReadMessage(); err != nil {
					return
				}
			}
		}
		var cmd command
		if err := json.Unmarshal(msg, &cmd); err != nil {
			c.sendCtrl(map[string]string{"t": "error", "msg": "invalid JSON command"})
			continue
		}
		c.handle(cmd)
	}
}

func (c *Client) handle(cmd command) {
	if cmd.Action == "auth" {
		c.authenticate(cmd)
		return
	}
	if !c.authed.Load() && cmd.Action != "ping" {
		c.sendCtrl(map[string]string{"t": "error", "msg": "Must authenticate first"})
		c.kick(CloseAuthRequired, "Must authenticate first")
		return
	}
	switch cmd.Action {
	case "ping":
		c.sendCtrl(map[string]any{"t": "pong", "ts": time.Now().UnixMilli()})
	case "subscribe", "unsubscribe":
		syms, bad := c.normalise(cmd.Symbols)
		if len(bad) > 0 {
			c.sendCtrl(map[string]any{"t": "error", "msg": "unknown symbols", "symbols": bad})
		}
		if syms != nil && len(syms) == 0 {
			return
		}
		if cmd.Action == "unsubscribe" {
			c.h.unsubscribe(c, syms)
			c.sendCtrl(map[string]any{"t": "unsubscribed", "symbols": orAll(syms)})
			return
		}
		c.h.log.Debug("ws subscribe", "merchant", c.merchantID, "symbols", orAll(syms))
		snap := c.h.subscribe(c, syms)
		c.sendCtrl(map[string]any{"t": "subscribed", "symbols": orAll(syms)})
		if snap == nil || len(snap) > 0 {
			if c.stage(c.h.eng.Snapshot(snap)) {
				c.wake()
			}
		}
	default:
		c.sendCtrl(map[string]string{"t": "error", "msg": "unknown action; use auth, subscribe, unsubscribe or ping"})
	}
}

func (c *Client) authenticate(cmd command) {
	h := c.h
	if c.authed.Load() {
		c.sendCtrl(map[string]string{"t": "error", "msg": "already authenticated"})
		return
	}
	if h.auth == nil || !h.authLim.Allow() {
		c.sendCtrl(map[string]string{"t": "error", "msg": "authentication unavailable, retry later"})
		c.kick(CloseAuthRequired, "authentication unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	id, err := h.auth.AuthenticateKey(ctx, cmd.APIKey, cmd.Secret)
	cancel()
	if err != nil {
		code, msg := CloseAuthRequired, "invalid credentials"
		if errors.Is(err, ErrNotPermitted) {
			code, msg = CloseForbidden, "trading not permitted: KYC verification required"
		}
		h.log.Warn("ws auth rejected", "remote", c.remote, "reason", msg)
		c.sendCtrl(map[string]string{"t": "error", "msg": msg})
		c.kick(code, msg)
		return
	}
	if !h.bind(c, id) {
		c.sendCtrl(map[string]string{"t": "error", "msg": "too many sessions for this API key"})
		c.kick(CloseTooMany, "too many sessions")
		return
	}
	h.log.Info("ws authenticated", "merchant", id.ID, "via", "api_key", "remote", c.remote)
	c.sendCtrl(map[string]any{"t": "authenticated", "merchant": id.Email})
	c.subscribeDeferred()
}

// normalise upper-cases and validates symbols. nil result means "all" ("*").
func (c *Client) normalise(in []string) (ok, bad []string) {
	ok = []string{}
	for _, s := range in {
		s = strings.ToUpper(strings.TrimSpace(s))
		switch {
		case s == "*":
			return nil, bad
		case c.h.eng.Has(s):
			ok = append(ok, s)
		case s != "":
			bad = append(bad, s)
		}
	}
	return ok, bad
}

func orAll(s []string) any {
	if s == nil {
		return "*"
	}
	return s
}
