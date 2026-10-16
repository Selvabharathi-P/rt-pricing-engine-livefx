package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"

	"pricing-engine/internal/engine"
	"pricing-engine/internal/metrics"
	"pricing-engine/internal/model"
	"pricing-engine/internal/security"
	"pricing-engine/internal/server"
)

// 1x1 PNG
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

type env struct {
	t   *testing.T
	mr  *miniredis.Miniredis
	svc *Service
	hub *server.Hub
	srv *httptest.Server
	eng *engine.Engine
}

func newEnv(t *testing.T) *env {
	t.Helper()
	mr := miniredis.RunT(t)
	sec, err := LoadOrCreateSecrets(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rc.Close() })
	svc, err := NewService(NewStore(rc, log), sec, log)
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.NewSet()
	eng := engine.New([]string{"EURUSD", "GBPUSD"}, m)
	eng.Apply([]model.Tick{{Symbol: "EURUSD", Buy: 1.1001, Sell: 1.0999, TS: time.Now().UnixMilli()}})
	opts := server.DefaultOptions()
	opts.RevalidateEvery = 0
	hub := server.NewHub(opts, eng, m, log)
	hub.SetAuthorizer(HubAuthorizer{S: svc})
	svc.SetKicker(hub)
	price := func(sym string) (float64, float64, bool) {
		qs := eng.Snapshot([]string{sym})
		if len(qs) == 0 {
			return 0, 0, false
		}
		return qs[0].Buy, qs[0].Sell, true
	}
	api := NewAPI(svc, sec, hub, price, log, false)
	mux := server.Routes(hub, eng, m, func(context.Context) (bool, error) { return true, nil }, api.MarketAccess)
	api.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if _, err := svc.EnsureAdmin(context.Background(), "admin@example.com", "admin-password-123"); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, mr: mr, svc: svc, hub: hub, srv: srv, eng: eng}
}

type client struct {
	e *env
	c *http.Client
}

func (e *env) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, c: &http.Client{Jar: jar}}
}

// do behaves like the browser helper in app.js: on 401 it refreshes the
// session once (token version bumps are expected) and retries.
func (c *client) do(method, path string, body any, hdr map[string]string) (int, map[string]any) {
	c.e.t.Helper()
	code, out := c.raw(method, path, body, hdr)
	if code == 401 && !strings.HasPrefix(path, "/api/v1/auth/") {
		if rc, _ := c.raw("POST", "/api/v1/auth/refresh", nil, nil); rc == 200 {
			if mb, ok := body.(*multipartBody); ok {
				body = mb.again()
			}
			return c.raw(method, path, body, hdr)
		}
	}
	return code, out
}

func (c *client) raw(method, path string, body any, hdr map[string]string) (int, map[string]any) {
	c.e.t.Helper()
	var rd io.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
	case *multipartBody:
		rd, ct = &b.buf, b.ct
	default:
		js, _ := json.Marshal(b)
		rd, ct = bytes.NewReader(js), "application/json"
	}
	req, _ := http.NewRequest(method, c.e.srv.URL+path, rd)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.c.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

type multipartBody struct {
	buf  bytes.Buffer
	ct   string
	data []byte
}

func (m *multipartBody) again() *multipartBody {
	n := &multipartBody{ct: m.ct, data: m.data}
	n.buf.Write(m.data)
	return n
}

func kycForm(doc []byte) *multipartBody {
	mb := &multipartBody{}
	w := multipart.NewWriter(&mb.buf)
	for k, v := range map[string]string{"legal_name": "Asha Merchant", "date_of_birth": "1990-04-12",
		"country": "in", "doc_type": "passport", "doc_number": "X1234567"} {
		w.WriteField(k, v)
	}
	if doc != nil {
		fw, _ := w.CreateFormFile("document", "id.png")
		fw.Write(doc)
	}
	w.Close()
	mb.ct = w.FormDataContentType()
	mb.data = append([]byte(nil), mb.buf.Bytes()...)
	return mb
}

func (e *env) ws(t *testing.T, query string) *websocket.Conn {
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.srv.URL, "http")+"/ws"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func readType(t *testing.T, c *websocket.Conn, want string) map[string]any {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, b, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for %q: %v", want, err)
		}
		var m map[string]any
		json.Unmarshal(b, &m)
		if m["t"] == want {
			return m
		}
	}
}

func closeCode(t *testing.T, c *websocket.Conn) int {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				return ce.Code
			}
			t.Fatalf("expected close frame, got %v", err)
		}
	}
}

