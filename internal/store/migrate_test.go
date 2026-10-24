package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"unifiedgw/internal/store"
	"unifiedgw/internal/testutil"
	"unifiedgw/migrations"
)

func TestMigrationsAreRerunnable(t *testing.T) {
	pool := testutil.NewMigratedPool(t) // already applied once
	ctx := context.Background()

	applied, err := store.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Errorf("second run applied %v, want nothing", applied)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 13 {
		t.Errorf("schema_migrations has %d rows, want 13", n)
	}
}

func TestHistoryAndAuditAreAppendOnly(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO audit_log (actor_type, actor_id, action) VALUES ('admin','a','x')`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE audit_log SET action = 'y'`,
		`DELETE FROM audit_log`,
		`TRUNCATE audit_log`,
	} {
		_, err := pool.Exec(ctx, stmt)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || !strings.Contains(pgErr.Message, "append-only") {
			t.Errorf("%s: got %v, want append-only error", stmt, err)
		}
	}
}

func TestConstraints(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()
	fp := make([]byte, 32)

	var clientID, cfgID int64
	must(t, pool.QueryRow(ctx, `INSERT INTO api_clients (name, key_id, key_hash) VALUES ('c','k',$1) RETURNING id`, fp).Scan(&clientID))
	must(t, pool.QueryRow(ctx, `INSERT INTO provider_configs (name, provider, environment, allowed_currencies)
		VALUES ('p','fake','sandbox','{USD}') RETURNING id`).Scan(&cfgID))

	insert := func(code string, amount string, status string) error {
		_, err := pool.Exec(ctx, `INSERT INTO payment_transactions
			(client_id, provider_config_id, uniquecode, amount, currency, status, request_fingerprint)
			VALUES ($1,$2,$3,$4::numeric,'USD',$5,$6)`, clientID, cfgID, code, amount, status, fp)
		return err
	}
	must(t, insert("ORDER-1", "10.00", "creating"))

	cases := []struct {
		name string
		err  error
		code string
	}{
		{"duplicate uniquecode", insert("ORDER-1", "10.00", "creating"), "23505"},
		{"negative amount", insert("ORDER-2", "-1", "creating"), "23514"},
		{"bad status", insert("ORDER-3", "1", "bogus"), "23514"},
		{"bad uniquecode", insert("bad code!", "1", "creating"), "23514"},
	}
	for _, c := range cases {
		var pgErr *pgconn.PgError
		if !errors.As(c.err, &pgErr) || pgErr.Code != c.code {
			t.Errorf("%s: got %v, want SQLSTATE %s", c.name, c.err, c.code)
		}
	}

	// A different caller may reuse the same uniquecode (decision D1).
	var otherID int64
	must(t, pool.QueryRow(ctx, `INSERT INTO api_clients (name, key_id, key_hash) VALUES ('c2','k2',$1) RETURNING id`, fp).Scan(&otherID))
	_, err := pool.Exec(ctx, `INSERT INTO payment_transactions
		(client_id, provider_config_id, uniquecode, amount, currency, status, request_fingerprint)
		VALUES ($1,$2,'ORDER-1',10,'USD','creating',$3)`, otherID, cfgID, fp)
	must(t, err)
}

func TestAppRoleCannotRewriteHistory(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	must(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	// Superuser test connection can assume the app role.
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE gateway_app`); err != nil {
		t.Skipf("cannot assume gateway_app: %v", err)
	}
	for _, stmt := range []string{
		`UPDATE payment_status_history SET reason = 'x'`,
		`CREATE TABLE sneaky (id int)`,
		`UPDATE api_clients SET status = 'disabled'`,
	} {
		sp, err := tx.Begin(ctx)
		must(t, err)
		_, err = sp.Exec(ctx, stmt)
		_ = sp.Rollback(ctx)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" { // insufficient_privilege
			t.Errorf("%s: got %v, want insufficient_privilege", stmt, err)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
