package security

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (OWASP baseline: m=19MiB, t=2, p=1; we use 64MiB/t=3).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16
	minPassword  = 12
	maxPassword  = 128 // cap input to bound hashing cost (DoS)
)

var ErrPasswordPolicy = fmt.Errorf("password must be %d-%d characters", minPassword, maxPassword)

// HashPassword returns a PHC-formatted Argon2id hash.
func HashPassword(pw string) (string, error) {
	if len(pw) < minPassword || len(pw) > maxPassword {
		return "", ErrPasswordPolicy
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// VerifyPassword checks pw against a PHC hash in constant time.
func VerifyPassword(pw, phc string) (bool, error) {
	if len(pw) > maxPassword {
		return false, nil
	}
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("unsupported hash format")
	}
	var v int
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return false, errors.New("unsupported argon2 version")
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, errors.New("bad argon2 params")
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash lets login spend the same time whether or not the email exists,
// so response timing doesn't reveal which accounts are registered.
// Computed lazily so importing the package doesn't cost an Argon2 run.
var dummyHash = sync.OnceValue(func() string {
	h, _ := HashPassword("timing-equaliser-not-a-password")
	return h
})

func VerifyPasswordOrDummy(pw string, phc string) bool {
	if phc == "" {
		VerifyPassword(pw, dummyHash())
		return false
	}
	ok, err := VerifyPassword(pw, phc)
	return err == nil && ok
}
