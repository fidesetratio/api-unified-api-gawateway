package config

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DefaultKeyFile holds the auto-generated development master key.
const DefaultKeyFile = "master.key"

const devKeyID = "local1"

// LoadDotenv reads KEY=VALUE lines from path into the process environment.
// Variables already set in the environment win. A missing file is not an error.
func LoadDotenv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), string(rune(0xFEFF))))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set {
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

// LoadDefaultDotenv loads the first .env found in the working directory, next
// to the executable, or in the executable's parent directory (so bin\gateway.exe
// finds result\.env). It returns the directory used, "." when none was found.
func LoadDefaultDotenv() (string, error) {
	dirs := []string{"."}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		dirs = append(dirs, d, filepath.Dir(d))
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, ".env")); err == nil {
			return d, LoadDotenv(filepath.Join(d, ".env"))
		}
	}
	return ".", nil
}

// DatabaseURL builds a PostgreSQL URL from DB_HOST, DB_PORT, DB_USER,
// DB_PASSWORD and DB_NAME. Special characters in the password are escaped.
func DatabaseURL(getenv func(string) string) (string, error) {
	host := strings.TrimSpace(getenv("DB_HOST"))
	port := strings.TrimSpace(getenv("DB_PORT"))
	user := strings.TrimSpace(getenv("DB_USER"))
	name := strings.TrimSpace(getenv("DB_NAME"))
	var errs []error
	for k, v := range map[string]string{"DB_HOST": host, "DB_PORT": port, "DB_USER": user, "DB_NAME": name} {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", k))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return "", err
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, getenv("DB_PASSWORD")),
		Host:     net.JoinHostPort(host, port),
		Path:     "/" + name,
		RawQuery: "sslmode=disable",
	}
	return u.String(), nil
}

// WithDefaults returns a getenv that fills what the .env file does not carry:
// the database URL from DB_*, GATEWAY_ENV=dev, and (outside production) a
// master key kept in keyFile, generated on first use. Explicit GATEWAY_*
// variables always win.
func WithDefaults(getenv func(string) string, keyFile string) (func(string) string, error) {
	over := map[string]string{}

	if strings.TrimSpace(getenv("GATEWAY_ENV")) == "" {
		over["GATEWAY_ENV"] = string(EnvDev)
	}
	if strings.TrimSpace(getenv("GATEWAY_DATABASE_URL")) == "" {
		u, err := DatabaseURL(getenv)
		if err != nil {
			return nil, err
		}
		over["GATEWAY_DATABASE_URL"] = u
	}
	production := strings.EqualFold(strings.TrimSpace(getenv("GATEWAY_ENV")), string(EnvProduction))
	if strings.TrimSpace(getenv("GATEWAY_MASTER_KEYS")) == "" && !production {
		spec, err := loadOrCreateKey(keyFile)
		if err != nil {
			return nil, err
		}
		over["GATEWAY_MASTER_KEYS"] = spec
		over["GATEWAY_ACTIVE_KEY_ID"] = devKeyID
	}

	return func(k string) string {
		if v, ok := over[k]; ok {
			return v
		}
		return getenv(k)
	}, nil
}

func loadOrCreateKey(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if spec := strings.TrimSpace(string(b)); spec != "" {
			return spec, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	spec := devKeyID + ":" + base64.StdEncoding.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(spec+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return spec, nil
}
