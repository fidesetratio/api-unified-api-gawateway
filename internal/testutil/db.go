// Package testutil provides helpers for integration tests that need PostgreSQL.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"unifiedgw/internal/store"
	"unifiedgw/migrations"
)

// EnvTestDB names the variable holding the integration-test database URL, e.g.
// postgres://postgres:PASSWORD@127.0.0.1:5432/gateway_test?sslmode=disable
const EnvTestDB = "GATEWAY_TEST_DATABASE_URL"

// NewMigratedPool returns a pool whose search_path is a fresh, throwaway schema
// with every migration applied. The schema is dropped when the test ends, so
// tests are isolated and never touch the public schema. The test is skipped when
// EnvTestDB is not set.
func NewMigratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(EnvTestDB)
	if url == "" {
		t.Skipf("%s not set; skipping database test", EnvTestDB)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		admin.Close()
		t.Fatalf("parse url: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		admin.Close()
		t.Fatalf("connect with schema: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_, _ = admin.Exec(cctx, `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
	})

	if _, err := store.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}
