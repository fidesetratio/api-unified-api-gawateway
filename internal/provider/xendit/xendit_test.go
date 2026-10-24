package xendit_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
	"unifiedgw/internal/provider/xendit"
	"unifiedgw/internal/provider/xendit/xendittest"
	"unifiedgw/internal/secrets"
)

const (
	apiKey   = "xnd_development_SECRET-KEY-DO-NOT-LEAK"
	cbToken  = "callback-token-123"
	returnTo = "https://shop.example/return"
	cancelTo = "https://shop.example/cancel"
)

type fixture struct {
	srv *xendittest.Server
	ad  *xendit.Adapter
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	srv := xendittest.New(apiKey)
	t.Cleanup(srv.Close)
	f := &fixture{srv: srv, now: time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)}
	f.ad = xendit.New(xendit.Options{BaseURL: srv.URL, Now: func() time.Time { return f.now }})
	return f
}

func creds() provider.Credentials { return provider.Credentials{"api_key": secrets.New(apiKey)} }

func settings() map[string]any { return map[string]any{"return_url": returnTo, "cancel_url": cancelTo} }

func amt(t *testing.T, v, cur string) domain.Amount {
	t.Helper()
	a, err := domain.ParseAmount(v, cur)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *fixture) create(t *testing.T, value, cur string, s map[string]any) (provider.CreateResult, error) {
	t.Helper()
	return f.ad.CreatePayment(context.Background(), provider.CreateRequest{
		Reference: "tx-uuid-1", IdempotencyKey: "idem", Amount: amt(t, value, cur), Environment: "sandbox", Credentials: creds(), Settings: s,
	})
}

func TestCreateSessionRequestAndResult(t *testing.T) {
	f := newFixture(t)
	res, err := f.create(t, "100000.00", "IDR", settings())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.ProviderRef, "ps-") || !strings.HasPrefix(res.PaymentURL, "https://checkout-staging.xendit.co/") || res.RawStatus != "ACTIVE" {
		t.Errorf("result %+v", res)
	}
	if res.ExpiresAt == nil {
		t.Error("expires_at missing")
	}
	body := string(f.srv.LastCreate)
	for _, want := range []string{
		`"reference_id":"tx-uuid-1"`, `"session_type":"PAY"`, `"mode":"PAYMENT_LINK"`, `"capture_method":"AUTOMATIC"`,
		`"currency":"IDR"`, `"country":"ID"`, `"amount":100000,`, // a JSON number, exact, not "100000.00" or 1e+05
		`"success_return_url":"` + returnTo + `"`, `"cancel_return_url":"` + cancelTo + `"`,
		`"expires_at":"2026-10-24T10:00:00Z"`, // now + 24h default
	} {
		if !strings.Contains(body, want) {
			t.Errorf("request lacks %s\n%s", want, body)
		}
	}
	if strings.Contains(body, apiKey) {
		t.Error("secret key sent in the body")
	}
}

func TestAmountIsAnExactJSONNumber(t *testing.T) {
	f := newFixture(t)
	s := settings()
	s["country"] = "ID"
	for in, want := range map[string]string{"10.50": `"amount":10.50,`, "0.01": `"amount":0.01,`, "1234567.89": `"amount":1234567.89,`} {
		if _, err := f.create(t, in, "USD", s); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(f.srv.LastCreate), want) {
			t.Errorf("%s: %s", in, f.srv.LastCreate)
		}
	}
}

func TestCreateSettingsAreRequired(t *testing.T) {
	f := newFixture(t)
	var pe *provider.Error
	cases := map[string]struct {
		cur  string
		s    map[string]any
		code string
	}{
		"no urls":             {"IDR", map[string]any{}, "config_incomplete_return_urls"},
		"http return url":     {"IDR", map[string]any{"return_url": "http://x.example/r", "cancel_url": cancelTo}, "config_incomplete_return_urls"},
		"usd without country": {"USD", settings(), "config_incomplete_country"},
		"usd bad country":     {"USD", map[string]any{"return_url": returnTo, "cancel_url": cancelTo, "country": "ZZ"}, "config_incomplete_country"},
	}
	for name, c := range cases {
		if _, err := f.create(t, "10", c.cur, c.s); !errors.As(err, &pe) || pe.Code != c.code {
			t.Errorf("%s: %v", name, err)
		}
	}
	usd := settings()
	usd["country"] = "id" // case-insensitive
	if _, err := f.create(t, "10.00", "USD", usd); err != nil {
		t.Errorf("USD with country: %v", err)
	}
	if n := f.srv.CreateCount(); n != 1 {
		t.Errorf("rejected requests reached Xendit: %d calls", n)
	}
	if _, err := f.create(t, "100", "JPY", settings()); !errors.As(err, &pe) || pe.Code != "amount_not_accepted" {
		t.Errorf("unsupported currency: %v", err)
	}
}

