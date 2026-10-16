package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) lines() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func accessLines(buf *syncBuf) []map[string]any {
	var out []map[string]any
	for _, l := range buf.lines() {
		if l["msg"] == "http request" {
			out = append(out, l)
		}
	}
	return out
}

func TestAccessLog(t *testing.T) {
	h := newHarness(t, "EURUSD")
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		buf := &syncBuf{}
		log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: level}))
		srv := httptest.NewServer(AccessLog(log, h.m, Routes(h.hub, h.eng, h.m, func(context.Context) (bool, error) { return true, nil }, nil)))
		resp, _ := http.Get(srv.URL + "/api/v1/quotes?symbols=EURUSD")
		resp.Body.Close()
		reqID := resp.Header.Get("X-Request-ID")
		resp, _ = http.Get(srv.URL + "/healthz")
		resp.Body.Close()
		resp, _ = http.Get(srv.URL + "/nope")
		resp.Body.Close()
		// WebSocket through the logging wrapper (needs Hijack), with a ticket to redact.
		ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws?ticket=wt_SECRET&symbols=EURUSD", nil)
		if err != nil {
			t.Fatalf("level %v: ws upgrade through AccessLog failed: %v", level, err)
		}
		ws.ReadMessage()
		ws.Close()
		time.Sleep(300 * time.Millisecond) // /ws is logged when the socket closes
		srv.Close()

		lines := accessLines(buf)
		byPath := map[string]map[string]any{}
		for _, l := range lines {
			byPath[l["path"].(string)] = l
		}
		q := byPath["/api/v1/quotes"]
		if q == nil || q["status"] != float64(200) || q["req_id"] != reqID || q["method"] != "GET" || q["dur_ms"] == nil {
			t.Fatalf("level %v: quotes line %v (req id %q)", level, q, reqID)
		}
		if byPath["/nope"]["status"] != float64(404) {
			t.Fatalf("level %v: 404 not logged: %v", level, byPath["/nope"])
		}
		if w := byPath["/ws"]; w == nil || w["status"] != float64(101) {
			t.Fatalf("level %v: ws line %v", level, w)
		}
		_, healthLogged := byPath["/healthz"]
		if level == slog.LevelInfo && healthLogged {
			t.Fatal("/healthz must not be logged at INFO")
		}
		if level == slog.LevelDebug {
			if !healthLogged {
				t.Fatal("/healthz missing at DEBUG")
			}
			if qs, _ := byPath["/ws"]["query"].(string); strings.Contains(qs, "wt_SECRET") || !strings.Contains(qs, "REDACTED") {
				t.Fatalf("ticket not redacted: %q", qs)
			}
		}
		if level == slog.LevelInfo && byPath["/api/v1/quotes"]["query"] != nil {
			t.Fatal("query should only be logged at DEBUG")
		}
	}
}

func TestRequestIDPassThrough(t *testing.T) {
	buf := &syncBuf{}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	h := AccessLog(log, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	for in, keep := range map[string]bool{"abc-123": true, "<script>": false, strings.Repeat("a", 65): false} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set("X-Request-ID", in)
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("X-Request-ID"); (got == in) != keep || got == "" {
			t.Errorf("X-Request-ID %q -> %q (keep=%v)", in, got, keep)
		}
	}
}
