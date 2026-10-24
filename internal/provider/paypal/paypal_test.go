package paypal_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
	"unifiedgw/internal/provider/paypal"
	"unifiedgw/internal/provider/paypal/paypaltest"
	"unifiedgw/internal/secrets"
)

const (
	clientID  = "client-abc"
	secret    = "SUPER-SECRET-VALUE"
	webhookID = "WH-ID-123"
)

type fixture struct {
	srv *paypaltest.Server
	ad  *paypal.Adapter
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	srv := paypaltest.New(clientID, secret, webhookID)
	t.Cleanup(srv.Close)
	f := &fixture{srv: srv, now: time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)}
	f.ad = paypal.New(paypal.Options{SandboxBase: srv.URL, LiveBase: srv.URL, Now: func() time.Time { return f.now }})
	return f
}

func creds() provider.Credentials {
	return provider.Credentials{"client_id": secrets.New(clientID), "client_secret": secrets.New(secret)}
}

func settings() map[string]any {
	return map[string]any{"return_url": "https://shop.example/return", "cancel_url": "https://shop.example/cancel"}
}

func amount(t *testing.T, v, cur string) domain.Amount {
	t.Helper()
	a, err := domain.ParseAmount(v, cur)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *fixture) create(t *testing.T, key, value, cur string) (provider.CreateResult, error) {
	t.Helper()
	return f.ad.CreatePayment(context.Background(), provider.CreateRequest{
		Reference: "tx-uuid-1", IdempotencyKey: key, Amount: amount(t, value, cur),
		Environment: "sandbox", Credentials: creds(), Settings: settings(),
	})
}

func TestCreateOrderRequestAndResult(t *testing.T) {
	f := newFixture(t)
	res, err := f.create(t, "idem-1", "10.00", "USD")
	if err != nil {
		t.Fatal(err)
	}
	if res.ProviderRef == "" || !strings.Contains(res.PaymentURL, "/checkoutnow?token="+res.ProviderRef) || res.RawStatus != "PAYER_ACTION_REQUIRED" {
		t.Errorf("result %+v", res)
	}
	if res.ExpiresAt == nil || !res.ExpiresAt.Equal(f.now.Add(24*time.Hour)) {
		t.Errorf("expires %v, want now+24h", res.ExpiresAt)
	}

	var sent struct {
		Intent        string `json:"intent"`
		PurchaseUnits []struct {
			CustomID  string                               `json:"custom_id"`
			InvoiceID string                               `json:"invoice_id"`
			Amount    struct{ CurrencyCode, Value string } `json:"-"`
			AmountRaw map[string]string                    `json:"amount"`
		} `json:"purchase_units"`
		PaymentSource struct {
			PayPal struct {
				Ctx map[string]string `json:"experience_context"`
			} `json:"paypal"`
		} `json:"payment_source"`
	}
	if err := json.Unmarshal(f.srv.LastCreate, &sent); err != nil {
		t.Fatal(err)
	}
	pu := sent.PurchaseUnits[0]
	if sent.Intent != "CAPTURE" || pu.CustomID != "tx-uuid-1" || pu.InvoiceID != "tx-uuid-1" ||
		pu.AmountRaw["currency_code"] != "USD" || pu.AmountRaw["value"] != "10.00" {
		t.Errorf("order body: %s", f.srv.LastCreate)
	}
	ctx := sent.PaymentSource.PayPal.Ctx
	if ctx["return_url"] != "https://shop.example/return" || ctx["cancel_url"] != "https://shop.example/cancel" || ctx["user_action"] != "PAY_NOW" {
		t.Errorf("experience_context: %v", ctx)
	}
	if got := f.srv.LastCreateHdr.Get("PayPal-Request-Id"); got != "idem-1" {
		t.Errorf("PayPal-Request-Id = %q", got)
	}
	if got := f.srv.LastCreateHdr.Get("Prefer"); got != "return=representation" {
		t.Errorf("Prefer = %q", got)
	}
	if strings.Contains(string(f.srv.LastCreate), secret) {
		t.Error("secret leaked into the order body")
	}
}

func TestCreateIsIdempotentAndTokenIsCached(t *testing.T) {
	f := newFixture(t)
	a, err := f.create(t, "same-key", "10.00", "USD")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.create(t, "same-key", "10.00", "USD")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := f.create(t, "other-key", "10.00", "USD")
	if a.ProviderRef != b.ProviderRef || a.ProviderRef == c.ProviderRef {
		t.Errorf("refs %s %s %s", a.ProviderRef, b.ProviderRef, c.ProviderRef)
	}
	if n := f.srv.TokenRequests(); n != 1 {
		t.Errorf("token requests = %d, want 1 (cached)", n)
	}
	// After the token expires a new one is fetched.
	f.now = f.now.Add(10 * time.Hour)
	if _, err := f.create(t, "k3", "10.00", "USD"); err != nil {
		t.Fatal(err)
	}
	if n := f.srv.TokenRequests(); n != 2 {
		t.Errorf("token requests after expiry = %d, want 2", n)
	}
}

