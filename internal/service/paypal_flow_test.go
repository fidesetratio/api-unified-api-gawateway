package service_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"unifiedgw/internal/auth"
	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
	"unifiedgw/internal/provider/paypal"
	"unifiedgw/internal/provider/paypal/paypaltest"
	"unifiedgw/internal/secrets"
	"unifiedgw/internal/service"
	"unifiedgw/internal/store"
	"unifiedgw/internal/testutil"
)

const (
	ppClientID  = "pp-client"
	ppSecret    = "pp-SECRET-do-not-leak"
	ppWebhookID = "WH-PP-1"
)

// ppEnv wires the REAL PayPal adapter to the in-process PayPal stand-in and a
// real database, so the whole create -> approve -> capture -> paid path runs.
type ppEnv struct {
	srv      *paypaltest.Server
	pay      *service.Payments
	settle   *service.Settlement
	payments *store.Payments
	configs  *store.Configs
	caller   auth.Principal
	cfg      store.Config
	query    func(sql string, args ...any) string
}

func newPPEnv(t *testing.T) *ppEnv {
	t.Helper()
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()

	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	keys, err := secrets.ParseKeyRing("k1:"+base64.StdEncoding.EncodeToString(raw), "k1")
	must(t, err)

	srv := paypaltest.New(ppClientID, ppSecret, ppWebhookID)
	t.Cleanup(srv.Close)
	reg := provider.NewRegistry()
	must(t, reg.Register(paypal.New(paypal.Options{SandboxBase: srv.URL, LiveBase: srv.URL})))

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	configs, clients, payments := store.NewConfigs(pool), store.NewClients(pool), store.NewPayments(pool)
	e := &ppEnv{srv: srv, configs: configs, payments: payments,
		pay:    service.NewPayments(payments, configs, clients, keys, reg, log, service.Options{}),
		settle: service.NewSettlement(payments, configs, keys, reg, log)}

	e.caller, _, err = clients.Create(ctx, "shop", auth.RoleCaller)
	must(t, err)
	e.cfg, err = configs.Create(ctx, store.NewConfig{
		Name: "pp", Provider: "paypal", Environment: "sandbox", AllowedCurrencies: []string{"USD", "EUR", "IDR", "HUF"},
		Settings: map[string]any{"return_url": "https://shop.example/return", "cancel_url": "https://shop.example/cancel"},
	}, service.NewSealer(keys, "paypal",
		map[string]string{"client_id": ppClientID, "client_secret": ppSecret},
		map[string]string{"webhook_id": ppWebhookID}), testActor)
	must(t, err)
	must(t, configs.Grant(ctx, e.cfg.ID, e.caller.ID, testActor))
	e.query = func(sql string, args ...any) string {
		var s string
		must(t, pool.QueryRow(ctx, sql, args...).Scan(&s))
		return s
	}
	return e
}

// startPayment creates a payment and returns its PayPal order id.
func (e *ppEnv) startPayment(t *testing.T, code, amount, currency string) (orderID string, v service.View) {
	t.Helper()
	v, created, err := e.pay.Create(context.Background(), e.caller, service.CreateInput{
		ConfigID: e.cfg.ID, UniqueCode: code, Amount: amount, Currency: currency})
	must(t, err)
	if !created || v.Status != domain.StatusPending || v.PaymentURL == "" {
		t.Fatalf("create: created=%v %+v", created, v)
	}
	p, err := e.payments.GetByCode(context.Background(), e.caller.ID, code)
	must(t, err)
	return p.ProviderRef, v
}

// deliver sends a correctly signed webhook through the provider-level entry point.
func (e *ppEnv) deliver(t *testing.T, body []byte) (string, error) {
	t.Helper()
	return e.settle.ProcessProviderWebhook(context.Background(), "paypal", paypaltest.Sign(ppWebhookID, body), body)
}

func (e *ppEnv) status(t *testing.T, code string) domain.Status {
	t.Helper()
	p, err := e.payments.GetByCode(context.Background(), e.caller.ID, code)
	must(t, err)
	return p.Status
}

