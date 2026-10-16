package accounts

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Secrets are the service's key material. They are kept outside Redis (in
// production: a KMS / secret manager) so a database dump alone can't decrypt
// KYC data, forge sessions or verify stolen API secrets offline.
type Secrets struct {
	SigningKey ed25519.PrivateKey // session tokens (EdDSA)
	EncKey     []byte             // 32 bytes, KYC field + document encryption
	Pepper     []byte             // 32 bytes, HMAC key for API secrets
}

// LoadOrCreateSecrets reads key files from dir, generating any that are
// missing (0600). In Docker, dir is a dedicated volume.
func LoadOrCreateSecrets(dir string) (*Secrets, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secrets dir: %w", err)
	}
	seed, err := loadOrCreate(filepath.Join(dir, "signing.key"), ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	enc, err := loadOrCreate(filepath.Join(dir, "kyc-enc.key"), 32)
	if err != nil {
		return nil, err
	}
	pep, err := loadOrCreate(filepath.Join(dir, "apikey-pepper.key"), 32)
	if err != nil {
		return nil, err
	}
	return &Secrets{SigningKey: ed25519.NewKeyFromSeed(seed), EncKey: enc, Pepper: pep}, nil
}

func loadOrCreate(path string, n int) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if len(b) != n {
			return nil, fmt.Errorf("%s: want %d bytes, got %d", path, n, len(b))
		}
		return b, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	b = make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return b, nil
}
