package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"pricing-engine/internal/security"
	"pricing-engine/internal/server"
)

const (
	refreshCookie = "pe_refresh"
	accessTTL     = 15 * time.Minute
	refreshTTL    = 7 * 24 * time.Hour
	maxJSONBody   = 16 << 10
	maxKYCBody    = 3 << 20 // 2 MB document + form fields
	maxDocBytes   = 2 << 20
)

// SessionLister exposes live WebSocket sessions (the hub).
type SessionLister interface{ Sessions() []server.SessionInfo }

// PriceFunc returns the current buy/sell for a symbol (simulated fills).
type PriceFunc func(symbol string) (buy, sell float64, ok bool)

type API struct {
	svc      *Service
	tok      *security.Tokens
	refresh  *security.RefreshStore
	sessions SessionLister
	price    PriceFunc
	log      *slog.Logger
	secure   bool // Secure flag on cookies (enable behind HTTPS)

	apiLim *security.KeyedLimiter
	regLim *security.KeyedLimiter
}

func NewAPI(svc *Service, sec *Secrets, sessions SessionLister, price PriceFunc, log *slog.Logger, secureCookies bool) *API {
	return &API{
		svc: svc, sessions: sessions, price: price, log: log, secure: secureCookies,
		tok:     security.NewTokens(sec.SigningKey, "pricing-engine", "pricing-engine-web", accessTTL, security.NewRevocationList()),
		refresh: security.NewRefreshStore(refreshTTL),
		apiLim:  security.NewKeyedLimiter(20, 40),
		regLim:  security.NewKeyedLimiter(rate.Every(12*time.Second), 5),
	}
}

// Register mounts every account, merchant and admin route on mux.
func (a *API) Register(mux *http.ServeMux) {
	pub := func(h http.HandlerFunc) http.Handler {
		return security.Chain(h, security.SameOrigin, security.RateLimit(a.apiLim))
	}
	authed := func(h http.HandlerFunc) http.Handler {
		return security.Chain(h, security.SameOrigin, security.Authenticate(a.tok, a.svc, a.log), security.RateLimit(a.apiLim))
	}
	trading := func(h http.HandlerFunc) http.Handler {
		return security.Chain(h, security.SameOrigin, security.Authenticate(a.tok, a.svc, a.log),
			security.RateLimit(a.apiLim), security.RequireTrading(a.log))
	}
	admin := func(h http.HandlerFunc) http.Handler {
		return security.Chain(h, security.SameOrigin, security.Authenticate(a.tok, a.svc, a.log),
			security.RateLimit(a.apiLim), security.RequireRole(security.RoleAdmin))
	}

	mux.Handle("POST /api/v1/auth/register", pub(a.handleRegister))
	mux.Handle("POST /api/v1/auth/login", pub(a.handleLogin))
	mux.Handle("POST /api/v1/auth/refresh", pub(a.handleRefresh))
	mux.Handle("POST /api/v1/auth/logout", pub(a.handleLogout))

	mux.Handle("GET /api/v1/me", authed(a.handleMe))
	mux.Handle("POST /api/v1/kyc", authed(a.handleKYC))
	mux.Handle("POST /api/v1/apikey", trading(a.handleIssueKey))
	mux.Handle("DELETE /api/v1/apikey", authed(a.handleRevokeOwnKey))
	mux.Handle("POST /api/v1/ws-ticket", authed(a.handleTicket)) // view permission checked in IssueTicket
	mux.Handle("POST /api/v1/orders", a.TradingAccess(http.HandlerFunc(a.handleOrder)))

	mux.Handle("GET /api/v1/admin/merchants", admin(a.handleAdminList))
	mux.Handle("GET /api/v1/admin/merchants/{id}", admin(a.handleAdminDetail))
	mux.Handle("GET /api/v1/admin/merchants/{id}/document", admin(a.handleAdminDocument))
	mux.Handle("POST /api/v1/admin/merchants/{id}/kyc", admin(a.handleAdminReview))
	mux.Handle("POST /api/v1/admin/merchants/{id}/disable", admin(a.handleAdminDisable(true)))
	mux.Handle("POST /api/v1/admin/merchants/{id}/enable", admin(a.handleAdminDisable(false)))
	mux.Handle("DELETE /api/v1/admin/merchants/{id}/apikey", admin(a.handleAdminRevokeKey))
	mux.Handle("GET /api/v1/admin/sessions", admin(a.handleAdminSessions))
	mux.Handle("GET /api/v1/admin/audit", admin(a.handleAdminAudit))
}

