package service_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/service"
	"unifiedgw/internal/store"
)

const webhookToken = "s3cret-token"

// payEnv is an env plus a fake config that has a webhook secret and one pending payment.
type payEnv struct {
	*env
	settle  *service.Settlement
	cfg     store.Config
	payID   string
	ref     string
	webhook http.Header
}

func newPayEnv(t *testing.T, opt service.Options) *payEnv {
	t.Helper()
	e := newEnv(t, opt)
	ctx := context.Background()
	cfg, err := e.configs.Create(ctx, store.NewConfig{
		Name: "wh-" + randHex(), Provider: "fake", Environment: "sandbox", AllowedCurrencies: []string{"USD", "EUR"},
	}, service.NewSealer(e.keys, "fake", map[string]string{"client_secret": "x"}, map[string]string{"token": webhookToken}), testActor)
	must(t, err)
	must(t, e.configs.Grant(ctx, cfg.ID, e.a.ID, testActor))

	_, _, err = e.svc.Create(ctx, e.a, in(cfg.ID, "PAY-1", "10.00"))
	must(t, err)
	p, err := e.payments.GetByCode(ctx, e.a.ID, "PAY-1")
	must(t, err)

	h := http.Header{}
	h.Set("X-Fake-Token", webhookToken)
	settle := service.NewSettlement(e.payments, e.configs, e.keys, e.reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &payEnv{env: e, settle: settle, cfg: cfg, payID: p.ID, ref: p.ProviderRef, webhook: h}
}

func (p *payEnv) deliver(t *testing.T, eventID, status, amount, currency string) string {
	t.Helper()
	body := fmt.Sprintf(`{"event_id":%q,"provider_ref":%q,"status":%q,"amount":%q,"currency":%q}`,
		eventID, p.ref, status, amount, currency)
	out, err := p.settle.ProcessWebhook(context.Background(), p.cfg.ID, p.webhook, []byte(body))
	must(t, err)
	return out
}

func (p *payEnv) status(t *testing.T) domain.Status {
	t.Helper()
	cur, err := p.payments.GetByID(context.Background(), p.payID)
	must(t, err)
	return cur.Status
}

func (p *payEnv) paidAtSet(t *testing.T) bool {
	t.Helper()
	var set bool
	must(t, p.pool.QueryRow(context.Background(),
		`SELECT paid_at IS NOT NULL FROM payment_transactions WHERE id=$1::uuid`, p.payID).Scan(&set))
	return set
}

func TestVerifiedPaidEventMarksPaid(t *testing.T) {
	p := newPayEnv(t, service.Options{})
	if p.status(t) != domain.StatusPending || p.paidAtSet(t) {
		t.Fatal("precondition: pending and no paid_at")
	}
	if out := p.deliver(t, "E1", "paid", "10.00", "USD"); out != store.OutcomeProcessed {
		t.Fatalf("outcome = %s", out)
	}
	if p.status(t) != domain.StatusPaid || !p.paidAtSet(t) {
		t.Errorf("status=%s paid_at set=%v", p.status(t), p.paidAtSet(t))
	}
	var actor, reason string
	must(t, p.pool.QueryRow(context.Background(), `SELECT actor_type, reason FROM payment_status_history
		WHERE transaction_id=$1::uuid AND to_status='paid'`, p.payID).Scan(&actor, &reason))
	if actor != "webhook" {
		t.Errorf("history actor = %s", actor)
	}
	v, err := p.svc.Status(context.Background(), p.a, "PAY-1")
	must(t, err)
	if v.Status.APIName() != "PAID" || v.PaymentURL != "" {
		t.Errorf("view = %+v", v)
	}
}

func TestUnverifiedWebhookNeverChangesAnything(t *testing.T) {
	p := newPayEnv(t, service.Options{})
	ctx := context.Background()
	body := []byte(fmt.Sprintf(`{"event_id":"EV","provider_ref":%q,"status":"paid","amount":"10.00","currency":"USD"}`, p.ref))

	for name, h := range map[string]http.Header{
		"wrong token":   {"X-Fake-Token": {"nope"}},
		"missing token": {},
		"empty token":   {"X-Fake-Token": {""}},
	} {
		out, err := p.settle.ProcessWebhook(ctx, p.cfg.ID, h, body)
		must(t, err)
		if out != store.OutcomeNotVerified {
			t.Errorf("%s: outcome = %s", name, out)
		}
	}
	if p.status(t) != domain.StatusPending || p.paidAtSet(t) {
		t.Error("unverified webhook changed the payment")
	}
	if n := countRows(t, p.env, `SELECT count(*) FROM webhook_events WHERE verified`); n != 0 {
		t.Errorf("verified events = %d, want 0", n)
	}
	if n := countRows(t, p.env, `SELECT count(*) FROM webhook_events WHERE NOT verified`); n != 3 {
		t.Errorf("unverified events recorded = %d, want 3", n)
	}
	// Unverified deliveries must not block the real event that follows.
	if out := p.deliver(t, "EV", "paid", "10.00", "USD"); out != store.OutcomeProcessed {
		t.Errorf("real event after unverified ones: %s", out)
	}
}

func TestAmountAndCurrencyMustMatch(t *testing.T) {
	p := newPayEnv(t, service.Options{})
	for i, c := range []struct{ amount, currency string }{
		{"9.99", "USD"}, {"10.01", "USD"}, {"10.00", "EUR"}, {"", ""},
	} {
		out := p.deliver(t, fmt.Sprintf("M%d", i), "paid", c.amount, c.currency)
		if out != store.OutcomeAmountMismatch {
			t.Errorf("amount=%q currency=%q: outcome %s", c.amount, c.currency, out)
		}
		if p.status(t) != domain.StatusPending {
			t.Fatalf("mismatching event marked the payment %s", p.status(t))
		}
	}
	reviews, err := p.payments.OpenReviews(context.Background(), p.payID)
	must(t, err)
	if len(reviews) != 1 || reviews[0].Kind != "amount_mismatch" {
		t.Errorf("reviews = %+v", reviews)
	}
	if out := p.deliver(t, "OK", "paid", "10", "USD"); out != store.OutcomeProcessed { // "10" equals "10.00"
		t.Errorf("matching event: %s", out)
	}
}

func TestDuplicateAndUnknownAndNonFinalEvents(t *testing.T) {
	p := newPayEnv(t, service.Options{})
	if out := p.deliver(t, "D1", "paid", "10.00", "USD"); out != store.OutcomeProcessed {
		t.Fatal(out)
	}
	if out := p.deliver(t, "D1", "paid", "10.00", "USD"); out != store.OutcomeDuplicate {
		t.Errorf("same event id: %s", out)
	}
	if out := p.deliver(t, "D2", "paid", "10.00", "USD"); out != store.OutcomeIgnored {
		t.Errorf("new event id, already paid: %s", out)
	}
	if n := countRows(t, p.env, `SELECT count(*) FROM payment_status_history WHERE to_status='paid'`); n != 1 {
		t.Errorf("paid history rows = %d, want 1", n)
	}

	body := []byte(`{"event_id":"U1","provider_ref":"FAKE-unknown","status":"paid","amount":"10.00","currency":"USD"}`)
	out, err := p.settle.ProcessWebhook(context.Background(), p.cfg.ID, p.webhook, body)
	must(t, err)
	if out != store.OutcomeUnknownReference {
		t.Errorf("unknown reference: %s", out)
	}

	// A paid payment cannot be failed or expired by a later event.
	if out := p.deliver(t, "X1", "expired", "", ""); out != store.OutcomeInvalidTransit {
		t.Errorf("paid -> expired: %s", out)
	}
	if p.status(t) != domain.StatusPaid {
		t.Error("status changed")
	}
}

func TestPaidAfterExpiryIsAcceptedAndReviewed(t *testing.T) {
	p := newPayEnv(t, service.Options{})
	ctx := context.Background()
	// Expire it through the sweeper.
	_, err := p.pool.Exec(ctx, `UPDATE payment_transactions SET expires_at = now() - interval '1 minute' WHERE id=$1::uuid`, p.payID)
	must(t, err)
	n, err := p.settle.ExpireDue(ctx)
	must(t, err)
	if n != 1 || p.status(t) != domain.StatusExpired {
		t.Fatalf("expired=%d status=%s", n, p.status(t))
	}
	if again, _ := p.settle.ExpireDue(ctx); again != 0 {
		t.Errorf("second sweep expired %d", again)
	}

	if out := p.deliver(t, "LATE", "paid", "10.00", "USD"); out != store.OutcomeProcessed {
		t.Fatalf("late paid: %s", out)
	}
	if p.status(t) != domain.StatusPaid {
		t.Errorf("status = %s", p.status(t))
	}
	reviews, err := p.payments.OpenReviews(ctx, p.payID)
	must(t, err)
	if len(reviews) != 1 || reviews[0].Kind != "paid_after_expiry" {
		t.Errorf("reviews = %+v", reviews)
	}
}

func TestConcurrentDeliveriesApplyOnce(t *testing.T) {
	p := newPayEnv(t, service.Options{})
	const n = 25
	outcomes := make(map[string]int)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "SAME"
			if i%2 == 1 {
				id = fmt.Sprintf("DIFF-%d", i) // different event ids, same fact
			}
			body := fmt.Sprintf(`{"event_id":%q,"provider_ref":%q,"status":"paid","amount":"10.00","currency":"USD"}`, id, p.ref)
			out, err := p.settle.ProcessWebhook(context.Background(), p.cfg.ID, p.webhook, []byte(body))
			if err != nil {
				out = "error: " + err.Error()
			}
			mu.Lock()
			outcomes[out]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if outcomes[store.OutcomeProcessed] != 1 {
		t.Errorf("processed %d times, want exactly 1: %v", outcomes[store.OutcomeProcessed], outcomes)
	}
	for out := range outcomes {
		if strings.HasPrefix(out, "error") {
			t.Errorf("unexpected failure: %v", outcomes)
		}
	}
	if k := countRows(t, p.env, `SELECT count(*) FROM payment_status_history WHERE to_status='paid'`); k != 1 {
		t.Errorf("paid history rows = %d, want 1", k)
	}
	if p.status(t) != domain.StatusPaid {
		t.Errorf("status = %s", p.status(t))
	}
}

func TestResumeCreatingFinishesStuckPayment(t *testing.T) {
	e := newEnv(t, service.Options{Lease: 200 * time.Millisecond})
	ctx := context.Background()
	cfg := e.newConfig(t, "fake", map[string]any{"behavior": "timeout"})
	must(t, e.configs.Grant(ctx, cfg.ID, e.a.ID, testActor))

	if _, _, err := e.svc.Create(ctx, e.a, in(cfg.ID, "STUCK-1", "2.00")); !isDomainErr(err, domain.ErrProviderTimeout) {
		t.Fatalf("setup: %v", err)
	}
	healthy := map[string]any{"behavior": "success"}
	_, err := e.configs.Update(ctx, cfg.ID, store.ConfigUpdate{Settings: healthy}, testActor)
	must(t, err)

	// Too fresh: the reconciler leaves it alone.
	if n, err := e.svc.ResumeCreating(ctx); err != nil || n != 0 {
		t.Fatalf("fresh row resumed=%d err=%v", n, err)
	}
	time.Sleep(500 * time.Millisecond)
	n, err := e.svc.ResumeCreating(ctx)
	must(t, err)
	if n != 1 {
		t.Fatalf("resumed = %d, want 1", n)
	}
	v, err := e.svc.Status(ctx, e.a, "STUCK-1")
	must(t, err)
	if v.Status != domain.StatusPending || v.PaymentURL == "" {
		t.Errorf("view = %+v", v)
	}
	if e.fake.UniquePayments() != 1 {
		t.Errorf("provider payments = %d, want 1", e.fake.UniquePayments())
	}
	if n, _ := e.svc.ResumeCreating(ctx); n != 0 {
		t.Errorf("second pass resumed %d", n)
	}
}

func TestResumeGivesUpAndOpensReview(t *testing.T) {
	e := newEnv(t, service.Options{Lease: 100 * time.Millisecond})
	ctx := context.Background()
	cfg := e.newConfig(t, "fake", map[string]any{"behavior": "timeout"})
	must(t, e.configs.Grant(ctx, cfg.ID, e.a.ID, testActor))
	if _, _, err := e.svc.Create(ctx, e.a, in(cfg.ID, "HOPELESS", "2.00")); !isDomainErr(err, domain.ErrProviderTimeout) {
		t.Fatalf("setup: %v", err)
	}
	var id string
	must(t, e.pool.QueryRow(ctx, `SELECT id::text FROM payment_transactions WHERE uniquecode='HOPELESS'`).Scan(&id))

	for i := 0; i < 8; i++ {
		time.Sleep(250 * time.Millisecond)
		if _, err := e.svc.ResumeCreating(ctx); err != nil {
			t.Fatal(err)
		}
	}
	attempts, _ := e.payments.AttemptCount(ctx, id)
	if attempts != 5 {
		t.Errorf("attempts = %d, want capped at 5", attempts)
	}
	reviews, err := e.payments.OpenReviews(ctx, id)
	must(t, err)
	if len(reviews) != 1 || reviews[0].Kind != "unknown_outcome_exhausted" {
		t.Errorf("reviews = %+v", reviews)
	}
}
