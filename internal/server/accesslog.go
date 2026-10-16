package server

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pricing-engine/internal/metrics"
	"pricing-engine/internal/security"
)

// AccessLog logs every HTTP request, one line each, after it completes:
//
//	INFO  method, path, status, dur_ms, bytes, ip, merchant (if signed in), req_id
//	DEBUG the same, plus redacted query, user agent and request size
//
// Polling and probe endpoints (/healthz, /metrics, /static/*, /favicon.ico)
// are logged at DEBUG so INFO isn't flooded; 5xx responses are logged at
// ERROR. Every response carries X-Request-ID matching its log line. The
// WebSocket upgrade keeps working (Hijack is passed through); a /ws line is
// written when the socket closes, with the session length as dur_ms.
func AccessLog(log *slog.Logger, m *metrics.Set, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := requestID(r.Header.Get("X-Request-ID"))
		w.Header().Set("X-Request-ID", id)
		info := &security.RequestInfo{ID: id}
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if m != nil {
				m.HTTPRequests.Inc()
				switch {
				case rec.status >= 500:
					m.HTTP5xx.Inc()
				case rec.status >= 400:
					m.HTTP4xx.Inc()
				}
				if rec.status != http.StatusSwitchingProtocols {
					m.HTTPLatency.Observe(time.Since(start).Seconds())
				}
			}
			lvl := slog.LevelInfo
			switch {
			case rec.status >= 500:
				lvl = slog.LevelError
			case quietPath(r.URL.Path):
				lvl = slog.LevelDebug
			}
			ctx := r.Context()
			if !log.Enabled(ctx, lvl) {
				return
			}
			attrs := []slog.Attr{
				slog.String("req_id", id),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Float64("dur_ms", float64(time.Since(start).Microseconds())/1000),
				slog.Int64("bytes", rec.bytes),
				slog.String("ip", security.ClientIP(r)),
			}
			if info.Merchant != "" {
				attrs = append(attrs, slog.String("merchant", info.Merchant))
			}
			if log.Enabled(ctx, slog.LevelDebug) {
				attrs = append(attrs,
					slog.String("query", redactQuery(r.URL.RawQuery)),
					slog.String("ua", truncateUA(r.UserAgent())),
					slog.Int64("bytes_in", max(r.ContentLength, 0)))
			}
			log.LogAttrs(ctx, lvl, "http request", attrs...)
		}()
		next.ServeHTTP(rec, r.WithContext(security.WithRequestInfo(r.Context(), info)))
	})
}

func quietPath(p string) bool {
	return p == "/healthz" || p == "/metrics" || p == "/favicon.ico" || strings.HasPrefix(p, "/static/")
}

// requestID keeps a sane caller-supplied ID (for tracing through proxies),
// otherwise generates one.
func requestID(in string) string {
	if n := len(in); n > 0 && n <= 64 && strings.Trim(in, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") == "" {
		return in
	}
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// redactQuery hides values that grant access (WebSocket tickets, keys).
func redactQuery(raw string) string {
	if raw == "" {
		return ""
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return "[unparseable]"
	}
	for k := range q {
		switch strings.ToLower(k) {
		case "ticket", "token", "secret", "api_key", "key", "password":
			q.Set(k, "[REDACTED]")
		}
	}
	return q.Encode()
}

func truncateUA(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// recorder captures status and size while staying usable for WebSocket
// upgrades (Hijacker) and streaming (Flusher).
type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack not supported")
	}
	r.status, r.wroteHeader = http.StatusSwitchingProtocols, true
	return h.Hijack()
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
