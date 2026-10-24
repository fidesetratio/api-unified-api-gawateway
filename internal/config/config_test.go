package config

import (
	"fmt"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// testKeys is a throwaway 32-byte key (base64 of 32 x 'k').
const testKeys = "k1:a2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2s="

func TestLoadValid(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"GATEWAY_ENV":           "Dev",
		"GATEWAY_DATABASE_URL":  "postgres://u:p@h/db",
		"GATEWAY_MASTER_KEYS":   testKeys,
		"GATEWAY_ACTIVE_KEY_ID": "k1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != EnvDev || cfg.Addr != ":8080" {
		t.Errorf("got env=%q addr=%q", cfg.Env, cfg.Addr)
	}
	if cfg.DatabaseURL.Reveal() != "postgres://u:p@h/db" {
		t.Error("database url not kept")
	}
	if strings.Contains(fmt.Sprintf("%+v", cfg), "u:p@h") {
		t.Error("config formatting leaked the database url")
	}
}

func TestLoadReportsAllProblems(t *testing.T) {
	_, err := Load(env(nil))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"GATEWAY_ENV", "GATEWAY_DATABASE_URL", "GATEWAY_MASTER_KEYS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadRejectsUnknownEnv(t *testing.T) {
	_, err := Load(env(map[string]string{
		"GATEWAY_ENV":          "staging",
		"GATEWAY_DATABASE_URL": "x",
	}))
	if err == nil || !strings.Contains(err.Error(), "staging") {
		t.Errorf("expected unknown env error, got %v", err)
	}
}

func TestFakeProviderNotAllowedInProduction(t *testing.T) {
	for e, want := range map[Environment]bool{EnvDev: true, EnvTest: true, EnvProduction: false} {
		if got := (Config{Env: e}).FakeProviderAllowed(); got != want {
			t.Errorf("%s: got %v want %v", e, got, want)
		}
	}
}
