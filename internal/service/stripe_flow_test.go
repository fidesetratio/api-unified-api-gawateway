package service_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"unifiedgw/internal/auth"
	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
	"unifiedgw/internal/provider/stripe"
	"unifiedgw/internal/provider/stripe/stripetest"
	"unifiedgw/internal/secrets"
	"unifiedgw/internal/service"
	"unifiedgw/internal/store"
	"unifiedgw/internal/testutil"
)

const (
	stKey    = "sk_test_FLOW-KEY-DO-NOT-LEAK"
	stSecret = "whsec_flow_signing_secret"
)

// stEnv wires the REAL Stripe adapter to an in-process Stripe stand-in and a real database.
type stEnv struct {
	srv      *stripetest.Server
	pay      *service.Payments
	settle   *service.Settlement
	payments *store.Payments
	caller   auth.Principal
	cfg      store.Config
	now      time.Time
	query    func(sql string, args ...any) string
}

func newSTEnv(t *testing.T) *stEnv {
	t.Helper()
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	keys, err := secrets.ParseKeyRing("k1:"+base64.StdEncoding.EncodeToString(raw), "k1")
	must(t, err)

	e := &stEnv{now: time.Now()}
	e.srv = stripetest.New(stKey)
	t.Cleanup(e.srv.Close)
	reg := provider.NewRegistry()
	must(t, reg.Register(stripe.New(stripe.Options{BaseURL: e.srv.URL, Now: func() time.Time { return e.now }})))

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	configs, clients, payments := store.NewConfigs(pool), store.NewClients(pool), store.NewPayments(pool)
	e.payments = payments
	e.pay = service.NewPayments(payments, configs, clients, keys, reg, log, service.Options{})
	e.settle = service.NewSettlement(payments, configs, keys, reg, log)
	e.caller, _, err = clients.Create(ctx, "shop", auth.RoleCaller)
	must(t, err)
	e.cfg, err = configs.Create(ctx, store.NewConfig{
		Name: "st", Provider: "stripe", Environment: "sandbox", AllowedCurrencies: []string{"USD", "EUR", "JPY", "IDR"},
		Settings: map[string]any{"return_url": "https://shop.example/return", "cancel_url": "https://shop.example/cancel"},
	}, service.NewSealer(keys, "stripe", map[string]string{"api_key": stKey}, map[string]string{"signing_secret": stSecret}), testActor)
	must(t, err)
	must(t, configs.Grant(ctx, e.cfg.ID, e.caller.ID, testActor))
	e.query = func(sql string, args ...any) string {
		var s string
		must(t, pool.QueryRow(ctx, sql, args...).Scan(&s))
		return s
	}
	return e
}

func (e *stEnv) start(t *testing.T, code, amount, currency string) (session string, v service.View) {
	t.Helper()
	v, created, err := e.pay.Create(context.Background(), e.caller, service.CreateInput{
		ConfigID: e.cfg.ID, UniqueCode: code, Amount: amount, Currency: currency})
	must(t, err)
	if !created || v.Status != domain.StatusPending || !strings.HasPrefix(v.PaymentURL, "https://checkout.stripe.com/") {
		t.Fatalf("create: created=%v %+v", created, v)
	}
	p, err := e.payments.GetByCode(context.Background(), e.caller.ID, code)
	must(t, err)
	return p.ProviderRef, v
}

func (e *stEnv) deliver(t *testing.T, body []byte, h http.Header) (string, error) {
	t.Helper()
	return e.settle.ProcessProviderWebhook(context.Background(), "stripe", h, body)
}

func (e *stEnv) signed(body []byte) http.Header { return stripetest.Sign(e.now.Unix(), body, stSecret) }

func (e *stEnv) status(t *testing.T, code string) domain.Status {
	t.Helper()
	p, err := e.payments.GetByCode(context.Background(), e.caller.ID, code)
	must(t, err)
	return p.Status
}

func TestStripeFullFlowToPaid(t *testing.T) {
	e := newSTEnv(t)
	session, v := e.start(t, "ORDER-10001", "10.00", "USD")
	if !strings.Contains(v.PaymentURL, session) {
		t.Errorf("payment_url %q does not carry the session id", v.PaymentURL)
	}
	p, _ := e.payments.GetByCode(context.Background(), e.caller.ID, "ORDER-10001")
	if e.srv.LastForm.Get("client_reference_id") != p.ID || e.srv.LastForm.Get("line_items[0][price_data][unit_amount]") != "1000" {
		t.Errorf("session form: %v", e.srv.LastForm)
	}
	if e.srv.LastHeaders.Get("Idempotency-Key") != p.ID {
		t.Errorf("Idempotency-Key = %q, want the transaction id", e.srv.LastHeaders.Get("Idempotency-Key"))
	}

	e.srv.Pay(session)
	body := stripetest.Event("evt_100", "checkout.session.completed", session)
	out, err := e.deliver(t, body, e.signed(body))
	must(t, err)
	if out != store.OutcomeProcessed || e.status(t, "ORDER-10001") != domain.StatusPaid {
		t.Fatalf("outcome %s status %s", out, e.status(t, "ORDER-10001"))
	}
	if e.query(`SELECT (paid_at IS NOT NULL)::text FROM payment_transactions WHERE uniquecode='ORDER-10001'`) != "true" {
		t.Error("paid_at not set")
	}
	// Stripe retries the same event, and also sends async_payment_succeeded: neither changes anything.
	if out, err := e.deliver(t, body, e.signed(body)); err != nil || out != store.OutcomeDuplicate {
		t.Errorf("redelivery: %s %v", out, err)
	}
	later := stripetest.Event("evt_101", "checkout.session.async_payment_succeeded", session)
	if out, err := e.deliver(t, later, e.signed(later)); err != nil || out != store.OutcomeIgnored {
		t.Errorf("second paid event: %s %v", out, err)
	}
	if n := e.query(`SELECT count(*)::text FROM payment_status_history WHERE to_status='paid'`); n != "1" {
		t.Errorf("paid history rows = %s", n)
	}
}

