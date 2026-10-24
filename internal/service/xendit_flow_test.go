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

	"unifiedgw/internal/auth"
	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
	"unifiedgw/internal/provider/xendit"
	"unifiedgw/internal/provider/xendit/xendittest"
	"unifiedgw/internal/secrets"
	"unifiedgw/internal/service"
	"unifiedgw/internal/store"
	"unifiedgw/internal/testutil"
)

const (
	xdKey   = "xnd_development_KEY-DO-NOT-LEAK"
	xdToken = "xendit-callback-token"
)

// xdEnv wires the REAL Xendit adapter to an in-process Xendit stand-in and a real database.
type xdEnv struct {
	srv      *xendittest.Server
	pay      *service.Payments
	settle   *service.Settlement
	payments *store.Payments
	caller   auth.Principal
	cfg      store.Config
	query    func(sql string, args ...any) string
}

func newXDEnv(t *testing.T) *xdEnv {
	t.Helper()
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	keys, err := secrets.ParseKeyRing("k1:"+base64.StdEncoding.EncodeToString(raw), "k1")
	must(t, err)

	srv := xendittest.New(xdKey)
	t.Cleanup(srv.Close)
	reg := provider.NewRegistry()
	must(t, reg.Register(xendit.New(xendit.Options{BaseURL: srv.URL})))

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	configs, clients, payments := store.NewConfigs(pool), store.NewClients(pool), store.NewPayments(pool)
	e := &xdEnv{srv: srv, payments: payments,
		pay:    service.NewPayments(payments, configs, clients, keys, reg, log, service.Options{}),
		settle: service.NewSettlement(payments, configs, keys, reg, log)}
	e.caller, _, err = clients.Create(ctx, "shop", auth.RoleCaller)
	must(t, err)
	e.cfg, err = configs.Create(ctx, store.NewConfig{
		Name: "xd", Provider: "xendit", Environment: "sandbox", AllowedCurrencies: []string{"IDR", "USD"},
		Settings: map[string]any{"return_url": "https://shop.example/return", "cancel_url": "https://shop.example/cancel", "country": "ID"},
	}, service.NewSealer(keys, "xendit", map[string]string{"api_key": xdKey}, map[string]string{"callback_token": xdToken}), testActor)
	must(t, err)
	must(t, configs.Grant(ctx, e.cfg.ID, e.caller.ID, testActor))
	e.query = func(sql string, args ...any) string {
		var s string
		must(t, pool.QueryRow(ctx, sql, args...).Scan(&s))
		return s
	}
	return e
}

func (e *xdEnv) start(t *testing.T, code, amount, currency string) (session string, v service.View) {
	t.Helper()
	v, created, err := e.pay.Create(context.Background(), e.caller, service.CreateInput{
		ConfigID: e.cfg.ID, UniqueCode: code, Amount: amount, Currency: currency})
	must(t, err)
	if !created || v.Status != domain.StatusPending || !strings.HasPrefix(v.PaymentURL, "https://") {
		t.Fatalf("create: created=%v %+v", created, v)
	}
	p, err := e.payments.GetByCode(context.Background(), e.caller.ID, code)
	must(t, err)
	return p.ProviderRef, v
}

func (e *xdEnv) deliver(t *testing.T, body []byte, token string) (string, error) {
	t.Helper()
	h := http.Header{}
	if token != "" {
		h.Set("x-callback-token", token)
	}
	return e.settle.ProcessProviderWebhook(context.Background(), "xendit", h, body)
}

func (e *xdEnv) status(t *testing.T, code string) domain.Status {
	t.Helper()
	p, err := e.payments.GetByCode(context.Background(), e.caller.ID, code)
	must(t, err)
	return p.Status
}

