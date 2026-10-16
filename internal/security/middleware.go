package security

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type ctxKey int

const (
	principalKey ctxKey = iota
	requestInfoKey
)

// RequestInfo is filled in while a request is handled so the access log
// (which wraps everything) can name the authenticated merchant.
type RequestInfo struct {
	ID       string // request ID (X-Request-ID)
	Merchant string // set once the caller is authenticated
}

func WithRequestInfo(ctx context.Context, ri *RequestInfo) context.Context {
	return context.WithValue(ctx, requestInfoKey, ri)
}

func RequestInfoFrom(ctx context.Context) *RequestInfo {
	ri, _ := ctx.Value(requestInfoKey).(*RequestInfo)
	return ri
}

// Principal is the authenticated caller, loaded fresh from the Directory.
type Principal struct {
	Merchant *Merchant
	Claims   *Claims
	Role     Role // derived server-side at request time
}

// WithPrincipal attaches p to ctx (for alternative authenticators such as API keys).
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	if ri := RequestInfoFrom(ctx); ri != nil && p.Merchant != nil {
		ri.Merchant = p.Merchant.ID
	}
	return context.WithValue(ctx, principalKey, p)
}

// SessionCookie carries the access token for browser clients
// (HttpOnly, SameSite=Strict, so page JavaScript can never read it).
const SessionCookie = "pe_session"

// BearerOrCookie extracts the access token from the Authorization header,
// falling back to the session cookie.
func BearerOrCookie(r *http.Request) string {
	if raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return raw
	}
	if c, err := r.Cookie(SessionCookie); err == nil {
		return c.Value
	}
	return ""
}

func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey).(*Principal)
	return p, ok
}

// Chain applies middleware so the first listed runs first (outermost).
func Chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

func deny(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	}
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// Authenticate verifies the bearer token, then loads the merchant from the
// Directory. A token is rejected if the merchant is disabled or the token
// predates the merchant's latest TokenVersion (password/KYC/role change).
// Error messages are generic so they don't help an attacker probe.
func Authenticate(tok *Tokens, dir Directory, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := BearerOrCookie(r)
			if raw == "" {
				deny(w, http.StatusUnauthorized, "authentication required")
				return
			}
			claims, err := tok.Verify(raw)
			if err != nil {
				deny(w, http.StatusUnauthorized, "authentication required")
				return
			}
			m, err := dir.Merchant(r.Context(), claims.Subject)
			if err != nil || m.Disabled || claims.Ver != m.TokenVersion {
				log.Info("token rejected", "sub", claims.Subject, "reason", reason(err, m, claims))
				deny(w, http.StatusUnauthorized, "authentication required")
				return
			}
			p := &Principal{Merchant: m, Claims: claims, Role: m.Role(time.Now())}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

func reason(err error, m *Merchant, c *Claims) string {
	switch {
	case err != nil:
		return "lookup_failed"
	case m.Disabled:
		return "disabled"
	case c.Ver != m.TokenVersion:
		return "stale_token_version"
	}
	return ""
}

// SameOrigin rejects state-changing requests whose Origin header names a
// different site. With SameSite=Strict cookies this closes CSRF; requests
// without an Origin (curl, server-to-server) carry no ambient cookies anyway.
func SameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host && o != "https://"+r.Host {
				deny(w, http.StatusForbidden, "cross-origin request rejected")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// RequireRole allows only the listed roles. Must run after Authenticate.
func RequireRole(roles ...Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				deny(w, http.StatusUnauthorized, "authentication required")
				return
			}
			for _, role := range roles {
				if p.Role == role {
					next.ServeHTTP(w, r)
					return
				}
			}
			deny(w, http.StatusForbidden, "insufficient permissions")
		})
	}
}

// RequireTrading is the gate for every trading endpoint: authenticated,
// not disabled, KYC approved and unexpired *right now*. The KYC status in
// the response tells a legitimate merchant what to do next.
func RequireTrading(log *slog.Logger) func(http.Handler) http.Handler {
	return requirePerm(log, "trading", (*Merchant).CanTrade)
}

