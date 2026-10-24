package service_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"unifiedgw/internal/auth"
	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
	"unifiedgw/internal/provider/fake"
	"unifiedgw/internal/secrets"
	"unifiedgw/internal/service"
	"unifiedgw/internal/store"
	"unifiedgw/internal/testutil"
)

type env struct {
	pool     *pgxpool.Pool
	svc      *service.Payments
	fake     *fake.Provider
	configs  *store.Configs
	payments *store.Payments
	clients  *store.Clients
	keys     *secrets.KeyRing
	reg      *provider.Registry
	a, b     auth.Principal
	cfgID    int64
}

var testActor = store.Actor{Type: "system", ID: "test"}

func newEnv(t *testing.T, opt service.Options) *env {
	t.Helper()
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()

	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	keys, err := secrets.ParseKeyRing("k1:"+base64.StdEncoding.EncodeToString(raw), "k1")
	must(t, err)

	e := &env{
		pool: pool, fake: fake.New(), keys: keys,
		configs: store.NewConfigs(pool), payments: store.NewPayments(pool), clients: store.NewClients(pool),
	}
	reg := provider.NewRegistry()
	must(t, reg.Register(e.fake))
	must(t, reg.Register(provider.NewStub("paypal")))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.reg = reg
	e.svc = service.NewPayments(e.payments, e.configs, e.clients, keys, reg, log, opt)

	e.a, _, err = e.clients.Create(ctx, "caller-a", auth.RoleCaller)
	must(t, err)
	e.b, _, err = e.clients.Create(ctx, "caller-b", auth.RoleCaller)
	must(t, err)

	cfg := e.newConfig(t, "fake", map[string]any{})
	e.cfgID = cfg.ID
	must(t, e.configs.Grant(ctx, cfg.ID, e.a.ID, testActor))
	must(t, e.configs.Grant(ctx, cfg.ID, e.b.ID, testActor))
	return e
}

func (e *env) newConfig(t *testing.T, providerName string, settings map[string]any) store.Config {
	t.Helper()
	cfg, err := e.configs.Create(context.Background(), store.NewConfig{
		Name: "cfg-" + providerName + "-" + randHex(), Provider: providerName, Environment: "sandbox",
		AllowedCurrencies: []string{"USD", "EUR"}, Settings: settings,
	}, service.NewSealer(e.keys, providerName, map[string]string{"client_secret": "very-secret-value"}, nil), testActor)
	must(t, err)
	return cfg
}

func randHex() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func in(cfg int64, code, amount string) service.CreateInput {
	return service.CreateInput{ConfigID: cfg, UniqueCode: code, Amount: amount, Currency: "USD"}
}

func isDomainErr(err error, want *domain.Error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == want.Code
}

