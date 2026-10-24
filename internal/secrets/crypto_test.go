package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func b64key(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func ring(t *testing.T) *KeyRing {
	t.Helper()
	r, err := ParseKeyRing("k1:"+b64key(t), "k1")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRoundTrip(t *testing.T) {
	r := ring(t)
	aad := CredentialAAD(7, 1, "paypal", "credentials")
	plain := []byte(`{"client_id":"abc","client_secret":"shh"}`)

	ct, keyID, err := r.Encrypt(plain, aad)
	if err != nil {
		t.Fatal(err)
	}
	if keyID != "k1" {
		t.Errorf("keyID = %q", keyID)
	}
	if bytes.Contains(ct, []byte("shh")) {
		t.Error("ciphertext contains plaintext")
	}
	got, err := r.Decrypt(keyID, ct, aad)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestEncryptUsesFreshNonce(t *testing.T) {
	r := ring(t)
	a, _, _ := r.Encrypt([]byte("x"), nil)
	b, _, _ := r.Encrypt([]byte("x"), nil)
	if bytes.Equal(a, b) {
		t.Error("two encryptions of the same plaintext were identical")
	}
}

func TestDecryptFailures(t *testing.T) {
	r := ring(t)
	aad := CredentialAAD(1, 1, "fake", "credentials")
	ct, keyID, _ := r.Encrypt([]byte("secret"), aad)

	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0x01

	cases := map[string]func() error{
		"tampered":      func() error { _, e := r.Decrypt(keyID, tampered, aad); return e },
		"wrong aad":     func() error { _, e := r.Decrypt(keyID, ct, CredentialAAD(2, 1, "fake", "credentials")); return e },
		"wrong version": func() error { _, e := r.Decrypt(keyID, ct, CredentialAAD(1, 2, "fake", "credentials")); return e },
		"wrong slot":    func() error { _, e := r.Decrypt(keyID, ct, CredentialAAD(1, 1, "fake", "webhook")); return e },
		"unknown key":   func() error { _, e := r.Decrypt("nope", ct, aad); return e },
		"truncated":     func() error { _, e := r.Decrypt(keyID, ct[:5], aad); return e },
		"empty":         func() error { _, e := r.Decrypt(keyID, nil, aad); return e },
		"other ring":    func() error { _, e := ring(t).Decrypt("k1", ct, aad); return e },
	}
	for name, f := range cases {
		if err := f(); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: got %v, want ErrDecrypt", name, err)
		}
	}
}

func TestRotationKeepsOldDataReadable(t *testing.T) {
	oldKey, newKey := b64key(t), b64key(t)
	oldRing, _ := ParseKeyRing("k1:"+oldKey, "k1")
	ct, keyID, _ := oldRing.Encrypt([]byte("data"), nil)

	rotated, err := ParseKeyRing("k1:"+oldKey+",k2:"+newKey, "k2")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rotated.Decrypt(keyID, ct, nil); err != nil || string(got) != "data" {
		t.Errorf("old data unreadable after rotation: %q %v", got, err)
	}
	_, id, _ := rotated.Encrypt([]byte("new"), nil)
	if id != "k2" {
		t.Errorf("new data used key %q, want k2", id)
	}
}

func TestParseKeyRingErrors(t *testing.T) {
	good := b64key(t)
	short := base64.StdEncoding.EncodeToString([]byte("too short"))
	tests := map[string]struct{ spec, active string }{
		"empty":        {"", "k1"},
		"no colon":     {"k1" + good, "k1"},
		"bad base64":   {"k1:!!!", "k1"},
		"short key":    {"k1:" + short, "k1"},
		"active unset": {"k1:" + good, "other"},
		"duplicate id": {"k1:" + good + ",k1:" + good, "k1"},
	}
	for name, tt := range tests {
		if _, err := ParseKeyRing(tt.spec, tt.active); err == nil {
			t.Errorf("%s: expected error", name)
		} else if strings.Contains(err.Error(), good) {
			t.Errorf("%s: error leaks key material", name)
		}
	}
}
