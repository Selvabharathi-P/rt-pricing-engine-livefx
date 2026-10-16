package security

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims carried by an access token. Role is informational only — every
// protected request re-derives the role from the Directory.
type Claims struct {
	jwt.RegisteredClaims
	Role Role `json:"role"`
	Ver  int  `json:"ver"` // Merchant.TokenVersion at issue time
}

// Tokens issues and verifies short-lived EdDSA access tokens. Asymmetric
// signing means other services can verify with the public key alone.
type Tokens struct {
	priv     ed25519.PrivateKey
	pub      ed25519.PublicKey
	issuer   string
	audience string
	ttl      time.Duration
	revoked  *RevocationList
	now      func() time.Time
}

func NewTokens(priv ed25519.PrivateKey, issuer, audience string, ttl time.Duration, rl *RevocationList) *Tokens {
	return &Tokens{priv: priv, pub: priv.Public().(ed25519.PublicKey), issuer: issuer, audience: audience,
		ttl: ttl, revoked: rl, now: time.Now}
}

func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (t *Tokens) Issue(m *Merchant) (string, error) {
	now := t.now()
	c := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: m.ID, Issuer: t.issuer, Audience: jwt.ClaimStrings{t.audience},
			IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(t.ttl)), ID: randomID(16),
		},
		Role: m.Role(now), Ver: m.TokenVersion,
	}
	return jwt.NewWithClaims(jwt.SigningMethodEdDSA, c).SignedString(t.priv)
}

var ErrInvalidToken = errors.New("invalid or expired token")

// Verify checks signature, algorithm, issuer, audience, expiry and revocation.
func (t *Tokens) Verify(raw string) (*Claims, error) {
	var c Claims
	_, err := jwt.ParseWithClaims(raw, &c, func(*jwt.Token) (any, error) { return t.pub, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}), // blocks alg=none / HS confusion
		jwt.WithIssuer(t.issuer), jwt.WithAudience(t.audience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(t.now), jwt.WithLeeway(5*time.Second))
	if err != nil {
		return nil, ErrInvalidToken
	}
	if t.revoked != nil && t.revoked.IsRevoked(c.ID) {
		return nil, ErrInvalidToken
	}
	return &c, nil
}

// Revoke kills one access token (logout) until it would have expired anyway.
func (t *Tokens) Revoke(c *Claims) {
	if t.revoked != nil && c.ExpiresAt != nil {
		t.revoked.Revoke(c.ID, c.ExpiresAt.Time)
	}
}

// RevocationList is an in-memory deny-list of token IDs. In production this
// lives in Redis (SET jti EX ttl) so all instances share it.
type RevocationList struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func NewRevocationList() *RevocationList { return &RevocationList{m: map[string]time.Time{}} }

func (r *RevocationList) Revoke(jti string, until time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[jti] = until
	now := time.Now()
	for k, exp := range r.m { // opportunistic cleanup keeps the list bounded
		if now.After(exp) {
			delete(r.m, k)
		}
	}
}

func (r *RevocationList) IsRevoked(jti string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	exp, ok := r.m[jti]
	return ok && time.Now().Before(exp)
}

// RefreshStore implements rotating, opaque refresh tokens. Only a SHA-256
// of each token is stored. Presenting an already-used token is treated as
// theft: the whole token family is revoked and the user must log in again.
type RefreshStore struct {
	mu     sync.Mutex
	byHash map[string]*refreshRec
	ttl    time.Duration
	now    func() time.Time
}

type refreshRec struct {
	merchantID string
	family     string
	expires    time.Time
	used       bool
}

var ErrRefreshReuse = errors.New("refresh token reuse detected; session revoked")

func NewRefreshStore(ttl time.Duration) *RefreshStore {
	return &RefreshStore{byHash: map[string]*refreshRec{}, ttl: ttl, now: time.Now}
}

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// New starts a new token family (at login).
func (s *RefreshStore) New(merchantID string) string {
	return s.issue(merchantID, randomID(16))
}

func (s *RefreshStore) issue(merchantID, family string) string {
	tok := randomID(32)
	s.mu.Lock()
	s.byHash[hashToken(tok)] = &refreshRec{merchantID: merchantID, family: family, expires: s.now().Add(s.ttl)}
	s.mu.Unlock()
	return tok
}

// Rotate consumes tok and returns a replacement in the same family.
func (s *RefreshStore) Rotate(tok string) (merchantID, next string, err error) {
	s.mu.Lock()
	rec, ok := s.byHash[hashToken(tok)]
	if !ok || s.now().After(rec.expires) {
		s.mu.Unlock()
		return "", "", ErrInvalidToken
	}
	if rec.used {
		for h, r := range s.byHash {
			if r.family == rec.family {
				delete(s.byHash, h)
			}
		}
		s.mu.Unlock()
		return "", "", ErrRefreshReuse
	}
	rec.used = true
	s.mu.Unlock()
	return rec.merchantID, s.issue(rec.merchantID, rec.family), nil
}

// RevokeAll ends every session for a merchant (password change, KYC change, admin action).
func (s *RefreshStore) RevokeAll(merchantID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, r := range s.byHash {
		if r.merchantID == merchantID {
			delete(s.byHash, h)
		}
	}
}

func (c *Claims) String() string {
	return fmt.Sprintf("sub=%s role=%s jti=%s", c.Subject, c.Role, c.ID)
}
