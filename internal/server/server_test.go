package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"pricing-engine/internal/engine"
	"pricing-engine/internal/metrics"
	"pricing-engine/internal/model"
)

func quotes(n int) []model.Quote {
	qs := make([]model.Quote, n)
	for i := range qs {
		qs[i] = model.Quote{Symbol: fmt.Sprintf("S%05d", i), Buy: 65432.12345, Sell: 65431.98765,
			High24h: 66000.5, Low24h: 64000.25, ChangePct: -1.2345, TS: 1750000000123, Seq: uint64(i + 1)}
	}
	return qs
}

func TestEncodeFramesRespectLimit(t *testing.T) {
	for _, bin := range []bool{false, true} {
		for _, n := range []int{1, 61, 500, 5000} {
			frames := EncodeFrames(quotes(n), 10*1024-256, bin)
			rows := 0
			for _, f := range frames {
				if len(f) > 10*1024-256 {
					t.Fatalf("bin=%v n=%d frame %d bytes", bin, n, len(f))
				}
				if bin {
					rows += int(binary.LittleEndian.Uint16(f[2:]))
					continue
				}
				var v struct {
					T string
					D [][]any
				}
				if err := json.Unmarshal(f, &v); err != nil {
					t.Fatalf("invalid JSON frame: %v\n%s", err, f)
				}
				rows += len(v.D)
			}
			if rows != n {
				t.Fatalf("bin=%v: %d rows encoded, want %d", bin, rows, n)
			}
		}
	}
}

func TestEncodeRowValues(t *testing.T) {
	q := quotes(1)
	f := EncodeFrames(q, 9000, false)[0]
	want := `{"t":"q","d":[["S00000",65432.12345,65431.98765,66000.5,64000.25,-1.2345,1750000000123]]}`
	if string(f) != want {
		t.Fatalf("got  %s\nwant %s", f, want)
	}
	b := EncodeFrames(q, 9000, true)[0]
	if b[0] != 'Q' || b[1] != 1 || binary.LittleEndian.Uint16(b[2:]) != 1 {
		t.Fatalf("bad header % x", b[:4])
	}
	p := 4 + 1 + len("S00000")
	if got := math.Float64frombits(binary.LittleEndian.Uint64(b[p:])); got != q[0].Buy {
		t.Fatalf("binary buy = %v", got)
	}
}

// 61 symbols — the whole assignment universe — fit in one frame under 10KB.
func TestFullSnapshotSize(t *testing.T) {
	f := EncodeFrames(quotes(61), 10*1024, false)
	if len(f) != 1 {
		t.Fatalf("61 quotes -> %d frames", len(f))
	}
	t.Logf("61-symbol JSON frame: %d bytes", len(f[0]))
}

type harness struct {
	eng *engine.Engine
	hub *Hub
	srv *httptest.Server
	m   *metrics.Set
}

func newHarness(t *testing.T, syms ...string) *harness {
	m := metrics.NewSet()
	eng := engine.New(syms, m)
	opts := DefaultOptions()
	opts.RequireAuth = false
	hub := NewHub(opts, eng, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(Routes(hub, eng, m, func(context.Context) (bool, error) { return true, nil }, nil))
	t.Cleanup(srv.Close)
	return &harness{eng, hub, srv, m}
}

func (h *harness) dial(t *testing.T, query string) *websocket.Conn {
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.srv.URL, "http")+"/ws"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func (h *harness) apply(ticks ...model.Tick) {
	h.hub.Publish(h.eng.Apply(ticks))
}

type frame struct {
	T       string  `json:"t"`
	D       [][]any `json:"d"`
	Symbols any     `json:"symbols"`
	Msg     string  `json:"msg"`
}

func read(t *testing.T, c *websocket.Conn, wantType string) frame {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, b, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for %q: %v", wantType, err)
		}
		var f frame
		json.Unmarshal(b, &f)
		if f.T == wantType {
			return f
		}
	}
}

