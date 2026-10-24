// Package config loads and validates process configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"strings"

	"unifiedgw/internal/secrets"
)

// Environment names the deployment mode.
type Environment string

const (
	EnvDev        Environment = "dev"
	EnvTest       Environment = "test"
	EnvProduction Environment = "production"
)

const defaultAddr = ":8080"

// Config is the validated process configuration.
type Config struct {
	Env         Environment
	Addr        string
	DatabaseURL secrets.Secret
	// Keys encrypts provider credentials. The process refuses to start without it.
	Keys *secrets.KeyRing
}

// Load reads configuration through getenv (os.Getenv in production) and
// reports every problem at once so a bad deployment is fixed in one pass.
func Load(getenv func(string) string) (Config, error) {
	var errs []error

	env := Environment(strings.ToLower(strings.TrimSpace(getenv("GATEWAY_ENV"))))
	switch env {
	case EnvDev, EnvTest, EnvProduction:
	case "":
		errs = append(errs, errors.New("GATEWAY_ENV is required (dev, test or production)"))
	default:
		errs = append(errs, fmt.Errorf("GATEWAY_ENV %q is not one of dev, test, production", env))
	}

	addr := strings.TrimSpace(getenv("GATEWAY_ADDR"))
	if addr == "" {
		addr = defaultAddr
	}

	dbURL := strings.TrimSpace(getenv("GATEWAY_DATABASE_URL"))
	if dbURL == "" {
		errs = append(errs, errors.New("GATEWAY_DATABASE_URL is required"))
	}

	keys, err := secrets.ParseKeyRing(getenv("GATEWAY_MASTER_KEYS"), getenv("GATEWAY_ACTIVE_KEY_ID"))
	if err != nil {
		errs = append(errs, fmt.Errorf("GATEWAY_MASTER_KEYS / GATEWAY_ACTIVE_KEY_ID: %w", err))
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return Config{Env: env, Addr: addr, DatabaseURL: secrets.New(dbURL), Keys: keys}, nil
}

// FakeProviderAllowed reports whether the Fake Provider may be registered.
func (c Config) FakeProviderAllowed() bool { return c.Env != EnvProduction }