func TestPayPalFullFlowToPaid(t *testing.T) {
	e := newPPEnv(t)
	order, v := e.startPayment(t, "ORDER-10001", "10.00", "USD")

	if !strings.Contains(v.PaymentURL, "/checkoutnow?token="+order) {
		t.Errorf("payment_url %q does not carry the order id", v.PaymentURL)
	}
	p, _ := e.payments.GetByCode(context.Background(), e.caller.ID, "ORDER-10001")
	if !strings.Contains(string(e.srv.LastCreate), `"custom_id":"`+p.ID+`"`) {
		t.Errorf("custom_id must be the gateway transaction id: %s", e.srv.LastCreate)
	}
	if p.ExpiresAt == nil {
		t.Error("expires_at must be set from the config default")
	}

	// The buyer approves at PayPal; PayPal notifies us. Approval alone moves no money.
	e.srv.Approve(order)
	if st := e.status(t, "ORDER-10001"); st != domain.StatusPending {
		t.Fatalf("before the webhook: %s", st)
	}
	out, err := e.deliver(t, paypaltest.ApprovedEvent("WH-A1", order))
	must(t, err)
	if out != store.OutcomeProcessed {
		t.Errorf("outcome = %s", out)
	}
	if _, caps, _ := e.srv.Counts(); caps != 1 {
		t.Errorf("captures = %d, want exactly 1", caps)
	}
	if st := e.status(t, "ORDER-10001"); st != domain.StatusPaid {
		t.Fatalf("after capture: %s", st)
	}
	if e.query(`SELECT (paid_at IS NOT NULL)::text FROM payment_transactions WHERE uniquecode='ORDER-10001'`) != "true" {
		t.Error("paid_at not set")
	}

	// Redeliveries and PayPal's own capture notification change nothing and capture nothing more.
	if out, _ := e.deliver(t, paypaltest.ApprovedEvent("WH-A1", order)); out != store.OutcomeDuplicate {
		t.Errorf("redelivery: %s", out)
	}
	if out, _ := e.deliver(t, paypaltest.CaptureEvent("WH-C1", "PAYMENT.CAPTURE.COMPLETED", order, "10.00", "USD")); out != store.OutcomeIgnored {
		t.Errorf("capture notification after paid: %s", out)
	}
	if _, caps, _ := e.srv.Counts(); caps != 1 {
		t.Errorf("captures after redeliveries = %d, want still 1", caps)
	}
	if n := e.query(`SELECT count(*)::text FROM payment_status_history WHERE to_status='paid'`); n != "1" {
		t.Errorf("paid history rows = %s", n)
	}
}

func TestPayPalCaptureNotificationAloneMarksPaidWhenAmountMatches(t *testing.T) {
	e := newPPEnv(t)
	order, _ := e.startPayment(t, "N1", "10.00", "USD")
	out, err := e.deliver(t, paypaltest.CaptureEvent("WH-C1", "PAYMENT.CAPTURE.COMPLETED", order, "10.00", "USD"))
	must(t, err)
	if out != store.OutcomeProcessed || e.status(t, "N1") != domain.StatusPaid {
		t.Errorf("outcome %s status %s", out, e.status(t, "N1"))
	}
}

func TestPayPalNeverPaidOnWrongOrMissingEvidence(t *testing.T) {
	e := newPPEnv(t)
	order, _ := e.startPayment(t, "W1", "10.00", "USD")

	for name, body := range map[string][]byte{
		"less money":     paypaltest.CaptureEvent("E1", "PAYMENT.CAPTURE.COMPLETED", order, "9.99", "USD"),
		"other currency": paypaltest.CaptureEvent("E2", "PAYMENT.CAPTURE.COMPLETED", order, "10.00", "EUR"),
	} {
		out, err := e.deliver(t, body)
		must(t, err)
		if out != store.OutcomeAmountMismatch {
			t.Errorf("%s: %s", name, out)
		}
	}
	if st := e.status(t, "W1"); st != domain.StatusPending {
		t.Fatalf("status = %s", st)
	}

	// Forged deliveries: wrong signature, signed for another webhook id, tampered after signing.
	good := paypaltest.CaptureEvent("E3", "PAYMENT.CAPTURE.COMPLETED", order, "10.00", "USD")
	tampered := []byte(strings.Replace(string(good), `"10.00"`, `"1.00"`, 1))
	for name, c := range map[string]struct {
		body []byte
		h    http.Header
	}{
		"wrong webhook id": {good, paypaltest.Sign("WH-OTHER", good)},
		"tampered body":    {tampered, paypaltest.Sign(ppWebhookID, good)},
		"no headers":       {good, http.Header{}},
	} {
		out, err := e.settle.ProcessProviderWebhook(context.Background(), "paypal", c.h, c.body)
		must(t, err)
		if out != store.OutcomeNotVerified {
			t.Errorf("%s: %s", name, out)
		}
	}
	if st := e.status(t, "W1"); st != domain.StatusPending {
		t.Fatalf("a forged delivery changed the status to %s", st)
	}
	if n := e.query(`SELECT count(*)::text FROM webhook_events WHERE NOT verified`); n != "3" {
		t.Errorf("unverified deliveries recorded = %s, want 3", n)
	}
	// Approval-triggered capture also needs a verified delivery.
	e.srv.Approve(order)
	forged := paypaltest.ApprovedEvent("E4", order)
	if out, _ := e.settle.ProcessProviderWebhook(context.Background(), "paypal", paypaltest.Sign("WH-OTHER", forged), forged); out != store.OutcomeNotVerified {
		t.Errorf("forged approval: %s", out)
	}
	if _, caps, _ := e.srv.Counts(); caps != 0 {
		t.Errorf("a forged approval caused %d captures", caps)
	}
}