// TestMerchantLifecycle walks the whole story through the real HTTP API
// and WebSocket: register -> blocked -> KYC -> admin approval -> API key ->
// authenticated feed -> admin re-verification -> sessions cut off.
func TestMerchantLifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	merchant, admin := e.client(), e.client()

	// 1. Register: unverified, cannot trade.
	code, me := merchant.do("POST", "/api/v1/auth/register", map[string]string{
		"email": "Asha@Example.com", "password": "correct-horse-battery", "name": "Asha Traders"}, nil)
	if code != 201 || me["role"] != "unverified_merchant" || me["can_trade"] != false {
		t.Fatalf("register: %d %v", code, me)
	}
	if code, _ := e.client().do("POST", "/api/v1/auth/register", map[string]string{
		"email": "asha@example.com", "password": "another-password-1", "name": "Dup"}, nil); code != 409 {
		t.Fatalf("duplicate email: %d", code)
	}
	id := me["id"].(string)

	// 2. Everything trading-related is refused server-side.
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/v1/apikey"}, {"POST", "/api/v1/ws-ticket"}, {"GET", "/api/v1/quotes"}, {"GET", "/api/v1/candles?symbol=EURUSD"},
	} {
		if code, _ := merchant.do(c.method, c.path, nil, nil); code != 403 {
			t.Fatalf("%s %s as unverified: %d, want 403", c.method, c.path, code)
		}
	}
	code, body := merchant.do("POST", "/api/v1/orders", map[string]any{"symbol": "EURUSD", "side": "buy", "qty": 1}, nil)
	if code != 403 || body["kyc_status"] != "not_submitted" {
		t.Fatalf("order as unverified: %d %v", code, body)
	}
	if code, _ := merchant.do("GET", "/api/v1/admin/merchants", nil, nil); code != 403 {
		t.Fatalf("merchant reached admin API: %d", code)
	}
	if code, _ := e.client().do("GET", "/api/v1/me", nil, nil); code != 401 {
		t.Fatalf("anonymous /me: %d", code)
	}

	// 3. KYC submission: validated, then encrypted at rest.
	bad := kycForm([]byte("MZ\x90\x00 not a document"))
	if code, _ := merchant.do("POST", "/api/v1/kyc", bad, nil); code != 400 {
		t.Fatalf("executable accepted as KYC document: %d", code)
	}
	code, me = merchant.do("POST", "/api/v1/kyc", kycForm(pngBytes), nil)
	if code != 200 || me["kyc"].(map[string]any)["status"] != "pending" {
		t.Fatalf("kyc submit: %d %v", code, me)
	}
	raw := allValues(e.mr)
	for _, secret := range []string{"X1234567", "Asha Merchant", "1990-04-12"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("PII %q stored in plaintext", secret)
		}
	}
	if strings.Contains(raw, "correct-horse-battery") || !strings.Contains(raw, "$argon2id$") {
		t.Fatal("password not hashed with Argon2id")
	}
	if code, _ := merchant.do("POST", "/api/v1/apikey", nil, nil); code != 403 {
		t.Fatalf("pending merchant got an API key: %d", code)
	}

	// 4. Admin reviews: sees decrypted PII + document, approves.
	if code, _ := admin.do("POST", "/api/v1/auth/login", map[string]string{"email": "admin@example.com", "password": "admin-password-123"}, nil); code != 200 {
		t.Fatalf("admin login: %d", code)
	}
	code, det := admin.do("GET", "/api/v1/admin/merchants/"+id, nil, nil)
	if code != 200 || det["pii"].(map[string]any)["document_number"] != "X1234567" {
		t.Fatalf("admin detail: %d %v", code, det)
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/v1/admin/merchants/"+id+"/document", nil)
	resp, err := admin.c.Do(req)
	if err != nil || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("document download: %v %v", err, resp)
	}
	doc, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(doc, pngBytes) {
		t.Fatal("document round-trip mismatch")
	}
	if code, _ := admin.do("POST", "/api/v1/admin/merchants/"+id+"/kyc", map[string]any{"decision": "reject", "reason": ""}, nil); code != 400 {
		t.Fatalf("rejection without reason accepted: %d", code)
	}
	if code, _ := admin.do("POST", "/api/v1/admin/merchants/"+id+"/kyc", map[string]any{"decision": "approve", "valid_days": 30}, nil); code != 200 {
		t.Fatalf("approve: %d", code)
	}

	// 5. The approval bumped the token version: the old session is refused
	// until refreshed, and the refreshed session reflects the new role.
	if code, _ := merchant.raw("GET", "/api/v1/me", nil, nil); code != 401 {
		t.Fatalf("stale session after KYC change: %d", code)
	}
	if code, _ := merchant.raw("POST", "/api/v1/auth/refresh", nil, nil); code != 200 {
		t.Fatalf("refresh: %d", code)
	}
	if _, me = merchant.do("GET", "/api/v1/me", nil, nil); me["role"] != "verified_merchant" || me["can_trade"] != true {
		t.Fatalf("after approval: %v", me)
	}

	// 6. API key: issued once, secret never stored in plaintext.
	code, key := merchant.do("POST", "/api/v1/apikey", nil, nil)
	if code != 201 || !strings.HasPrefix(key["api_key"].(string), "pk_") || !strings.HasPrefix(key["secret"].(string), "sk_") {
		t.Fatalf("issue key: %d %v", code, key)
	}
	pk, sk := key["api_key"].(string), key["secret"].(string)
	if strings.Contains(allValues(e.mr), sk) {
		t.Fatal("API secret stored in plaintext")
	}

	// 7. WebSocket: auth required, wrong secret refused, right one streams.
	anon := e.ws(t, "?symbols=EURUSD")
	if w := readType(t, anon, "welcome"); w["auth_required"] != true {
		t.Fatalf("welcome %v", w)
	}
	anon.WriteJSON(map[string]any{"action": "subscribe", "symbols": []string{"EURUSD"}})
	if c := closeCode(t, anon); c != server.CloseAuthRequired {
		t.Fatalf("subscribe before auth: close %d", c)
	}
	wrong := e.ws(t, "")
	readType(t, wrong, "welcome")
	wrong.WriteJSON(map[string]string{"action": "auth", "api_key": pk, "secret": "sk_wrong"})
	if c := closeCode(t, wrong); c != server.CloseAuthRequired {
		t.Fatalf("wrong secret: close %d", c)
	}
	feed := e.ws(t, "?symbols=EURUSD")
	readType(t, feed, "welcome")
	feed.WriteJSON(map[string]string{"action": "auth", "api_key": pk, "secret": sk})
	readType(t, feed, "authenticated")
	if q := readType(t, feed, "q"); q["d"].([]any)[0].([]any)[0] != "EURUSD" {
		t.Fatalf("no quote after auth: %v", q)
	}
	if code, _ := e.client().do("GET", "/api/v1/quotes", nil, map[string]string{"X-API-Key": pk, "X-API-Secret": sk}); code != 200 {
		t.Fatalf("REST with API key: %d", code)
	}
	code, ord := merchant.do("POST", "/api/v1/orders", map[string]any{"symbol": "EURUSD", "side": "buy", "qty": 1000}, nil)
	if code != 201 || ord["price"] != 1.1001 {
		t.Fatalf("verified order: %d %v", code, ord)
	}

	// 8. Admin view shows the live session and its subscription.
	_, _ = admin.do("GET", "/api/v1/me", nil, nil)
	var listing struct{ Items []map[string]any }
	req, _ = http.NewRequest("GET", e.srv.URL+"/api/v1/admin/merchants", nil)
	resp, _ = admin.c.Do(req)
	json.NewDecoder(resp.Body).Decode(&listing)
	resp.Body.Close()
	rows := listing.Items
	found := false
	for _, r := range rows {
		if r["id"] == id {
			found = r["sessions"] == float64(1) && len(r["symbols"].([]any)) == 1
		}
	}
	if !found {
		t.Fatalf("admin list missing live session: %v", rows)
	}

	// 9. Admin forces re-verification: live socket is cut (4003) and the
	// same key is now refused.
	if code, _ := admin.do("POST", "/api/v1/admin/merchants/"+id+"/kyc", map[string]any{"decision": "reverify", "reason": "document expiring"}, nil); code != 200 {
		t.Fatalf("reverify: %d", code)
	}
	if c := closeCode(t, feed); c != server.CloseForbidden {
		t.Fatalf("live session after reverify: close %d", c)
	}
	again := e.ws(t, "")
	readType(t, again, "welcome")
	again.WriteJSON(map[string]string{"action": "auth", "api_key": pk, "secret": sk})
	if c := closeCode(t, again); c != server.CloseForbidden {
		t.Fatalf("key after reverify: close %d", c)
	}

	// 10. Disabled accounts cannot log in at all.
	if code, _ := admin.do("POST", "/api/v1/admin/merchants/"+id+"/disable", nil, nil); code != 200 {
		t.Fatalf("disable: %d", code)
	}
	if code, _ := e.client().do("POST", "/api/v1/auth/login", map[string]string{"email": "asha@example.com", "password": "correct-horse-battery"}, nil); code != 401 {
		t.Fatalf("disabled login: %d", code)
	}

	// Every security decision is in the audit log.
	evs, _ := e.svc.AuditLog(ctx)
	seen := map[string]bool{}
	for _, ev := range evs {
		seen[ev.Action] = true
	}
	for _, want := range []string{"register", "kyc_submitted", "pii_viewed", "document_viewed", "kyc_approve", "apikey_issued", "kyc_reverify", "account_disabled", "order_filled"} {
		if !seen[want] {
			t.Errorf("audit log missing %q", want)
		}
	}
}