// MarketAccess protects read-only market data: verified merchants (session
// or X-API-Key / X-API-Secret headers) and admins (session).
func (a *API) MarketAccess(next http.Handler) http.Handler {
	return a.access(next, security.RequireMarketView(a.log))
}

// TradingAccess protects trading actions: verified merchants only.
func (a *API) TradingAccess(next http.Handler) http.Handler {
	return a.access(next, security.RequireTrading(a.log))
}

func (a *API) access(next http.Handler, perm func(http.Handler) http.Handler) http.Handler {
	gate := security.Chain(next, security.RateLimit(a.apiLim), perm)
	sessionGate := security.Authenticate(a.tok, a.svc, a.log)(gate)
	return security.SameOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key == "" {
			sessionGate.ServeHTTP(w, r)
			return
		}
		acct, err := a.svc.AuthenticateKey(r.Context(), key, r.Header.Get("X-API-Secret"))
		if errors.Is(err, ErrNotVerified) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "trading not permitted"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid API credentials"})
			return
		}
		p := &security.Principal{Merchant: &acct.Merchant, Role: acct.Role(time.Now())}
		gate.ServeHTTP(w, r.WithContext(security.WithPrincipal(r.Context(), p)))
	}))
}

// ---- helpers -----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return false
	}
	return true
}

// fail maps service errors to HTTP responses without leaking internals.
func (a *API) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrEmailTaken):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrBadCredentials):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrLocked):
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrNotVerified):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	case errors.Is(err, security.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	default:
		a.log.Error("request failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

func (a *API) current(r *http.Request) (*Account, error) {
	p, _ := security.PrincipalFrom(r.Context())
	return a.svc.Account(r.Context(), p.Merchant.ID)
}

func (a *API) setSession(w http.ResponseWriter, m *security.Merchant, refreshTok string) error {
	access, err := a.tok.Issue(m)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: security.SessionCookie, Value: access, Path: "/", MaxAge: int(accessTTL.Seconds()),
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode})
	http.SetCookie(w, &http.Cookie{Name: refreshCookie, Value: refreshTok, Path: "/api/v1/auth", MaxAge: int(refreshTTL.Seconds()),
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode})
	return nil
}

func (a *API) clearSession(w http.ResponseWriter) {
	for _, c := range []struct{ name, path string }{{security.SessionCookie, "/"}, {refreshCookie, "/api/v1/auth"}} {
		http.SetCookie(w, &http.Cookie{Name: c.name, Value: "", Path: c.path, MaxAge: -1, HttpOnly: true,
			Secure: a.secure, SameSite: http.SameSiteStrictMode})
	}
}

// ---- views (explicit DTOs: never serialise Account, it holds hashes) ----------

type kycView struct {
	Status      security.KYCStatus `json:"status"`
	Reason      string             `json:"reason,omitempty"`
	ExpiresAt   *time.Time         `json:"expires_at,omitempty"`
	SubmittedAt *time.Time         `json:"submitted_at,omitempty"`
	Country     string             `json:"country,omitempty"`
	DocType     string             `json:"doc_type,omitempty"`
	HasDocument bool               `json:"has_document"`
}

type keyView struct {
	ID         string     `json:"id"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

type profileView struct {
	ID        string        `json:"id"`
	Email     string        `json:"email"`
	Name      string        `json:"name"`
	Role      security.Role `json:"role"`
	CanTrade  bool          `json:"can_trade"`
	CanView   bool          `json:"can_view_market"`
	Disabled  bool          `json:"disabled"`
	KYC       kycView       `json:"kyc"`
	APIKey    *keyView      `json:"api_key,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
}

