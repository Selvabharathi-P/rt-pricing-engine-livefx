package main

import (
	"encoding/json"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"pricing-engine/internal/config"
)

// sim serves one WebSocket endpoint that behaves like the upstream feed.
//
// livefxhub mode reproduces what was observed on wss://v3-prod.livefxhub.com:
//   - no greeting; the first message must be {"action":"auth",...}
//   - any other first message:  {"event":"error","message":"Must authenticate first"}, close 4001
//   - wrong credentials:        {"event":"error","message":"invalid credentials"}, close 4001
//   - key already connected:    {"event":"error","message":"already have an active session"}, close 4003
//   - prices: one object per frame, no timestamp:
//     {"event":"marketPriceSnapshot","symbol":"AUDUSD","bid":0.69563,"ask":0.69588,"high":0.69631,"low":0.69194,"percentage":0.57}
//   - some subscribed symbols never stream (config: silent_symbols)
//
// Never observed live (so these are assumptions): the exact success replies
// to auth and subscribe. The engine only requires them not to be errors.
type sim struct {
	cfg    config.SimulatorConfig
	log    *slog.Logger
	silent map[string]bool
	up     websocket.Upgrader
	mkt    *market

	mu     sync.Mutex
	active map[string]bool // API keys with a live session
}

func newSim(cfg config.SimulatorConfig, log *slog.Logger) *sim {
	s := &sim{cfg: cfg, log: log, silent: map[string]bool{}, active: map[string]bool{}, mkt: newMarket(),
		up: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, EnableCompression: true}}
	for _, sym := range cfg.SilentSymbols {
		s.silent[sym] = true
	}
	return s
}

type cmd struct {
	Action  string   `json:"action"`
	APIKey  string   `json:"api_key"`
	Secret  string   `json:"secret"`
	Symbols []string `json:"symbols"`
}

func (s *sim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(64 << 10)
	s.log.Info("client connected", "remote", r.RemoteAddr, "mode", s.cfg.Mode)

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	var first cmd
	if conn.ReadJSON(&first) != nil {
		return
	}
	if first.Action != "auth" {
		s.reject(conn, 4001, "Must authenticate first")
		return
	}
	if first.APIKey != s.cfg.APIKey || first.Secret != s.cfg.APISecret {
		s.log.Warn("auth rejected: invalid credentials", "remote", r.RemoteAddr)
		s.reject(conn, 4001, "invalid credentials")
		return
	}
	if s.cfg.OneSessionPerKey {
		s.mu.Lock()
		busy := s.active[first.APIKey]
		if !busy {
			s.active[first.APIKey] = true
		}
		s.mu.Unlock()
		if busy {
			s.log.Warn("auth rejected: session already active", "remote", r.RemoteAddr)
			s.reject(conn, 4003, "already have an active session")
			return
		}
		defer func() { s.mu.Lock(); delete(s.active, first.APIKey); s.mu.Unlock() }()
	}
	conn.SetReadDeadline(time.Time{})
	if s.cfg.Mode == "legacy" {
		conn.WriteJSON(map[string]any{"action": "auth", "status": "success"})
		s.legacy(conn)
		return
	}
	conn.WriteJSON(map[string]any{"event": "auth", "status": "success"})
	s.live(conn)
}

func (s *sim) reject(conn *websocket.Conn, code int, msg string) {
	conn.WriteJSON(map[string]string{"event": "error", "message": msg})
	conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, msg), time.Now().Add(time.Second))
	time.Sleep(50 * time.Millisecond) // let the close frame reach the client
}

// readCommands forwards subscribe requests; it exits when the client goes.
func (s *sim) readCommands(conn *websocket.Conn, subs chan<- []string, done chan<- struct{}) {
	defer close(done)
	for {
		var c cmd
		if err := conn.ReadJSON(&c); err != nil {
			return
		}
		switch c.Action {
		case "subscribeMarketPrice":
			subs <- c.Symbols
		default:
			subs <- nil // unknown action: answered by the writer
		}
	}
}