func TestXenditFullFlowToPaid(t *testing.T) {
	e := newXDEnv(t)
	session, v := e.start(t, "ORDER-10001", "100000.00", "IDR") // the sample request, in IDR

	if !strings.Contains(v.PaymentURL, session) {
		t.Errorf("payment_url %q should carry the session id %q", v.PaymentURL, session)
	}
	p, _ := e.payments.GetByCode(context.Background(), e.caller.ID, "ORDER-10001")
	if !strings.Contains(string(e.srv.LastCreate), `"reference_id":"`+p.ID+`"`) || !strings.Contains(string(e.srv.LastCreate), `"amount":100000,`) {
		t.Errorf("session body: %s", e.srv.LastCreate)
	}

	// The buyer pays at Xendit; Xendit calls the webhook.
	e.srv.Complete(session, "IDR", "100000")
	body := xendittest.SessionEvent("payment_session.completed", session, "COMPLETED")
	out, err := e.deliver(t, body, xdToken)
	must(t, err)
	if out != store.OutcomeProcessed || e.status(t, "ORDER-10001") != domain.StatusPaid {
		t.Fatalf("outcome %s status %s", out, e.status(t, "ORDER-10001"))
	}
	if e.query(`SELECT (paid_at IS NOT NULL)::text FROM payment_transactions WHERE uniquecode='ORDER-10001'`) != "true" {
		t.Error("paid_at not set")
	}

	// Redelivery (Xendit retries, or sends twice) is a no-op.
	if out, err := e.deliver(t, body, xdToken); err != nil || out != store.OutcomeDuplicate {
		t.Errorf("redelivery: %s %v", out, err)
	}
	if n := e.query(`SELECT count(*)::text FROM payment_status_history WHERE to_status='paid'`); n != "1" {
		t.Errorf("paid history rows = %s", n)
	}
	v2, err := e.pay.Status(context.Background(), e.caller, "ORDER-10001")
	must(t, err)
	if v2.Status.APIName() != "PAID" || v2.PaymentURL != "" {
		t.Errorf("status view %+v", v2)
	}
}

func TestXenditNeverPaidWithoutRealEvidence(t *testing.T) {
	e := newXDEnv(t)
	session, _ := e.start(t, "N1", "100000", "IDR")
	body := xendittest.SessionEvent("payment_session.completed", session, "COMPLETED")

	// 1. Forged: wrong or missing token. Nothing is read and nothing changes.
	reads := e.srv.ReadCount()
	for name, tok := range map[string]string{"wrong": "nope", "missing": ""} {
		out, err := e.deliver(t, body, tok)
		must(t, err)
		if out != store.OutcomeNotVerified {
			t.Errorf("%s token: %s", name, out)
		}
	}
	if e.srv.ReadCount() != reads {
		t.Error("an unauthenticated delivery triggered API reads")
	}

	// 2. Valid token, but the session is not actually completed at Xendit.
	if out, err := e.deliver(t, body, xdToken); err != nil || out != store.OutcomeIgnored {
		t.Errorf("claimed completion of an ACTIVE session: %s %v", out, err)
	}
	if e.status(t, "N1") != domain.StatusPending {
		t.Fatalf("status = %s", e.status(t, "N1"))
	}

	// 3. Completed, but less money was captured than the payment is for.
	e.srv.Complete(session, "IDR", "60000")
	if out, err := e.deliver(t, body, xdToken); err != nil || out != store.OutcomeAmountMismatch {
		t.Errorf("partial capture: %s %v", out, err)
	}
	if e.status(t, "N1") != domain.StatusPending {
		t.Fatalf("a partial capture marked the payment %s", e.status(t, "N1"))
	}
	reviews, err := e.payments.OpenReviews(context.Background(), e.query(`SELECT id::text FROM payment_transactions WHERE uniquecode='N1'`))
	must(t, err)
	if len(reviews) != 1 || reviews[0].Kind != "amount_mismatch" {
		t.Errorf("reviews %+v", reviews)
	}

	// 4. Payment still settling: the delivery fails so Xendit retries.
	e.srv.Complete(session, "IDR", "100000")
	e.srv.SetPaymentStatus(session, "PENDING")
	if _, err := e.deliver(t, body, xdToken); err == nil {
		t.Error("an unsettled payment must make the webhook fail (retry)")
	}
	if e.status(t, "N1") != domain.StatusPending {
		t.Fatal("status changed")
	}

	// 5. Settled: now it is paid, exactly once.
	e.srv.SetPaymentStatus(session, "SUCCEEDED")
	if out, err := e.deliver(t, body, xdToken); err != nil || out != store.OutcomeProcessed {
		t.Errorf("settled: %s %v", out, err)
	}
	if e.status(t, "N1") != domain.StatusPaid {
		t.Errorf("status = %s", e.status(t, "N1"))
	}
}

