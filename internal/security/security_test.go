package security

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/time/rate"
)

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$") {
		t.Fatalf("unexpected format %s", h)
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("hashes must be salted")
	}
	if ok, _ := VerifyPassword("correct horse battery", h); !ok {
		t.Fatal("valid password rejected")
	}
	if ok, _ := VerifyPassword("correct horse batterY", h); ok {
		t.Fatal("wrong password accepted")
	}
	if _, err := HashPassword("short"); err != ErrPasswordPolicy {
		t.Fatal("weak password accepted")
	}
	if VerifyPasswordOrDummy("anything-at-all", "") {
		t.Fatal("unknown account authenticated")
	}
}

func newTokens(t *testing.T) (*Tokens, *RevocationList) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	rl := NewRevocationList()
	return NewTokens(priv, "pricing-engine", "trading-api", 15*time.Minute, rl), rl
}

func TestTokenLifecycle(t *testing.T) {
	tok, _ := newTokens(t)
	m := &Merchant{ID: "m1"}
	raw, err := tok.Issue(m)
	if err != nil {
		t.Fatal(err)
	}
	c, err := tok.Verify(raw)
	if err != nil || c.Subject != "m1" {
		t.Fatalf("verify: %v %v", c, err)
	}
	// Tampered payload
	parts := strings.Split(raw, ".")
	parts[1] = strings.ToUpper(parts[1])
	if _, err := tok.Verify(strings.Join(parts, ".")); err == nil {
		t.Fatal("tampered token accepted")
	}
	// Expired
	tok.now = func() time.Time { return time.Now().Add(-time.Hour) }
	old, _ := tok.Issue(m)
	tok.now = time.Now
	if _, err := tok.Verify(old); err == nil {
		t.Fatal("expired token accepted")
	}
	// Revoked (logout)
	tok.Revoke(c)
	if _, err := tok.Verify(raw); err == nil {
		t.Fatal("revoked token accepted")
	}
}

func TestTokenAlgorithmConfusion(t *testing.T) {
	tok, _ := newTokens(t)
	claims := Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "m1", Issuer: "pricing-engine",
		Audience: jwt.ClaimStrings{"trading-api"}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		IssuedAt: jwt.NewNumericDate(time.Now())}, Role: RoleAdmin}
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, err := tok.Verify(none); err == nil {
		t.Fatal("alg=none accepted")
	}
	hs, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(tok.pub))
	if _, err := tok.Verify(hs); err == nil {
		t.Fatal("HS256 signed with the public key accepted")
	}
}

func TestRefreshRotationAndReuse(t *testing.T) {
	rs := NewRefreshStore(24 * time.Hour)
	t1 := rs.New("m1")
	id, t2, err := rs.Rotate(t1)
	if err != nil || id != "m1" {
		t.Fatal(err)
	}
	// Replaying t1 (stolen) kills the whole family, including t2.
	if _, _, err := rs.Rotate(t1); err != ErrRefreshReuse {
		t.Fatalf("want reuse error, got %v", err)
	}
	if _, _, err := rs.Rotate(t2); err == nil {
		t.Fatal("family not revoked after reuse")
	}
}

func TestFieldCipher(t *testing.T) {
	k1, k2 := make([]byte, 32), make([]byte, 32)
	rand.Read(k1)
	rand.Read(k2)
	fc, _ := NewFieldCipher(1, map[byte][]byte{1: k1})
	ct, _ := fc.Encrypt("merchant-1", "passport_number", []byte("X1234567"))
	if bytes.Contains(ct, []byte("X1234567")) {
		t.Fatal("plaintext visible")
	}
	if pt, err := fc.Decrypt("merchant-1", "passport_number", ct); err != nil || string(pt) != "X1234567" {
		t.Fatal("round trip failed", err)
	}
	if _, err := fc.Decrypt("merchant-2", "passport_number", ct); err == nil {
		t.Fatal("ciphertext moved to another record decrypted")
	}
	if _, err := fc.Decrypt("merchant-1", "tax_id", ct); err == nil {
		t.Fatal("ciphertext moved to another field decrypted")
	}
	// Key rotation: new active key, old data still readable.
	fc2, _ := NewFieldCipher(2, map[byte][]byte{1: k1, 2: k2})
	if _, err := fc2.Decrypt("merchant-1", "passport_number", ct); err != nil {
		t.Fatal("old key not readable after rotation")
	}
}