func countRows(t *testing.T, e *env, query string, args ...any) int {
	t.Helper()
	var n int
	must(t, e.pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func TestCreateHappyPathAndReplay(t *testing.T) {
	e := newEnv(t, service.Options{})
	ctx := context.Background()

	v, created, err := e.svc.Create(ctx, e.a, in(e.cfgID, "ORDER-10001", "10.00"))
	must(t, err)
	if !created || v.Status != domain.StatusPending || !strings.HasPrefix(v.PaymentURL, "https://fake.invalid/pay/") || v.UniqueCode != "ORDER-10001" {
		t.Fatalf("created=%v view=%+v", created, v)
	}

	again, created2, err := e.svc.Create(ctx, e.a, in(e.cfgID, "ORDER-10001", "10.00"))
	must(t, err)
	if created2 || again != v {
		t.Errorf("replay: created=%v view=%+v want %+v", created2, again, v)
	}
	// "10" is the same amount as "10.00", so it is a replay, not a conflict.
	if _, c, err := e.svc.Create(ctx, e.a, in(e.cfgID, "ORDER-10001", "10")); err != nil || c {
		t.Errorf("equivalent amount: created=%v err=%v", c, err)
	}
	if e.fake.CreateCalls() != 1 {
		t.Errorf("provider create calls = %d, want 1", e.fake.CreateCalls())
	}
	if n := countRows(t, e, `SELECT count(*) FROM payment_status_history`); n != 2 { // creating, pending
		t.Errorf("history rows = %d, want 2", n)
	}
}

func TestConflictOnDifferentParameters(t *testing.T) {
	e := newEnv(t, service.Options{})
	ctx := context.Background()
	_, _, err := e.svc.Create(ctx, e.a, in(e.cfgID, "X1", "10.00"))
	must(t, err)

	if _, _, err := e.svc.Create(ctx, e.a, in(e.cfgID, "X1", "11.00")); !isDomainErr(err, domain.ErrUniquecodeConflict) {
		t.Errorf("different amount: %v", err)
	}
	other := e.newConfig(t, "fake", nil)
	must(t, e.configs.Grant(ctx, other.ID, e.a.ID, testActor))
	if _, _, err := e.svc.Create(ctx, e.a, in(other.ID, "X1", "10.00")); !isDomainErr(err, domain.ErrUniquecodeConflict) {
		t.Errorf("different config: %v", err)
	}
	eur := in(e.cfgID, "X1", "10.00")
	eur.Currency = "EUR"
	if _, _, err := e.svc.Create(ctx, e.a, eur); !isDomainErr(err, domain.ErrUniquecodeConflict) {
		t.Errorf("different currency: %v", err)
	}
}

func TestAuthorizationAndValidation(t *testing.T) {
	e := newEnv(t, service.Options{})
	ctx := context.Background()

	ungranted := e.newConfig(t, "fake", nil)
	disabled := e.newConfig(t, "fake", nil)
	must(t, e.configs.Grant(ctx, disabled.ID, e.a.ID, testActor))
	off := "disabled"
	_, err := e.configs.Update(ctx, disabled.ID, store.ConfigUpdate{Status: &off}, testActor)
	must(t, err)

	cases := []struct {
		name string
		in   service.CreateInput
		want *domain.Error
	}{
		{"not granted", in(ungranted.ID, "A", "1.00"), domain.ErrConfigNotPermitted},
		{"unknown config looks the same", in(999999, "A", "1.00"), domain.ErrConfigNotPermitted},
		{"disabled config", in(disabled.ID, "A", "1.00"), domain.ErrConfigNotFound},
		{"currency not allowed", service.CreateInput{ConfigID: e.cfgID, UniqueCode: "A", Amount: "1.00", Currency: "IDR"}, domain.ErrCurrencyUnsupported},
		{"zero id", in(0, "A", "1.00"), domain.ErrInvalidRequest},
		{"bad uniquecode", in(e.cfgID, "bad code!", "1.00"), domain.ErrInvalidRequest},
		{"empty uniquecode", in(e.cfgID, "", "1.00"), domain.ErrInvalidRequest},
		{"long uniquecode", in(e.cfgID, strings.Repeat("a", 65), "1.00"), domain.ErrInvalidRequest},
		{"zero amount", in(e.cfgID, "A", "0.00"), domain.ErrInvalidRequest},
		{"negative amount", in(e.cfgID, "A", "-1"), domain.ErrInvalidRequest},
		{"too many decimals", in(e.cfgID, "A", "1.234"), domain.ErrInvalidRequest},
	}
	for _, c := range cases {
		if _, _, err := e.svc.Create(ctx, e.a, c.in); !isDomainErr(err, c.want) {
			t.Errorf("%s: got %v, want %s", c.name, err, c.want.Code)
		}
	}
	if e.fake.CreateCalls() != 0 {
		t.Error("rejected requests must never reach the provider")
	}
	if n := countRows(t, e, `SELECT count(*) FROM payment_transactions`); n != 0 {
		t.Errorf("rejected requests left %d transactions", n)
	}
}

func TestUniquecodeIsPerCallerAndStatusIsScoped(t *testing.T) {
	e := newEnv(t, service.Options{})
	ctx := context.Background()

	va, _, err := e.svc.Create(ctx, e.a, in(e.cfgID, "SHARED", "5.00"))
	must(t, err)
	vb, created, err := e.svc.Create(ctx, e.b, in(e.cfgID, "SHARED", "7.00"))
	must(t, err)
	if !created || va.PaymentURL == vb.PaymentURL {
		t.Errorf("callers must get separate payments: %+v %+v", va, vb)
	}

	only, _, err := e.svc.Create(ctx, e.a, in(e.cfgID, "ONLY-A", "1.00"))
	must(t, err)
	if got, err := e.svc.Status(ctx, e.a, "ONLY-A"); err != nil || got != only {
		t.Errorf("status = %+v %v", got, err)
	}
	if _, err := e.svc.Status(ctx, e.b, "ONLY-A"); !isDomainErr(err, domain.ErrPaymentNotFound) {
		t.Errorf("other caller's status: %v, want not found", err)
	}
	if _, err := e.svc.Status(ctx, e.a, "nope"); !isDomainErr(err, domain.ErrPaymentNotFound) {
		t.Errorf("unknown code: %v", err)
	}
}

func TestProviderTimeoutThenRetryCreatesOnePayment(t *testing.T) {
	e := newEnv(t, service.Options{})
	ctx := context.Background()
	cfg := e.newConfig(t, "fake", map[string]any{"behavior": "timeout"})
	must(t, e.configs.Grant(ctx, cfg.ID, e.a.ID, testActor))

	if _, _, err := e.svc.Create(ctx, e.a, in(cfg.ID, "T1", "3.00")); !isDomainErr(err, domain.ErrProviderTimeout) {
		t.Fatalf("first call: %v", err)
	}
	got, err := e.svc.Status(ctx, e.a, "T1")
	must(t, err)
	if got.Status != domain.StatusCreating { // shown as PENDING by APIName
		t.Errorf("after timeout status = %s", got.Status)
	}
	if got.Status.APIName() != "PENDING" {
		t.Errorf("api name = %s", got.Status.APIName())
	}

	// The provider recovers; the same request is retried.
	healthy := map[string]any{"behavior": "success"}
	_, err = e.configs.Update(ctx, cfg.ID, store.ConfigUpdate{Settings: healthy}, testActor)
	must(t, err)
	v, _, err := e.svc.Create(ctx, e.a, in(cfg.ID, "T1", "3.00"))
	must(t, err)
	if v.Status != domain.StatusPending || v.PaymentURL == "" {
		t.Errorf("retry result %+v", v)
	}
	if e.fake.UniquePayments() != 1 {
		t.Errorf("provider payments = %d, want 1", e.fake.UniquePayments())
	}
	if n := countRows(t, e, `SELECT count(*) FROM payment_attempts WHERE transaction_id = (SELECT id FROM payment_transactions WHERE uniquecode='T1')`); n != 2 {
		t.Errorf("attempts = %d, want 2", n)
	}
}

func TestProviderRejectionFailsThePayment(t *testing.T) {
	e := newEnv(t, service.Options{})
	ctx := context.Background()
	cfg := e.newConfig(t, "fake", map[string]any{"behavior": "reject"})
	must(t, e.configs.Grant(ctx, cfg.ID, e.a.ID, testActor))

	if _, _, err := e.svc.Create(ctx, e.a, in(cfg.ID, "R1", "3.00")); !isDomainErr(err, domain.ErrProviderError) {
		t.Fatalf("got %v", err)
	}
	v, err := e.svc.Status(ctx, e.a, "R1")
	must(t, err)
	if v.Status != domain.StatusFailed || v.PaymentURL != "" {
		t.Errorf("view = %+v", v)
	}
	// An identical repeat reports the failure and does not call the provider again.
	calls := e.fake.CreateCalls()
	v2, _, err := e.svc.Create(ctx, e.a, in(cfg.ID, "R1", "3.00"))
	must(t, err)
	if v2.Status != domain.StatusFailed || e.fake.CreateCalls() != calls {
		t.Errorf("repeat: %+v calls %d->%d", v2, calls, e.fake.CreateCalls())
	}
}

func TestUnimplementedProviderIsProviderError(t *testing.T) {
	e := newEnv(t, service.Options{})
	ctx := context.Background()
	pp := e.newConfig(t, "paypal", nil)
	must(t, e.configs.Grant(ctx, pp.ID, e.a.ID, testActor))
	if _, _, err := e.svc.Create(ctx, e.a, in(pp.ID, "P1", "3.00")); !isDomainErr(err, domain.ErrProviderError) {
		t.Errorf("got %v", err)
	}
}

func TestConcurrentIdenticalCreatesCallProviderOnce(t *testing.T) {
	e := newEnv(t, service.Options{Lease: 30 * time.Second})
	ctx := context.Background()
	cfg := e.newConfig(t, "fake", map[string]any{"behavior": "slow", "delay_ms": float64(150)})
	must(t, e.configs.Grant(ctx, cfg.ID, e.a.ID, testActor))

	const n = 30
	var wg sync.WaitGroup
	var mu sync.Mutex
	createdCount, errCount := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := e.svc.Create(ctx, e.a, in(cfg.ID, "CONC-1", "9.00"))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errCount++
			}
			if created {
				createdCount++
			}
		}()
	}
	wg.Wait()

	if errCount != 0 {
		t.Errorf("%d concurrent calls failed", errCount)
	}
	if createdCount != 1 {
		t.Errorf("created=true returned %d times, want exactly 1", createdCount)
	}
	if e.fake.CreateCalls() != 1 || e.fake.UniquePayments() != 1 {
		t.Errorf("provider calls=%d unique=%d, want 1/1", e.fake.CreateCalls(), e.fake.UniquePayments())
	}
	if rows := countRows(t, e, `SELECT count(*) FROM payment_transactions WHERE uniquecode='CONC-1'`); rows != 1 {
		t.Errorf("transactions = %d, want 1", rows)
	}
	if rows := countRows(t, e, `SELECT count(*) FROM payment_attempts`); rows != 1 {
		t.Errorf("attempts = %d, want 1", rows)
	}
}