func TestExpiryIsAtLeastTenMinutes(t *testing.T) {
	f := newFixture(t)
	s := settings()
	s["expiry_minutes"] = float64(3)
	if _, err := f.create(t, "100", "IDR", s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.srv.LastCreate), `"expires_at":"2026-10-23T10:10:00Z"`) {
		t.Errorf("expiry not raised to the 10 minute minimum: %s", f.srv.LastCreate)
	}
}

func TestCreateErrorMapping(t *testing.T) {
	f := newFixture(t)
	var pe *provider.Error

	bad := xendit.New(xendit.Options{BaseURL: f.srv.URL})
	_, err := bad.CreatePayment(context.Background(), provider.CreateRequest{Reference: "r", Amount: amt(t, "100", "IDR"),
		Credentials: provider.Credentials{"api_key": secrets.New("wrong-key")}, Settings: settings()})
	if !errors.As(err, &pe) || pe.Code != "INVALID_API_KEY" {
		t.Errorf("bad key: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "wrong-key") {
		t.Error("error echoes the key")
	}
	if _, err := bad.CreatePayment(context.Background(), provider.CreateRequest{Reference: "r", Amount: amt(t, "100", "IDR"),
		Credentials: provider.Credentials{}, Settings: settings()}); !errors.As(err, &pe) || pe.Code != "credentials_incomplete" {
		t.Errorf("no key: %v", err)
	}

	f.srv.CreateStatus, f.srv.CreateCode = http.StatusBadRequest, "INVALID_AMOUNT"
	if _, err := f.create(t, "100", "IDR", settings()); !errors.As(err, &pe) || pe.Code != "INVALID_AMOUNT" {
		t.Errorf("4xx must be definite: %v", err)
	}
	for _, c := range []struct {
		status int
		code   string
	}{{http.StatusServiceUnavailable, "SERVER_ERROR"}, {http.StatusTooManyRequests, "RATE_LIMIT_EXCEEDED"}, {http.StatusConflict, "DUPLICATE_ERROR"}} {
		f.srv.CreateStatus, f.srv.CreateCode = c.status, c.code
		if _, err := f.create(t, "100", "IDR", settings()); !errors.Is(err, provider.ErrTimeout) {
			t.Errorf("%d %s must be an unknown outcome: %v", c.status, c.code, err)
		}
	}
	f.srv.Close()
	if _, err := f.create(t, "100", "IDR", settings()); !errors.Is(err, provider.ErrTimeout) {
		t.Errorf("connection failure: %v", err)
	}
}

func TestGetStatusMapping(t *testing.T) {
	f := newFixture(t)
	res, _ := f.create(t, "100000", "IDR", settings())
	get := func() (provider.StatusResult, error) {
		return f.ad.GetStatus(context.Background(), provider.StatusRequest{ProviderRef: res.ProviderRef, Credentials: creds()})
	}
	if st, err := get(); err != nil || st.Status != domain.StatusPending {
		t.Fatalf("active: %+v %v", st, err)
	}
	f.srv.SetSessionStatus(res.ProviderRef, "EXPIRED")
	if st, _ := get(); st.Status != domain.StatusExpired {
		t.Errorf("expired: %+v", st)
	}
	f.srv.SetSessionStatus(res.ProviderRef, "CANCELED")
	if st, _ := get(); st.Status != domain.StatusCancelled {
		t.Errorf("canceled: %+v", st)
	}
	if _, err := f.ad.GetStatus(context.Background(), provider.StatusRequest{ProviderRef: "ps-nope", Credentials: creds()}); err == nil {
		t.Error("unknown session should fail")
	}
}

func TestCompletedSessionIsPaidOnlyWhenThePaymentSucceededWithTheCapturedAmount(t *testing.T) {
	f := newFixture(t)
	res, _ := f.create(t, "100000", "IDR", settings())
	get := func() provider.StatusResult {
		st, err := f.ad.GetStatus(context.Background(), provider.StatusRequest{ProviderRef: res.ProviderRef, Credentials: creds()})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	f.srv.CompleteWithoutPayment(res.ProviderRef) // session says COMPLETED, no payment yet
	if st := get(); st.Status != domain.StatusPending || st.Amount != nil {
		t.Errorf("completed session without a payment must not be paid: %+v", st)
	}

	// A fractional IDR capture cannot be a valid amount: it is refused, never rounded into a match.
	f.srv.Complete(res.ProviderRef, "IDR", "60000", "40000.50")
	_, err := f.ad.GetStatus(context.Background(), provider.StatusRequest{ProviderRef: res.ProviderRef, Credentials: creds()})
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Code != "capture_amount_unreadable" {
		t.Errorf("fractional IDR capture: %v", err)
	}

	f.srv.Complete(res.ProviderRef, "IDR", "60000", "40000") // two captures, summed exactly
	st := get()
	if st.Status != domain.StatusPaid || st.Amount == nil || st.Amount.String() != "100000" {
		t.Errorf("paid: %+v", st)
	}

	for _, ps := range []string{"PENDING", "AUTHORIZED"} {
		f.srv.SetPaymentStatus(res.ProviderRef, ps)
		if st := get(); st.Status != domain.StatusPending {
			t.Errorf("payment %s must not be paid: %+v", ps, st)
		}
	}
	f.srv.SetPaymentStatus(res.ProviderRef, "FAILED")
	if st := get(); st.Status != domain.StatusFailed {
		t.Errorf("failed payment: %+v", st)
	}
}

// ---- webhooks --------------------------------------------------------------

func (f *fixture) verify(body []byte, token string) (provider.Event, error) {
	h := http.Header{}
	if token != "" {
		h.Set("x-callback-token", token)
	}
	return f.ad.VerifyWebhook(context.Background(), provider.WebhookRequest{
		Headers: h, Body: body, Credentials: creds(),
		WebhookSecret: provider.Credentials{"callback_token": secrets.New(cbToken)},
	})
}

func TestWebhookTokenIsRequiredWhenConfigured(t *testing.T) {
	f := newFixture(t)
	res, _ := f.create(t, "100000", "IDR", settings())
	f.srv.Complete(res.ProviderRef, "IDR", "100000")
	body := xendittest.SessionEvent("payment_session.completed", res.ProviderRef, "COMPLETED")

	reads := f.srv.ReadCount()
	for name, tok := range map[string]string{"missing": "", "wrong": "nope", "prefix": cbToken[:5], "longer": cbToken + "x"} {
		if _, err := f.verify(body, tok); !errors.Is(err, provider.ErrNotVerified) {
			t.Errorf("%s token: %v", name, err)
		}
	}
	if f.srv.ReadCount() != reads {
		t.Error("an unauthenticated delivery must not even trigger reads")
	}
	if _, err := f.verify(body, cbToken); err != nil {
		t.Errorf("correct token: %v", err)
	}
}

func TestCompletedWebhookReadsTheTruthFromTheAPI(t *testing.T) {
	f := newFixture(t)
	res, _ := f.create(t, "100000", "IDR", settings())
	f.srv.Complete(res.ProviderRef, "IDR", "100000")

	// The body lies (status/amount); only the API read counts.
	ev, err := f.verify(xendittest.SessionEvent("payment_session.completed", res.ProviderRef, "COMPLETED"), cbToken)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != domain.StatusPaid || ev.ProviderRef != res.ProviderRef || ev.Amount == nil || ev.Amount.String() != "100000" ||
		ev.EventID != "session:"+res.ProviderRef+":paid:100000IDR" || ev.Ignore {
		t.Errorf("event %+v", ev)
	}
}

func TestForgedCompletedWebhookForAnUnpaidSessionPaysNothing(t *testing.T) {
	f := newFixture(t)
	res, _ := f.create(t, "100000", "IDR", settings()) // still ACTIVE at Xendit
	ev, err := f.verify(xendittest.SessionEvent("payment_session.completed", res.ProviderRef, "COMPLETED"), cbToken)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status == domain.StatusPaid || !ev.Ignore {
		t.Errorf("a body claiming completion for an ACTIVE session must be ignored: %+v", ev)
	}

	// Same with no token configured: the API read still protects the payment.
	h := http.Header{}
	ev, err = f.ad.VerifyWebhook(context.Background(), provider.WebhookRequest{Headers: h,
		Body: xendittest.SessionEvent("payment_session.completed", res.ProviderRef, "COMPLETED"), Credentials: creds()})
	if err != nil || ev.Status == domain.StatusPaid {
		t.Errorf("no token configured: %+v %v", ev, err)
	}
}

func TestWebhookWaitsForTheSettledPayment(t *testing.T) {
	f := newFixture(t)
	res, _ := f.create(t, "100000", "IDR", settings())
	body := xendittest.SessionEvent("payment_session.completed", res.ProviderRef, "COMPLETED")

	f.srv.CompleteWithoutPayment(res.ProviderRef)
	if _, err := f.verify(body, cbToken); !errors.Is(err, provider.ErrTimeout) {
		t.Errorf("no payment yet must be retryable: %v", err)
	}
	f.srv.Complete(res.ProviderRef, "IDR", "100000")
	f.srv.SetPaymentStatus(res.ProviderRef, "PENDING")
	if _, err := f.verify(body, cbToken); !errors.Is(err, provider.ErrTimeout) {
		t.Errorf("pending payment must be retryable: %v", err)
	}
	f.srv.SetPaymentStatus(res.ProviderRef, "SUCCEEDED")
	if ev, err := f.verify(body, cbToken); err != nil || ev.Status != domain.StatusPaid {
		t.Errorf("after settlement: %+v %v", ev, err)
	}
	f.srv.ReadStatus = http.StatusServiceUnavailable
	if _, err := f.verify(body, cbToken); err == nil || errors.Is(err, provider.ErrNotVerified) {
		t.Errorf("an API outage is not a verdict: %v", err)
	}
}

func TestOtherWebhookEvents(t *testing.T) {
	f := newFixture(t)
	res, _ := f.create(t, "100000", "IDR", settings())

	f.srv.SetSessionStatus(res.ProviderRef, "EXPIRED")
	ev, err := f.verify(xendittest.SessionEvent("payment_session.expired", res.ProviderRef, "EXPIRED"), cbToken)
	if err != nil || ev.Status != domain.StatusExpired || ev.Ignore || ev.EventID != "session:"+res.ProviderRef+":expired" {
		t.Errorf("expired: %+v %v", ev, err)
	}

	ev, err = f.verify(xendittest.PaymentEvent("py-1"), cbToken)
	if err != nil || !ev.Ignore {
		t.Errorf("payment.* deliveries are acknowledged and ignored: %+v %v", ev, err)
	}
	for _, bad := range []string{"", "nope", `{"nothing":true}`} {
		if _, err := f.verify([]byte(bad), cbToken); !errors.Is(err, provider.ErrNotVerified) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestLocateAndValidate(t *testing.T) {
	ad := xendit.New(xendit.Options{})
	if ref, ok := ad.LocateWebhook(xendittest.SessionEvent("payment_session.completed", "ps-1", "COMPLETED")); !ok || ref != "ps-1" {
		t.Errorf("session event: %q %v", ref, ok)
	}
	for _, body := range [][]byte{xendittest.PaymentEvent("py-1"), []byte(""), []byte("junk"), []byte(`{"event":"payment_session.completed","data":{}}`)} {
		if _, ok := ad.LocateWebhook(body); ok {
			t.Errorf("%q should not locate", body)
		}
	}
	for _, c := range []string{"IDR", "PHP", "VND", "THB", "SGD", "MYR", "USD"} {
		if !xendit.SupportsCurrency(c) {
			t.Errorf("%s should be supported", c)
		}
	}
	if xendit.SupportsCurrency("EUR") || xendit.SupportsCurrency("JPY") {
		t.Error("EUR/JPY are not session currencies")
	}
	if err := ad.ValidateAmount(amt(t, "10", "EUR")); err == nil {
		t.Error("EUR must be refused")
	}
}
