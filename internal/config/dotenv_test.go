package config

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatabaseURLEscapesPassword(t *testing.T) {
	got, err := DatabaseURL(env(map[string]string{
		"DB_HOST": "h", "DB_PORT": "5433", "DB_USER": "u", "DB_PASSWORD": "p@ss:w/rd", "DB_NAME": "db",
	}))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if pw, _ := u.User.Password(); pw != "p@ss:w/rd" || u.Host != "h:5433" || u.Path != "/db" {
		t.Errorf("bad url %q", got)
	}
}

func TestDatabaseURLRequiresFields(t *testing.T) {
	_, err := DatabaseURL(env(nil))
	if err == nil || !strings.Contains(err.Error(), "DB_HOST") {
		t.Errorf("got %v", err)
	}
}

func TestWithDefaultsCreatesStableKeyAndLoads(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "master.key")
	base := env(map[string]string{"DB_HOST": "h", "DB_PORT": "1", "DB_USER": "u", "DB_NAME": "d"})

	g1, err := WithDefaults(base, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(g1)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != EnvDev {
		t.Errorf("env = %q", cfg.Env)
	}
	g2, _ := WithDefaults(base, keyFile)
	if g1("GATEWAY_MASTER_KEYS") != g2("GATEWAY_MASTER_KEYS") {
		t.Error("key changed between runs")
	}
}

func TestWithDefaultsNoKeyFileInProduction(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "master.key")
	g, err := WithDefaults(env(map[string]string{"GATEWAY_ENV": "production", "GATEWAY_DATABASE_URL": "x"}), keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if g("GATEWAY_MASTER_KEYS") != "" {
		t.Error("production must not get a generated key")
	}
	if _, err := os.Stat(keyFile); err == nil {
		t.Error("key file created in production")
	}
}

func TestLoadDotenvDoesNotOverrideEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# c\nZZ_A=1\nZZ_B=\"two\"\nZZ_C=3\n"), 0o600)
	t.Setenv("ZZ_C", "keep")
	t.Cleanup(func() { os.Unsetenv("ZZ_A"); os.Unsetenv("ZZ_B") })
	if err := LoadDotenv(p); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ZZ_A") != "1" || os.Getenv("ZZ_B") != "two" || os.Getenv("ZZ_C") != "keep" {
		t.Error("unexpected values")
	}
}