func TestStripeForgedAndReplayedDeliveriesChangeNothing(t *testing.T) {
	e := newSTEnv(t)
	session, _ := e.start(t, "F1", "10.00", "USD")
	e.srv.Pay(session)
	body := stripetest.Event("evt_9", "checkout.session.completed", session)
	good := e.signed(body)

	forged := map[string]http.Header{
		"wrong secret": stripetest.Sign(e.now.Unix(), body, "whsec_attacker"),
		"no header":    {},
		"too old":      stripetest.Sign(e.now.Unix()-600, body, stSecret),
	}
	reads := func() int { _, r := e.srv.Counts(); return r }
	before := reads()
	for name, h := range forged {
		out, err := e.deliver(t, body, h)
		must(t, err)
		if out != store.OutcomeNotVerified {
			t.Errorf("%s: %s", name, out)
		}
	}
	tampered := []byte(strings.Replace(string(body), "evt_9", "evt_8", 1))
	if out, _ := e.deliver(t, tampered, good); out != store.OutcomeNotVerified {
		t.Errorf("tampered: %s", out)
	}
	if reads() != before {
		t.Error("unauthentic deliveries triggered API reads")
	}
	if e.status(t, "F1") != domain.StatusPending {
		t.Fatalf("status = %s", e.status(t, "F1"))
	}
	if n := e.query(`SELECT count(*)::text FROM webhook_events WHERE NOT verified`); n != "4" {
		t.Errorf("unverified deliveries recorded = %s, want 4", n)
	}
	if out, err := e.deliver(t, body, good); err != nil || out != store.OutcomeProcessed {
		t.Errorf("genuine delivery after the forged ones: %s %v", out, err)
	}
}

func TestStripeNeverPaidOnTheBodyAlone(t *testing.T) {
	e := newSTEnv(t)
	session, _ := e.start(t, "B1", "10.00", "USD")

	// A correctly signed 'completed' whose session is not paid at Stripe pays nothing.
	body := stripetest.Event("evt_1", "checkout.session.completed", session) // body says paid; API says open
	if out, err := e.deliver(t, body, e.signed(body)); err != nil || out != store.OutcomeIgnored {
		t.Errorf("open session: %s %v", out, err)
	}
	e.srv.CompleteUnpaid(session) // delayed payment method
	if out, err := e.deliver(t, body, e.signed(body)); err != nil || out == store.OutcomeProcessed {
		t.Errorf("complete+unpaid: %s %v", out, err)
	}
	if e.status(t, "B1") != domain.StatusPending {
		t.Fatalf("status = %s", e.status(t, "B1"))
	}

	// The money arrives later: the async event settles it.
	e.srv.Pay(session)
	async := stripetest.Event("evt_2", "checkout.session.async_payment_succeeded", session)
	if out, err := e.deliver(t, async, e.signed(async)); err != nil || out != store.OutcomeProcessed {
		t.Errorf("async success: %s %v", out, err)
	}
	if e.status(t, "B1") != domain.StatusPaid {
		t.Errorf("status = %s", e.status(t, "B1"))
	}
}

func TestStripeAmountMismatchIsNotPaid(t *testing.T) {
	e := newSTEnv(t)
	session, _ := e.start(t, "M1", "10.00", "USD")
	e.srv.Pay(session)
	e.srv.SetAmountTotal(session, 999) // Stripe reports 9.99 for a 10.00 payment
	body := stripetest.Event("evt_m", "checkout.session.completed", session)
	if out, err := e.deliver(t, body, e.signed(body)); err != nil || out != store.OutcomeAmountMismatch {
		t.Errorf("outcome %s %v", out, err)
	}
	if e.status(t, "M1") != domain.StatusPending {
		t.Errorf("status = %s", e.status(t, "M1"))
	}
	if n := e.query(`SELECT count(*)::text FROM review_items WHERE kind='amount_mismatch'`); n != "1" {
		t.Errorf("amount_mismatch reviews = %s", n)
	}
}