// RequireMarketView gates read-only market data (CanViewMarket).
func RequireMarketView(log *slog.Logger) func(http.Handler) http.Handler {
	return requirePerm(log, "market data", (*Merchant).CanViewMarket)
}

func requirePerm(log *slog.Logger, what string, allowed func(*Merchant, time.Time) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				deny(w, http.StatusUnauthorized, "authentication required")
				return
			}
			now := time.Now()
			if !allowed(p.Merchant, now) {
				status := p.Merchant.EffectiveKYC(now)
				log.Warn(what+" denied", "merchant_id", p.Merchant.ID, "kyc_status", status, "role", p.Role,
					"path", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(map[string]string{"error": what + " not permitted", "kyc_status": string(status)})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// KeyedLimiter is a token-bucket rate limiter per key (IP, merchant ID,
// login email). Idle keys are evicted so memory stays bounded.
type KeyedLimiter struct {
	mu    sync.Mutex
	r     rate.Limit
	burst int
	m     map[string]*limEntry
	last  time.Time
}

type limEntry struct {
	l    *rate.Limiter
	seen time.Time
}

func NewKeyedLimiter(r rate.Limit, burst int) *KeyedLimiter {
	return &KeyedLimiter{r: r, burst: burst, m: map[string]*limEntry{}}
}

func (k *KeyedLimiter) get(key string) *rate.Limiter {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	if now.Sub(k.last) > time.Minute {
		for key, e := range k.m {
			if now.Sub(e.seen) > 10*time.Minute {
				delete(k.m, key)
			}
		}
		k.last = now
	}
	e := k.m[key]
	if e == nil {
		e = &limEntry{l: rate.NewLimiter(k.r, k.burst)}
		k.m[key] = e
	}
	e.seen = now
	return e.l
}

func (k *KeyedLimiter) Allow(key string) bool { return k.get(key).Allow() }

// Reserve reports whether key may proceed and, if not, how long to wait.
func (k *KeyedLimiter) Reserve(key string) (bool, time.Duration) {
	res := k.get(key).Reserve()
	if d := res.Delay(); d > 0 {
		res.Cancel()
		return false, d
	}
	return true, 0
}

// ClientIP uses the socket address. Only trust X-Forwarded-For when behind
// a proxy you control; otherwise it is attacker-supplied.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RateLimit limits requests per key; key defaults to the authenticated
// merchant, falling back to client IP.
func RateLimit(l *KeyedLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := "ip:" + ClientIP(r)
			if p, ok := PrincipalFrom(r.Context()); ok {
				key = "m:" + p.Merchant.ID
			}
			if ok, wait := l.Reserve(key); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
				deny(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// LoginGuard throttles login attempts by IP and by account, and locks an
// account temporarily after repeated failures (brute-force / stuffing).
type LoginGuard struct {
	perIP      *KeyedLimiter
	perAccount *KeyedLimiter
	mu         sync.Mutex
	fails      map[string]int
	lockUntil  map[string]time.Time
	maxFails   int
	lockFor    time.Duration
}

func NewLoginGuard() *LoginGuard {
	return &LoginGuard{
		perIP: NewKeyedLimiter(rate.Every(time.Second), 10), perAccount: NewKeyedLimiter(rate.Every(10*time.Second), 5),
		fails: map[string]int{}, lockUntil: map[string]time.Time{}, maxFails: 5, lockFor: 15 * time.Minute,
	}
}

// Allow is called before checking credentials.
func (g *LoginGuard) Allow(ip, account string) bool {
	account = strings.ToLower(account)
	g.mu.Lock()
	locked := time.Now().Before(g.lockUntil[account])
	g.mu.Unlock()
	return !locked && g.perIP.Allow(ip) && g.perAccount.Allow(account)
}

// Result records the outcome of a credential check.
func (g *LoginGuard) Result(account string, ok bool) {
	account = strings.ToLower(account)
	g.mu.Lock()
	defer g.mu.Unlock()
	if ok {
		delete(g.fails, account)
		return
	}
	g.fails[account]++
	if g.fails[account] >= g.maxFails {
		g.lockUntil[account] = time.Now().Add(g.lockFor)
		delete(g.fails, account)
	}
}
