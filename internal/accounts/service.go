package accounts

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"time"

	"pricing-engine/internal/security"
)

var (
	ErrInvalidInput   = errors.New("invalid input")
	ErrBadCredentials = errors.New("invalid email or password")
	ErrLocked         = errors.New("too many attempts; try again later")
	ErrNotVerified    = errors.New("trading not permitted: KYC verification required")
)

// SessionKicker disconnects a merchant's live WebSocket sessions. The hub
// implements it; the service calls it whenever access is taken away.
type SessionKicker interface {
	Kick(merchantID, reason string)
}

type Service struct {
	store   *Store
	cipher  *security.FieldCipher
	pepper  []byte
	guard   *security.LoginGuard
	tickets *tickets
	log     *slog.Logger
	kicker  SessionKicker
	now     func() time.Time

	mu sync.Mutex // serialises read-modify-write of account records
}

func NewService(store *Store, sec *Secrets, log *slog.Logger) (*Service, error) {
	fc, err := security.NewFieldCipher(1, map[byte][]byte{1: sec.EncKey})
	if err != nil {
		return nil, err
	}
	return &Service{store: store, cipher: fc, pepper: sec.Pepper, guard: security.NewLoginGuard(),
		tickets: newTickets(), log: log, now: time.Now}, nil
}

// SetKicker wires the WebSocket hub (created after the service).
func (s *Service) SetKicker(k SessionKicker) { s.kicker = k }

func (s *Service) kick(id, reason string) {
	if s.kicker != nil {
		s.kicker.Kick(id, reason)
	}
}

