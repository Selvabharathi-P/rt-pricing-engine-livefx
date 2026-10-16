// Package feed maintains the authenticated upstream WebSocket session and
// turns raw frames into normalised ticks.
package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"pricing-engine/internal/metrics"
	"pricing-engine/internal/model"
)

type Config struct {
	URL         string
	APIKey      string
	APISecret   string
	Symbols     []string
	AuthTimeout time.Duration
	ReadTimeout time.Duration
}

type Client struct {
	cfg Config
	out chan<- []model.Tick
	log *slog.Logger
	m   *metrics.Set

	// OnStatus, if set, is told whenever the upstream session goes up or
	// down, with one of the Status* reasons (used to inform web clients).
	OnStatus func(up bool, reason string)
}

// Upstream status reasons.
const (
	StatusConnected    = "connected"
	StatusSessionInUse = "session_in_use" // another client holds the one-per-key session
	StatusAuthRejected = "auth_rejected"  // credentials refused
	StatusDisconnected = "disconnected"   // network error, timeout, server closed
)

// Reason classifies why a session ended.
func Reason(err error) string {
	switch {
	case errors.Is(err, ErrSessionInUse):
		return StatusSessionInUse
	case errors.Is(err, ErrAuth):
		return StatusAuthRejected
	default:
		return StatusDisconnected
	}
}

func (c *Client) status(up bool, reason string) {
	if c.OnStatus != nil {
		c.OnStatus(up, reason)
	}
}

func NewClient(cfg Config, out chan<- []model.Tick, log *slog.Logger, m *metrics.Set) *Client {
	return &Client{cfg: cfg, out: out, log: log, m: m}
}

// ErrAuth means upstream explicitly rejected our credentials.
var ErrAuth = errors.New("upstream authentication rejected")

// ErrSessionInUse means the credentials are valid but upstream allows one
// session per key and another one is still open (another client using the
// same keys, or our previous session not yet released by the server).
var ErrSessionInUse = errors.New("upstream session already active for these credentials")

// Retrying a busy session quickly only keeps refreshing the server-side
// lock, so wait much longer than for ordinary network errors.
const sessionInUseWait = 30 * time.Second

const (
	minBackoff  = 500 * time.Millisecond
	maxBackoff  = 30 * time.Second
	stableAfter = 30 * time.Second // a session this long resets the backoff
	pingEvery   = 15 * time.Second
)

