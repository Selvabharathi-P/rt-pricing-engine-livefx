package feed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"pricing-engine/internal/metrics"
	"pricing-engine/internal/model"
)

func TestParseShapes(t *testing.T) {
	recv := int64(1_750_000_000_000) * 1e6
	cases := map[string]struct {
		in   string
		want int
	}{
		"array":      {`[{"symbol":"EURUSD","buy":1.1,"sell":1.0,"timestamp":1750000000000},{"symbol":"GBPUSD","buy":1.3,"sell":1.2,"timestamp":1750000000000}]`, 2},
		"single":     {`{"symbol":"EURUSD","buy":1.1,"sell":1.0,"timestamp":1750000000000}`, 1},
		"wrapped":    {`{"type":"price","data":[{"symbol":"EURUSD","buy":"1.1","sell":"1.0","timestamp":"1750000000000"}]}`, 1},
		"nested":     {`{"data":{"prices":[{"symbol":"EURUSD","buy":1.1,"sell":1.0}]}}`, 1},
		"keyed map":  {`{"EURUSD":{"buy":1.1,"sell":1.0,"timestamp":1750000000},"GBPUSD":{"buy":1.3,"sell":1.2}}`, 2},
		"auth ack":   {`{"action":"auth","status":"success"}`, 0},
		"sub ack":    {`{"action":"subscribeMarketPrice","status":"success","count":61}`, 0},
		"empty data": {`{"data":[]}`, 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ticks, err := Parse([]byte(c.in), recv)
			if err != nil {
				t.Fatal(err)
			}
			if len(ticks) != c.want {
				t.Fatalf("got %d ticks, want %d: %+v", len(ticks), c.want, ticks)
			}
		})
	}
}

// Exact frame captured from wss://v3-prod.livefxhub.com/ws/algo.
func TestParseLiveFrame(t *testing.T) {
	in := `{"event":"marketPriceSnapshot","symbol":"AUDUSD","bid":0.69563,"ask":0.69588,"high":0.69631,"low":0.69194,"percentage":0.57}`
	recv := int64(1_750_000_000_123) * 1e6
	ticks, err := Parse([]byte(in), recv)
	if err != nil || len(ticks) != 1 {
		t.Fatal(err, ticks)
	}
	want := model.Tick{Symbol: "AUDUSD", Buy: 0.69588, Sell: 0.69563, TS: 1_750_000_000_123, RecvNano: recv}
	if ticks[0] != want {
		t.Fatalf("got %+v want %+v", ticks[0], want)
	}
}

func TestParseIgnoresUpstream24hFields(t *testing.T) {
	in := `{"symbol":"eurusd","buy":1.1,"sell":1.0,"low_24h":0.5,"high_24h":9.9,"change_24h":42,"timestamp":1750000000000}`
	ticks, err := Parse([]byte(in), 0)
	if err != nil || len(ticks) != 1 {
		t.Fatal(err, ticks)
	}
	want := model.Tick{Symbol: "EURUSD", Buy: 1.1, Sell: 1.0, TS: 1750000000000}
	if ticks[0] != want {
		t.Fatalf("got %+v want %+v", ticks[0], want)
	}
}

func TestTimestampUnits(t *testing.T) {
	const ms = int64(1750000000123)
	for _, in := range []string{`1750000000.123`, `1750000000123`, `1750000000123000`, `1750000000123000000`, `"2025-06-15T15:06:40.123Z"`} {
		ticks, err := Parse([]byte(`{"symbol":"X","buy":1,"sell":1,"timestamp":`+in+`}`), 0)
		if err != nil || len(ticks) != 1 {
			t.Fatal(in, err)
		}
		if d := ticks[0].TS - ms; d < -1 || d > 1 {
			t.Fatalf("%s -> %d, want %d", in, ticks[0].TS, ms)
		}
	}
}

func TestIsFailure(t *testing.T) {
	for in, want := range map[string]bool{
		`{"action":"auth","status":"success"}`:                false,
		`{"success":true}`:                                    false,
		`{"success":false}`:                                   true,
		`{"status":"error","message":"bad key"}`:              true,
		`{"error":"unauthorized"}`:                            true,
		`{"type":"auth","message":"Invalid API credentials"}`: true,
		`not json`: false,
	} {
		if got := isFailure([]byte(in)); got != want {
			t.Errorf("isFailure(%s) = %v", in, got)
		}
	}
}

// fakeUpstream speaks the auth/subscribe protocol and streams one frame per
// connection, then drops the connection to exercise reconnects.
func fakeUpstream(t *testing.T, key string, conns *atomic.Int32, gotSub chan<- []string) *httptest.Server {
	up := websocket.Upgrader{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		conns.Add(1)
		var auth map[string]string
		if c.ReadJSON(&auth) != nil {
			return
		}
		if auth["api_key"] != key {
			c.WriteJSON(map[string]string{"status": "error", "message": "invalid credentials"})
			return
		}
		c.WriteJSON(map[string]string{"action": "auth", "status": "success"})
		var sub struct{ Symbols []string }
		if c.ReadJSON(&sub) != nil {
			return
		}
		select {
		case gotSub <- sub.Symbols:
		default:
		}
		c.WriteJSON([]map[string]any{{"symbol": "EURUSD", "buy": 1.1, "sell": 1.0, "timestamp": time.Now().UnixMilli()}})
		time.Sleep(50 * time.Millisecond)
	}))
}