func tptr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (a *API) profile(ctx context.Context, acct *Account) profileView {
	now := time.Now()
	v := profileView{ID: acct.ID, Email: acct.Email, Name: acct.Name, Role: acct.Role(now), CanTrade: acct.CanTrade(now), CanView: acct.CanViewMarket(now),
		Disabled: acct.Disabled, CreatedAt: acct.CreatedAt,
		KYC: kycView{Status: acct.EffectiveKYC(now), Reason: acct.KYC.Reason, ExpiresAt: tptr(acct.KYC.ExpiresAt),
			SubmittedAt: tptr(acct.KYCSubmittedAt), Country: acct.Country, DocType: acct.DocType, HasDocument: acct.Doc != nil}}
	if k, err := a.svc.KeyFor(ctx, acct.ID); err == nil && k != nil {
		v.APIKey = &keyView{ID: k.ID, CreatedAt: k.CreatedAt, LastUsedAt: tptr(k.LastUsedAt)}
	}
	return v
}

// ---- auth --------------------------------------------------------------------

func (a *API) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !a.regLim.Allow("reg:" + security.ClientIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many registrations; try later"})
		return
	}
	var in struct{ Email, Password, Name string }
	if !readJSON(w, r, &in) {
		return
	}
	acct, err := a.svc.Register(r.Context(), in.Email, in.Password, in.Name)
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := a.setSession(w, &acct.Merchant, a.refresh.New(acct.ID)); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a.profile(r.Context(), acct))
}

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Password string }
	if !readJSON(w, r, &in) {
		return
	}
	acct, err := a.svc.Login(r.Context(), in.Email, in.Password, security.ClientIP(r))
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := a.setSession(w, &acct.Merchant, a.refresh.New(acct.ID)); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.profile(r.Context(), acct))
}

func (a *API) handleRefresh(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(refreshCookie)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no session"})
		return
	}
	id, next, err := a.refresh.Rotate(c.Value)
	if err != nil {
		if errors.Is(err, security.ErrRefreshReuse) {
			a.svc.Audit(r.Context(), "system", "refresh_reuse_detected", id, "ip="+security.ClientIP(r))
		}
		a.clearSession(w)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "session expired"})
		return
	}
	acct, err := a.svc.Account(r.Context(), id)
	if err != nil || acct.Disabled {
		a.refresh.RevokeAll(id)
		a.clearSession(w)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "session expired"})
		return
	}
	if err := a.setSession(w, &acct.Merchant, next); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.profile(r.Context(), acct))
}

func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	if raw := security.BearerOrCookie(r); raw != "" {
		if c, err := a.tok.Verify(raw); err == nil {
			a.tok.Revoke(c)
			a.refresh.RevokeAll(c.Subject)
		}
	}
	a.clearSession(w)
	w.WriteHeader(http.StatusNoContent)
}

// ---- merchant ----------------------------------------------------------------

func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	acct, err := a.current(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.profile(r.Context(), acct))
}

func (a *API) handleKYC(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxKYCBody)
	if err := r.ParseMultipartForm(maxKYCBody); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "form too large or malformed (document max 2 MB)"})
		return
	}
	defer r.MultipartForm.RemoveAll()
	f := KYCForm{LegalName: r.FormValue("legal_name"), DateOfBirth: r.FormValue("date_of_birth"),
		Country: r.FormValue("country"), DocType: r.FormValue("doc_type"), DocNumber: r.FormValue("doc_number")}
	if file, _, err := r.FormFile("document"); err == nil {
		b, err := io.ReadAll(io.LimitReader(file, maxDocBytes+1))
		file.Close()
		if err != nil || len(b) > maxDocBytes {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "document max 2 MB"})
			return
		}
		f.Doc = b
	}
	p, _ := security.PrincipalFrom(r.Context())
	acct, err := a.svc.SubmitKYC(r.Context(), p.Merchant.ID, f)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.profile(r.Context(), acct))
}

func (a *API) handleIssueKey(w http.ResponseWriter, r *http.Request) {
	p, _ := security.PrincipalFrom(r.Context())
	id, secret, err := a.svc.IssueAPIKey(r.Context(), p.Merchant.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"api_key": id, "secret": secret,
		"note": "Store the secret now; it cannot be shown again."})
}