func TestExpiredServerTokenIsRefreshedOnce(t *testing.T) {
	f := newFixture(t)
	if _, err := f.create(t, "k1", "1.00", "USD"); err != nil {
		t.Fatal(err)
	}
	f.srv.RevokeToken() // PayPal invalidates the token we still cache
	if _, err := f.create(t, "k2", "1.00", "USD"); err != nil {
		t.Fatalf("a 401 must trigger one token refresh: %v", err)
	}
	if n := f.srv.TokenRequests(); n != 2 {
		t.Errorf("token requests = %d", n)
	}
}

func TestCreateErrors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	bad := provider.Credentials{"client_id": secrets.New(clientID), "client_secret": secrets.New("wrong")}
	_, err := f.ad.CreatePayment(ctx, provider.CreateRequest{Reference: "r", IdempotencyKey: "k", Amount: amount(t, "1.00", "USD"),
		Environment: "sandbox", Credentials: bad, Settings: settings()})
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Code != "invalid_client" {
		t.Errorf("bad credentials: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "wrong") {
		t.Error("error echoes the secret")
	}

	_, err = f.ad.CreatePayment(ctx, provider.CreateRequest{Reference: "r", IdempotencyKey: "k", Amount: amount(t, "1.00", "USD"),
		Environment: "sandbox", Credentials: creds(), Settings: map[string]any{}})
	if !errors.As(err, &pe) || pe.Code != "config_incomplete_return_urls" {
		t.Errorf("missing urls: %v", err)
	}
	_, err = f.ad.CreatePayment(ctx, provider.CreateRequest{Reference: "r", IdempotencyKey: "k", Amount: amount(t, "1.00", "USD"),
		Environment: "sandbox", Credentials: creds(), Settings: map[string]any{"return_url": "javascript:alert(1)", "cancel_url": "https://x.example/c"}})
	if !errors.As(err, &pe) {
		t.Errorf("non-http return url accepted: %v", err)
	}
	if _, err := f.create(t, "k", "1.00", "EUR"); err != nil { // supported currency passes
		t.Errorf("EUR: %v", err)
	}
	if _, err := f.create(t, "k2", "100000", "IDR"); !errors.As(err, &pe) || pe.Code != "amount_not_accepted" {
		t.Errorf("IDR must be refused before calling PayPal: %v", err)
	}

	f.srv.CreateStatus = http.StatusServiceUnavailable
	if _, err := f.create(t, "k3", "1.00", "USD"); !errors.Is(err, provider.ErrTimeout) {
		t.Errorf("5xx must be an unknown outcome: %v", err)
	}
	f.srv.CreateStatus = http.StatusTooManyRequests
	if _, err := f.create(t, "k4", "1.00", "USD"); !errors.Is(err, provider.ErrTimeout) {
		t.Errorf("429 must be retryable: %v", err)
	}
	f.srv.CreateStatus = http.StatusUnprocessableEntity
	if _, err := f.create(t, "k5", "1.00", "USD"); !errors.As(err, &pe) || !strings.HasPrefix(pe.Code, "INTERNAL_SERVER_ERROR.") {
		// the stub names every forced error INTERNAL_SERVER_ERROR; the point is a 4xx is definite
		t.Errorf("4xx must be a definite rejection with a code: %v", err)
	} else if pe.DebugID == "" {
		t.Error("debug_id should be kept for support")
	}
}

func TestTransportFailureIsUnknownOutcome(t *testing.T) {
	f := newFixture(t)
	f.srv.Close()
	if _, err := f.create(t, "k", "1.00", "USD"); !errors.Is(err, provider.ErrTimeout) {
		t.Errorf("connection refused must be an unknown outcome: %v", err)
	}
}

func TestValidateAmount(t *testing.T) {
	ad := paypal.New(paypal.Options{})
	ok := []struct{ v, cur string }{{"10.00", "USD"}, {"100", "JPY"}, {"100.00", "JPY"}, {"1000", "HUF"}, {"1000.00", "HUF"}, {"500", "TWD"}}
	for _, c := range ok {
		if err := ad.ValidateAmount(amount(t, c.v, c.cur)); err != nil {
			t.Errorf("%s %s: %v", c.v, c.cur, err)
		}
	}
	for _, cur := range []string{"IDR", "XYZ", "KRW", "VND"} {
		if err := ad.ValidateAmount(amount(t, "100", cur)); err == nil {
			t.Errorf("%s must be refused", cur)
		}
	}
	if err := ad.ValidateAmount(amount(t, "10.50", "HUF")); err == nil {
		t.Error("HUF with decimals must be refused")
	}
	if !paypal.SupportsCurrency("USD") || paypal.SupportsCurrency("IDR") {
		t.Error("SupportsCurrency wrong")
	}
}