// A process that dies after taking the lease leaves a creating row. After the
// lease expires the next identical request takes over and finishes the payment.
func TestTakeoverAfterLeaseExpiry(t *testing.T) {
	e := newEnv(t, service.Options{Lease: 300 * time.Millisecond})
	ctx := context.Background()

	amt, _ := domain.ParseAmount("4.00", "USD")
	p, inserted, err := e.payments.InsertOrGet(ctx, store.NewPayment{
		ClientID: e.a.ID, ConfigID: e.cfgID, UniqueCode: "CRASH-1", Amount: amt,
		Fingerprint: service.RequestFingerprint(e.cfgID, amt),
	})
	must(t, err)
	if !inserted {
		t.Fatal("expected insert")
	}
	// The "process" takes the lease and dies without finishing.
	if _, err := e.payments.BeginAttempt(ctx, p.ID, "dead-process", 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	// While the lease is live, an identical request must not call the provider.
	v, created, err := e.svc.Create(ctx, e.a, in(e.cfgID, "CRASH-1", "4.00"))
	must(t, err)
	if created || v.Status.APIName() != "PENDING" || v.PaymentURL != "" || e.fake.CreateCalls() != 0 {
		t.Errorf("during lease: created=%v view=%+v calls=%d", created, v, e.fake.CreateCalls())
	}

	time.Sleep(400 * time.Millisecond) // lease expires
	v, _, err = e.svc.Create(ctx, e.a, in(e.cfgID, "CRASH-1", "4.00"))
	must(t, err)
	if v.Status != domain.StatusPending || v.PaymentURL == "" {
		t.Errorf("after takeover: %+v", v)
	}
	if e.fake.UniquePayments() != 1 {
		t.Errorf("provider payments = %d, want 1", e.fake.UniquePayments())
	}
	if n := countRows(t, e, `SELECT count(*) FROM payment_attempts WHERE state='unknown'`); n != 1 {
		t.Errorf("abandoned attempts marked unknown = %d, want 1", n)
	}
}

func TestStaleResultIsFencedAndReviewed(t *testing.T) {
	e := newEnv(t, service.Options{})
	ctx := context.Background()

	amt, _ := domain.ParseAmount("6.00", "USD")
	p, _, err := e.payments.InsertOrGet(ctx, store.NewPayment{
		ClientID: e.a.ID, ConfigID: e.cfgID, UniqueCode: "FENCE-1", Amount: amt, Fingerprint: make([]byte, 32),
	})
	must(t, err)

	first, err := e.payments.BeginAttempt(ctx, p.ID, "worker-1", 100*time.Millisecond)
	must(t, err)
	if _, err := e.payments.BeginAttempt(ctx, p.ID, "worker-2", time.Minute); !errors.Is(err, store.ErrAttemptInFlight) {
		t.Fatalf("live lease must block takeover, got %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	second, err := e.payments.BeginAttempt(ctx, p.ID, "worker-2", time.Minute)
	must(t, err)
	if second.Fence <= first.Fence {
		t.Fatalf("fence not increasing: %d then %d", first.Fence, second.Fence)
	}

	// worker-1 wakes up late with a successful provider result.
	applied, err := e.payments.CompleteSuccess(ctx, first, store.ProviderResult{ProviderRef: "LATE-1", PaymentURL: "https://late"})
	must(t, err)
	if applied {
		t.Fatal("stale result must not be applied")
	}
	cur, _ := e.payments.GetByID(ctx, p.ID)
	if cur.Status != domain.StatusCreating || cur.ProviderRef != "" {
		t.Errorf("stale result changed the row: %+v", cur)
	}
	reviews, err := e.payments.OpenReviews(ctx, p.ID)
	must(t, err)
	if len(reviews) != 1 || reviews[0].Kind != "stale_result" || !strings.Contains(reviews[0].Details, "LATE-1") {
		t.Errorf("reviews = %+v", reviews)
	}

	// The current attempt then completes normally.
	applied, err = e.payments.CompleteSuccess(ctx, second, store.ProviderResult{ProviderRef: "REAL-1", PaymentURL: "https://real"})
	must(t, err)
	cur, _ = e.payments.GetByID(ctx, p.ID)
	if !applied || cur.Status != domain.StatusPending || cur.ProviderRef != "REAL-1" {
		t.Errorf("applied=%v row=%+v", applied, cur)
	}
	// Completing again with the old fence stays rejected, and a second stale report does not pile up.
	_, err = e.payments.CompleteSuccess(ctx, first, store.ProviderResult{ProviderRef: "LATE-1"})
	must(t, err)
	if reviews, _ := e.payments.OpenReviews(ctx, p.ID); len(reviews) != 1 {
		t.Errorf("open reviews = %d, want 1", len(reviews))
	}
}

func TestCredentialsNeverStoredInPlaintext(t *testing.T) {
	e := newEnv(t, service.Options{})
	var n int
	must(t, e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM provider_credentials WHERE position('very-secret-value'::bytea in ciphertext) > 0`).Scan(&n))
	if n != 0 {
		t.Error("plaintext credential found in provider_credentials")
	}
	row, err := e.configs.ActiveCredentials(context.Background(), e.cfgID)
	must(t, err)
	if row.KeyID != "k1" || len(row.Ciphertext) < 20 {
		t.Errorf("unexpected credential row %+v", row)
	}
}