// Run keeps a session alive until ctx is cancelled, reconnecting with
// exponential backoff and full jitter. Every reconnect re-authenticates and
// re-subscribes; duplicate ticks replayed after a reconnect are removed by
// the engine.
func (c *Client) Run(ctx context.Context) {
	backoff := minBackoff
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx)
		c.m.UpstreamUp.Set(0)
		if ctx.Err() != nil {
			return
		}
		c.status(false, Reason(err))
		if time.Since(start) > stableAfter {
			backoff = minBackoff
		}
		wait := time.Duration(rand.Int64N(int64(backoff))) + minBackoff/2
		lvl := slog.LevelWarn
		switch {
		case errors.Is(err, ErrSessionInUse):
			wait = sessionInUseWait + time.Duration(rand.Int64N(int64(10*time.Second)))
			c.log.Warn("upstream rejected login: another session is active for these API keys. "+
				"Close any other client using them; a stale session is released by the server after its timeout",
				"err", err, "retry_in", wait.Round(time.Second).String())
		case errors.Is(err, ErrAuth):
			lvl = slog.LevelError
			fallthrough
		default:
			c.log.Log(ctx, lvl, "upstream session ended; reconnecting", "err", err, "retry_in", wait.Round(time.Millisecond).String())
		}
		c.m.UpstreamReconn.Inc()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (c *Client) session(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, EnableCompression: true, ReadBufferSize: 64 << 10}
	conn, _, err := dialer.DialContext(ctx, c.cfg.URL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	// Always end with a WebSocket close frame so upstream releases the
	// session immediately instead of waiting for its own timeout.
	defer closeGracefully(conn)
	conn.SetReadLimit(16 << 20)

	// Unblock reads when the service shuts down.
	stop := context.AfterFunc(ctx, func() { closeGracefully(conn) })
	defer stop()

	if err := c.authenticate(conn); err != nil {
		return err
	}
	sub := map[string]any{"action": "subscribeMarketPrice", "symbols": c.cfg.Symbols}
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteJSON(sub); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	c.log.Info("upstream subscribed", "symbols", len(c.cfg.Symbols))
	c.m.UpstreamUp.Set(1)
	c.status(true, StatusConnected)

	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(c.cfg.ReadTimeout))
	})
	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-t.C:
				// WriteControl is safe to call concurrently with reads.
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			}
		}
	}()

	first := true
	samples := 0
	for {
		conn.SetReadDeadline(time.Now().Add(c.cfg.ReadTimeout))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		recv := time.Now().UnixNano()
		c.m.UpstreamFrames.Inc()
		c.m.UpstreamBytes.Observe(float64(len(msg)))
		ticks, err := Parse(msg, recv)
		if err != nil {
			c.m.ParseErrors.Inc()
			c.log.Debug("unparseable upstream frame", "err", err, "bytes", len(msg))
			continue
		}
		if len(ticks) == 0 {
			if isFailure(msg) {
				c.log.Warn("upstream error message", "msg", truncate(msg, 300))
				continue
			}
			// A frame with no recognisable ticks: count it and log a few
			// samples per session so a schema mismatch is diagnosable.
			c.m.UnrecognisedFrames.Inc()
			if samples < 5 {
				samples++
				c.log.Info("upstream frame without recognised ticks (sample)", "n", samples, "msg", truncate(msg, 500))
			}
			continue
		}
		if first {
			c.log.Info("receiving live market prices", "ticks_in_frame", len(ticks))
			first = false
		}
		// Blocking send = backpressure onto the TCP socket during spikes;
		// the engine is far faster than the feed, so this rarely waits.
		select {
		case c.out <- ticks:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// authenticate sends the auth action and waits for the server's verdict.
// Any non-error reply (or the first price frame) counts as success.
func (c *Client) authenticate(conn *websocket.Conn) error {
	auth := map[string]string{"action": "auth", "api_key": c.cfg.APIKey, "secret": c.cfg.APISecret}
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteJSON(auth); err != nil {
		return fmt.Errorf("auth write: %w", err)
	}
	conn.SetReadDeadline(time.Now().Add(c.cfg.AuthTimeout))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("waiting for auth response: %w", err)
	}
	if isFailure(msg) {
		if strings.Contains(strings.ToLower(string(msg)), "active session") {
			return fmt.Errorf("%w: %s", ErrSessionInUse, truncate(msg, 200))
		}
		return fmt.Errorf("%w: %s", ErrAuth, truncate(msg, 200))
	}
	c.log.Info("upstream authenticated")
	return nil
}

// isFailure recognises the usual shapes of an error reply.
func isFailure(msg []byte) bool {
	var m map[string]any
	if json.Unmarshal(msg, &m) != nil {
		return false
	}
	if v, ok := m["success"].(bool); ok && !v {
		return true
	}
	if v, ok := m["error"]; ok && v != nil && v != false && v != "" {
		return true
	}
	for _, k := range []string{"status", "type", "event", "result"} {
		if s, ok := m[k].(string); ok {
			switch strings.ToLower(s) {
			case "error", "fail", "failed", "failure", "unauthorized", "denied", "invalid", "auth_failed":
				return true
			}
		}
	}
	if s, ok := m["message"].(string); ok {
		s = strings.ToLower(s)
		for _, w := range []string{"invalid", "unauthor", "denied", "failed"} {
			if strings.Contains(s, w) {
				return true
			}
		}
	}
	return false
}

// closeGracefully sends a normal-closure frame, then closes the socket.
// Safe to call more than once and concurrently with reads.
func closeGracefully(conn *websocket.Conn) {
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "client shutdown"), time.Now().Add(time.Second))
	_ = conn.Close()
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
