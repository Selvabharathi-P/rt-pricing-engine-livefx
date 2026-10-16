package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// FieldCipher encrypts individual KYC fields (document numbers, DOB, ...)
// with AES-256-GCM. Ciphertext layout: keyID(1) | nonce(12) | sealed.
//
// The record ID and field name are bound in as associated data, so a
// ciphertext copied into another merchant's row or another column fails to
// decrypt. Multiple key IDs allow rotation: new writes use the active key,
// old rows stay readable until re-encrypted. Keys come from a KMS / secret
// manager, never from source code or the database itself.
type FieldCipher struct {
	active byte
	aeads  map[byte]cipher.AEAD
}

func NewFieldCipher(active byte, keys map[byte][]byte) (*FieldCipher, error) {
	fc := &FieldCipher{active: active, aeads: map[byte]cipher.AEAD{}}
	for id, k := range keys {
		if len(k) != 32 {
			return nil, fmt.Errorf("key %d: need 32 bytes", id)
		}
		blk, err := aes.NewCipher(k)
		if err != nil {
			return nil, err
		}
		if fc.aeads[id], err = cipher.NewGCM(blk); err != nil {
			return nil, err
		}
	}
	if fc.aeads[active] == nil {
		return nil, errors.New("active key missing")
	}
	return fc, nil
}

func aad(recordID, field string) []byte { return []byte(recordID + "\x00" + field) }

func (fc *FieldCipher) Encrypt(recordID, field string, plaintext []byte) ([]byte, error) {
	a := fc.aeads[fc.active]
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{fc.active}, nonce...)
	return a.Seal(out, nonce, plaintext, aad(recordID, field)), nil
}

var ErrDecrypt = errors.New("decryption failed")

func (fc *FieldCipher) Decrypt(recordID, field string, ct []byte) ([]byte, error) {
	if len(ct) < 1 {
		return nil, ErrDecrypt
	}
	a := fc.aeads[ct[0]]
	if a == nil || len(ct) < 1+a.NonceSize()+a.Overhead() {
		return nil, ErrDecrypt
	}
	n := 1 + a.NonceSize()
	pt, err := a.Open(nil, ct[1:n], ct[n:], aad(recordID, field))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}