// live streams LiveFXHub-style frames: one symbol per frame, no timestamp.
func (s *sim) live(conn *websocket.Conn) {
	subs := make(chan []string, 4)
	done := make(chan struct{})
	go s.readCommands(conn, subs, done)
	var symbols []string // subscribed and not silent
	tick := time.NewTicker(s.cfg.TickInterval.Duration)
	defer tick.Stop()
	start := time.Now()
	var last []byte
	send := func(b []byte) bool {
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteMessage(websocket.TextMessage, b) == nil
	}
	frame := func(sym string) []byte {
		bid, ask, hi, lo, pct := s.mkt.step(sym, s.cfg.Volatility)
		b, _ := json.Marshal(struct {
			Event      string  `json:"event"`
			Symbol     string  `json:"symbol"`
			Bid        float64 `json:"bid"`
			Ask        float64 `json:"ask"`
			High       float64 `json:"high"`
			Low        float64 `json:"low"`
			Percentage float64 `json:"percentage"`
		}{"marketPriceSnapshot", sym, bid, ask, hi, lo, pct})
		return b
	}
	for {
		select {
		case <-done:
			return
		case req := <-subs:
			if req == nil {
				conn.WriteJSON(map[string]string{"event": "error", "message": "unknown action"})
				continue
			}
			symbols = symbols[:0]
			for _, sym := range req {
				if sym = strings.ToUpper(strings.TrimSpace(sym)); sym != "" && !s.silent[sym] {
					symbols = append(symbols, sym)
				}
			}
			conn.WriteJSON(map[string]any{"event": "subscribed", "symbols": len(req)})
			for _, sym := range symbols { // initial snapshot of every streaming symbol
				if !send(frame(sym)) {
					return
				}
			}
		case <-tick.C:
			if s.cfg.DropEvery.Duration > 0 && time.Since(start) > s.cfg.DropEvery.Duration {
				s.log.Info("injecting disconnect")
				return
			}
			for _, sym := range symbols {
				if rand.Float64() >= s.cfg.UpdateRatio {
					continue
				}
				b := frame(sym)
				if !send(b) {
					return
				}
				if last != nil && rand.Float64() < s.cfg.DuplicateRate {
					send(last) // replay the previous frame (duplicate)
				}
				last = b
			}
		}
	}
}

// legacy streams the assignment brief's format: arrays of
// {symbol,buy,sell,low_24h,high_24h,change_24h,timestamp} with bogus 24h
// fields, padded to large frames, plus duplicates and late ticks.
func (s *sim) legacy(conn *websocket.Conn) {
	subs := make(chan []string, 4)
	done := make(chan struct{})
	go s.readCommands(conn, subs, done)
	var symbols []string
	select {
	case symbols = <-subs:
	case <-done:
		return
	}
	conn.WriteJSON(map[string]any{"action": "subscribeMarketPrice", "status": "success", "count": len(symbols)})
	type wire struct {
		Symbol    string  `json:"symbol"`
		Buy       float64 `json:"buy"`
		Sell      float64 `json:"sell"`
		Low24h    float64 `json:"low_24h"`
		High24h   float64 `json:"high_24h"`
		Change24h float64 `json:"change_24h"`
		Timestamp int64   `json:"timestamp"`
		Book      string  `json:"book,omitempty"`
	}
	pad := ""
	if s.cfg.PadBytes > 0 && len(symbols) > 0 {
		pad = strings.Repeat("x", s.cfg.PadBytes/len(symbols))
	}
	tick := time.NewTicker(s.cfg.TickInterval.Duration)
	defer tick.Stop()
	start := time.Now()
	var prev []byte
	for {
		select {
		case <-done:
			return
		case <-subs:
		case <-tick.C:
			if s.cfg.DropEvery.Duration > 0 && time.Since(start) > s.cfg.DropEvery.Duration {
				return
			}
			now := time.Now().UnixMilli()
			frame := make([]wire, 0, len(symbols)+1)
			for _, sym := range symbols {
				bid, ask, _, _, _ := s.mkt.step(sym, s.cfg.Volatility)
				frame = append(frame, wire{Symbol: sym, Buy: ask, Sell: bid, Low24h: -1, High24h: -1, Change24h: 999, Timestamp: now, Book: pad})
			}
			if len(frame) > 0 && rand.Float64() < s.cfg.OutOfOrderRate {
				late := frame[rand.IntN(len(frame))]
				late.Timestamp -= 2000
				frame = append(frame, late)
			}
			b, _ := json.Marshal(frame)
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if conn.WriteMessage(websocket.TextMessage, b) != nil {
				return
			}
			if prev != nil && rand.Float64() < s.cfg.DuplicateRate {
				conn.WriteMessage(websocket.TextMessage, prev)
			}
			prev = b
		}
	}
}

// ---- prices -----------------------------------------------------------------

// market is one random walk per symbol shared by all connections, so a
// reconnecting client continues the same price path.
type market struct {
	mu sync.Mutex
	px map[string]*instrument
}