func TestPayPalUnknownAndJunkDeliveries(t *testing.T) {
	e := newPPEnv(t)
	e.startPayment(t, "U1", "10.00", "USD")

	unknown := paypaltest.CaptureEvent("E1", "PAYMENT.CAPTURE.COMPLETED", "NO-SUCH-ORDER", "10.00", "USD")
	if out, err := e.deliver(t, unknown); err != nil || out != store.OutcomeUnknownReference {
		t.Errorf("unknown order: %s %v", out, err)
	}
	before := e.query(`SELECT count(*)::text FROM webhook_events`)
	if out, err := e.settle.ProcessProviderWebhook(context.Background(), "paypal", http.Header{}, []byte("garbage")); err != nil || out != store.OutcomeNotVerified {
		t.Errorf("junk: %s %v", out, err)
	}
	if after := e.query(`SELECT count(*)::text FROM webhook_events`); after != before {
		t.Error("junk deliveries must not write rows")
	}
	if _, err := e.settle.ProcessProviderWebhook(context.Background(), "stripe", http.Header{}, []byte("{}")); !errors.Is(err, service.ErrUnknownProvider) {
		t.Errorf("unknown provider: %v", err)
	}
	if _, err := e.settle.ProcessProviderWebhook(context.Background(), "xendit", http.Header{}, []byte("{}")); !errors.Is(err, service.ErrUnknownProvider) {
		t.Errorf("provider without a locator: %v", err)
	}
}

func TestPayPalCaptureDeclinedAndPending(t *testing.T) {
	e := newPPEnv(t)
	order, _ := e.startPayment(t, "D1", "10.00", "USD")
	e.srv.Approve(order)
	e.srv.CaptureMode = "PENDING"
	if out, err := e.deliver(t, paypaltest.ApprovedEvent("E1", order)); err != nil || out == store.OutcomeProcessed {
		t.Errorf("pending capture: %s %v", out, err)
	}
	if st := e.status(t, "D1"); st != domain.StatusPending {
		t.Fatalf("a PENDING capture must not mark paid, got %s", st)
	}

	e2 := newPPEnv(t)
	order2, _ := e2.startPayment(t, "D2", "10.00", "USD")
	e2.srv.Approve(order2)
	e2.srv.CaptureMode = "DECLINED"
	if out, err := e2.deliver(t, paypaltest.ApprovedEvent("E1", order2)); err != nil || out != store.OutcomeProcessed {
		t.Errorf("declined capture: %s %v", out, err)
	}
	if st := e2.status(t, "D2"); st != domain.StatusFailed {
		t.Errorf("a DECLINED capture must fail the payment, got %s", st)
	}
}

func TestPayPalCaptureRetriesAfterOutageButRefusalsAreFinal(t *testing.T) {
	e := newPPEnv(t)
	order, _ := e.startPayment(t, "R1", "10.00", "USD")
	e.srv.Approve(order)

	// PayPal is down for the capture: the webhook must fail so PayPal redelivers.
	e.srv.CaptureStatus = http.StatusServiceUnavailable
	approved := paypaltest.ApprovedEvent("E1", order)
	if _, err := e.deliver(t, approved); err == nil {
		t.Fatal("an unknown capture outcome must fail the webhook (retry)")
	}
	if st := e.status(t, "R1"); st != domain.StatusPending {
		t.Fatalf("status = %s", st)
	}
	// Redelivery after recovery captures once, even though the first event row already exists.
	e.srv.CaptureStatus = 0
	out, err := e.deliver(t, approved)
	must(t, err)
	if e.status(t, "R1") != domain.StatusPaid {
		t.Errorf("redelivery did not complete the capture (outcome %s)", out)
	}

	// An order PayPal refuses to capture (not approved) stays pending and the webhook is acknowledged.
	e2 := newPPEnv(t)
	order2, _ := e2.startPayment(t, "R2", "10.00", "USD") // never approved at the stub
	out, err = e2.deliver(t, paypaltest.ApprovedEvent("E9", order2))
	must(t, err)
	if out != "capture_refused" || e2.status(t, "R2") != domain.StatusPending {
		t.Errorf("outcome %s status %s", out, e2.status(t, "R2"))
	}
}

