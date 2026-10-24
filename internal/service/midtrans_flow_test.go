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
	"unifiedgw/internal/provider/midtrans"
	"unifiedgw/internal/provider/midtrans/midtranstest"
	"unifiedgw/internal/secrets"
	"unifiedgw/internal/service"
	"unifiedgw/internal/store"
	"unifiedgw/internal/testutil"
)

const mtKey = "SB-Mid-server-FLOW-KEY-DO-NOT-LEAK"

// mtEnv wires the REAL Midtrans adapter to an in-process Midtrans stand-in and a real database.
type mtEnv struct {
	srv      *midtranstest.Server
	pay      *service.Payments
	settle   *service.Settlement
	payments *store.Payments
	caller   auth.Principal
	cfg      store.Config
	query    func(sql string, args ...any) string
}

func newMTEnv(t *testing.T) *mtEnv {
	t.Helper()
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	keys, err := secrets.ParseKeyRing("k1:"+base64.StdEncoding.EncodeToString(raw), "k1")
	must(t, err)

	e := &mtEnv{srv: midtranstest.New(mtKey)}
	t.Cleanup(e.srv.Close)
	reg := provider.NewRegistry()
	must(t, reg.Register(midtrans.New(midtrans.Options{SnapBase: e.srv.URL, CoreBase: e.srv.URL})))

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	configs, clients, payments := store.NewConfigs(pool), store.NewClients(pool), store.NewPayments(pool)
	e.payments = payments
	e.pay = service.NewPayments(payments, configs, clients, keys, reg, log, service.Options{})
	e.settle = service.NewSettlement(payments, configs, keys, reg, log)
	e.caller, _, err = clients.Create(ctx, "shop", auth.RoleCaller)
	must(t, err)
	e.cfg, err = configs.Create(ctx, store.NewConfig{
		Name: "mt", Provider: "midtrans", Environment: "sandbox", AllowedCurrencies: []string{"IDR", "USD"},
		Settings: map[string]any{"return_url": "https://shop.example/return", "cancel_url": "https://shop.example/cancel"},
	}, service.NewSealer(keys, "midtrans", map[string]string{"server_key": mtKey}, nil), testActor)
	must(t, err)
	must(t, configs.Grant(ctx, e.cfg.ID, e.caller.ID, testActor))
	e.query = func(sql string, args ...any) string {
		var s string
		must(t, pool.QueryRow(ctx, sql, args...).Scan(&s))
		return s
	}
	return e
}

// start creates a payment and returns the Midtrans order id (the gateway transaction id).
func (e *mtEnv) start(t *testing.T, code, amount string) (order string, v service.View) {
	t.Helper()
	v, created, err := e.pay.Create(context.Background(), e.caller, service.CreateInput{
		ConfigID: e.cfg.ID, UniqueCode: code, Amount: amount, Currency: "IDR"})
	must(t, err)
	if !created || v.Status != domain.StatusPending || !strings.HasPrefix(v.PaymentURL, "https://") {
		t.Fatalf("create: created=%v %+v", created, v)
	}
	p, err := e.payments.GetByCode(context.Background(), e.caller.ID, code)
	must(t, err)
	return p.ProviderRef, v
}

func (e *mtEnv) notify(t *testing.T, order, claimed, key string) (string, error) {
	t.Helper()
	body := midtranstest.Notification(order, "200", "100000.00", key, claimed)
	return e.settle.ProcessProviderWebhook(context.Background(), "midtrans", http.Header{}, body)
}

func (e *mtEnv) status(t *testing.T, code string) domain.Status {
	t.Helper()
	p, err := e.payments.GetByCode(context.Background(), e.caller.ID, code)
	must(t, err)
	return p.Status
}

