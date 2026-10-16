package server

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pricing-engine/internal/engine"
	"pricing-engine/internal/metrics"
)

//go:embed web
var webFS embed.FS

type HealthFunc func(ctx context.Context) (upstream bool, storeErr error)

// Routes wires the HTTP API and pages:
//
//	GET /  /login  /account  /admin   UI pages (embedded, no external assets)
//	GET /static/*                     page scripts and styles
//	GET /ws                           downstream WebSocket
//	GET /healthz                      liveness + dependency status
//	GET /metrics                      Prometheus text format
//	GET /api/v1/quotes                current quotes (?symbols=EURUSD,GBPUSD)
//	GET /api/v1/candles               1m OHLC (?symbol=EURUSD&limit=60)
//
// protect, when non-nil, gates the market-data REST endpoints (verified
// merchants only). The returned mux is extended by the accounts API.
func Routes(h *Hub, eng *engine.Engine, m *metrics.Set, health HealthFunc, protect func(http.Handler) http.Handler) *http.ServeMux {
	if protect == nil {
		protect = func(h http.Handler) http.Handler { return h }
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", h.ServeWS)

	for path, file := range map[string]string{"GET /{$}": "web/index.html", "GET /login": "web/login.html",
		"GET /account": "web/account.html", "GET /admin": "web/admin.html"} {
		page, err := webFS.ReadFile(file)
		if err != nil {
			panic(err) // embedded at build time; missing means a broken build
		}
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			securityHeaders(w, r)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(page)
		})
	}
	static, _ := fs.Sub(webFS, "web/static")
	files := http.StripPrefix("/static/", http.FileServerFS(static))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w, r)
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))

	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		up, serr := health(ctx)
		body := map[string]any{"status": "ok", "upstream": up, "store": "ok", "clients": h.Clients()}
		code := http.StatusOK
		if serr != nil {
			// Still serving from memory: degraded, not down.
			body["status"], body["store"] = "degraded", serr.Error()
		}
		writeJSON(w, code, body)
	})

	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		m.Registry.WritePrometheus(w)
	})

	mux.Handle("GET /api/v1/quotes", protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var syms []string
		if s := r.URL.Query().Get("symbols"); s != "" {
			for _, x := range strings.Split(s, ",") {
				syms = append(syms, strings.ToUpper(strings.TrimSpace(x)))
			}
		}
		qs := eng.Snapshot(syms)
		rows := make([]map[string]any, len(qs))
		for i, q := range qs {
			rows[i] = map[string]any{"symbol": q.Symbol, "buy": q.Buy, "sell": q.Sell, "high24h": q.High24h,
				"low24h": q.Low24h, "change24hPct": q.ChangePct, "ts": q.TS}
		}
		writeJSON(w, http.StatusOK, rows)
	})))

	mux.Handle("GET /api/v1/candles", protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sym := strings.ToUpper(r.URL.Query().Get("symbol"))
		if !eng.Has(sym) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown symbol"})
			return
		}
		limit := 60
		if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
			limit = min(l, engine.WindowBuckets)
		}
		bs := eng.Candles(sym, limit)
		rows := make([][6]any, len(bs))
		for i, b := range bs {
			rows[i] = [6]any{b.Minute * engine.BucketMillis, b.Open, b.High, b.Low, b.Close, b.Count}
		}
		writeJSON(w, http.StatusOK, map[string]any{"symbol": sym, "interval": "1m",
			"fields": []string{"ts", "open", "high", "low", "close", "ticks"}, "candles": rows})
	})))
	return mux
}

// securityHeaders: strict CSP (no inline script or style), no framing.
// ws(s)://host is listed explicitly: older browsers don't treat it as 'self'.
func securityHeaders(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	hd.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; "+
		"connect-src 'self' ws://"+r.Host+" wss://"+r.Host+"; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "no-referrer")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
