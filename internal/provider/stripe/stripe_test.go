package stripe_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
	"unifiedgw/internal/provider/stripe"
	"unifiedgw/internal/provider/stripe/stripetest"
	"unifiedgw/internal/secrets"
)

const (
	apiKey = "sk_test_SECRET-DO-NOT-LEAK"
	whSec  = "whsec_test_signing_secret"
)

var fixedNow = time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)

type fixture struct {
	srv *stripetest.Server
	ad  *stripe.Adapter
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	srv := stripetest.New(apiKey)
	t.Cleanup(srv.Close)
	return &fixture{srv: srv, ad: stripe.New(stripe.Options{BaseURL: srv.URL, Now: func() time.Time { return fixedNow }})}
}

func creds() provider.Credentials { return provider.Credentials{"api_key": secrets.New(apiKey)} }

func settings() map[string]any {
	return map[string]any{"return_url": "https://shop.example/return", "cancel_url": "https://shop.example/cancel"}
}

func amt(t *testing.T, v, cur string) domain.Amount {
	t.Helper()
	a, err := domain.ParseAmount(v, cur)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *fixture) create(t *testing.T, key, value, cur string, s map[string]any) (provider.CreateResult, error) {
	t.Helper()
	return f.ad.CreatePayment(context.Background(), provider.CreateRequest{
		Reference: "tx-uuid-1", IdempotencyKey: key, Amount: amt(t, value, cur), Credentials: creds(), Settings: s})
}

func TestCreateSessionRequestAndResult(t *testing.T) {
	f := newFixture(t)
	res, err := f.create(t, "idem-1", "10.50", "USD", settings())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.ProviderRef, "cs_test_") || !strings.HasPrefix(res.PaymentURL, "https://checkout.stripe.com/") || res.RawStatus != "open" || res.ExpiresAt == nil {
		t.Errorf("result %+v", res)
	}
	form := f.srv.LastForm
	want := map[string]string{
		"mode": "payment", "success_url": "https://shop.example/return", "cancel_url": "https://shop.example/cancel",
		"line_items[0][price_data][currency]": "usd", "line_items[0][price_data][unit_amount]": "1050",
		"line_items[0][quantity]": "1", "client_reference_id": "tx-uuid-1",
		"metadata[gateway_transaction_id]": "tx-uuid-1", "payment_intent_data[metadata][gateway_transaction_id]": "tx-uuid-1",
		"expires_at": "1792836000", // now + 24h default
	}
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, form.Get(k), v)
		}
	}
	if got := f.srv.LastHeaders.Get("Idempotency-Key"); got != "idem-1" {
		t.Errorf("Idempotency-Key = %q", got)
	}
	if strings.Contains(form.Encode(), apiKey) {
		t.Error("secret key in the body")
	}
}

func TestAmountsAreSentInMinorUnits(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct{ value, cur, want string }{
		{"10.50", "USD", "1050"}, {"0.50", "EUR", "50"}, {"1234567.89", "GBP", "123456789"},
		{"100", "JPY", "100"}, {"5000.00", "KRW", "5000"}, {"7", "CHF", "700"},
	} {
		if _, err := f.create(t, "k-"+c.cur+c.value, c.value, c.cur, settings()); err != nil {
			t.Fatalf("%s %s: %v", c.value, c.cur, err)
		}
		if got := f.srv.LastForm.Get("line_items[0][price_data][unit_amount]"); got != c.want {
			t.Errorf("%s %s sent %s, want %s", c.value, c.cur, got, c.want)
		}
	}
	// A currency whose decimal handling is not certain is refused before any call.
	creates, _ := f.srv.Counts()
	var pe *provider.Error
	for _, cur := range []string{"IDR", "KWD", "BHD", "ISK", "XYZ"} {
		if _, err := f.create(t, "x-"+cur, "100", cur, settings()); !errors.As(err, &pe) || pe.Code != "amount_not_accepted" {
			t.Errorf("%s: %v", cur, err)
		}
	}
	if after, _ := f.srv.Counts(); after != creates {
		t.Error("an unsupported currency reached Stripe")
	}
	if !stripe.SupportsCurrency("USD") || stripe.SupportsCurrency("IDR") {
		t.Error("SupportsCurrency wrong")
	}
}