func TestStripeFailureExpiryAndIgnoredEvents(t *testing.T) {
	e := newSTEnv(t)

	s1, _ := e.start(t, "X1", "10.00", "USD")
	exp := stripetest.Event("evt_e", "checkout.session.expired", s1)
	if out, err := e.deliver(t, exp, e.signed(exp)); err != nil || out != store.OutcomeProcessed || e.status(t, "X1") != domain.StatusExpired {
		t.Errorf("expired: %s %v %s", out, err, e.status(t, "X1"))
	}
	// A late payment after expiry is accepted and flagged.
	e.srv.Pay(s1)
	late := stripetest.Event("evt_l", "checkout.session.async_payment_succeeded", s1)
	if out, err := e.deliver(t, late, e.signed(late)); err != nil || out != store.OutcomeProcessed || e.status(t, "X1") != domain.StatusPaid {
		t.Errorf("late payment: %s %v %s", out, err, e.status(t, "X1"))
	}
	if e.query(`SELECT count(*)::text FROM review_items WHERE kind='paid_after_expiry'`) != "1" {
		t.Error("no paid_after_expiry review")
	}

	s2, _ := e.start(t, "X2", "10.00", "USD")
	fail := stripetest.Event("evt_f", "checkout.session.async_payment_failed", s2)
	if out, err := e.deliver(t, fail, e.signed(fail)); err != nil || out != store.OutcomeProcessed || e.status(t, "X2") != domain.StatusFailed {
		t.Errorf("failed: %s %v %s", out, err, e.status(t, "X2"))
	}

	for _, typ := range []string{"charge.refunded", "payment_intent.created"} {
		b := []byte(`{"id":"evt_` + typ + `","type":"` + typ + `","data":{"object":{"id":"ch_1"}}}`)
		if out, err := e.deliver(t, b, e.signed(b)); err != nil || out != store.OutcomeIgnored {
			t.Errorf("%s: %s %v", typ, out, err)
		}
	}
	unknown := stripetest.Event("evt_u", "checkout.session.completed", "cs_test_unknown")
	if out, err := e.deliver(t, unknown, e.signed(unknown)); err != nil || out != store.OutcomeUnknownReference {
		t.Errorf("unknown session: %s %v", out, err)
	}
}

func TestStripeCreateRules(t *testing.T) {
	e := newSTEnv(t)
	ctx := context.Background()

	// The config (created directly) allows IDR, but the adapter refuses currencies whose
	// decimal handling is uncertain, before any call and without leaving a row.
	if _, _, err := e.pay.Create(ctx, e.caller, service.CreateInput{ConfigID: e.cfg.ID, UniqueCode: "I1", Amount: "100000", Currency: "IDR"}); !isDomainErr(err, domain.ErrInvalidRequest) {
		t.Errorf("IDR: %v", err)
	}
	if creates, _ := e.srv.Counts(); creates != 0 {
		t.Error("Stripe was called for an amount it cannot take")
	}
	if n := e.query(`SELECT count(*)::text FROM payment_transactions`); n != "0" {
		t.Errorf("rejected request left %s rows", n)
	}

	// Zero-decimal currency goes through without multiplication.
	e.start(t, "J1", "5000", "JPY")
	if e.srv.LastForm.Get("line_items[0][price_data][unit_amount]") != "5000" {
		t.Errorf("JPY amount: %v", e.srv.LastForm)
	}

	// Timeout then retry reuses the idempotency key, so Stripe returns the same session.
	e.srv.CreateStatus, e.srv.CreateType = http.StatusServiceUnavailable, "api_error"
	if _, _, err := e.pay.Create(ctx, e.caller, service.CreateInput{ConfigID: e.cfg.ID, UniqueCode: "T1", Amount: "10.00", Currency: "USD"}); !isDomainErr(err, domain.ErrProviderTimeout) {
		t.Fatalf("first: %v", err)
	}
	key1 := e.srv.LastHeaders.Get("Idempotency-Key")
	e.srv.CreateStatus = 0
	v, _, err := e.pay.Create(ctx, e.caller, service.CreateInput{ConfigID: e.cfg.ID, UniqueCode: "T1", Amount: "10.00", Currency: "USD"})
	must(t, err)
	if v.Status != domain.StatusPending || e.srv.LastHeaders.Get("Idempotency-Key") != key1 || key1 == "" {
		t.Errorf("retry: %+v key %q vs %q", v, key1, e.srv.LastHeaders.Get("Idempotency-Key"))
	}
}

func TestStripeSecretsNeverLeak(t *testing.T) {
	e := newSTEnv(t)
	_, v := e.start(t, "L1", "10.00", "USD")
	if strings.Contains(v.PaymentURL, stKey) || strings.Contains(e.srv.LastForm.Encode(), stKey) || strings.Contains(e.srv.LastForm.Encode(), stSecret) {
		t.Error("a secret appeared in output or in the session form")
	}
}