func TestZeroDecimalCurrencyIsSentWithoutDecimals(t *testing.T) {
	f := newFixture(t)
	if _, err := f.create(t, "k", "1500.00", "HUF"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.srv.LastCreate), `"value":"1500"`) {
		t.Errorf("HUF value must have no decimals: %s", f.srv.LastCreate)
	}
}

func (f *fixture) capture(ref, key string) (provider.CaptureResult, error) {
	return f.ad.Capture(context.Background(), provider.CaptureRequest{
		ProviderRef: ref, IdempotencyKey: key, Environment: "sandbox", Credentials: creds(), Settings: settings()})
}

func TestCapture(t *testing.T) {
	f := newFixture(t)
	res, _ := f.create(t, "k", "10.00", "USD")

	// Not approved yet: PayPal refuses (422), and nothing is reported as paid.
	if got, err := f.capture(res.ProviderRef, "cap-1"); err == nil {
		t.Errorf("capture of an unapproved order succeeded: %+v", got)
	}

	f.srv.Approve(res.ProviderRef)
	got, err := f.capture(res.ProviderRef, "cap-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusPaid || got.Amount == nil || got.Amount.String() != "10.00" || got.CaptureID == "" {
		t.Errorf("capture %+v", got)
	}

	// Capturing again (a retry after a lost answer) returns the existing capture.
	again, err := f.capture(res.ProviderRef, "cap-1")
	if err != nil || again.Status != domain.StatusPaid || again.CaptureID != got.CaptureID {
		t.Errorf("repeat capture: %+v %v", again, err)
	}
}

func TestCaptureDeclinedAndPendingAreNeverPaid(t *testing.T) {
	for mode, want := range map[string]domain.Status{"DECLINED": domain.StatusFailed, "PENDING": domain.StatusPending} {
		f := newFixture(t)
		f.srv.CaptureMode = mode
		res, _ := f.create(t, "k", "10.00", "USD")
		f.srv.Approve(res.ProviderRef)
		got, err := f.capture(res.ProviderRef, "c")
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if got.Status != want || got.Status == domain.StatusPaid {
			t.Errorf("%s capture mapped to %s, want %s", mode, got.Status, want)
		}
	}
}

func TestGetStatus(t *testing.T) {
	f := newFixture(t)
	res, _ := f.create(t, "k", "10.00", "USD")
	get := func() provider.StatusResult {
		st, err := f.ad.GetStatus(context.Background(), provider.StatusRequest{ProviderRef: res.ProviderRef, Environment: "sandbox", Credentials: creds()})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := get(); st.Status != domain.StatusPending || st.RawStatus != "PAYER_ACTION_REQUIRED" {
		t.Errorf("fresh order %+v", st)
	}
	f.srv.Approve(res.ProviderRef)
	if st := get(); st.Status != domain.StatusPending {
		t.Errorf("approved but not captured must still be pending: %+v", st)
	}
	if _, err := f.capture(res.ProviderRef, "c"); err != nil {
		t.Fatal(err)
	}
	if st := get(); st.Status != domain.StatusPaid || st.Amount == nil || st.Amount.String() != "10.00" {
		t.Errorf("captured %+v", st)
	}
	_, err := f.ad.GetStatus(context.Background(), provider.StatusRequest{ProviderRef: "NOPE", Environment: "sandbox", Credentials: creds()})
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Errorf("unknown order: %v", err)
	}
}

// ---- webhooks --------------------------------------------------------------

func (f *fixture) verify(body []byte, h http.Header, wid string) (provider.Event, error) {
	return f.ad.VerifyWebhook(context.Background(), provider.WebhookRequest{
		Headers: h, Body: body, Environment: "sandbox", Credentials: creds(),
		WebhookSecret: provider.Credentials{"webhook_id": secrets.New(wid)},
	})
}