func TestSubscribeSnapshotAndUpdates(t *testing.T) {
	h := newHarness(t, "EURUSD", "GBPUSD")
	now := time.Now().UnixMilli()
	h.apply(model.Tick{Symbol: "EURUSD", Buy: 1.1, Sell: 1.0, TS: now})

	c := h.dial(t, "")
	read(t, c, "welcome")
	c.WriteJSON(map[string]any{"action": "subscribe", "symbols": []string{"eurusd"}})
	read(t, c, "subscribed")
	snap := read(t, c, "q")
	if len(snap.D) != 1 || snap.D[0][0] != "EURUSD" {
		t.Fatalf("snapshot %+v", snap)
	}
	// GBPUSD is not subscribed: only EURUSD must arrive.
	h.apply(model.Tick{Symbol: "GBPUSD", Buy: 1.3, Sell: 1.2, TS: now + 1}, model.Tick{Symbol: "EURUSD", Buy: 1.2, Sell: 1.1, TS: now + 1})
	up := read(t, c, "q")
	if len(up.D) != 1 || up.D[0][0] != "EURUSD" || up.D[0][1].(float64) != 1.2 {
		t.Fatalf("update %+v", up)
	}
}

func TestWildcardAndUnknownSymbol(t *testing.T) {
	h := newHarness(t, "EURUSD", "GBPUSD")
	c := h.dial(t, "?symbols=*")
	read(t, c, "welcome")
	c.WriteJSON(map[string]any{"action": "subscribe", "symbols": []string{"NOPE"}})
	if e := read(t, c, "error"); e.Msg != "unknown symbols" {
		t.Fatalf("error %+v", e)
	}
	now := time.Now().UnixMilli()
	h.apply(model.Tick{Symbol: "EURUSD", Buy: 1.1, Sell: 1.0, TS: now}, model.Tick{Symbol: "GBPUSD", Buy: 1.3, Sell: 1.2, TS: now})
	got := map[any]bool{}
	for len(got) < 2 {
		for _, row := range read(t, c, "q").D {
			got[row[0]] = true
		}
	}
}

// A client that never reads must not stall the hub or other clients, and
// its queue must stay bounded (one pending quote per symbol).
func TestSlowClientConflation(t *testing.T) {
	h := newHarness(t, "EURUSD")
	fast := h.dial(t, "?symbols=EURUSD")
	read(t, fast, "welcome")
	slow := &Client{h: h.hub, pending: map[string]model.Quote{}, sent: map[string]uint64{}, notify: make(chan struct{}, 1)}

	now := time.Now().UnixMilli()
	start := time.Now()
	for i := 0; i < 1000; i++ {
		qs := h.eng.Apply([]model.Tick{{Symbol: "EURUSD", Buy: 1 + float64(i)/1e4, Sell: 1, TS: now + int64(i)}})
		for j := range qs {
			slow.enqueue(&qs[j])
		}
		h.hub.Publish(qs)
	}
	if time.Since(start) > time.Second {
		t.Fatal("publishing stalled")
	}
	if len(slow.pending) != 1 {
		t.Fatalf("slow client pending = %d, want 1", len(slow.pending))
	}
	if h.m.WSConflated.Value() < 900 {
		t.Fatalf("conflated = %d", h.m.WSConflated.Value())
	}
	// The fast client eventually sees the final price.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f := read(t, fast, "q")
		if f.D[len(f.D)-1][1].(float64) == 1+999.0/1e4 {
			return
		}
	}
	t.Fatal("fast client never saw the latest price")
}

func TestClientRateLimit(t *testing.T) {
	h := newHarness(t, "EURUSD")
	c := h.dial(t, "")
	read(t, c, "welcome")
	for i := 0; i < 100; i++ {
		c.WriteJSON(map[string]string{"action": "ping"})
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
			t.Fatalf("want policy-violation close, got %v", err)
		}
		return
	}
}

func TestPagesAndCSP(t *testing.T) {
	h := newHarness(t, "EURUSD")
	get := func(path string) (*http.Response, string) {
		resp, err := http.Get(h.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	for _, path := range []string{"/", "/login", "/account", "/admin"} {
		resp, body := get(path)
		csp := resp.Header.Get("Content-Security-Policy")
		if resp.StatusCode != 200 || !strings.Contains(body, "/static/app.js") {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
		if strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatalf("%s: weak CSP %q", path, csp)
		}
		if strings.Contains(body, "<script>") || strings.Contains(body, "style=") {
			t.Fatalf("%s: inline script/style would be blocked by CSP", path)
		}
	}
	if resp, body := get("/static/quotes.js"); resp.StatusCode != 200 || !strings.Contains(body, "/api/v1/ws-ticket") {
		t.Fatalf("quotes.js: %d", resp.StatusCode)
	}
	if resp, _ := get("/nope"); resp.StatusCode != 404 {
		t.Fatalf("unknown path served: %d", resp.StatusCode)
	}
}