func TestPayPalDoesNotCaptureAfterExpiry(t *testing.T) {
	e := newPPEnv(t)
	order, _ := e.startPayment(t, "X1", "10.00", "USD")
	// The payment expires (expiry sweeper) before the buyer approves.
	_ = e.query(`UPDATE payment_transactions SET expires_at = now() - interval '1 minute' WHERE uniquecode='X1' RETURNING 'x'`)
	if n, err := e.settle.ExpireDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("expired %d, %v", n, err)
	}
	e.srv.Approve(order)

	out, err := e.deliver(t, paypaltest.ApprovedEvent("E1", order))
	must(t, err)
	if _, caps, _ := e.srv.Counts(); caps != 0 {
		t.Errorf("captured %d times for an expired payment (outcome %s)", caps, out)
	}
	if st := e.status(t, "X1"); st != domain.StatusExpired {
		t.Errorf("status = %s", st)
	}
}

func TestPayPalVerifyOutageChangesNothing(t *testing.T) {
	e := newPPEnv(t)
	order, _ := e.startPayment(t, "V1", "10.00", "USD")
	e.srv.VerifyStatus = http.StatusServiceUnavailable
	body := paypaltest.CaptureEvent("E1", "PAYMENT.CAPTURE.COMPLETED", order, "10.00", "USD")
	if _, err := e.deliver(t, body); err == nil {
		t.Fatal("verification outage must surface as an error so PayPal retries")
	}
	if st := e.status(t, "V1"); st != domain.StatusPending {
		t.Errorf("status = %s", st)
	}
	e.srv.VerifyStatus = 0
	if out, err := e.deliver(t, body); err != nil || out != store.OutcomeProcessed {
		t.Errorf("after recovery: %s %v", out, err)
	}
}

func TestPayPalAmountRulesAtCreate(t *testing.T) {
	e := newPPEnv(t)
	ctx := context.Background()
	// The config (created directly, bypassing admin validation) lists IDR and HUF;
	// the adapter must still refuse what PayPal cannot take, before calling PayPal.
	for name, in := range map[string]service.CreateInput{
		"IDR":          {ConfigID: e.cfg.ID, UniqueCode: "I1", Amount: "100000", Currency: "IDR"},
		"HUF decimals": {ConfigID: e.cfg.ID, UniqueCode: "I2", Amount: "10.50", Currency: "HUF"},
	} {
		_, _, err := e.pay.Create(ctx, e.caller, in)
		if !isDomainErr(err, domain.ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if creates, _, _ := e.srv.Counts(); creates != 0 {
		t.Errorf("PayPal was called %d times for amounts it cannot take", creates)
	}
	if n := e.query(`SELECT count(*)::text FROM payment_transactions`); n != "0" {
		t.Errorf("rejected amounts left %s rows", n)
	}
}

func TestPayPalCreateTimeoutThenRetryUsesTheSameIdempotencyKey(t *testing.T) {
	e := newPPEnv(t)
	ctx := context.Background()
	in := service.CreateInput{ConfigID: e.cfg.ID, UniqueCode: "T1", Amount: "10.00", Currency: "USD"}

	e.srv.CreateStatus = http.StatusServiceUnavailable
	if _, _, err := e.pay.Create(ctx, e.caller, in); !isDomainErr(err, domain.ErrProviderTimeout) {
		t.Fatalf("first: %v", err)
	}
	key1 := e.srv.LastCreateHdr.Get("PayPal-Request-Id")

	e.srv.CreateStatus = 0
	v, _, err := e.pay.Create(ctx, e.caller, in)
	must(t, err)
	if e.srv.LastCreateHdr.Get("PayPal-Request-Id") != key1 || key1 == "" {
		t.Errorf("retry must reuse the idempotency key: %q vs %q", key1, e.srv.LastCreateHdr.Get("PayPal-Request-Id"))
	}
	if v.Status != domain.StatusPending {
		t.Errorf("view %+v", v)
	}
	if creates, _, _ := e.srv.Counts(); creates != 2 {
		t.Errorf("create calls = %d", creates)
	}
}

func TestPayPalSecretsNeverReachResponsesOrErrors(t *testing.T) {
	e := newPPEnv(t)
	order, v := e.startPayment(t, "S1", "10.00", "USD")
	for _, s := range []string{v.PaymentURL, order} {
		if strings.Contains(s, ppSecret) {
			t.Error("secret in output")
		}
	}
	if strings.Contains(string(e.srv.LastCreate), ppSecret) || strings.Contains(string(e.srv.LastCreate), ppWebhookID) {
		t.Error("secret or webhook id sent in the order body")
	}
}