func TestMidtransFullFlowToPaid(t *testing.T) {
	e := newMTEnv(t)
	order, v := e.start(t, "ORDER-10001", "100000.00") // the sample request

	p, _ := e.payments.GetByCode(context.Background(), e.caller.ID, "ORDER-10001")
	if order != p.ID {
		t.Errorf("order id %q should be the gateway transaction id %q", order, p.ID)
	}
	if !strings.Contains(v.PaymentURL, "/snap/v2/vtweb/") {
		t.Errorf("payment_url %q", v.PaymentURL)
	}
	if !strings.Contains(string(e.srv.LastCreate), `"gross_amount":100000,`) {
		t.Errorf("snap body: %s", e.srv.LastCreate)
	}

	e.srv.Pay(order, "settlement", "", "100000.00")
	out, err := e.notify(t, order, "settlement", mtKey)
	must(t, err)
	if out != store.OutcomeProcessed || e.status(t, "ORDER-10001") != domain.StatusPaid {
		t.Fatalf("outcome %s status %s", out, e.status(t, "ORDER-10001"))
	}
	if e.query(`SELECT (paid_at IS NOT NULL)::text FROM payment_transactions WHERE uniquecode='ORDER-10001'`) != "true" {
		t.Error("paid_at not set")
	}
	if out, err := e.notify(t, order, "settlement", mtKey); err != nil || out != store.OutcomeDuplicate {
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

func TestMidtransNeverPaidWithoutRealEvidence(t *testing.T) {
	e := newMTEnv(t)
	order, _ := e.start(t, "N1", "100000.00")

	// Signed with another key: not verified, nothing read.
	_, reads0 := e.srv.Counts()
	if out, err := e.notify(t, order, "settlement", "another-key"); err != nil || out != store.OutcomeNotVerified {
		t.Errorf("forged: %s %v", out, err)
	}
	if _, reads := e.srv.Counts(); reads != reads0 {
		t.Error("a forged notification triggered an API read")
	}

	// Validly signed, but Midtrans has no payment / only a pending one / a card under review.
	if out, err := e.notify(t, order, "settlement", mtKey); err != nil || out != store.OutcomeIgnored {
		t.Errorf("no payment at Midtrans: %s %v", out, err)
	}
	e.srv.Pay(order, "pending", "", "100000.00")
	if out, err := e.notify(t, order, "settlement", mtKey); err != nil || out != store.OutcomeIgnored {
		t.Errorf("pending: %s %v", out, err)
	}
	e.srv.Pay(order, "capture", "challenge", "100000.00")
	if out, err := e.notify(t, order, "settlement", mtKey); err != nil || out != store.OutcomeIgnored {
		t.Errorf("challenge: %s %v", out, err)
	}
	if e.status(t, "N1") != domain.StatusPending {
		t.Fatalf("status = %s", e.status(t, "N1"))
	}

	// Settled for the wrong amount: flagged, not paid.
	e.srv.Pay(order, "settlement", "", "90000.00")
	if out, err := e.notify(t, order, "settlement", mtKey); err != nil || out != store.OutcomeAmountMismatch {
		t.Errorf("wrong amount: %s %v", out, err)
	}
	if e.status(t, "N1") != domain.StatusPending {
		t.Fatalf("status = %s", e.status(t, "N1"))
	}
	if e.query(`SELECT count(*)::text FROM review_items WHERE kind='amount_mismatch'`) != "1" {
		t.Error("no amount_mismatch review")
	}

	// The right amount arrives: paid (a different read, so a different event key).
	e.srv.Pay(order, "settlement", "", "100000.00")
	if out, err := e.notify(t, order, "settlement", mtKey); err != nil || out != store.OutcomeProcessed {
		t.Errorf("correct amount: %s %v", out, err)
	}
	if e.status(t, "N1") != domain.StatusPaid {
		t.Errorf("status = %s", e.status(t, "N1"))
	}
}

func TestMidtransCardCaptureAndFailureStates(t *testing.T) {
	e := newMTEnv(t)

	o1, _ := e.start(t, "C1", "100000.00")
	e.srv.Pay(o1, "capture", "accept", "100000.00")
	if out, err := e.notify(t, o1, "capture", mtKey); err != nil || out != store.OutcomeProcessed || e.status(t, "C1") != domain.StatusPaid {
		t.Errorf("card capture/accept: %s %v %s", out, err, e.status(t, "C1"))
	}

	for code, c := range map[string]struct {
		state string
		want  domain.Status
	}{"D1": {"deny", domain.StatusFailed}, "D2": {"expire", domain.StatusExpired}, "D3": {"cancel", domain.StatusCancelled}} {
		o, _ := e.start(t, code, "100000.00")
		e.srv.Pay(o, c.state, "", "100000.00")
		if out, err := e.notify(t, o, c.state, mtKey); err != nil || out != store.OutcomeProcessed || e.status(t, code) != c.want {
			t.Errorf("%s: %s %v %s", c.state, out, err, e.status(t, code))
		}
	}

	// Expired, then actually paid late: accepted and flagged.
	o, _ := e.start(t, "L1", "100000.00")
	e.srv.Pay(o, "expire", "", "100000.00")
	e.notify(t, o, "expire", mtKey)
	e.srv.Pay(o, "settlement", "", "100000.00")
	if out, err := e.notify(t, o, "settlement", mtKey); err != nil || out != store.OutcomeProcessed || e.status(t, "L1") != domain.StatusPaid {
		t.Errorf("late settlement: %s %v %s", out, err, e.status(t, "L1"))
	}
	if e.query(`SELECT count(*)::text FROM review_items WHERE kind='paid_after_expiry'`) != "1" {
		t.Error("no paid_after_expiry review")
	}

	// Known limitation (see midtrans-research.md section 5.7): a settlement that Midtrans later
	// reverses to deny cannot move a PAID payment, and is recorded as an invalid transition.
	e.srv.Pay(o1, "deny", "", "100000.00")
	if out, err := e.notify(t, o1, "deny", mtKey); err != nil || out != store.OutcomeInvalidTransit {
		t.Errorf("reversal after paid: %s %v", out, err)
	}
	if e.status(t, "C1") != domain.StatusPaid {
		t.Error("a reversal must not silently change a paid payment")
	}
}

func TestMidtransUnknownJunkAndOutage(t *testing.T) {
	e := newMTEnv(t)
	order, _ := e.start(t, "U1", "100000.00")

	if out, err := e.notify(t, "NO-SUCH-ORDER", "settlement", mtKey); err != nil || out != store.OutcomeUnknownReference {
		t.Errorf("unknown order: %s %v", out, err)
	}
	if out, err := e.settle.ProcessProviderWebhook(context.Background(), "midtrans", http.Header{}, []byte("junk")); err != nil || out != store.OutcomeNotVerified {
		t.Errorf("junk: %s %v", out, err)
	}

	e.srv.Pay(order, "settlement", "", "100000.00")
	e.srv.ReadStatus = http.StatusServiceUnavailable
	if _, err := e.notify(t, order, "settlement", mtKey); err == nil {
		t.Error("a status API outage must fail the notification so Midtrans retries")
	}
	if e.status(t, "U1") != domain.StatusPending {
		t.Fatal("status changed during an outage")
	}
	e.srv.ReadStatus = 0
	if out, err := e.notify(t, order, "settlement", mtKey); err != nil || out != store.OutcomeProcessed {
		t.Errorf("after recovery: %s %v", out, err)
	}
}

func TestMidtransCreateRules(t *testing.T) {
	e := newMTEnv(t)
	ctx := context.Background()

	// The config (created directly) allows USD, but Midtrans takes IDR only: refused before any call.
	if _, _, err := e.pay.Create(ctx, e.caller, service.CreateInput{ConfigID: e.cfg.ID, UniqueCode: "X1", Amount: "10.00", Currency: "USD"}); !isDomainErr(err, domain.ErrInvalidRequest) {
		t.Errorf("USD: %v", err)
	}
	if creates, _ := e.srv.Counts(); creates != 0 {
		t.Error("Midtrans was called for an amount it cannot take")
	}
	if n := e.query(`SELECT count(*)::text FROM payment_transactions`); n != "0" {
		t.Errorf("rejected request left %s rows", n)
	}

	// Outage then retry creates the order once more with the same order id.
	e.srv.CreateStatus = http.StatusServiceUnavailable
	in := service.CreateInput{ConfigID: e.cfg.ID, UniqueCode: "T1", Amount: "5000", Currency: "IDR"}
	if _, _, err := e.pay.Create(ctx, e.caller, in); !isDomainErr(err, domain.ErrProviderTimeout) {
		t.Fatalf("first: %v", err)
	}
	e.srv.CreateStatus = 0
	v, _, err := e.pay.Create(ctx, e.caller, in)
	must(t, err)
	if v.Status != domain.StatusPending {
		t.Errorf("retry: %+v", v)
	}
	if strings.Count(e.query(`SELECT string_agg(provider_ref, ',') FROM payment_transactions WHERE uniquecode='T1'`), ",") != 0 {
		t.Error("one payment expected")
	}
}

func TestMidtransSecretsNeverLeak(t *testing.T) {
	e := newMTEnv(t)
	_, v := e.start(t, "L2", "100000.00")
	if strings.Contains(v.PaymentURL, mtKey) || strings.Contains(string(e.srv.LastCreate), mtKey) {
		t.Error("the server key appeared in output or in the request body")
	}
}