func TestTicketSingleUse(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, err := e.svc.Register(ctx, "t@example.com", "ticket-password-1", "T")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.IssueTicket(ctx, a.ID); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("unverified got a ticket: %v", err)
	}
	approve(t, e, a.ID)
	tk, err := e.svc.IssueTicket(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := e.ws(t, "?ticket="+tk+"&symbols=EURUSD")
	if w := readType(t, c, "welcome"); w["auth_required"] != false {
		t.Fatalf("ticket not accepted: %v", w)
	}
	readType(t, c, "q")
	if _, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.srv.URL, "http")+"/ws?ticket="+tk, nil); err == nil {
		t.Fatal("ticket reused")
	}
}

func TestKYCExpiryEndsAccess(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, _ := e.svc.Register(ctx, "x@example.com", "expiry-password-1", "X")
	approve(t, e, a.ID)
	pk, sk, err := e.svc.IssueAPIKey(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !e.svc.Allowed(ctx, a.ID) {
		t.Fatal("approved merchant not allowed")
	}
	e.svc.now = func() time.Time { return time.Now().AddDate(0, 0, 31) } // approval was for 30 days
	if e.svc.Allowed(ctx, a.ID) {
		t.Fatal("expired approval still allowed")
	}
	if _, err := e.svc.AuthenticateKey(ctx, pk, sk); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("expired merchant's key accepted: %v", err)
	}
}