func TestClientAuthSubscribeReconnect(t *testing.T) {
	var conns atomic.Int32
	subs := make(chan []string, 4)
	srv := fakeUpstream(t, "good", &conns, subs)
	defer srv.Close()

	out := make(chan []model.Tick, 16)
	c := NewClient(Config{
		URL: "ws" + strings.TrimPrefix(srv.URL, "http"), APIKey: "good", APISecret: "s",
		Symbols: []string{"EURUSD"}, AuthTimeout: time.Second, ReadTimeout: time.Second,
	}, out, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.NewSet())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	if s := <-subs; len(s) != 1 || s[0] != "EURUSD" {
		t.Fatalf("subscribe payload %v", s)
	}
	select {
	case ticks := <-out:
		if ticks[0].Symbol != "EURUSD" || ticks[0].RecvNano == 0 {
			t.Fatalf("bad tick %+v", ticks[0])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no ticks received")
	}
	// The fake drops every connection; the client must come back.
	deadline := time.Now().Add(5 * time.Second)
	for conns.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if conns.Load() < 2 {
		t.Fatal("client did not reconnect")
	}
}

func TestClientAuthRejected(t *testing.T) {
	var conns atomic.Int32
	srv := fakeUpstream(t, "good", &conns, make(chan []string, 1))
	defer srv.Close()
	c := NewClient(Config{
		URL: "ws" + strings.TrimPrefix(srv.URL, "http"), APIKey: "bad", APISecret: "s",
		Symbols: []string{"EURUSD"}, AuthTimeout: time.Second, ReadTimeout: time.Second,
	}, make(chan []model.Tick, 1), slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.NewSet())
	err := c.session(context.Background())
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
}

// BenchmarkParse256KB measures decoding a frame shaped like the upstream
// (61 ticks padded to ~256KB, as the simulator sends).
func BenchmarkParse256KB(b *testing.B) {
	pad := strings.Repeat("x", 250000/61)
	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < 61; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"symbol":"EURUSD","buy":1.10012,"sell":1.10001,"low_24h":-1,"high_24h":-1,"change_24h":999,"timestamp":1750000000123,"book":"` + pad + `"}`)
	}
	sb.WriteByte(']')
	msg := []byte(sb.String())
	b.SetBytes(int64(len(msg)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Parse(msg, 1); err != nil {
			b.Fatal(err)
		}
	}
}

// One-session-per-key upstreams reply like this when the keys are already in use.
func TestClientSessionInUse(t *testing.T) {
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		var auth map[string]string
		c.ReadJSON(&auth)
		c.WriteJSON(map[string]string{"event": "error", "message": "already have an active session"})
	}))
	defer srv.Close()
	c := NewClient(Config{
		URL: "ws" + strings.TrimPrefix(srv.URL, "http"), APIKey: "k", APISecret: "s",
		Symbols: []string{"EURUSD"}, AuthTimeout: time.Second, ReadTimeout: time.Second,
	}, make(chan []model.Tick, 1), slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.NewSet())
	err := c.session(context.Background())
	if !errors.Is(err, ErrSessionInUse) || errors.Is(err, ErrAuth) {
		t.Fatalf("want ErrSessionInUse only, got %v", err)
	}
}

func TestReason(t *testing.T) {
	for err, want := range map[error]string{
		fmt.Errorf("%w: busy", ErrSessionInUse): StatusSessionInUse,
		fmt.Errorf("%w: bad", ErrAuth):          StatusAuthRejected,
		errors.New("read: i/o timeout"):         StatusDisconnected,
	} {
		if got := Reason(err); got != want {
			t.Errorf("Reason(%v) = %s, want %s", err, got, want)
		}
	}
}

func TestRegistry(t *testing.T) {
	if got := strings.Join(Types(), ","); !strings.Contains(got, "livefxhub") {
		t.Fatalf("types: %s", got)
	}
	d := Deps{Out: make(chan []model.Tick, 1), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: metrics.NewSet()}
	if _, err := New("nope", Config{URL: "ws://x"}, d); err == nil || !strings.Contains(err.Error(), "registered: ") {
		t.Fatalf("unknown type: %v", err)
	}
	src, err := New("livefxhub", Config{Name: "primary", URL: "ws://x", Symbols: []string{"EURUSD"}}, d)
	if err != nil || src.Name() != "primary" || src.Symbols()[0] != "EURUSD" {
		t.Fatalf("build: %v %v", src, err)
	}
}

// The configured retry settings drive reconnects: with a 50 ms
// "session in use" wait (instead of the default 30 s) the client retries fast.
func TestConfiguredSessionInUseWait(t *testing.T) {
	var conns atomic.Int32
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		conns.Add(1)
		var auth map[string]string
		c.ReadJSON(&auth)
		c.WriteJSON(map[string]string{"event": "error", "message": "already have an active session"})
	}))
	defer srv.Close()
	var reasons []string
	var mu sync.Mutex
	src, _ := New("livefxhub", Config{Name: "t", URL: "ws" + strings.TrimPrefix(srv.URL, "http"), APIKey: "k", APISecret: "s",
		Symbols: []string{"EURUSD"}, AuthTimeout: time.Second, ReadTimeout: time.Second, PingInterval: 500 * time.Millisecond,
		Retry: Retry{MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond, SessionInUseWait: 50 * time.Millisecond}},
		Deps{Out: make(chan []model.Tick, 1), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: metrics.NewSet(),
			OnStatus: func(up bool, reason string) { mu.Lock(); reasons = append(reasons, reason); mu.Unlock() }})
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	src.Run(ctx)
	if n := conns.Load(); n < 4 {
		t.Fatalf("only %d connection attempts in 700 ms; configured wait not applied", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reasons) == 0 || reasons[0] != StatusSessionInUse {
		t.Fatalf("status reasons: %v", reasons)
	}
}