func TestKYCTransitions(t *testing.T) {
	m := &Merchant{ID: "m1", KYC: KYC{Status: KYCPending}}
	if err := Transition(m, KYCApproved, "m1", RoleUnverifiedMerchant, "", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("merchant self-approved")
	}
	if err := Transition(m, KYCRejected, "admin1", RoleAdmin, "", time.Time{}); err == nil {
		t.Fatal("rejection without reason")
	}
	if err := Transition(m, KYCRejected, "admin1", RoleAdmin, "document blurry", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if m.CanTrade(time.Now()) {
		t.Fatal("rejected merchant can trade")
	}
	if err := Transition(m, KYCApproved, "admin1", RoleAdmin, "", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("rejected -> approved without resubmission")
	}
	if err := Transition(m, KYCPending, "m1", RoleUnverifiedMerchant, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	v := m.TokenVersion
	if err := Transition(m, KYCApproved, "admin1", RoleAdmin, "", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !m.CanTrade(time.Now()) || m.TokenVersion != v+1 {
		t.Fatal("approved merchant cannot trade or token version not bumped")
	}
	if m.CanTrade(time.Now().Add(2 * time.Hour)) {
		t.Fatal("expired approval still allows trading")
	}
}

// --- HTTP middleware matrix --------------------------------------------------

type dir struct {
	mu sync.Mutex
	m  map[string]*Merchant
}

func (d *dir) Merchant(_ context.Context, id string) (*Merchant, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m, ok := d.m[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *m
	return &cp, nil
}

func TestTradingEndpointProtection(t *testing.T) {
	tok, _ := newTokens(t)
	now := time.Now()
	d := &dir{m: map[string]*Merchant{
		"verified":   {ID: "verified", KYC: KYC{Status: KYCApproved, ExpiresAt: now.Add(time.Hour)}},
		"pending":    {ID: "pending", KYC: KYC{Status: KYCPending}},
		"rejected":   {ID: "rejected", KYC: KYC{Status: KYCRejected}},
		"expired":    {ID: "expired", KYC: KYC{Status: KYCApproved, ExpiresAt: now.Add(-time.Minute)}},
		"disabled":   {ID: "disabled", Disabled: true, KYC: KYC{Status: KYCApproved, ExpiresAt: now.Add(time.Hour)}},
		"admin":      {ID: "admin", IsAdmin: true},
		"downgraded": {ID: "downgraded", KYC: KYC{Status: KYCApproved, ExpiresAt: now.Add(time.Hour)}},
	}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	placeOrder := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) })
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/orders", Chain(placeOrder,
		Authenticate(tok, d, log), RateLimit(NewKeyedLimiter(rate.Inf, 1)), RequireTrading(log)))
	mux.Handle("POST /api/v1/admin/kyc", Chain(placeOrder, Authenticate(tok, d, log), RequireRole(RoleAdmin)))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	issue := func(id string) string {
		m, _ := d.Merchant(context.Background(), id)
		s, _ := tok.Issue(m)
		return s
	}
	tokens := map[string]string{}
	for id := range d.m {
		tokens[id] = issue(id)
	}
	// After the token is issued, an admin downgrades this merchant: the
	// version bump must invalidate the token immediately.
	d.mu.Lock()
	d.m["downgraded"].KYC.Status = KYCPending
	d.m["downgraded"].TokenVersion++
	d.mu.Unlock()

	call := func(path, bearer string, body string) (int, string) {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v map[string]string
		json.NewDecoder(resp.Body).Decode(&v)
		return resp.StatusCode, v["kyc_status"]
	}

	cases := []struct {
		name, path, who string
		want            int
		kyc             string
	}{
		{"no token", "/api/v1/orders", "", 401, ""},
		{"garbage token", "/api/v1/orders", "x.y.z", 401, ""},
		{"verified merchant trades", "/api/v1/orders", "verified", 201, ""},
		{"pending blocked", "/api/v1/orders", "pending", 403, "pending"},
		{"rejected blocked", "/api/v1/orders", "rejected", 403, "rejected"},
		{"expired blocked", "/api/v1/orders", "expired", 403, "expired"},
		{"disabled account", "/api/v1/orders", "disabled", 401, ""},
		{"admin cannot trade", "/api/v1/orders", "admin", 403, ""},
		{"stale token after downgrade", "/api/v1/orders", "downgraded", 401, ""},
		{"merchant cannot use admin API", "/api/v1/admin/kyc", "verified", 403, ""},
		{"admin uses admin API", "/api/v1/admin/kyc", "admin", 201, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bearer := c.who
			if tk, ok := tokens[c.who]; ok {
				bearer = tk
			}
			// Privilege-escalation attempt in the body must be ignored.
			code, kyc := call(c.path, bearer, `{"role":"admin","kyc_status":"approved"}`)
			if code != c.want || (c.kyc != "" && kyc != c.kyc) {
				t.Fatalf("got %d kyc=%q, want %d kyc=%q", code, kyc, c.want, c.kyc)
			}
		})
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}), RateLimit(NewKeyedLimiter(rate.Every(time.Hour), 3)))
	codes := map[int]int{}
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		h.ServeHTTP(rec, req)
		codes[rec.Code]++
		if rec.Code == 429 && rec.Header().Get("Retry-After") == "" {
			t.Fatal("429 without Retry-After")
		}
	}
	if codes[200] != 3 || codes[429] != 2 {
		t.Fatalf("codes %v", codes)
	}
}

func TestLoginGuardLockout(t *testing.T) {
	g := NewLoginGuard()
	g.perAccount = NewKeyedLimiter(rate.Inf, 1) // isolate the lockout logic
	g.perIP = NewKeyedLimiter(rate.Inf, 1)
	for i := 0; i < 5; i++ {
		if !g.Allow("1.2.3.4", "a@b.c") {
			t.Fatalf("blocked too early at attempt %d", i)
		}
		g.Result("A@B.C", false)
	}
	if g.Allow("5.6.7.8", "a@b.c") {
		t.Fatal("account not locked after repeated failures (even from a new IP)")
	}
}

func TestRedactingHandler(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewRedactingHandler(slog.NewJSONHandler(&buf, nil)))
	log.With("api_key", "pk_live_123").Info("login",
		"email", "a@b.c", "password", "hunter2hunter2", "Authorization", "Bearer abc",
		slog.Group("kyc", "passport_number", "X123", "status", "pending"))
	out := buf.String()
	for _, secret := range []string{"pk_live_123", "hunter2hunter2", "Bearer abc", "X123"} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q leaked: %s", secret, out)
		}
	}
	if !strings.Contains(out, "pending") || !strings.Contains(out, "a@b.c") {
		t.Fatalf("non-sensitive data removed: %s", out)
	}
}
