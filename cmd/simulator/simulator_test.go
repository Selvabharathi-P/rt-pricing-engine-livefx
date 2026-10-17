package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"pricing-engine/internal/config"
	"pricing-engine/internal/feed"
	"pricing-engine/internal/metrics"
	"pricing-engine/internal/model"
)

func testCfg(mode string) config.SimulatorConfig {
	c := config.Defaults().Simulator
	c.Mode, c.APIKey, c.APISecret = mode, "k", "s"
	c.TickInterval.Duration = 20 * time.Millisecond
	c.UpdateRatio, c.DuplicateRate = 1, 0
	return c
}

func start(t *testing.T, cfg config.SimulatorConfig) string {
	t.Helper()
	srv := httptest.NewServer(newSim(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// readUntilClose returns the text messages received and the close code.
func readUntilClose(t *testing.T, c *websocket.Conn) ([]string, int) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var msgs []string
	for {
		_, b, err := c.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				return msgs, ce.Code
			}
			return msgs, 0
		}
		msgs = append(msgs, string(b))
	}
}

func TestLiveProtocolErrorsMatchLiveFXHub(t *testing.T) {
	url := start(t, testCfg("livefxhub"))
	for name, tc := range map[string]struct {
		first    any
		wantMsg  string
		wantCode int
	}{
		"command before auth": {map[string]any{"action": "subscribeMarketPrice", "symbols": []string{"EURUSD"}}, `{"event":"error","message":"Must authenticate first"}`, 4001},
		"wrong credentials":   {map[string]string{"action": "auth", "api_key": "k", "secret": "nope"}, `{"event":"error","message":"invalid credentials"}`, 4001},
	} {
		t.Run(name, func(t *testing.T) {
			c := dial(t, url)
			c.WriteJSON(tc.first)
			msgs, code := readUntilClose(t, c)
			if len(msgs) == 0 || strings.TrimSpace(msgs[0]) != tc.wantMsg || code != tc.wantCode {
				t.Fatalf("got %v close %d", msgs, code)
			}
		})
	}
}

func TestOneSessionPerKey(t *testing.T) {
	url := start(t, testCfg("livefxhub"))
	first := dial(t, url)
	first.WriteJSON(map[string]string{"action": "auth", "api_key": "k", "secret": "s"})
	first.SetReadDeadline(time.Now().Add(time.Second))
	if _, b, err := first.ReadMessage(); err != nil || strings.Contains(string(b), "error") {
		t.Fatalf("first auth: %s %v", b, err)
	}
	second := dial(t, url)
	second.WriteJSON(map[string]string{"action": "auth", "api_key": "k", "secret": "s"})
	msgs, code := readUntilClose(t, second)
	if len(msgs) == 0 || !strings.Contains(msgs[0], "already have an active session") || code != 4003 {
		t.Fatalf("second session: %v close %d", msgs, code)
	}
	first.Close() // releasing the first session frees the key
	time.Sleep(100 * time.Millisecond)
	third := dial(t, url)
	third.WriteJSON(map[string]string{"action": "auth", "api_key": "k", "secret": "s"})
	third.SetReadDeadline(time.Now().Add(time.Second))
	if _, b, err := third.ReadMessage(); err != nil || strings.Contains(string(b), "error") {
		t.Fatalf("key not released after disconnect: %s %v", b, err)
	}
}

// Frames look exactly like the live feed: one object, bid/ask, upstream
// 24h fields, no timestamp; silent symbols never appear.
func TestLiveFrameShape(t *testing.T) {
	url := start(t, testCfg("livefxhub"))
	c := dial(t, url)
	c.WriteJSON(map[string]string{"action": "auth", "api_key": "k", "secret": "s"})
	c.WriteJSON(map[string]any{"action": "subscribeMarketPrice", "symbols": []string{"EURUSD", "XAUUSD", "GBPCAD"}})
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	seen := map[string]int{}
	for n := 0; n < 40; n++ {
		_, b, err := c.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		json.Unmarshal(b, &m)
		if m["event"] != "marketPriceSnapshot" {
			continue
		}
		for _, k := range []string{"symbol", "bid", "ask", "high", "low", "percentage"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("missing %s in %s", k, b)
			}
		}
		if _, ok := m["timestamp"]; ok {
			t.Fatalf("live frames have no timestamp: %s", b)
		}
		if m["ask"].(float64) <= m["bid"].(float64) {
			t.Fatalf("ask must exceed bid: %s", b)
		}
		seen[m["symbol"].(string)]++
	}
	if seen["EURUSD"] == 0 || seen["XAUUSD"] == 0 || seen["GBPCAD"] != 0 {
		t.Fatalf("symbols seen: %v (GBPCAD is silent)", seen)
	}
}

// The engine's own LiveFXHub source connects to the simulator unchanged.
func TestEngineSourceAgainstSimulator(t *testing.T) {
	url := start(t, testCfg("livefxhub"))
	out := make(chan []model.Tick, 64)
	var (
		mu       sync.Mutex
		statuses []string
	)
	src, err := feed.New("livefxhub", feed.Config{Name: "simulator", URL: url, APIKey: "k", APISecret: "s",
		Symbols: []string{"EURUSD", "BTCUSD", "GBPCAD"}}, feed.Deps{Out: out, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: metrics.NewSet(), OnStatus: func(up bool, reason string) { mu.Lock(); statuses = append(statuses, reason); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	go src.Run(ctx)
	got := map[string]model.Tick{}
	for {
		select {
		case batch := <-out:
			for _, tk := range batch {
				got[tk.Symbol] = tk
			}
			continue
		case <-ctx.Done():
		}
		break
	}
	if got["EURUSD"].Buy <= got["EURUSD"].Sell || got["BTCUSD"].Buy < 10000 {
		t.Fatalf("ticks: %+v", got)
	}
	if _, silent := got["GBPCAD"]; silent {
		t.Fatal("silent symbol streamed")
	}
	if eu := got["EURUSD"].Buy; eu < 0.9 || eu > 1.4 {
		t.Fatalf("EURUSD %.5f outside a realistic range", eu)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(statuses) == 0 || statuses[0] != feed.StatusConnected {
		t.Fatalf("status: %v", statuses)
	}
}

func TestLegacyMode(t *testing.T) {
	cfg := testCfg("legacy")
	cfg.PadBytes = 1000
	url := start(t, cfg)
	c := dial(t, url)
	c.WriteJSON(map[string]string{"action": "auth", "api_key": "k", "secret": "s"})
	c.WriteJSON(map[string]any{"action": "subscribeMarketPrice", "symbols": []string{"EURUSD"}})
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for i := 0; i < 5; i++ {
		_, b, err := c.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(string(b), "[") {
			ticks, err := feed.Parse(b, time.Now().UnixNano())
			if err != nil || len(ticks) == 0 || !strings.Contains(string(b), `"low_24h":-1`) {
				t.Fatalf("legacy frame: %v %s", err, b[:120])
			}
			return
		}
	}
	t.Fatal("no legacy price frame")
}