func TestLoginLockout(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.svc.Register(ctx, "l@example.com", "lockout-password-1", "L")
	var err error
	for i := 0; i < 6; i++ {
		_, err = e.svc.Login(ctx, "l@example.com", "wrong-password-xx", "10.0.0.1")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("no lockout after repeated failures: %v", err)
	}
	if _, err := e.svc.Login(ctx, "l@example.com", "lockout-password-1", "10.0.0.2"); !errors.Is(err, ErrLocked) {
		t.Fatalf("lockout bypassed from another IP: %v", err)
	}
}

func approve(t *testing.T, e *env, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.svc.SubmitKYC(ctx, id, KYCForm{LegalName: "Name Here", DateOfBirth: "1985-01-01", Country: "GB",
		DocType: "national_id", DocNumber: "AB12345"}); err != nil {
		t.Fatal(err)
	}
	adm, err := e.svc.store.GetByEmail(ctx, "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Review(ctx, adm, id, Approve, "", 30); err != nil {
		t.Fatal(err)
	}
}

// allValues concatenates every stored value in full (Dump truncates).
func allValues(mr *miniredis.Miniredis) string {
	var sb strings.Builder
	for _, k := range mr.Keys() {
		switch mr.Type(k) {
		case "string":
			v, _ := mr.Get(k)
			sb.WriteString(v)
		case "list":
			vs, _ := mr.List(k)
			sb.WriteString(strings.Join(vs, ""))
		case "set":
			vs, _ := mr.Members(k)
			sb.WriteString(strings.Join(vs, ""))
		}
	}
	return sb.String()
}

var _ = security.RoleAdmin