func (a *API) handleRevokeOwnKey(w http.ResponseWriter, r *http.Request) {
	acct, err := a.current(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := a.svc.RevokeAPIKey(r.Context(), acct, acct.ID); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleTicket(w http.ResponseWriter, r *http.Request) {
	p, _ := security.PrincipalFrom(r.Context())
	t, err := a.svc.IssueTicket(r.Context(), p.Merchant.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ticket": t, "expires_in": int(ticketTTL.Seconds())})
}

// handleOrder is a simulated fill: it exists to demonstrate that trading is
// enforced server-side (RequireTrading), not to implement an order book.
func (a *API) handleOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Symbol string  `json:"symbol"`
		Side   string  `json:"side"`
		Qty    float64 `json:"qty"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Symbol = strings.ToUpper(strings.TrimSpace(in.Symbol))
	if (in.Side != "buy" && in.Side != "sell") || in.Qty <= 0 || in.Qty > 1e6 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "side must be buy|sell and 0 < qty <= 1e6"})
		return
	}
	buy, sell, ok := a.price(in.Symbol)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no live price for symbol"})
		return
	}
	px := buy
	if in.Side == "sell" {
		px = sell
	}
	p, _ := security.PrincipalFrom(r.Context())
	id := newID("ord_", 9)
	a.svc.Audit(r.Context(), p.Merchant.Email, "order_filled", p.Merchant.ID,
		in.Side+" "+strconv.FormatFloat(in.Qty, 'f', -1, 64)+" "+in.Symbol+" @ "+strconv.FormatFloat(px, 'f', -1, 64))
	writeJSON(w, http.StatusCreated, map[string]any{"order_id": id, "symbol": in.Symbol, "side": in.Side, "qty": in.Qty,
		"price": px, "status": "filled", "simulated": true, "ts": time.Now().UnixMilli()})
}

// ---- admin -------------------------------------------------------------------

type adminRow struct {
	profileView
	Sessions int      `json:"sessions"`
	Symbols  []string `json:"symbols"`
}

func (a *API) sessionIndex() map[string][]server.SessionInfo {
	idx := map[string][]server.SessionInfo{}
	for _, s := range a.sessions.Sessions() {
		if s.MerchantID != "" {
			idx[s.MerchantID] = append(idx[s.MerchantID], s)
		}
	}
	return idx
}

func symbolUnion(ss []server.SessionInfo) []string {
	set := map[string]bool{}
	for _, s := range ss {
		for _, sym := range s.Symbols {
			set[sym] = true
		}
	}
	if set["*"] {
		return []string{"*"}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

const (
	defaultPageSize = 10
	maxPageSize     = 100
)

// pageParams reads ?page= (1-based) and ?size= with safe bounds.
func pageParams(r *http.Request) (page, size int) {
	page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	size, _ = strconv.Atoi(r.URL.Query().Get("size"))
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = defaultPageSize
	}
	return page, min(size, maxPageSize)
}

// pageOf clamps page to the last page and returns the slice bounds.
func pageOf(total, page, size int) (p, pages, lo, hi int) {
	pages = max(1, (total+size-1)/size)
	p = min(page, pages)
	lo = (p - 1) * size
	return p, pages, min(lo, total), min(lo+size, total)
}

type pageResp[T any] struct {
	Items []T `json:"items"`
	Total int `json:"total"`
	Page  int `json:"page"`
	Size  int `json:"size"`
	Pages int `json:"pages"`
}

// handleAdminList: GET /api/v1/admin/merchants?page=1&size=10&q=&status=
// Search and status filter apply server-side across all accounts; counts
// summarise every account (for the dashboard tiles), not just this page.
func (a *API) handleAdminList(w http.ResponseWriter, r *http.Request) {
	accts, err := a.svc.List(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	now := time.Now()
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	status := security.KYCStatus(r.URL.Query().Get("status"))
	counts := map[string]int{}
	match := make([]*Account, 0, len(accts))
	for _, acct := range accts {
		st, role := acct.EffectiveKYC(now), acct.Role(now)
		if role != security.RoleAdmin {
			counts["merchants"]++
		}
		if st == security.KYCPending {
			counts["pending"]++
		}
		if role == security.RoleVerifiedMerchant {
			counts["verified"]++
		}
		if acct.Disabled {
			counts["disabled"]++
		}
		if q != "" && !strings.Contains(acct.Email, q) && !strings.Contains(strings.ToLower(acct.Name), q) {
			continue
		}
		if status != "" && st != status {
			continue
		}
		match = append(match, acct)
	}
	sort.Slice(match, func(i, j int) bool { return match[i].CreatedAt.After(match[j].CreatedAt) })
	page, size := pageParams(r)
	page, pages, lo, hi := pageOf(len(match), page, size)
	idx := a.sessionIndex()
	rows := make([]adminRow, 0, hi-lo)
	for _, acct := range match[lo:hi] { // per-row lookups (API key) only for this page
		ss := idx[acct.ID]
		rows = append(rows, adminRow{profileView: a.profile(r.Context(), acct), Sessions: len(ss), Symbols: symbolUnion(ss)})
	}
	writeJSON(w, http.StatusOK, struct {
		pageResp[adminRow]
		Counts map[string]int `json:"counts"`
	}{pageResp[adminRow]{Items: rows, Total: len(match), Page: page, Size: size, Pages: pages}, counts})
}

func (a *API) handleAdminDetail(w http.ResponseWriter, r *http.Request) {
	admin, err := a.current(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	acct, err := a.svc.Account(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	ss := a.sessionIndex()[acct.ID]
	if ss == nil {
		ss = []server.SessionInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"profile":  a.profile(r.Context(), acct),
		"pii":      a.svc.DecryptPII(r.Context(), admin, acct), // audit-logged
		"document": acct.Doc,
		"sessions": ss,
	})
}

func (a *API) handleAdminDocument(w http.ResponseWriter, r *http.Request) {
	admin, err := a.current(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	b, mime, err := a.svc.Document(r.Context(), admin, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", mime)
	h.Set("Content-Disposition", "inline")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; object-src 'self'")
	w.Write(b)
}

func (a *API) handleAdminReview(w http.ResponseWriter, r *http.Request) {
	admin, err := a.current(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	var in struct {
		Decision  Decision `json:"decision"`
		Reason    string   `json:"reason"`
		ValidDays int      `json:"valid_days"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	acct, err := a.svc.Review(r.Context(), admin, r.PathValue("id"), in.Decision, in.Reason, in.ValidDays)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.profile(r.Context(), acct))
}

func (a *API) handleAdminDisable(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin, err := a.current(r)
		if err != nil {
			a.fail(w, err)
			return
		}
		acct, err := a.svc.SetDisabled(r.Context(), admin, r.PathValue("id"), disabled)
		if err != nil {
			a.fail(w, err)
			return
		}
		if disabled {
			a.refresh.RevokeAll(acct.ID)
		}
		writeJSON(w, http.StatusOK, a.profile(r.Context(), acct))
	}
}

func (a *API) handleAdminRevokeKey(w http.ResponseWriter, r *http.Request) {
	admin, err := a.current(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := a.svc.RevokeAPIKey(r.Context(), admin, r.PathValue("id")); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleAdminSessions(w http.ResponseWriter, r *http.Request) {
	ss := a.sessions.Sessions()
	sort.Slice(ss, func(i, j int) bool { return ss[i].ConnectedAt.Before(ss[j].ConnectedAt) })
	writeJSON(w, http.StatusOK, ss)
}

// handleAdminAudit: GET /api/v1/admin/audit?page=1&size=10 (newest first).
func (a *API) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	page, size := pageParams(r)
	ev, total, err := a.svc.AuditPage(r.Context(), (page-1)*size, size)
	if err != nil {
		a.fail(w, err)
		return
	}
	page, pages, _, _ := pageOf(total, page, size)
	if (page-1)*size >= total {
		ev = []AuditEvent{} // requested past the end
	}
	writeJSON(w, http.StatusOK, pageResp[AuditEvent]{Items: ev, Total: total, Page: page, Size: size, Pages: pages})
}
