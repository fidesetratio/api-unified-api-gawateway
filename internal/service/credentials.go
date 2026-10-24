// Package service holds the application use cases. It applies the domain rules,
// coordinates the stores and provider adapters, and contains no HTTP code.
package service

import (
	"context"
	"encoding/json"
	"fmt"

	"unifiedgw/internal/provider"
	"unifiedgw/internal/secrets"
	"unifiedgw/internal/store"
)

// NewSealer returns a store.Sealer that encrypts the given plaintext maps with
// the active master key, binding each ciphertext to its config, version,
// provider and slot through AAD.
func NewSealer(keys *secrets.KeyRing, providerName string, cred, webhook map[string]string) store.Sealer {
	return func(configID int64, version int) ([]byte, []byte, string, error) {
		credJSON, err := json.Marshal(nonNilStrings(cred))
		if err != nil {
			return nil, nil, "", err
		}
		ct, keyID, err := keys.Encrypt(credJSON, secrets.CredentialAAD(configID, version, providerName, "credentials"))
		if err != nil {
			return nil, nil, "", err
		}
		var wct []byte
		if len(webhook) > 0 {
			wJSON, err := json.Marshal(webhook)
			if err != nil {
				return nil, nil, "", err
			}
			wct, _, err = keys.Encrypt(wJSON, secrets.CredentialAAD(configID, version, providerName, "webhook"))
			if err != nil {
				return nil, nil, "", err
			}
		}
		return ct, wct, keyID, nil
	}
}

func nonNilStrings(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// loadCredentials decrypts the config's active credentials (and webhook secret,
// if any). The plaintext exists only in the returned redacting map.
func loadCredentials(ctx context.Context, cs *store.Configs, keys *secrets.KeyRing, cfg store.Config) (cred, webhook provider.Credentials, version int, err error) {
	row, err := cs.ActiveCredentials(ctx, cfg.ID)
	if err != nil {
		return nil, nil, 0, err
	}
	cred, err = decryptMap(keys, row.KeyID, row.Ciphertext, secrets.CredentialAAD(cfg.ID, row.Version, cfg.Provider, "credentials"))
	if err != nil {
		return nil, nil, 0, err
	}
	if len(row.WebhookCiphertext) > 0 {
		webhook, err = decryptMap(keys, row.KeyID, row.WebhookCiphertext, secrets.CredentialAAD(cfg.ID, row.Version, cfg.Provider, "webhook"))
		if err != nil {
			return nil, nil, 0, err
		}
	}
	return cred, webhook, row.Version, nil
}

func decryptMap(keys *secrets.KeyRing, keyID string, ct, aad []byte) (provider.Credentials, error) {
	plain, err := keys.Decrypt(keyID, ct, aad)
	if err != nil {
		return nil, fmt.Errorf("decrypt credentials: %w", err)
	}
	var raw map[string]string
	if err := json.Unmarshal(plain, &raw); err != nil {
		return nil, fmt.Errorf("decode credentials: %w", err)
	}
	out := make(provider.Credentials, len(raw))
	for k, v := range raw {
		out[k] = secrets.New(v)
	}
	return out, nil
}