// Admins get read-only market data for oversight, but no trading surface.
func TestAdminViewOnly(t *testing.T) {
	e := newEnv(t)
	admin := e.client()
	if code, _ := admin.do("POST", "/api/v1/auth/login", map[string]string{"email": "admin@example.com", "password": "admin-password-123"}, nil); code != 200 {
		t.Fatalf("admin login: %d", code)
	}
	_, me := admin.do("GET", "/api/v1/me", nil, nil)
	if me["can_view_market"] != true || me["can_trade"] != false {
		t.Fatalf("admin permissions: %v", me)
	}
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"GET", "/api/v1/quotes", nil, 200},
		{"GET", "/api/v1/candles?symbol=EURUSD", nil, 200},
		{"POST", "/api/v1/ws-ticket", nil, 200},
		{"POST", "/api/v1/orders", map[string]any{"symbol": "EURUSD", "side": "buy", "qty": 1}, 403},
		{"POST", "/api/v1/apikey", nil, 403},
	} {
		if code, _ := admin.do(c.method, c.path, c.body, nil); code != c.want {
			t.Errorf("admin %s %s: %d, want %d", c.method, c.path, code, c.want)
		}
	}
	_, tk := admin.do("POST", "/api/v1/ws-ticket", nil, nil)
	c := e.ws(t, "?symbols=EURUSD&ticket="+tk["ticket"].(string))
	readType(t, c, "q")
}

func TestAdminPagination(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < 23; i++ {
		a, err := e.svc.Register(ctx, fmt.Sprintf("m%02d@example.com", i), "paging-password-1", fmt.Sprintf("Merchant %02d", i))
		if err != nil {
			t.Fatal(err)
		}
		if i < 4 {
			e.svc.SubmitKYC(ctx, a.ID, KYCForm{LegalName: "Some Name", DateOfBirth: "1980-01-01", Country: "GB", DocType: "passport", DocNumber: "PP1234"})
		}
		time.Sleep(time.Millisecond) // distinct CreatedAt for a stable order
	}
	admin := e.client()
	admin.do("POST", "/api/v1/auth/login", map[string]string{"email": "admin@example.com", "password": "admin-password-123"}, nil)
	type listing struct {
		Items                    []map[string]any
		Total, Page, Size, Pages int
		Counts                   map[string]int
	}
	get := func(path string, v any) {
		t.Helper()
		req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
		resp, err := admin.c.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("GET %s: %v %v", path, err, resp.StatusCode)
		}
		defer resp.Body.Close()
		json.NewDecoder(resp.Body).Decode(v)
	}
	var l listing
	get("/api/v1/admin/merchants", &l) // 23 merchants + admin = 24 accounts
	if l.Size != 10 || len(l.Items) != 10 || l.Total != 24 || l.Pages != 3 || l.Page != 1 {
		t.Fatalf("default page: size=%d items=%d total=%d pages=%d", l.Size, len(l.Items), l.Total, l.Pages)
	}
	if l.Items[0]["email"] != "m22@example.com" {
		t.Fatalf("not newest first: %v", l.Items[0]["email"])
	}
	if l.Counts["merchants"] != 23 || l.Counts["pending"] != 4 {
		t.Fatalf("counts must cover all pages: %v", l.Counts)
	}
	get("/api/v1/admin/merchants?page=3&size=10", &l)
	if len(l.Items) != 4 || l.Page != 3 {
		t.Fatalf("last page: %d items, page %d", len(l.Items), l.Page)
	}
	get("/api/v1/admin/merchants?page=99", &l)
	if l.Page != 3 || len(l.Items) != 4 {
		t.Fatalf("past-the-end not clamped: page %d, %d items", l.Page, len(l.Items))
	}
	get("/api/v1/admin/merchants?status=pending", &l)
	if l.Total != 4 || l.Pages != 1 {
		t.Fatalf("status filter: total %d", l.Total)
	}
	get("/api/v1/admin/merchants?q=merchant%2001", &l)
	if l.Total != 1 || l.Items[0]["email"] != "m01@example.com" {
		t.Fatalf("search: %+v", l)
	}
	get("/api/v1/admin/merchants?size=1000", &l)
	if l.Size != 100 {
		t.Fatalf("size not capped: %d", l.Size)
	}

	var a1, a2 listing
	get("/api/v1/admin/audit?page=1&size=10", &a1)
	get("/api/v1/admin/audit?page=2&size=10", &a2)
	if len(a1.Items) != 10 || len(a2.Items) != 10 || a1.Total < 28 || a1.Pages != (a1.Total+9)/10 {
		t.Fatalf("audit paging: %d/%d items, total %d, pages %d", len(a1.Items), len(a2.Items), a1.Total, a1.Pages)
	}
	if a1.Items[0]["action"] != "login" { // the admin login is the newest event
		t.Fatalf("audit not newest first: %v", a1.Items[0])
	}
	if a1.Items[9]["ts"] == a2.Items[0]["ts"] && a1.Items[9]["target"] == a2.Items[0]["target"] && a1.Items[9]["action"] == a2.Items[0]["action"] {
		t.Fatal("pages overlap")
	}
}
