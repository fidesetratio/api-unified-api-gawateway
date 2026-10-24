package cli_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"unifiedgw/internal/auth"
	"unifiedgw/internal/cli"
	"unifiedgw/internal/domain"
	"unifiedgw/internal/secrets"
	"unifiedgw/internal/service"
	"unifiedgw/internal/store"
	"unifiedgw/internal/testutil"
)

type result struct {
	code     int
	out, err string
}

func run(t *testing.T, pool *pgxpool.Pool, env map[string]string, keys *secrets.KeyRing, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := cli.Run(context.Background(), args, cli.Deps{
		Out: &out, Err: &errb, Pool: pool, Keys: keys,
		Getenv:    func(k string) string { return env[k] },
		Reconcile: func(context.Context) (int, int, error) { return 2, 1, nil },
	})
	return result{code, strings.TrimSpace(out.String()), errb.String()}
}

func count(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOperatorIdentityIsRequired(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	r := run(t, pool, nil, nil, "client", "create", "--name", "shop", "--role", "caller")
	if r.code != 1 || !strings.Contains(r.err, "operator identity is required") {
		t.Errorf("got %+v", r)
	}
	if n := count(t, pool, `SELECT count(*) FROM api_clients`); n != 0 {
		t.Errorf("a client was created without an operator: %d", n)
	}
	for _, args := range [][]string{
		{"client", "disable", "--name", "x"},
		{"review", "resolve", "--id", "1", "--resolution", "x"},
		{"key", "reencrypt"},
		{"reconcile", "run"},
	} {
		if r := run(t, pool, nil, nil, args...); r.code != 1 || !strings.Contains(r.err, "operator identity") {
			t.Errorf("%v: %+v", args, r)
		}
	}
	// Read-only commands do not need one.
	if r := run(t, pool, nil, nil, "review", "list"); r.code != 0 {
		t.Errorf("review list: %+v", r)
	}
	if r := run(t, pool, nil, nil, "migrate", "status"); r.code != 0 || !strings.Contains(r.out, "13 migrations applied") {
		t.Errorf("migrate status: %+v", r)
	}
}

func TestClientLifecycleAndAudit(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()
	authn := auth.New(store.NewClients(pool))

	r := run(t, pool, nil, nil, "client", "create", "--name", "shop", "--role", "caller", "--operator", "alice")
	if r.code != 0 || !strings.HasPrefix(r.out, "gw_") {
		t.Fatalf("create: %+v", r)
	}
	if !strings.Contains(r.err, "only once") || strings.Contains(r.err, r.out) {
		t.Errorf("the key must go to stdout only, with a warning on stderr: %q", r.err)
	}
	key := r.out
	if p, err := authn.Authenticate(ctx, key); err != nil || p.Name != "shop" || p.Role != auth.RoleCaller {
		t.Fatalf("new key does not authenticate: %+v %v", p, err)
	}
	// Only a hash is stored.
	if n := count(t, pool, `SELECT count(*) FROM api_clients WHERE position($1::bytea in key_hash) > 0`, []byte(key)); n != 0 {
		t.Error("plaintext key found in key_hash")
	}

	// Operator may also come from the environment.
	r2 := run(t, pool, map[string]string{"GATEWAYCTL_OPERATOR": "bob"}, nil, "client", "create", "--name", "ops-admin", "--role", "admin")
	if r2.code != 0 {
		t.Fatalf("%+v", r2)
	}

	// Rotate: the old key stops working.
	rot := run(t, pool, nil, nil, "client", "rotate-key", "--name", "shop", "--operator", "alice")
	if rot.code != 0 || rot.out == key {
		t.Fatalf("rotate: %+v", rot)
	}
	if _, err := authn.Authenticate(ctx, key); err == nil {
		t.Error("old key still works after rotation")
	}
	if _, err := authn.Authenticate(ctx, rot.out); err != nil {
		t.Errorf("new key rejected: %v", err)
	}

	// Disable / enable.
	if r := run(t, pool, nil, nil, "client", "disable", "--name", "shop", "--operator", "alice"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if _, err := authn.Authenticate(ctx, rot.out); err == nil {
		t.Error("disabled client still authenticates")
	}
	run(t, pool, nil, nil, "client", "enable", "--name", "shop", "--operator", "alice")
	if _, err := authn.Authenticate(ctx, rot.out); err != nil {
		t.Errorf("re-enabled client rejected: %v", err)
	}

	// List shows ids and statuses, never keys.
	list := run(t, pool, nil, nil, "client", "list")
	if list.code != 0 || !strings.Contains(list.out, "shop\tcaller\tactive") || !strings.Contains(list.out, "ops-admin\tadmin") ||
		strings.Contains(list.out, "gw_") {
		t.Errorf("list: %+v", list)
	}

	// Errors.
	if r := run(t, pool, nil, nil, "client", "create", "--name", "shop", "--operator", "alice"); r.code != 1 {
		t.Errorf("duplicate name should fail: %+v", r)
	}
	if r := run(t, pool, nil, nil, "client", "create", "--name", "x", "--role", "root", "--operator", "alice"); r.code != 1 {
		t.Errorf("bad role should fail: %+v", r)
	}
	if r := run(t, pool, nil, nil, "client", "disable", "--name", "ghost", "--operator", "alice"); r.code != 1 {
		t.Errorf("unknown client should fail: %+v", r)
	}

	// Audit trail names the operator and never holds a key.
	for action, op := range map[string]string{"client.create": "alice", "client.rotate_key": "alice", "client.disable": "alice", "client.enable": "alice"} {
		if n := count(t, pool, `SELECT count(*) FROM audit_log WHERE action=$1 AND actor_type='operator' AND actor_id=$2`, action, op); n == 0 {
			t.Errorf("no audit row %s by %s", action, op)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM audit_log WHERE actor_id='bob' AND action='client.create'`); n != 1 {
		t.Errorf("env operator not audited: %d", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM audit_log WHERE position($1 in details::text) > 0`, key); n != 0 {
		t.Error("API key found in audit log")
	}
}

func TestReviewListAndResolve(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()
	clients, payments := store.NewClients(pool), store.NewPayments(pool)
	p, _, err := clients.Create(ctx, "shop", auth.RoleCaller)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.NewConfigs(pool).Create(ctx, store.NewConfig{Name: "c", Provider: "fake", Environment: "sandbox",
		AllowedCurrencies: []string{"USD"}}, func(int64, int) ([]byte, []byte, string, error) { return []byte{1}, nil, "k", nil },
		store.Actor{Type: "system", ID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	amt, _ := domain.ParseAmount("5.00", "USD")
	tx, _, err := payments.InsertOrGet(ctx, store.NewPayment{ClientID: p.ID, ConfigID: cfg.ID, UniqueCode: "REV-1", Amount: amt, Fingerprint: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if err := payments.OpenReview(ctx, tx.ID, "paid_after_expiry", map[string]any{"why": "test"}); err != nil {
		t.Fatal(err)
	}

	list := run(t, pool, nil, nil, "review", "list")
	if list.code != 0 || !strings.Contains(list.out, "paid_after_expiry") || !strings.Contains(list.out, "REV-1") {
		t.Fatalf("list: %+v", list)
	}
	var id string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM review_items`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if r := run(t, pool, nil, nil, "review", "resolve", "--id", id, "--operator", "carol"); r.code != 1 {
		t.Errorf("resolve without a resolution must fail: %+v", r)
	}
	if r := run(t, pool, nil, nil, "review", "resolve", "--id", id, "--resolution", "customer refunded manually", "--operator", "carol"); r.code != 0 {
		t.Fatalf("resolve: %+v", r)
	}
	if r := run(t, pool, nil, nil, "review", "resolve", "--id", id, "--resolution", "again", "--operator", "carol"); r.code != 1 {
		t.Errorf("resolving twice must fail: %+v", r)
	}
	if after := run(t, pool, nil, nil, "review", "list"); !strings.Contains(after.out, "no open review items") {
		t.Errorf("after resolve: %+v", after)
	}
	var by, why string
	if err := pool.QueryRow(ctx, `SELECT resolved_by, resolution FROM review_items WHERE id=$1::bigint`, id).Scan(&by, &why); err != nil || by != "carol" || why != "customer refunded manually" {
		t.Errorf("resolved_by=%q resolution=%q err=%v", by, why, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM audit_log WHERE action='review.resolve' AND actor_id='carol'`); n != 1 {
		t.Errorf("review.resolve audit rows = %d", n)
	}
}

func newKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func TestKeyReencrypt(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()
	k1, k2 := newKey(t), newKey(t)
	oldRing, _ := secrets.ParseKeyRing("k1:"+k1, "k1")
	newRing, err := secrets.ParseKeyRing("k1:"+k1+",k2:"+k2, "k2")
	if err != nil {
		t.Fatal(err)
	}

	configs := store.NewConfigs(pool)
	cfg, err := configs.Create(ctx, store.NewConfig{Name: "p", Provider: "paypal", Environment: "sandbox", AllowedCurrencies: []string{"USD"}},
		service.NewSealer(oldRing, "paypal", map[string]string{"client_id": "a", "client_secret": "s3"}, map[string]string{"webhook_id": "w1"}),
		store.Actor{Type: "system", ID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if row, _ := configs.ActiveCredentials(ctx, cfg.ID); row.KeyID != "k1" {
		t.Fatalf("setup key = %s", row.KeyID)
	}

	r := run(t, pool, nil, newRing, "key", "reencrypt", "--operator", "dave")
	if r.code != 0 || !strings.Contains(r.out, "re-encrypted 1 credential rows under key \"k2\"") {
		t.Fatalf("%+v", r)
	}
	row, err := configs.ActiveCredentials(ctx, cfg.ID)
	if err != nil || row.KeyID != "k2" {
		t.Fatalf("key after reencrypt = %s (%v)", row.KeyID, err)
	}
	// Readable with only the new key, and bound to the same AAD.
	onlyNew, _ := secrets.ParseKeyRing("k2:"+k2, "k2")
	plain, err := onlyNew.Decrypt("k2", row.Ciphertext, secrets.CredentialAAD(cfg.ID, row.Version, "paypal", "credentials"))
	if err != nil || !strings.Contains(string(plain), "s3") {
		t.Errorf("credentials unreadable after reencrypt: %v", err)
	}
	if _, err := onlyNew.Decrypt("k2", row.WebhookCiphertext, secrets.CredentialAAD(cfg.ID, row.Version, "paypal", "webhook")); err != nil {
		t.Errorf("webhook secret unreadable after reencrypt: %v", err)
	}
	// Second run has nothing to do.
	if again := run(t, pool, nil, newRing, "key", "reencrypt", "--operator", "dave"); !strings.Contains(again.out, "re-encrypted 0") {
		t.Errorf("second run: %+v", again)
	}
	// Without master keys the command refuses.
	if r := run(t, pool, nil, nil, "key", "reencrypt", "--operator", "dave"); r.code != 1 {
		t.Errorf("no keys: %+v", r)
	}
	if n := count(t, pool, `SELECT count(*) FROM audit_log WHERE action='key.reencrypt' AND actor_id='dave'`); n != 2 {
		t.Errorf("audit rows = %d, want 2", n)
	}
}

func TestReconcileAndUsage(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	r := run(t, pool, nil, nil, "reconcile", "run", "--operator", "erin")
	if r.code != 0 || r.out != "expired 2, resumed 1" {
		t.Errorf("%+v", r)
	}
	if n := count(t, pool, `SELECT count(*) FROM audit_log WHERE action='reconcile.run' AND actor_id='erin'`); n != 1 {
		t.Errorf("audit rows = %d", n)
	}
	if r := run(t, pool, nil, nil); r.code != 2 || !strings.Contains(r.err, "usage") {
		t.Errorf("no args: %+v", r)
	}
	if r := run(t, pool, nil, nil, "frobnicate"); r.code != 2 {
		t.Errorf("unknown command: %+v", r)
	}
	if r := run(t, pool, nil, nil, "help"); r.code != 0 || !strings.Contains(r.out, "gatewayctl") {
		t.Errorf("help: %+v", r)
	}
}