func TestVerifyWebhookAndNormalize(t *testing.T) {
	f := newFixture(t)

	approved := paypaltest.ApprovedEvent("WH-1", "ORDER-A")
	ev, err := f.verify(approved, paypaltest.Sign(webhookID, approved), webhookID)
	if err != nil {
		t.Fatal(err)
	}
	if ev.EventID != "WH-1" || ev.ProviderRef != "ORDER-A" || !ev.Capturable || ev.Status != domain.StatusPending || ev.Ignore {
		t.Errorf("approved %+v", ev)
	}

	done := paypaltest.CaptureEvent("WH-2", "PAYMENT.CAPTURE.COMPLETED", "ORDER-A", "10.00", "USD")
	ev, err = f.verify(done, paypaltest.Sign(webhookID, done), webhookID)
	if err != nil || ev.Status != domain.StatusPaid || ev.ProviderRef != "ORDER-A" || ev.Amount == nil || ev.Amount.String() != "10.00" || ev.Capturable {
		t.Errorf("completed %+v %v", ev, err)
	}

	declined := paypaltest.CaptureEvent("WH-3", "PAYMENT.CAPTURE.DECLINED", "ORDER-A", "10.00", "USD")
	if ev, err = f.verify(declined, paypaltest.Sign(webhookID, declined), webhookID); err != nil || ev.Status != domain.StatusFailed {
		t.Errorf("declined %+v %v", ev, err)
	}

	for _, typ := range []string{"PAYMENT.CAPTURE.REFUNDED", "PAYMENT.CAPTURE.PENDING", "PAYMENT.CAPTURE.REVERSED", "SOMETHING.ELSE"} {
		b := paypaltest.CaptureEvent("WH-x-"+typ, typ, "ORDER-A", "10.00", "USD")
		if ev, err = f.verify(b, paypaltest.Sign(webhookID, b), webhookID); err != nil || !ev.Ignore {
			t.Errorf("%s should be acknowledged and ignored: %+v %v", typ, ev, err)
		}
	}
}

func TestWebhookThatIsNotAuthenticNeverVerifies(t *testing.T) {
	f := newFixture(t)
	body := paypaltest.CaptureEvent("WH-9", "PAYMENT.CAPTURE.COMPLETED", "ORDER-A", "10.00", "USD")
	good := paypaltest.Sign(webhookID, body)

	tampered := []byte(strings.Replace(string(body), `"10.00"`, `"1.00"`, 1))
	reformatted, _ := json.Marshal(json.RawMessage(body)) // same JSON, different bytes
	_ = reformatted
	compact := []byte(strings.ReplaceAll(string(body), "  ", " "))

	missing := func(name string) http.Header { h := good.Clone(); h.Del(name); return h }
	cases := map[string]struct {
		body []byte
		h    http.Header
		wid  string
	}{
		"tampered amount":       {tampered, good, webhookID},
		"whitespace changed":    {compact, good, webhookID},
		"wrong webhook id":      {body, good, "WH-OTHER"},
		"empty webhook id":      {body, good, ""},
		"no signature":          {body, missing("PAYPAL-TRANSMISSION-SIG"), webhookID},
		"no transmission id":    {body, missing("PAYPAL-TRANSMISSION-ID"), webhookID},
		"no cert url":           {body, missing("PAYPAL-CERT-URL"), webhookID},
		"no headers at all":     {body, http.Header{}, webhookID},
		"not json":              {[]byte("hello"), good, webhookID},
		"json without event id": {[]byte(`{"event_type":"PAYMENT.CAPTURE.COMPLETED"}`), good, webhookID},
	}
	for name, c := range cases {
		if _, err := f.verify(c.body, c.h, c.wid); !errors.Is(err, provider.ErrNotVerified) {
			t.Errorf("%s: got %v, want ErrNotVerified", name, err)
		}
	}
}

func TestVerifyOutageIsNotAVerdict(t *testing.T) {
	f := newFixture(t)
	body := paypaltest.ApprovedEvent("WH-5", "ORDER-A")
	f.srv.VerifyStatus = http.StatusServiceUnavailable
	_, err := f.verify(body, paypaltest.Sign(webhookID, body), webhookID)
	if err == nil || errors.Is(err, provider.ErrNotVerified) {
		t.Errorf("a verify outage must be retryable, not 'not verified': %v", err)
	}
	f.srv.VerifyStatus = http.StatusBadRequest
	if _, err := f.verify(body, paypaltest.Sign(webhookID, body), webhookID); !errors.Is(err, provider.ErrNotVerified) {
		t.Errorf("a 400 from verify means not verified: %v", err)
	}
}

func TestLocateWebhook(t *testing.T) {
	ad := paypal.New(paypal.Options{})
	if ref, ok := ad.LocateWebhook(paypaltest.ApprovedEvent("E", "ORD-1")); !ok || ref != "ORD-1" {
		t.Errorf("approved: %q %v", ref, ok)
	}
	if ref, ok := ad.LocateWebhook(paypaltest.CaptureEvent("E", "PAYMENT.CAPTURE.COMPLETED", "ORD-2", "1.00", "USD")); !ok || ref != "ORD-2" {
		t.Errorf("capture: %q %v", ref, ok)
	}
	for _, bad := range []string{"", "nope", `{"event_type":"X","resource":{}}`} {
		if _, ok := ad.LocateWebhook([]byte(bad)); ok {
			t.Errorf("%q should not locate", bad)
		}
	}
}