func newID(prefix string, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

func (s *Service) audit(ctx context.Context, actor, action, target, detail string) {
	s.store.Audit(ctx, AuditEvent{TS: s.now(), Actor: actor, Action: action, Target: target, Detail: detail})
}

// Merchant implements security.Directory: the source of truth for every
// authorization check.
func (s *Service) Merchant(ctx context.Context, id string) (*security.Merchant, error) {
	a, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &a.Merchant, nil
}

func (s *Service) Account(ctx context.Context, id string) (*Account, error) {
	return s.store.Get(ctx, id)
}

// Register creates an unverified merchant. Role is derived, so a new
// account can log in but cannot trade until KYC is approved.
func (s *Service) Register(ctx context.Context, email, password, name string) (*Account, error) {
	email, name = normEmail(email), strings.TrimSpace(name)
	if _, err := mail.ParseAddress(email); err != nil || len(email) > 254 {
		return nil, fmt.Errorf("%w: email", ErrInvalidInput)
	}
	if name == "" || len(name) > 100 {
		return nil, fmt.Errorf("%w: name", ErrInvalidInput)
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	a := &Account{
		Merchant:  security.Merchant{ID: newID("m_", 9), Email: email, PasswordHash: hash, KYC: security.KYC{Status: security.KYCNotSubmitted}},
		Name:      name,
		CreatedAt: s.now(),
	}
	if err := s.store.Create(ctx, a); err != nil {
		return nil, err
	}
	s.audit(ctx, email, "register", a.ID, "")
	return a, nil
}

// EnsureAdmin creates the bootstrap admin if no account has that email.
func (s *Service) EnsureAdmin(ctx context.Context, email, password string) (bool, error) {
	email = normEmail(email)
	if _, err := s.store.GetByEmail(ctx, email); err == nil {
		return false, nil
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return false, err
	}
	a := &Account{Merchant: security.Merchant{ID: newID("m_", 9), Email: email, PasswordHash: hash, IsAdmin: true,
		KYC: security.KYC{Status: security.KYCNotSubmitted}}, Name: "Administrator", CreatedAt: s.now()}
	if err := s.store.Create(ctx, a); err != nil {
		return false, err
	}
	s.audit(ctx, "system", "bootstrap_admin", a.ID, email)
	return true, nil
}

// Login checks credentials with throttling and lockout. Errors are generic.
func (s *Service) Login(ctx context.Context, email, password, ip string) (*Account, error) {
	email = normEmail(email)
	if !s.guard.Allow(ip, email) {
		s.audit(ctx, email, "login_throttled", "", "ip="+ip)
		return nil, ErrLocked
	}
	a, err := s.store.GetByEmail(ctx, email)
	hash := ""
	if err == nil {
		hash = a.PasswordHash
	}
	ok := security.VerifyPasswordOrDummy(password, hash) // constant work for unknown emails
	s.guard.Result(email, ok)
	if !ok || a.Disabled {
		s.audit(ctx, email, "login_failed", "", "ip="+ip)
		return nil, ErrBadCredentials
	}
	s.audit(ctx, email, "login", a.ID, "")
	return a, nil
}

// KYCForm is what a merchant submits. Free-text fields are length-limited.
type KYCForm struct {
	LegalName   string
	DateOfBirth string // YYYY-MM-DD
	Country     string // ISO 3166-1 alpha-2
	DocType     string
	DocNumber   string
	Doc         []byte // optional upload, already size-limited by the handler
}

var (
	reCountry = regexp.MustCompile(`^[A-Z]{2}$`)
	reDocNum  = regexp.MustCompile(`^[A-Za-z0-9-]{4,30}$`)
	docTypes  = map[string]bool{"passport": true, "national_id": true, "driving_licence": true}
	docMIMEs  = map[string]bool{"image/jpeg": true, "image/png": true, "application/pdf": true}
)

// SniffDoc returns the detected MIME type if it is an accepted document type.
func SniffDoc(b []byte) (string, bool) {
	mime := httpDetect(b)
	return mime, docMIMEs[mime]
}

func (f *KYCForm) validate(now time.Time) error {
	f.LegalName, f.DocNumber = strings.TrimSpace(f.LegalName), strings.TrimSpace(f.DocNumber)
	f.Country = strings.ToUpper(strings.TrimSpace(f.Country))
	if l := len(f.LegalName); l < 2 || l > 120 {
		return fmt.Errorf("%w: legal name", ErrInvalidInput)
	}
	dob, err := time.Parse("2006-01-02", f.DateOfBirth)
	if err != nil || dob.After(now.AddDate(-18, 0, 0)) || dob.Before(now.AddDate(-120, 0, 0)) {
		return fmt.Errorf("%w: date of birth (must be 18+)", ErrInvalidInput)
	}
	if !reCountry.MatchString(f.Country) {
		return fmt.Errorf("%w: country (2-letter code)", ErrInvalidInput)
	}
	if !docTypes[f.DocType] {
		return fmt.Errorf("%w: document type", ErrInvalidInput)
	}
	if !reDocNum.MatchString(f.DocNumber) {
		return fmt.Errorf("%w: document number", ErrInvalidInput)
	}
	if len(f.Doc) > 0 {
		if _, ok := SniffDoc(f.Doc); !ok {
			return fmt.Errorf("%w: document must be JPEG, PNG or PDF", ErrInvalidInput)
		}
	}
	return nil
}

// SubmitKYC moves the merchant to pending, encrypting every PII field.
func (s *Service) SubmitKYC(ctx context.Context, id string, f KYCForm) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.IsAdmin {
		return nil, fmt.Errorf("%w: admins do not submit KYC", ErrInvalidInput)
	}
	if err := f.validate(s.now()); err != nil {
		return nil, err
	}
	if err := security.Transition(&a.Merchant, security.KYCPending, a.ID, a.Role(s.now()), "", time.Time{}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	a.PII = map[string][]byte{}
	for field, v := range map[string]string{PIILegalName: f.LegalName, PIIDateOfBirth: f.DateOfBirth, PIIDocNumber: f.DocNumber} {
		if a.PII[field], err = s.cipher.Encrypt(a.ID, field, []byte(v)); err != nil {
			return nil, err
		}
	}
	a.Country, a.DocType, a.KYCSubmittedAt, a.Doc = f.Country, f.DocType, s.now(), nil
	if len(f.Doc) > 0 {
		mime, _ := SniffDoc(f.Doc)
		ct, err := s.cipher.Encrypt(a.ID, "document", f.Doc)
		if err != nil {
			return nil, err
		}
		if err := s.store.PutDoc(ctx, a.ID, ct); err != nil {
			return nil, err
		}
		a.Doc = &DocMeta{MIME: mime, Size: len(f.Doc), UploadedAt: s.now()}
	}
	if err := s.store.Put(ctx, a); err != nil {
		return nil, err
	}
	s.audit(ctx, a.Email, "kyc_submitted", a.ID, f.DocType)
	return a, nil
}

// Decision is an admin KYC action.
type Decision string

const (
	Approve  Decision = "approve"
	Reject   Decision = "reject"
	Reverify Decision = "reverify" // approved -> pending: forced re-review
)

// Review applies an admin decision. Anything that removes trading access
// disconnects the merchant's live sessions immediately.
func (s *Service) Review(ctx context.Context, admin *Account, id string, d Decision, reason string, validDays int) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	var to security.KYCStatus
	var exp time.Time
	switch d {
	case Approve:
		if validDays < 1 || validDays > 3650 {
			return nil, fmt.Errorf("%w: validity days 1-3650", ErrInvalidInput)
		}
		to, exp = security.KYCApproved, s.now().AddDate(0, 0, validDays)
	case Reject:
		to = security.KYCRejected
	case Reverify:
		to = security.KYCPending
	default:
		return nil, fmt.Errorf("%w: decision", ErrInvalidInput)
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 300 {
		return nil, fmt.Errorf("%w: reason too long", ErrInvalidInput)
	}
	if err := security.Transition(&a.Merchant, to, admin.ID, admin.Role(s.now()), reason, exp); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.store.Put(ctx, a); err != nil {
		return nil, err
	}
	s.audit(ctx, admin.Email, "kyc_"+string(d), a.ID, reason)
	if to != security.KYCApproved {
		s.kick(a.ID, "kyc "+string(to))
	}
	return a, nil
}

// SetDisabled blocks or unblocks an account; blocking ends all sessions.
func (s *Service) SetDisabled(ctx context.Context, admin *Account, id string, disabled bool) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == admin.ID {
		return nil, fmt.Errorf("%w: cannot disable yourself", ErrInvalidInput)
	}
	a, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	a.Disabled = disabled
	a.TokenVersion++ // invalidate existing session tokens
	if err := s.store.Put(ctx, a); err != nil {
		return nil, err
	}
	action := "account_enabled"
	if disabled {
		action = "account_disabled"
		s.kick(a.ID, "account disabled")
	}
	s.audit(ctx, admin.Email, action, a.ID, "")
	return a, nil
}

// DecryptPII is for admin review only and is always audit-logged.
func (s *Service) DecryptPII(ctx context.Context, admin *Account, a *Account) map[string]string {
	out := map[string]string{}
	for field, ct := range a.PII {
		if pt, err := s.cipher.Decrypt(a.ID, field, ct); err == nil {
			out[field] = string(pt)
		}
	}
	s.audit(ctx, admin.Email, "pii_viewed", a.ID, "")
	return out
}

func (s *Service) Document(ctx context.Context, admin *Account, id string) ([]byte, string, error) {
	a, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if a.Doc == nil {
		return nil, "", security.ErrNotFound
	}
	ct, err := s.store.GetDoc(ctx, id)
	if err != nil {
		return nil, "", err
	}
	pt, err := s.cipher.Decrypt(id, "document", ct)
	if err != nil {
		return nil, "", err
	}
	s.audit(ctx, admin.Email, "document_viewed", id, "")
	return pt, a.Doc.MIME, nil
}

func (s *Service) mac(secret string) []byte {
	h := hmac.New(sha256.New, s.pepper)
	h.Write([]byte(secret))
	return h.Sum(nil)
}

// IssueAPIKey creates (or rotates) the merchant's key. Only verified
// merchants may hold one; the plaintext secret is returned exactly once.
// Secrets are 256-bit random, so a keyed HMAC is the right hash here (a
// slow password hash would only add latency to every WebSocket login).
func (s *Service) IssueAPIKey(ctx context.Context, id string) (keyID, secret string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.store.Get(ctx, id)
	if err != nil {
		return "", "", err
	}
	if !a.CanTrade(s.now()) {
		return "", "", ErrNotVerified
	}
	if err := s.store.DeleteKeyFor(ctx, id); err != nil {
		return "", "", err
	}
	s.kick(id, "api key rotated")
	keyID, secret = newID("pk_", 12), newID("sk_", 32)
	k := &APIKey{ID: keyID, MerchantID: id, SecretMAC: s.mac(secret), CreatedAt: s.now()}
	if err := s.store.PutKey(ctx, k); err != nil {
		return "", "", err
	}
	s.audit(ctx, a.Email, "apikey_issued", id, keyID)
	return keyID, secret, nil
}

func (s *Service) RevokeAPIKey(ctx context.Context, actor *Account, id string) error {
	if err := s.store.DeleteKeyFor(ctx, id); err != nil {
		return err
	}
	s.kick(id, "api key revoked")
	s.audit(ctx, actor.Email, "apikey_revoked", id, "")
	return nil
}

func (s *Service) KeyFor(ctx context.Context, id string) (*APIKey, error) {
	return s.store.KeyFor(ctx, id)
}

// AuthenticateKey validates an API key + secret and the owner's right to
// trade right now. Failures are indistinguishable to the caller.
func (s *Service) AuthenticateKey(ctx context.Context, keyID, secret string) (*Account, error) {
	k, err := s.store.GetKey(ctx, keyID)
	if err != nil || !hmac.Equal(k.SecretMAC, s.mac(secret)) {
		s.audit(ctx, keyID, "apikey_auth_failed", "", "")
		return nil, ErrBadCredentials
	}
	a, err := s.store.Get(ctx, k.MerchantID)
	if err != nil {
		return nil, ErrBadCredentials
	}
	if !a.CanTrade(s.now()) {
		return nil, ErrNotVerified
	}
	s.store.TouchKey(ctx, k, s.now())
	return a, nil
}

// Allowed reports whether an account may still receive the feed (periodic
// re-check of live sessions: catches KYC expiry without any admin action).
// Market data is view permission; API-key auth itself demands CanTrade.
func (s *Service) Allowed(ctx context.Context, id string) bool {
	a, err := s.store.Get(ctx, id)
	return err == nil && a.CanViewMarket(s.now())
}

// IssueTicket returns a single-use, 30s WebSocket ticket for a browser
// session, so the API secret never has to live in page JavaScript.
func (s *Service) IssueTicket(ctx context.Context, id string) (string, error) {
	if !s.Allowed(ctx, id) {
		return "", ErrNotVerified
	}
	return s.tickets.issue(id), nil
}

func (s *Service) RedeemTicket(ctx context.Context, t string) (*Account, error) {
	id, ok := s.tickets.redeem(t)
	if !ok {
		return nil, ErrBadCredentials
	}
	a, err := s.store.Get(ctx, id)
	if err != nil || !a.CanViewMarket(s.now()) {
		return nil, ErrNotVerified
	}
	return a, nil
}

func (s *Service) List(ctx context.Context) ([]*Account, error) { return s.store.List(ctx) }
func (s *Service) AuditLog(ctx context.Context) ([]AuditEvent, error) {
	return s.store.AuditLog(ctx, 200)
}

func (s *Service) AuditPage(ctx context.Context, offset, n int) ([]AuditEvent, int, error) {
	ev, total, err := s.store.AuditPage(ctx, int64(offset), int64(n))
	return ev, int(total), err
}
func (s *Service) Audit(ctx context.Context, actor, action, target, detail string) {
	s.audit(ctx, actor, action, target, detail)
}
