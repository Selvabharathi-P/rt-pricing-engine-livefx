// Package accounts implements merchant registration, KYC submission and
// review, API key issuance and the HTTP API behind the account and admin UI.
// Authorization decisions use the primitives in internal/security.
package accounts

import (
	"time"

	"pricing-engine/internal/security"
)

// Account is the stored merchant record. Sensitive KYC fields live only in
// PII, encrypted per field (AES-256-GCM bound to the account ID + field).
type Account struct {
	security.Merchant
	Name           string            `json:"name"`
	CreatedAt      time.Time         `json:"created_at"`
	KYCSubmittedAt time.Time         `json:"kyc_submitted_at,omitempty"`
	Country        string            `json:"country,omitempty"`  // not sensitive on its own
	DocType        string            `json:"doc_type,omitempty"` // passport / national_id / driving_licence
	PII            map[string][]byte `json:"pii,omitempty"`      // field -> ciphertext
	Doc            *DocMeta          `json:"doc,omitempty"`
}

type DocMeta struct {
	MIME       string    `json:"mime"`
	Size       int       `json:"size"`
	UploadedAt time.Time `json:"uploaded_at"`
}

// Encrypted PII field names.
const (
	PIILegalName   = "legal_name"
	PIIDateOfBirth = "date_of_birth"
	PIIDocNumber   = "document_number"
)

// APIKey grants programmatic access to the WebSocket feed. Only an HMAC of
// the secret is stored; the secret itself is shown to the merchant once.
type APIKey struct {
	ID         string    `json:"id"` // public, "pk_..."
	MerchantID string    `json:"merchant_id"`
	SecretMAC  []byte    `json:"secret_mac"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
}

// AuditEvent records security-relevant actions for the admin audit log.
type AuditEvent struct {
	TS     time.Time `json:"ts"`
	Actor  string    `json:"actor"` // account email, or "system"
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	Detail string    `json:"detail,omitempty"`
}
