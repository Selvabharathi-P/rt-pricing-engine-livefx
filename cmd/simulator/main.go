// Command simulator imitates the upstream market feed so the engine can be
// developed, demoed and benchmarked without production credentials.
//
// It speaks the same protocol (auth -> subscribeMarketPrice -> price frames)
// and can inject the faults the engine must tolerate: duplicate frames,
// out-of-order ticks, oversized frames and periodic disconnects. It also
// sends deliberately wrong low_24h/high_24h/change_24h values, which the
// engine must ignore.
package main

import (
	"encoding/json"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type cfg struct {
	addr, path, key, secret string
	interval                time.Duration
	padBytes                int
	dupRate, oooRate        float64
	dropEvery               time.Duration
}

func load() cfg {
	f := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	dur := func(k, d string) time.Duration { v, _ := time.ParseDuration(f(k, d)); return v }
	num := func(k, d string) float64 { v, _ := strconv.ParseFloat(f(k, d), 64); return v }
	return cfg{
		addr: f("SIM_LISTEN_ADDR", ":9001"), path: f("SIM_PATH", "/ws/algo"),
		key: f("SIM_API_KEY", "demo-key"), secret: f("SIM_API_SECRET", "demo-secret"),
		interval: dur("SIM_TICK_INTERVAL", "100ms"), padBytes: int(num("SIM_PAD_BYTES", "0")),
		dupRate: num("SIM_DUP_RATE", "0.02"), oooRate: num("SIM_OOO_RATE", "0.02"),
		dropEvery: dur("SIM_DROP_EVERY", "0s"),
	}
}

type wireTick struct {
	Symbol    string  `json:"symbol"`
	Buy       float64 `json:"buy"`
	Sell      float64 `json:"sell"`
	Low24h    float64 `json:"low_24h"`
	High24h   float64 `json:"high_24h"`
	Change24h float64 `json:"change_24h"`
	Timestamp int64   `json:"timestamp"`
	Book      string  `json:"book,omitempty"` // padding to emulate large upstream frames
}

// prices is one random walk shared by all connections, so a reconnecting
// client continues the same price path instead of seeing a jump.
var (
	pricesMu sync.Mutex
	prices   = map[string]float64{}
)

func nextMid(sym string) float64 {
	pricesMu.Lock()
	defer pricesMu.Unlock()
	m, ok := prices[sym]
	if !ok {
		m = basePrice(sym)
	}
	m *= 1 + rand.NormFloat64()*2e-4
	prices[sym] = m
	return m
}

func basePrice(sym string) float64 {
	switch {
	case sym == "BTCUSD":
		return 65000
	case sym == "ETHUSD":
		return 3500
	case sym == "BNBUSD":
		return 580
	case sym == "SOLUSD":
		return 150
	case sym == "XAUUSD":
		return 2400
	case sym == "XAGUSD":
		return 30
	case sym == "LTCUSD" || sym == "BCHUSD":
		return 80
	case strings.HasSuffix(sym, "JPY"):
		return 150
	case strings.HasPrefix(sym, "USD") && sym != "USDCAD" && sym != "USDCHF":
		return 18
	default:
		return 1 + rand.Float64()*0.5
	}
}

func main() {
	c := load()
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, EnableCompression: true}

	http.HandleFunc(c.path, func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		log.Info("client connected", "remote", r.RemoteAddr)

		var auth struct{ Action, APIKey, Secret string }
		var raw map[string]any
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		if conn.ReadJSON(&raw) != nil {
			return
		}
		auth.Action, _ = raw["action"].(string)
		auth.APIKey, _ = raw["api_key"].(string)
		auth.Secret, _ = raw["secret"].(string)
		if auth.Action != "auth" || auth.APIKey != c.key || auth.Secret != c.secret {
			conn.WriteJSON(map[string]any{"action": "auth", "status": "error", "message": "invalid credentials"})
			log.Warn("auth rejected", "remote", r.RemoteAddr)
			return
		}
		conn.WriteJSON(map[string]any{"action": "auth", "status": "success"})

		var sub struct {
			Action  string   `json:"action"`
			Symbols []string `json:"symbols"`
		}
		if conn.ReadJSON(&sub) != nil || sub.Action != "subscribeMarketPrice" {
			return
		}
		conn.SetReadDeadline(time.Time{})
		conn.WriteJSON(map[string]any{"action": "subscribeMarketPrice", "status": "success", "count": len(sub.Symbols)})
		go func() { // drain pings/close frames
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		stream(conn, c, sub.Symbols, log)
	})
	log.Info("simulator listening", "addr", c.addr, "path", c.path)
	if err := http.ListenAndServe(c.addr, nil); err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}

func stream(conn *websocket.Conn, c cfg, syms []string, log *slog.Logger) {
	pad := ""
	if c.padBytes > 0 && len(syms) > 0 {
		pad = strings.Repeat("x", c.padBytes/len(syms))
	}
	t := time.NewTicker(c.interval)
	defer t.Stop()
	start := time.Now()
	var prev []byte
	for range t.C {
		if c.dropEvery > 0 && time.Since(start) > c.dropEvery {
			log.Info("injecting disconnect")
			return
		}
		now := time.Now().UnixMilli()
		frame := make([]wireTick, 0, len(syms)+1)
		for _, s := range syms {
			m := nextMid(s)
			spread := m * 5e-5
			frame = append(frame, wireTick{
				Symbol: s, Buy: round(m+spread, m), Sell: round(m-spread, m),
				Low24h: -1, High24h: -1, Change24h: 999, // garbage on purpose: must be ignored
				Timestamp: now, Book: pad,
			})
		}
		if rand.Float64() < c.oooRate && len(frame) > 0 {
			late := frame[rand.IntN(len(frame))]
			late.Timestamp -= 2000
			late.Buy, late.Sell = late.Buy*1.0001, late.Sell*1.0001
			frame = append(frame, late)
		}
		b, _ := json.Marshal(frame)
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if conn.WriteMessage(websocket.TextMessage, b) != nil {
			return
		}
		if prev != nil && rand.Float64() < c.dupRate {
			conn.WriteMessage(websocket.TextMessage, prev) // replay an old frame
		}
		prev = b
	}
}

// round keeps a realistic number of decimals for the instrument.
func round(v, ref float64) float64 {
	d := 5.0
	if ref > 100 {
		d = 2
	} else if ref > 10 {
		d = 3
	}
	p := math.Pow(10, d)
	return math.Round(v*p) / p
}