func TestExpiryIsClampedToStripesBounds(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		minutes float64
		want    string
	}{{5, "1792751400"}, {60, "1792753200"}, {99999, "1792836000"}} { // 30 min, 60 min, 24 h after fixedNow
		s := settings()
		s["expiry_minutes"] = c.minutes
		if _, err := f.create(t, fmt.Sprint("k", c.minutes), "10.00", "USD", s); err != nil {
			t.Fatal(err)
		}
		if got := f.srv.LastForm.Get("expires_at"); got != c.want {
			t.Errorf("expiry_minutes %v sent %s, want %s", c.minutes, got, c.want)
		}
	}
}

func TestCreateIsIdempotent(t *testing.T) {
	f := newFixture(t)
	a, _ := f.create(t, "same", "10.00", "USD", settings())
	b, _ := f.create(t, "same", "10.00", "USD", settings())
	c, _ := f.create(t, "other", "10.00", "USD", settings())
	if a.ProviderRef != b.ProviderRef || a.ProviderRef == c.ProviderRef {
		t.Errorf("refs %s %s %s", a.ProviderRef, b.ProviderRef, c.ProviderRef)
	}
	if len(f.srv.SessionIDs()) != 2 {
		t.Errorf("sessions = %d, want 2", len(f.srv.SessionIDs()))
	}
	// Same key with different parameters is a definite error, not a silent second payment.
	var pe *provider.Error
	if _, err := f.create(t, "same", "99.00", "USD", settings()); !errors.As(err, &pe) || pe.Code != "idempotency_key_in_use" {
		t.Errorf("key reuse with other params: %v", err)
	}
}

