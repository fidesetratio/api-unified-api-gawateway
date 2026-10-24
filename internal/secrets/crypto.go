package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const keySize = 32 // AES-256

// ErrDecrypt is returned for any decryption failure (wrong key, wrong AAD,
// tampering, truncation). It deliberately carries no detail.
var ErrDecrypt = errors.New("decryption failed")

// KeyRing holds master keys by id. New data is encrypted with the active key;
// old ids stay available so data encrypted before a rotation can still be read.
type KeyRing struct {
	activeID string
	keys     map[string][]byte
}

// ParseKeyRing builds a ring from "id:base64key,id2:base64key2" and the id of the
// key to encrypt with. Every key must decode to exactly 32 bytes.
func ParseKeyRing(spec, activeID string) (*KeyRing, error) {
	ring := &KeyRing{activeID: strings.TrimSpace(activeID), keys: map[string][]byte{}}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, enc, ok := strings.Cut(part, ":")
		id = strings.TrimSpace(id)
		if !ok || id == "" {
			return nil, errors.New("master key entries must look like id:base64key")
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
		if err != nil {
			return nil, fmt.Errorf("master key %q is not valid base64", id)
		}
		if len(key) != keySize {
			return nil, fmt.Errorf("master key %q must be %d bytes, got %d", id, keySize, len(key))
		}
		if _, dup := ring.keys[id]; dup {
			return nil, fmt.Errorf("master key id %q appears twice", id)
		}
		ring.keys[id] = key
	}
	if len(ring.keys) == 0 {
		return nil, errors.New("no master keys configured")
	}
	if _, ok := ring.keys[ring.activeID]; !ok {
		return nil, fmt.Errorf("active master key id %q is not in the key ring", ring.activeID)
	}
	return ring, nil
}

// ActiveID returns the id of the key used for new encryptions.
func (r *KeyRing) ActiveID() string { return r.activeID }

// Encrypt seals plaintext with the active key. aad binds the ciphertext to its
// context; the same aad must be supplied to Decrypt. The result is
// nonce || ciphertext+tag, and the key id used is returned for storage.
func (r *KeyRing) Encrypt(plaintext, aad []byte) (ciphertext []byte, keyID string, err error) {
	gcm, err := newGCM(r.keys[r.activeID])
	if err != nil {
		return nil, "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", fmt.Errorf("generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), r.activeID, nil
}

// Decrypt opens data produced by Encrypt using the key named keyID.
func (r *KeyRing) Decrypt(keyID string, data, aad []byte) ([]byte, error) {
	key, ok := r.keys[keyID]
	if !ok {
		return nil, ErrDecrypt
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, ErrDecrypt
	}
	if len(data) < gcm.NonceSize()+gcm.Overhead() {
		return nil, ErrDecrypt
	}
	nonce, sealed := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, sealed, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// CredentialAAD binds a ciphertext to one config, version, provider and slot
// ("credentials" or "webhook"), so rows cannot be swapped without detection.
func CredentialAAD(configID int64, version int, provider, slot string) []byte {
	return []byte("cred|" + strconv.FormatInt(configID, 10) + "|" + strconv.Itoa(version) + "|" + provider + "|" + slot)
}
