// Package auth issues and verifies gateway API keys. Keys look like
// gw_<key_id>_<secret>; only a SHA-256 hash of the whole key is stored, which is
// adequate because keys are 256-bit random values (no password KDF needed).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
)

const (
	keyPrefix    = "gw"
	keyIDBytes   = 8
	keySecretLen = 32
)

// Roles a client can hold.
const (
	RoleCaller = "caller"
	RoleAdmin  = "admin"
)

// Principal is an authenticated API client.
type Principal struct {
	ID   int64
	Name string
	Role string
}

// Record is what the directory stores for a client key.
type Record struct {
	Principal Principal
	Hash      []byte
	Active    bool
}

// Directory looks up client records by public key id.
type Directory interface {
	// FindByKeyID returns ErrNotFound when no such key exists.
	FindByKeyID(ctx context.Context, keyID string) (Record, error)
}

// ErrNotFound is returned by a Directory for an unknown key id.
var ErrNotFound = errors.New("key not found")

// ErrUnauthenticated is the single error for every authentication failure, so
// callers cannot tell a bad key from a disabled client or an unknown id.
var ErrUnauthenticated = errors.New("unauthenticated")

// NewKey creates a key. The plaintext is shown to the operator once; store only
// keyID and hash.
func NewKey() (plaintext, keyID string, hash []byte, err error) {
	id := make([]byte, keyIDBytes)
	secret := make([]byte, keySecretLen)
	if _, err = rand.Read(id); err != nil {
		return "", "", nil, err
	}
	if _, err = rand.Read(secret); err != nil {
		return "", "", nil, err
	}
	keyID = hex.EncodeToString(id)
	plaintext = keyPrefix + "_" + keyID + "_" + hex.EncodeToString(secret)
	return plaintext, keyID, HashKey(plaintext), nil
}

// HashKey returns the SHA-256 of a full key string.
func HashKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// ParseKeyID extracts the public key id from a presented key, or "" if the
// shape is wrong.
func ParseKeyID(key string) string {
	parts := strings.SplitN(key, "_", 3)
	if len(parts) != 3 || parts[0] != keyPrefix || len(parts[1]) != keyIDBytes*2 || parts[2] == "" {
		return ""
	}
	return parts[1]
}

// dummyHash is compared against when the key id is unknown, so lookups of
// unknown and known ids take comparable time.
var dummyHash = HashKey("gw_dummy_dummy")

// Authenticator verifies presented keys against a Directory.
type Authenticator struct{ dir Directory }

// New builds an Authenticator.
func New(dir Directory) *Authenticator { return &Authenticator{dir: dir} }

// Authenticate verifies the plaintext key and returns its principal. Every
// failure returns ErrUnauthenticated (directory outages are returned as-is so
// the caller can answer 5xx instead of 401).
func (a *Authenticator) Authenticate(ctx context.Context, key string) (Principal, error) {
	keyID := ParseKeyID(key)
	if keyID == "" {
		subtle.ConstantTimeCompare(HashKey(key), dummyHash)
		return Principal{}, ErrUnauthenticated
	}
	rec, err := a.dir.FindByKeyID(ctx, keyID)
	if errors.Is(err, ErrNotFound) {
		subtle.ConstantTimeCompare(HashKey(key), dummyHash)
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, err
	}
	match := subtle.ConstantTimeCompare(HashKey(key), rec.Hash) == 1
	if !match || !rec.Active {
		return Principal{}, ErrUnauthenticated
	}
	return rec.Principal, nil
}

// BearerToken extracts the token from an "Authorization: Bearer <token>" value.
func BearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}
