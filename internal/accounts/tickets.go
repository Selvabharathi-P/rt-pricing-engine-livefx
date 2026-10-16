package accounts

import (
	"net/http"
	"sync"
	"time"
)

const ticketTTL = 30 * time.Second

// tickets are single-use, short-lived tokens that let a logged-in browser
// open the WebSocket without exposing the API secret to page JavaScript.
type tickets struct {
	mu sync.Mutex
	m  map[string]ticket
}

type ticket struct {
	merchantID string
	exp        time.Time
}

func newTickets() *tickets { return &tickets{m: map[string]ticket{}} }

func (t *tickets) issue(merchantID string) string {
	id := newID("wt_", 24)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for k, v := range t.m {
		if now.After(v.exp) {
			delete(t.m, k)
		}
	}
	t.m[id] = ticket{merchantID: merchantID, exp: now.Add(ticketTTL)}
	return id
}

func (t *tickets) redeem(id string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.m[id]
	delete(t.m, id) // single use, even if expired
	if !ok || time.Now().After(v.exp) {
		return "", false
	}
	return v.merchantID, true
}

func httpDetect(b []byte) string { return http.DetectContentType(b) }