type instrument struct {
	mid, open, hi, lo float64
	spread            float64 // as a fraction of mid
	decimals          int
}

func newMarket() *market { return &market{px: map[string]*instrument{}} }

// basePrices are close to the levels seen on the live feed.
var basePrices = map[string]float64{
	"AUDUSD": 0.6960, "AUDCAD": 0.9858, "AUDCHF": 0.5735, "AUDJPY": 109.97, "AUDNZD": 1.2279, "AUDSGD": 0.9036,
	"CADCHF": 0.5820, "CADJPY": 111.61, "CHFJPY": 191.79, "CHFSGD": 1.5825, "EURAUD": 1.6170, "EURCAD": 1.5933,
	"EURCHF": 0.9272, "EURGBP": 0.8438, "EURJPY": 177.82, "EURNZD": 1.9869, "EURUSD": 1.1253, "GBPAUD": 1.9162,
	"GBPCAD": 1.8880, "GBPCHF": 1.0990, "GBPJPY": 210.70, "GBPNZD": 2.3546, "GBPUSD": 1.3337, "NZDCAD": 0.8020,
	"NZDCHF": 0.4667, "NZDJPY": 89.50, "NZDUSD": 0.5664, "USDCAD": 1.4160, "USDCHF": 0.8240, "USDCNH": 7.1050,
	"USDJPY": 158.02, "USDMXN": 18.512, "USDNOK": 10.215, "USDPLN": 3.6420, "USDSEK": 9.6110, "USDSGD": 1.2905,
	"USDTRY": 49.612, "USDZAR": 17.605, "EURTRY": 55.782, "GBPTRY": 66.143, "NOKJPY": 15.470, "SEKJPY": 16.441,
	"SGDJPY": 122.45, "ZARJPY": 8.975, "XAUUSD": 4182.3, "XAGUSD": 52.04, "BTCUSD": 82358.0, "ETHUSD": 1923.1,
	"BNBUSD": 731.18, "SOLUSD": 155.42, "XRPUSD": 2.3105, "ADAUSD": 0.2311, "DOGUSD": 0.1702, "DOTUSD": 3.512,
	"LTCUSD": 82.15, "BCHUSD": 401.2, "XLMUSD": 0.2501, "TRXUSD": 0.2915, "UNIUSD": 6.811, "FILUSD": 2.402,
	"AVXUSD": 6.541,
}

var cryptoBases = []string{"BTC", "ETH", "BNB", "SOL", "XRP", "ADA", "DOG", "DOT", "LTC", "BCH", "XLM", "TRX", "UNI", "FIL", "AVX"}

func newInstrument(sym string) *instrument {
	mid, ok := basePrices[sym]
	if !ok {
		mid = 1 + rand.Float64()*0.5
	}
	spread := 0.00002 // FX
	switch {
	case strings.HasPrefix(sym, "XAU") || strings.HasPrefix(sym, "XAG"):
		spread = 0.00008
	case func() bool {
		for _, c := range cryptoBases {
			if strings.HasPrefix(sym, c) {
				return true
			}
		}
		return false
	}():
		spread = 0.0004
	}
	d := 5
	switch {
	case mid >= 1000:
		d = 2
	case mid >= 100:
		d = 3
	case mid >= 10:
		d = 4
	}
	return &instrument{mid: mid, open: mid, hi: mid, lo: mid, spread: spread, decimals: d}
}

func round(v float64, d int) float64 {
	p := math.Pow(10, float64(d))
	return math.Round(v*p) / p
}

// step moves the symbol's mid by a random walk and returns the quote plus
// the simulator's own session high/low/% (the engine ignores these, as the
// assignment requires).
func (m *market) step(sym string, vol float64) (bid, ask, hi, lo, pct float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in := m.px[sym]
	if in == nil {
		in = newInstrument(sym)
		m.px[sym] = in
	}
	in.mid *= 1 + rand.NormFloat64()*vol
	half := in.mid * in.spread / 2
	bid, ask = round(in.mid-half, in.decimals), round(in.mid+half, in.decimals)
	if ask <= bid { // keep at least one tick of spread after rounding
		ask = bid + math.Pow(10, -float64(in.decimals))
	}
	in.hi, in.lo = math.Max(in.hi, in.mid), math.Min(in.lo, in.mid)
	return bid, round(ask, in.decimals), round(in.hi, in.decimals), round(in.lo, in.decimals),
		math.Round((in.mid-in.open)/in.open*100*100) / 100
}