func TestCreateSettingsAndErrors(t *testing.T) {
	f := newFixture(t)
	var pe *provider.Error
	if _, err := f.create(t, "k", "10.00", "USD", map[string]any{}); !errors.As(err, &pe) || pe.Code != "config_incomplete_return_urls" {
		t.Errorf("no urls: %v", err)
	}
	if _, err := f.create(t, "k", "10.00", "USD", map[string]any{"return_url": "javascript:x", "cancel_url": "https://x.example/c"}); !errors.As(err, &pe) {
		t.Errorf("bad url: %v", err)
	}

	bad := stripe.New(stripe.Options{BaseURL: f.srv.URL})
	_, err := bad.CreatePayment(context.Background(), provider.CreateRequest{Reference: "r", IdempotencyKey: "k", Amount: amt(t, "1", "USD"),
		Credentials: provider.Credentials{"api_key": secrets.New("sk_test_wrong")}, Settings: settings()})
	if !errors.As(err, &pe) || pe.Code != "api_key_invalid" {
		t.Errorf("bad key: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "sk_test_wrong") {
		t.Error("error echoes the key")
	}
	if _, err := bad.CreatePayment(context.Background(), provider.CreateRequest{Reference: "r", Amount: amt(t, "1", "USD"),
		Credentials: provider.Credentials{}, Settings: settings()}); !errors.As(err, &pe) || pe.Code != "credentials_incomplete" {
		t.Errorf("no key: %v", err)
	}

	for _, c := range []struct {
		status int
		typ    string
	}{{http.StatusTooManyRequests, "invalid_request_error"}, {http.StatusServiceUnavailable, "api_error"}, {http.StatusConflict, "idempotency_error"}} {
		f.srv.CreateStatus, f.srv.CreateType = c.status, c.typ
		if _, err := f.create(t, "e", "10.00", "USD", settings()); !errors.Is(err, provider.ErrTimeout) {
			t.Errorf("%d must be an unknown outcome: %v", c.status, err)
		}
	}
	f.srv.CreateStatus, f.srv.CreateType = http.StatusBadRequest, "invalid_request_error"
	if _, err := f.create(t, "e", "10.00", "USD", settings()); !errors.As(err, &pe) {
		t.Errorf("400 must be definite: %v", err)
	}
	f.srv.Close()
	if _, err := f.create(t, "e", "10.00", "USD", settings()); !errors.Is(err, provider.ErrTimeout) {
		t.Errorf("connection failure: %v", err)
	}
}

func TestGetStatusMapping(t *testing.T) {
	f := newFixture(t)
	res, _ := f.create(t, "k", "10.50", "USD", settings())
	get := func() provider.StatusResult {
		st, err := f.ad.GetStatus(context.Background(), provider.StatusRequest{ProviderRef: res.ProviderRef, Credentials: creds()})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := get(); st.Status != domain.StatusPending || st.Amount != nil {
		t.Errorf("open: %+v", st)
	}
	f.srv.CompleteUnpaid(res.ProviderRef)
	if st := get(); st.Status != domain.StatusPending || st.Amount != nil {
		t.Errorf("complete but unpaid is a delayed payment, not paid: %+v", st)
	}
	f.srv.Pay(res.ProviderRef)
	if st := get(); st.Status != domain.StatusPaid || st.Amount == nil || st.Amount.String() != "10.50" || st.Amount.Currency() != "USD" {
		t.Errorf("paid: %+v", st)
	}
	f.srv.Expire(res.ProviderRef)
	if st := get(); st.Status != domain.StatusExpired {
		t.Errorf("expired: %+v", st)
	}
	if _, err := f.ad.GetStatus(context.Background(), provider.StatusRequest{ProviderRef: "cs_nope", Credentials: creds()}); err == nil {
		t.Error("unknown session should fail")
	}
}

func TestZeroDecimalAmountTotalIsReadBack(t *testing.T) {
	f := newFixture(t)
	res, _ := f.create(t, "k", "5000", "JPY", settings())
	f.srv.Pay(res.ProviderRef)
	st, err := f.ad.GetStatus(context.Background(), provider.StatusRequest{ProviderRef: res.ProviderRef, Credentials: creds()})
	if err != nil || st.Amount == nil || st.Amount.String() != "5000" {
		t.Errorf("%+v %v", st, err)
	}
}

// ---- webhooks --------------------------------------------------------------

func (f *fixture) verify(body []byte, h http.Header, secret string) (provider.Event, error) {
	return f.ad.VerifyWebhook(context.Background(), provider.WebhookRequest{
		Headers: h, Body: body, Credentials: creds(),
		WebhookSecret: provider.Credentials{"signing_secret": secrets.New(secret)},
	})
}

// Known-answer vector computed independently (Node crypto.createHmac) so this test
// does not just compare the adapter with itself.
func TestSignatureKnownAnswer(t *testing.T) {
	const secret = "whsec_known_answer_secret"
	body := []byte(`{"id":"evt_1","type":"checkout.session.completed"}`)
	const sig = "c7d099d5e4e165018d5dd142a8299e58229922d8fe8690b684488865770d7f89"
	at := time.Unix(1700000000, 0)
	ad := stripe.New(stripe.Options{Now: func() time.Time { return at }})
	check := func(header string) error {
		_, err := ad.VerifyWebhook(context.Background(), provider.WebhookRequest{
			Headers:       http.Header{"Stripe-Signature": {header}},
			Body:          body,
			WebhookSecret: provider.Credentials{"signing_secret": secrets.New(secret)},
		})
		return err
	}
	// The expected signature makes verification pass the signature step (the event is
	// then read from an API that is not running, which is a transport error, not a verdict).
	if err := check("t=1700000000,v1=" + sig); errors.Is(err, provider.ErrNotVerified) {
		t.Fatalf("the independently computed signature was rejected: %v", err)
	}
	if err := check("t=1700000000,v1=" + strings.Repeat("0", 64)); !errors.Is(err, provider.ErrNotVerified) {
		t.Errorf("wrong signature accepted: %v", err)
	}
}

func TestVerifyWebhookRejectsEverythingThatIsNotAuthentic(t *testing.T) {
	f := newFixture(t)
	res, _ := f.create(t, "k", "10.00", "USD", settings())
	f.srv.Pay(res.ProviderRef)
	body := stripetest.Event("evt_1", "checkout.session.completed", res.ProviderRef)
	now := fixedNow.Unix()
	good := stripetest.Sign(now, body, whSec)

	tampered := []byte(strings.Replace(string(body), `"amount_total":1`, `"amount_total":2`, 1))
	reformatted := []byte(strings.ReplaceAll(string(body), "  ", " "))
	hdr := func(s string) http.Header { h := http.Header{}; h.Set("Stripe-Signature", s); return h }

	cases := map[string]struct {
		body   []byte
		h      http.Header
		secret string
	}{
		"tampered body":       {tampered, good, whSec},
		"re-serialized body":  {reformatted, good, whSec},
		"wrong secret":        {body, good, "whsec_other"},
		"empty secret":        {body, good, ""},
		"no header":           {body, http.Header{}, whSec},
		"garbage header":      {body, hdr("hello"), whSec},
		"no timestamp":        {body, hdr("v1=abcd"), whSec},
		"no v1":               {body, hdr("t=" + itoa(now) + ",v0=abcd"), whSec},
		"only v0":             {body, hdr("t=" + itoa(now) + ",v0=" + strings.Repeat("a", 64)), whSec},
		"non-hex signature":   {body, hdr("t=" + itoa(now) + ",v1=zzzz"), whSec},
		"stale (6 min old)":   {body, stripetest.Sign(now-6*60, body, whSec), whSec},
		"from the future":     {body, stripetest.Sign(now+6*60, body, whSec), whSec},
		"signature for other": {body, stripetest.Sign(now, []byte("other body"), whSec), whSec},
	}
	for name, c := range cases {
		if _, err := f.verify(c.body, c.h, c.secret); !errors.Is(err, provider.ErrNotVerified) {
			t.Errorf("%s: got %v, want ErrNotVerified", name, err)
		}
	}
	_, reads := f.srv.Counts()
	if reads != 0 {
		t.Errorf("an unauthentic delivery triggered %d API reads", reads)
	}

	// Within tolerance, and with rotation (two v1 entries, only the second valid).
	if _, err := f.verify(body, stripetest.Sign(now-4*60, body, whSec), whSec); err != nil {
		t.Errorf("4 minutes old: %v", err)
	}
	if _, err := f.verify(body, stripetest.Sign(now, body, "whsec_old_rotated_out", whSec), whSec); err != nil {
		t.Errorf("rotation with a second v1: %v", err)
	}
}

func itoa(n int64) string { return intString(n) }

func intString(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestEventMapping(t *testing.T) {
	f := newFixture(t)
	res, _ := f.create(t, "k", "10.50", "USD", settings())
	ref := res.ProviderRef
	send := func(typ string) (provider.Event, error) {
		b := stripetest.Event("evt_"+typ, typ, ref)
		return f.verify(b, stripetest.Sign(fixedNow.Unix(), b, whSec), whSec)
	}

	// Completed but the API says the session is still open/unpaid: the body is not believed.
	if ev, err := send("checkout.session.completed"); err != nil || !ev.Ignore || ev.Status == domain.StatusPaid {
		t.Errorf("signed body claims paid, API says open: %+v %v", ev, err)
	}
	f.srv.CompleteUnpaid(ref)
	if ev, err := send("checkout.session.completed"); err != nil || !ev.Ignore {
		t.Errorf("delayed payment: %+v %v", ev, err)
	}

	f.srv.Pay(ref)
	ev, err := send("checkout.session.completed")
	if err != nil || ev.Status != domain.StatusPaid || ev.Amount == nil || ev.Amount.String() != "10.50" || ev.ProviderRef != ref || ev.EventID != "evt_checkout.session.completed" || ev.Ignore {
		t.Errorf("paid: %+v %v", ev, err)
	}
	if ev, err := send("checkout.session.async_payment_succeeded"); err != nil || ev.Status != domain.StatusPaid {
		t.Errorf("async succeeded: %+v %v", ev, err)
	}
	if ev, err := send("checkout.session.async_payment_failed"); err != nil || ev.Status != domain.StatusFailed {
		t.Errorf("async failed: %+v %v", ev, err)
	}
	if ev, err := send("checkout.session.expired"); err != nil || ev.Status != domain.StatusExpired {
		t.Errorf("expired: %+v %v", ev, err)
	}
	for _, typ := range []string{"charge.refunded", "payment_intent.succeeded", "customer.created"} {
		if ev, err := send(typ); err != nil || !ev.Ignore {
			t.Errorf("%s should be acknowledged and ignored: %+v %v", typ, ev, err)
		}
	}

	// A paid event whose amount differs at Stripe is reported with Stripe's number, not the body's.
	f.srv.SetAmountTotal(ref, 999)
	if ev, err := send("checkout.session.completed"); err != nil || ev.Amount == nil || ev.Amount.String() != "9.99" {
		t.Errorf("amount must come from the API: %+v %v", ev, err)
	}

	// API outage while reading: not a verdict, so Stripe retries.
	f.srv.ReadStatus = http.StatusServiceUnavailable
	if _, err := send("checkout.session.completed"); err == nil || errors.Is(err, provider.ErrNotVerified) {
		t.Errorf("outage: %v", err)
	}
}

func TestLocateWebhook(t *testing.T) {
	ad := stripe.New(stripe.Options{})
	if ref, ok := ad.LocateWebhook(stripetest.Event("e", "checkout.session.completed", "cs_1")); !ok || ref != "cs_1" {
		t.Errorf("session event: %q %v", ref, ok)
	}
	for _, b := range []string{"", "junk", `{"type":"charge.refunded","data":{"object":{"id":"ch_1"}}}`, `{"type":"checkout.session.completed","data":{"object":{}}}`} {
		if _, ok := ad.LocateWebhook([]byte(b)); ok {
			t.Errorf("%q should not locate", b)
		}
	}
}