func TestXenditExpiryFailureAndIgnoredEvents(t *testing.T) {
	e := newXDEnv(t)

	s1, _ := e.start(t, "E1", "100000", "IDR")
	e.srv.SetSessionStatus(s1, "EXPIRED")
	if out, err := e.deliver(t, xendittest.SessionEvent("payment_session.expired", s1, "EXPIRED"), xdToken); err != nil || out != store.OutcomeProcessed {
		t.Errorf("expired: %s %v", out, err)
	}
	if e.status(t, "E1") != domain.StatusExpired {
		t.Errorf("status %s", e.status(t, "E1"))
	}

	// Paid after expiry (a delayed webhook) is accepted and flagged for review.
	e.srv.Complete(s1, "IDR", "100000")
	if out, err := e.deliver(t, xendittest.SessionEvent("payment_session.completed", s1, "COMPLETED"), xdToken); err != nil || out != store.OutcomeProcessed {
		t.Errorf("late completion: %s %v", out, err)
	}
	if e.status(t, "E1") != domain.StatusPaid {
		t.Errorf("status %s", e.status(t, "E1"))
	}
	if n := e.query(`SELECT count(*)::text FROM review_items WHERE kind='paid_after_expiry'`); n != "1" {
		t.Errorf("paid_after_expiry reviews = %s", n)
	}

	s2, _ := e.start(t, "F1", "100000", "IDR")
	e.srv.Complete(s2, "IDR", "100000")
	e.srv.SetPaymentStatus(s2, "FAILED")
	if out, err := e.deliver(t, xendittest.SessionEvent("payment_session.completed", s2, "COMPLETED"), xdToken); err != nil || out != store.OutcomeProcessed {
		t.Errorf("failed payment: %s %v", out, err)
	}
	if e.status(t, "F1") != domain.StatusFailed {
		t.Errorf("status %s", e.status(t, "F1"))
	}

	// payment.* deliveries and unknown sessions are acknowledged without effect.
	if out, err := e.deliver(t, xendittest.PaymentEvent("py-1"), xdToken); err != nil || out != store.OutcomeIgnored {
		t.Errorf("payment.capture: %s %v", out, err)
	}
	if out, err := e.deliver(t, xendittest.SessionEvent("payment_session.completed", "ps-unknown", "COMPLETED"), xdToken); err != nil || out != store.OutcomeUnknownReference {
		t.Errorf("unknown session: %s %v", out, err)
	}
	if out, err := e.deliver(t, []byte("junk"), xdToken); err != nil || out != store.OutcomeNotVerified {
		t.Errorf("junk: %s %v", out, err)
	}
}

func TestXenditCreateRulesAndErrors(t *testing.T) {
	e := newXDEnv(t)
	ctx := context.Background()

	// EUR is not allowed by the config; a currency Xendit cannot take is refused before any call.
	if _, _, err := e.pay.Create(ctx, e.caller, service.CreateInput{ConfigID: e.cfg.ID, UniqueCode: "C1", Amount: "5", Currency: "EUR"}); !isDomainErr(err, domain.ErrCurrencyUnsupported) {
		t.Errorf("EUR: %v", err)
	}
	if e.srv.CreateCount() != 0 {
		t.Error("Xendit was called for a rejected request")
	}

	// Definite rejection from Xendit fails the payment with a normalized error.
	e.srv.CreateStatus, e.srv.CreateCode = http.StatusBadRequest, "INVALID_AMOUNT"
	if _, _, err := e.pay.Create(ctx, e.caller, service.CreateInput{ConfigID: e.cfg.ID, UniqueCode: "C2", Amount: "5", Currency: "USD"}); !isDomainErr(err, domain.ErrProviderError) {
		t.Errorf("rejection: %v", err)
	}
	if e.status(t, "C2") != domain.StatusFailed {
		t.Errorf("status %s", e.status(t, "C2"))
	}

	// 409 DUPLICATE_ERROR on a retried create is an unknown outcome, so it stays retryable.
	e.srv.CreateStatus, e.srv.CreateCode = http.StatusConflict, "DUPLICATE_ERROR"
	if _, _, err := e.pay.Create(ctx, e.caller, service.CreateInput{ConfigID: e.cfg.ID, UniqueCode: "C3", Amount: "5", Currency: "USD"}); !isDomainErr(err, domain.ErrProviderTimeout) {
		t.Errorf("duplicate: %v", err)
	}
	if e.status(t, "C3") != domain.StatusCreating {
		t.Errorf("status %s", e.status(t, "C3"))
	}
	e.srv.CreateStatus = 0
	v, _, err := e.pay.Create(ctx, e.caller, service.CreateInput{ConfigID: e.cfg.ID, UniqueCode: "C3", Amount: "5", Currency: "USD"})
	must(t, err)
	if v.Status != domain.StatusPending {
		t.Errorf("retry: %+v", v)
	}
}

func TestXenditSecretsNeverLeak(t *testing.T) {
	e := newXDEnv(t)
	_, v := e.start(t, "L1", "100000", "IDR")
	if strings.Contains(v.PaymentURL, xdKey) || strings.Contains(string(e.srv.LastCreate), xdKey) || strings.Contains(string(e.srv.LastCreate), xdToken) {
		t.Error("a secret appeared in output or in the session body")
	}
}
