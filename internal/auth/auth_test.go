package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeDir map[string]Record

func (d fakeDir) FindByKeyID(_ context.Context, id string) (Record, error) {
	r, ok := d[id]
	if !ok {
		return Record{}, ErrNotFound
	}
	return r, nil
}

type brokenDir struct{}

func (brokenDir) FindByKeyID(context.Context, string) (Record, error) {
	return Record{}, errors.New("db down")
}

func issue(t *testing.T, active bool, role string) (string, fakeDir) {
	t.Helper()
	key, id, hash, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return key, fakeDir{id: {Principal: Principal{ID: 3, Name: "shop", Role: role}, Hash: hash, Active: active}}
}

func TestNewKeyShapeAndHash(t *testing.T) {
	key, id, hash, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "gw_"+id+"_") {
		t.Errorf("key %q does not embed id %q", key, id)
	}
	if ParseKeyID(key) != id {
		t.Errorf("ParseKeyID = %q want %q", ParseKeyID(key), id)
	}
	if len(hash) != 32 || !bytes.Equal(hash, HashKey(key)) {
		t.Error("hash is not sha256 of the key")
	}
	other, _, _, _ := NewKey()
	if other == key {
		t.Error("keys must be unique")
	}
}

func TestAuthenticate(t *testing.T) {
	key, dir := issue(t, true, RoleCaller)
	a := New(dir)

	p, err := a.Authenticate(context.Background(), key)
	if err != nil || p.ID != 3 || p.Role != RoleCaller {
		t.Fatalf("got %+v, %v", p, err)
	}

	wrongSecret := key[:len(key)-1] + "0"
	if key[len(key)-1] == '0' {
		wrongSecret = key[:len(key)-1] + "1"
	}
	_, inactiveDir := issue(t, false, RoleCaller)
	inactiveKey := ""
	for id := range inactiveDir { // rebuild a matching key for the disabled client
		inactiveKey = "gw_" + id + "_x"
	}

	for name, tc := range map[string]struct {
		a   *Authenticator
		key string
	}{
		"wrong secret":   {a, wrongSecret},
		"empty":          {a, ""},
		"garbage":        {a, "hello"},
		"unknown id":     {a, "gw_0123456789abcdef_" + strings.Repeat("a", 64)},
		"wrong prefix":   {a, strings.Replace(key, "gw_", "xx_", 1)},
		"disabled":       {New(inactiveDir), inactiveKey},
		"missing secret": {a, "gw_" + ParseKeyID(key) + "_"},
	} {
		if _, err := tc.a.Authenticate(context.Background(), tc.key); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: got %v, want ErrUnauthenticated", name, err)
		}
	}
}

func TestDisabledClientWithCorrectKeyIsRejected(t *testing.T) {
	key, dir := issue(t, false, RoleCaller)
	if _, err := New(dir).Authenticate(context.Background(), key); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("got %v", err)
	}
}

func TestDirectoryOutageIsNotUnauthenticated(t *testing.T) {
	key, _, _, _ := NewKey()
	_, err := New(brokenDir{}).Authenticate(context.Background(), key)
	if err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a database outage must not look like a bad key, got %v", err)
	}
}

func TestBearerToken(t *testing.T) {
	for in, want := range map[string]string{"Bearer abc": "abc", "bearer abc": "abc", "Bearer  abc ": "abc"} {
		if got, ok := BearerToken(in); !ok || got != want {
			t.Errorf("%q: got %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "Bearer", "Bearer ", "Basic abc", "abc"} {
		if _, ok := BearerToken(in); ok {
			t.Errorf("%q should not parse", in)
		}
	}
}
