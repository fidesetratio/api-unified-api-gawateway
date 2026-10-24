package midtrans_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
	"unifiedgw/internal/provider/midtrans"
	"unifiedgw/internal/provider/midtrans/midtranstest"
	"unifiedgw/internal/secrets"
)

const serverKey = "SB-Mid-server-SECRET-DO-NOT-LEAK"

var fixedNow = time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)

type fixture struct {
	srv *midtranstest.Server
	ad  *midtrans.Adapter
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	srv := midtranstest.New(serverKey)
	t.Cleanup(srv.Close)
	return &fixture{srv: srv, ad: midtrans.New(midtrans.Options{SnapBase: srv.URL, CoreBase: srv.URL, Now: func() time.Time { return fixedNow }})}
}

func creds() provider.Credentials { return provider.Credentials{"server_key": secrets.New(serverKey)} }

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

func (f *fixture) create(t *testing.T, ref, value, cur string, s map[string]any) (provider.CreateResult, error) {
	t.Helper()
	return f.ad.CreatePayment(context.Background(), provider.CreateRequest{
		Reference: ref, IdempotencyKey: "idem", Amount: amt(t, value, cur), Environment: "sandbox", Credentials: creds(), Settings: s})
}

func TestCreateSnapRequestAndResult(t *testing.T) {
	f := newFixture(t)
	res, err := f.create(t, "0b6f8a52-1c2d-4e5f-9a7b-3c4d5e6f7a8b", "100000.00", "IDR", settings())
	if err != nil {
		t.Fatal(err)
	}
	if res.ProviderRef != "0b6f8a52-1c2d-4e5f-9a7b-3c4d5e6f7a8b" || !strings.Contains(res.PaymentURL, "/snap/v2/vtweb/tok-") || res.ExpiresAt == nil || !res.ExpiresAt.Equal(fixedNow.Add(24*time.Hour)) {
		t.Errorf("result %+v", res)
	}
	body := string(f.srv.LastCreate)
	for _, want := range []string{
		`"order_id":"0b6f8a52-1c2d-4e5f-9a7b-3c4d5e6f7a8b"`, `"gross_amount":100000,`, // an integer, no decimals, not a string
		`"expiry":{"duration":1440,"unit":"minutes"}`,
		`"finish":"https://shop.example/return"`, `"error":"https://shop.example/cancel"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("request lacks %s\n%s", want, body)
		}
	}
	if strings.Contains(body, serverKey) {
		t.Error("server key in the body")
	}
}

func TestCreateWithoutCallbacksAndExpiryBounds(t *testing.T) {
	f := newFixture(t)
	if _, err := f.create(t, "o1", "5000", "IDR", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(f.srv.LastCreate), "callbacks") {
		t.Errorf("callbacks should be omitted when not configured: %s", f.srv.LastCreate)
	}
	s := settings()
	s["expiry_minutes"] = float64(999999)
	if _, err := f.create(t, "o2", "5000", "IDR", s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.srv.LastCreate), `"duration":10080`) { // capped at 7 days
		t.Errorf("expiry not capped: %s", f.srv.LastCreate)
	}
}

func TestOnlyIDRAndValidOrderIdsAreAccepted(t *testing.T) {
	f := newFixture(t)
	var pe *provider.Error
	for _, cur := range []string{"USD", "EUR", "SGD"} {
		if _, err := f.create(t, "o1", "10.00", cur, settings()); !errors.As(err, &pe) || pe.Code != "amount_not_accepted" {
			t.Errorf("%s: %v", cur, err)
		}
	}
	for _, ref := range []string{"has space", "semi;colon", strings.Repeat("a", 51), ""} {
		if _, err := f.create(t, ref, "5000", "IDR", settings()); !errors.As(err, &pe) || pe.Code != "invalid_order_reference" {
			t.Errorf("order id %q: %v", ref, err)
		}
	}
	if creates, _ := f.srv.Counts(); creates != 0 {
		t.Errorf("rejected requests reached Midtrans: %d", creates)
	}
	if !midtrans.SupportsCurrency("IDR") || midtrans.SupportsCurrency("USD") {
		t.Error("SupportsCurrency wrong")
	}
	if err := f.ad.ValidateAmount(amt(t, "1", "USD")); err == nil {
		t.Error("USD must be refused")
	}
}

func TestCreateErrorMapping(t *testing.T) {
	f := newFixture(t)
	var pe *provider.Error

	bad := midtrans.New(midtrans.Options{SnapBase: f.srv.URL, CoreBase: f.srv.URL})
	_, err := bad.CreatePayment(context.Background(), provider.CreateRequest{Reference: "o", Amount: amt(t, "5000", "IDR"),
		Credentials: provider.Credentials{"server_key": secrets.New("wrong-key")}, Settings: settings()})
	if !errors.As(err, &pe) || pe.Code != "invalid_server_key" {
		t.Errorf("bad key: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "wrong-key") {
		t.Error("error echoes the key")
	}
	if _, err := bad.CreatePayment(context.Background(), provider.CreateRequest{Reference: "o", Amount: amt(t, "5000", "IDR"),
		Credentials: provider.Credentials{}, Settings: settings()}); !errors.As(err, &pe) || pe.Code != "credentials_incomplete" {
		t.Errorf("no key: %v", err)
	}

	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		f.srv.CreateStatus = status
		if _, err := f.create(t, "o", "5000", "IDR", settings()); !errors.Is(err, provider.ErrTimeout) {
			t.Errorf("%d must be an unknown outcome: %v", status, err)
		}
	}
	f.srv.CreateStatus = http.StatusBadRequest
	if _, err := f.create(t, "o", "5000", "IDR", settings()); !errors.As(err, &pe) {
		t.Errorf("400 must be definite: %v", err)
	}
	f.srv.CreateStatus = 0

	// A reused order id (once a payment exists) is an unknown outcome, so it is retried, not failed.
	f.create(t, "dup", "5000", "IDR", settings())
	f.srv.Pay("dup", "pending", "", "5000.00")
	if _, err := f.create(t, "dup", "5000", "IDR", settings()); !errors.Is(err, provider.ErrTimeout) {
		t.Errorf("duplicate order id: %v", err)
	}
	f.srv.Close()
	if _, err := f.create(t, "o", "5000", "IDR", settings()); !errors.Is(err, provider.ErrTimeout) {
		t.Errorf("connection failure: %v", err)
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	lastHost = r.URL.Host
	return nil, errors.New("no network in tests")
}

var lastHost string

func TestEnvironmentHosts(t *testing.T) {
	for env, want := range map[string]string{"sandbox": "app.sandbox.midtrans.com", "production": "app.midtrans.com"} {
		ad := midtrans.New(midtrans.Options{HTTPClient: &http.Client{Transport: failingTransport{}}})
		_, _ = ad.CreatePayment(context.Background(), provider.CreateRequest{Reference: "o", Amount: amt(t, "5000", "IDR"),
			Environment: env, Credentials: creds(), Settings: settings()})
		if lastHost != want {
			t.Errorf("%s: host %q, want %q", env, lastHost, want)
		}
		_, _ = ad.GetStatus(context.Background(), provider.StatusRequest{ProviderRef: "o", Environment: env, Credentials: creds()})
		wantCore := strings.Replace(want, "app.", "api.", 1)
		if lastHost != wantCore {
			t.Errorf("%s: core host %q, want %q", env, lastHost, wantCore)
		}
	}
}

func TestGetStatusMapping(t *testing.T) {
	f := newFixture(t)
	f.create(t, "o1", "10000", "IDR", settings())
	get := func() provider.StatusResult {
		st, err := f.ad.GetStatus(context.Background(), provider.StatusRequest{ProviderRef: "o1", Environment: "sandbox", Credentials: creds()})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := get(); st.Status != domain.StatusPending || st.RawStatus != "no_payment_yet" {
		t.Errorf("no payment yet (HTTP 404): %+v", st)
	}
	f.srv.BodyStatusCodeOn200 = true
	if st := get(); st.Status != domain.StatusPending {
		t.Errorf("no payment yet (404 in the body of a 200): %+v", st)
	}

	cases := []struct {
		status, fraud string
		want          domain.Status
	}{
		{"pending", "", domain.StatusPending}, {"authorize", "accept", domain.StatusPending},
		{"capture", "challenge", domain.StatusPending}, {"capture", "", domain.StatusPending},
		{"settlement", "", domain.StatusPaid}, {"capture", "accept", domain.StatusPaid},
		{"deny", "deny", domain.StatusFailed}, {"failure", "", domain.StatusFailed},
		{"cancel", "", domain.StatusCancelled}, {"expire", "", domain.StatusExpired},
		{"refund", "", domain.StatusPending}, {"chargeback", "", domain.StatusPending}, {"weird", "", domain.StatusPending},
	}
	for _, c := range cases {
		f.srv.Pay("o1", c.status, c.fraud, "10000.00")
		st := get()
		if st.Status != c.want {
			t.Errorf("%s/%s -> %s, want %s", c.status, c.fraud, st.Status, c.want)
		}
		if c.want == domain.StatusPaid && (st.Amount == nil || st.Amount.String() != "10000") {
			t.Errorf("%s/%s amount %+v", c.status, c.fraud, st.Amount)
		}
	}
	f.srv.ReadStatus = http.StatusServiceUnavailable
	if _, err := f.ad.GetStatus(context.Background(), provider.StatusRequest{ProviderRef: "o1", Credentials: creds()}); !errors.Is(err, provider.ErrTimeout) {
		t.Errorf("outage: %v", err)
	}
	f.srv.ReadStatus = 0
	f.srv.Pay("o1", "settlement", "", "not-a-number")
	var pe *provider.Error
	if _, err := f.ad.GetStatus(context.Background(), provider.StatusRequest{ProviderRef: "o1", Credentials: creds()}); !errors.As(err, &pe) || pe.Code != "gross_amount_unreadable" {
		t.Errorf("unreadable amount: %v", err)
	}
}

// ---- webhooks --------------------------------------------------------------

func (f *fixture) verify(body []byte, key string) (provider.Event, error) {
	return f.ad.VerifyWebhook(context.Background(), provider.WebhookRequest{
		Body: body, Environment: "sandbox", Credentials: provider.Credentials{"server_key": secrets.New(key)}})
}

// Known-answer vector computed independently with Node's crypto (SHA-512).
func TestSignatureKnownAnswer(t *testing.T) {
	const want = "f9826c00d14436851638f006aac18a3d802a79436f4ff9334fb2962dbc4d0b95917e6b79e578f95635224886fab7a61668929956e731416e9d8ced588b500f39"
	if got := midtranstest.Signature("ORDER-A1B2", "200", "10000.00", "SB-Mid-server-known-answer"); got != want {
		t.Fatalf("test helper disagrees with the independent vector: %s", got)
	}
	f := newFixture(t)
	body := []byte(`{"order_id":"ORDER-A1B2","status_code":"200","gross_amount":"10000.00","signature_key":"` + want + `"}`)
	// Accepted by the signature check: the next step (a status read of an order the stub never saw) reports "no payment yet".
	ev, err := f.ad.VerifyWebhook(context.Background(), provider.WebhookRequest{Body: body,
		Credentials: provider.Credentials{"server_key": secrets.New("SB-Mid-server-known-answer")}})
	if errors.Is(err, provider.ErrNotVerified) {
		t.Fatalf("the independently computed signature was rejected: %v", err)
	}
	_ = ev
}

func TestVerifyRejectsEverythingThatIsNotAuthentic(t *testing.T) {
	f := newFixture(t)
	f.create(t, "o1", "10000", "IDR", settings())
	f.srv.Pay("o1", "settlement", "", "10000.00")
	good := midtranstest.Notification("o1", "200", "10000.00", serverKey, "settlement")

	other := midtranstest.Notification("o1", "200", "10000.00", "some-other-key", "settlement")
	badAmount := []byte(strings.Replace(string(good), `"10000.00"`, `"1.00"`, 1))
	badOrder := []byte(strings.Replace(string(good), `"order_id":"o1"`, `"order_id":"o2"`, 1))
	upper := []byte(strings.Replace(string(good), midtranstest.Signature("o1", "200", "10000.00", serverKey), strings.ToUpper(midtranstest.Signature("o1", "200", "10000.00", serverKey)), 1))

	cases := map[string]struct {
		body []byte
		key  string
	}{
		"signed with another key": {other, serverKey},
		"amount changed":          {badAmount, serverKey},
		"order changed":           {badOrder, serverKey},
		"empty server key":        {good, ""},
		"no signature":            {[]byte(`{"order_id":"o1","status_code":"200","gross_amount":"10000.00"}`), serverKey},
		"no gross amount":         {[]byte(`{"order_id":"o1","status_code":"200","signature_key":"abc"}`), serverKey},
		"junk":                    {[]byte("hello"), serverKey},
		"invalid order id":        {[]byte(`{"order_id":"a b","status_code":"200","gross_amount":"1","signature_key":"x"}`), serverKey},
	}
	for name, c := range cases {
		if _, err := f.verify(c.body, c.key); !errors.Is(err, provider.ErrNotVerified) {
			t.Errorf("%s: got %v, want ErrNotVerified", name, err)
		}
	}
	if _, reads := f.srv.Counts(); reads != 0 {
		t.Errorf("an unauthentic notification triggered %d API reads", reads)
	}
	// Hex case does not matter.
	if _, err := f.verify(upper, serverKey); err != nil {
		t.Errorf("upper-case hex: %v", err)
	}
	if _, err := f.verify(good, serverKey); err != nil {
		t.Errorf("genuine: %v", err)
	}
}

func TestNotificationOutcomeComesFromTheStatusAPI(t *testing.T) {
	f := newFixture(t)
	f.create(t, "o1", "10000", "IDR", settings())

	// Signed notification claims settlement, but Midtrans has no payment for the order.
	body := midtranstest.Notification("o1", "200", "10000.00", serverKey, "settlement")
	ev, err := f.verify(body, serverKey)
	if err != nil || !ev.Ignore || ev.Status == domain.StatusPaid {
		t.Errorf("no payment at Midtrans: %+v %v", ev, err)
	}
	// ...or a pending one.
	f.srv.Pay("o1", "pending", "", "10000.00")
	if ev, err := f.verify(body, serverKey); err != nil || !ev.Ignore {
		t.Errorf("pending at Midtrans: %+v %v", ev, err)
	}
	// Challenge is not paid.
	f.srv.Pay("o1", "capture", "challenge", "10000.00")
	if ev, err := f.verify(body, serverKey); err != nil || !ev.Ignore {
		t.Errorf("challenge: %+v %v", ev, err)
	}

	f.srv.Pay("o1", "settlement", "", "10000.00")
	ev, err = f.verify(body, serverKey)
	if err != nil || ev.Status != domain.StatusPaid || ev.Ignore || ev.ProviderRef != "o1" || ev.Amount == nil || ev.Amount.String() != "10000" ||
		ev.EventID != "order:o1:settlement::10000.00" {
		t.Errorf("settlement: %+v %v", ev, err)
	}
	f.srv.Pay("o1", "capture", "accept", "10000.00")
	if ev, err := f.verify(body, serverKey); err != nil || ev.Status != domain.StatusPaid {
		t.Errorf("card capture accept: %+v %v", ev, err)
	}
	// The amount is Midtrans's, not the body's.
	f.srv.Pay("o1", "settlement", "", "9000.00")
	if ev, _ := f.verify(body, serverKey); ev.Amount == nil || ev.Amount.String() != "9000" {
		t.Errorf("amount must come from the API: %+v", ev)
	}

	for status, want := range map[string]domain.Status{"deny": domain.StatusFailed, "expire": domain.StatusExpired, "cancel": domain.StatusCancelled} {
		f.srv.Pay("o1", status, "", "10000.00")
		if ev, err := f.verify(body, serverKey); err != nil || ev.Status != want {
			t.Errorf("%s: %+v %v", status, ev, err)
		}
	}
	f.srv.Pay("o1", "refund", "", "10000.00")
	if ev, err := f.verify(body, serverKey); err != nil || !ev.Ignore {
		t.Errorf("refunds are ignored for now: %+v %v", ev, err)
	}

	f.srv.ReadStatus = http.StatusServiceUnavailable
	if _, err := f.verify(body, serverKey); err == nil || errors.Is(err, provider.ErrNotVerified) {
		t.Errorf("an API outage is not a verdict: %v", err)
	}
}

func TestLocateWebhook(t *testing.T) {
	ad := midtrans.New(midtrans.Options{})
	if ref, ok := ad.LocateWebhook(midtranstest.Notification("o1", "200", "1.00", "k", "pending")); !ok || ref != "o1" {
		t.Errorf("%q %v", ref, ok)
	}
	for _, b := range []string{"", "junk", `{"order_id":"a b"}`, `{"nothing":1}`} {
		if _, ok := ad.LocateWebhook([]byte(b)); ok {
			t.Errorf("%q should not locate", b)
		}
	}
}
