// Package security is a reference implementation of the controls described
// in docs/SECURITY.md: KYC-gated RBAC, Argon2id passwords, short-lived
// signed access tokens with revocation, rotating refresh tokens, field-level
// encryption for KYC PII, rate limiting and log redaction.
//
// It is intentionally storage-agnostic: Directory is the only thing a real
// service has to implement against its database.
package security

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Role string

const (
	RoleAdmin              Role = "admin"
	RoleVerifiedMerchant   Role = "verified_merchant"
	RoleUnverifiedMerchant Role = "unverified_merchant"
)

type KYCStatus string

const (
	KYCNotSubmitted KYCStatus = "not_submitted" // registered, no documents yet
	KYCPending      KYCStatus = "pending"       // submitted, awaiting review
	KYCApproved     KYCStatus = "approved"
	KYCRejected     KYCStatus = "rejected"
	KYCExpired      KYCStatus = "expired"
)

type KYC struct {
	Status     KYCStatus
	ExpiresAt  time.Time // approval validity (e.g. document expiry or periodic review)
	ReviewedBy string    // admin ID that made the last decision
	Reason     string    // rejection reason shown to the merchant
}

// Merchant is the server-side source of truth used for every authorization
// decision. Nothing in it is ever taken from the client.
type Merchant struct {
	ID           string
	Email        string
	PasswordHash string
	IsAdmin      bool
	Disabled     bool
	KYC          KYC
	TokenVersion int // bumped on any security-relevant change -> old tokens die
}

// EffectiveKYC returns the status as of now: an approval past ExpiresAt is
// treated as expired even before a background job rewrites the record.
func (m *Merchant) EffectiveKYC(now time.Time) KYCStatus {
	if m.KYC.Status == KYCApproved && !m.KYC.ExpiresAt.IsZero() && !now.Before(m.KYC.ExpiresAt) {
		return KYCExpired
	}
	return m.KYC.Status
}

// Role is derived, never stored or accepted from input: a merchant is a
// verified merchant only while their KYC is approved and unexpired.
func (m *Merchant) Role(now time.Time) Role {
	switch {
	case m.IsAdmin:
		return RoleAdmin
	case m.EffectiveKYC(now) == KYCApproved:
		return RoleVerifiedMerchant
	default:
		return RoleUnverifiedMerchant
	}
}

// CanTrade is the single trading-permission predicate.
func (m *Merchant) CanTrade(now time.Time) bool {
	return !m.Disabled && !m.IsAdmin && m.Role(now) == RoleVerifiedMerchant
}

// CanViewMarket allows read-only market data (quotes, candles, the price
// feed): verified merchants, plus admins for oversight. Admins still cannot
// trade or hold API keys (CanTrade), keeping separation of duties.
func (m *Merchant) CanViewMarket(now time.Time) bool {
	return !m.Disabled && (m.IsAdmin || m.CanTrade(now))
}

// Directory loads merchants from the system of record.
type Directory interface {
	Merchant(ctx context.Context, id string) (*Merchant, error)
}

var ErrNotFound = errors.New("merchant not found")

// allowed KYC transitions: from -> to -> who may perform it.
var transitions = map[KYCStatus]map[KYCStatus]string{
	KYCNotSubmitted: {KYCPending: "merchant"}, // first submission
	KYCPending:      {KYCApproved: "admin", KYCRejected: "admin"},
	KYCRejected:     {KYCPending: "merchant"},                    // resubmission
	KYCExpired:      {KYCPending: "merchant"},                    // re-verification
	KYCApproved:     {KYCExpired: "system", KYCPending: "admin"}, // expiry / forced re-review
}

// Transition applies a KYC status change, enforcing who may make it.
// Every change bumps TokenVersion so existing sessions are re-evaluated.
func Transition(m *Merchant, to KYCStatus, actor string, actorRole Role, reason string, expiresAt time.Time) error {
	from := m.EffectiveKYC(time.Now())
	need, ok := transitions[from][to]
	if !ok {
		return fmt.Errorf("kyc: transition %s -> %s not allowed", from, to)
	}
	switch need {
	case "admin":
		if actorRole != RoleAdmin {
			return errors.New("kyc: only an admin may make this decision")
		}
		if actor == m.ID {
			return errors.New("kyc: reviewers cannot decide on their own account")
		}
	case "merchant":
		if actor != m.ID {
			return errors.New("kyc: only the merchant may resubmit")
		}
	}
	if to == KYCApproved && expiresAt.IsZero() {
		return errors.New("kyc: approval requires an expiry")
	}
	if to == KYCRejected && reason == "" {
		return errors.New("kyc: rejection requires a reason")
	}
	m.KYC = KYC{Status: to, ExpiresAt: expiresAt, ReviewedBy: actor, Reason: reason}
	m.TokenVersion++
	return nil
}
