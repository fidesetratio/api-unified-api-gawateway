package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"unifiedgw/internal/provider"
	"unifiedgw/internal/provider/xendit"
	"unifiedgw/internal/provider/xendit/xendittest"
)

func TestAdminProviderSpecificRules(t *testing.T) {
	a := newApp(t)
	creds := func(p string) string {
		if p == "xendit" {
			return `"credentials":{"api_key":"xnd_development_X"}`
		}
		return `"credentials":{"client_id":"a","client_secret":"b"}`
	}
	body := func(p, extra string) string {
		return `{"provider":"` + p + `","environment":"sandbox","name":"n-` + p + extra[:0] + `","allowed_currencies":["USD"],` + creds(p) + extra + `}`
	}

	cases := []struct {
		name, body string
		want       int
	}{
		{"paypal IDR rejected", strings.Replace(body("paypal", ""), `["USD"]`, `["USD","IDR"]`, 1), 400},
		{"paypal http return url allowed", body("paypal", `,"settings":{"return_url":"http://x.example/r","cancel_url":"http://x.example/c"}`), 201},
		{"paypal bad url rejected", strings.Replace(body("paypal", `,"settings":{"return_url":"ftp://x","cancel_url":"https://x.example/c"}`), `"n-paypal"`, `"n-paypal-2"`, 1), 400},
		{"xendit EUR rejected", strings.Replace(body("xendit", ""), `["USD"]`, `["EUR"]`, 1), 400},
		{"xendit http url rejected", strings.Replace(body("xendit", `,"settings":{"return_url":"http://x.example/r","cancel_url":"https://x.example/c"}`), `"n-xendit"`, `"n-xendit-2"`, 1), 400},
		{"xendit bad country rejected", strings.Replace(body("xendit", `,"settings":{"country":"ZZ"}`), `"n-xendit"`, `"n-xendit-3"`, 1), 400},
		{"xendit expiry under 10 minutes rejected", strings.Replace(body("xendit", `,"settings":{"expiry_minutes":5}`), `"n-xendit"`, `"n-xendit-4"`, 1), 400},
		{"xendit needs api_key", strings.Replace(`{"provider":"xendit","environment":"sandbox","name":"x5","allowed_currencies":["IDR"],"credentials":{}}`, "x5", "x5", 1), 400},
		{"xendit valid with callback token", strings.Replace(body("xendit", `,"settings":{"return_url":"https://x.example/r","cancel_url":"https://x.example/c","country":"ID","expiry_minutes":60},"webhook_secret":{"callback_token":"tok"}`), `["USD"]`, `["IDR","USD"]`, 1), 201},
	}
	for _, c := range cases {
		if r := a.admin("POST", "/configs", c.body); r.code != c.want {
			t.Errorf("%s: got %d %s, want %d", c.name, r.code, r.body, c.want)
		}
	}

	// Partial updates are validated against the config's own provider.
	pp := a.mustStatus(a.admin("POST", "/configs", `{"provider":"paypal","environment":"sandbox","name":"upd",
		"allowed_currencies":["USD"],"credentials":{"client_id":"a","client_secret":"b"}}`), 201)
	id := int64(pp.json["id"].(float64))
	if r := a.admin("PATCH", fmt.Sprintf("/configs/%d", id), `{"allowed_currencies":["IDR"]}`); r.code != 400 {
		t.Errorf("PATCH IDR on PayPal: %d", r.code)
	}
	if r := a.admin("PATCH", fmt.Sprintf("/configs/%d", id), `{"settings":{"return_url":"not a url"}}`); r.code != 400 {
		t.Errorf("PATCH bad url: %d", r.code)
	}
	if r := a.admin("PATCH", fmt.Sprintf("/configs/%d", id), `{"allowed_currencies":["USD","EUR"]}`); r.code != 200 {
		t.Errorf("PATCH EUR on PayPal: %d", r.code)
	}
}

func TestWebhookRouteWithXendit(t *testing.T) {
	srv := xendittest.New("xnd_development_ROUTE-KEY")
	defer srv.Close()
	a := newApp(t, func(reg *provider.Registry) {
		_ = reg.Register(xendit.New(xendit.Options{BaseURL: srv.URL}))
	})

	r := a.mustStatus(a.admin("POST", "/configs", `{"provider":"xendit","environment":"sandbox","name":"route",
		"allowed_currencies":["IDR"],"settings":{"return_url":"https://shop.example/r","cancel_url":"https://shop.example/c"},
		"credentials":{"api_key":"xnd_development_ROUTE-KEY"},"webhook_secret":{"callback_token":"route-token"}}`), 201)
	id := int64(r.json["id"].(float64))
	a.mustStatus(a.admin("PUT", fmt.Sprintf("/configs/%d/grants/%d", id, a.callerID), ""), 204)

	pay := a.mustStatus(a.caller("POST", "/payments", fmt.Sprintf(`{"id":%d,"uniquecode":"HOOK-1","amount":"100000.00","currency":"IDR"}`, id)), 201)
	if !strings.HasPrefix(pay.json["payment_url"].(string), "https://checkout-staging.xendit.co/") {
		t.Fatalf("payment_url %v", pay.json["payment_url"])
	}
	session := strings.TrimPrefix(pay.json["payment_url"].(string), "https://checkout-staging.xendit.co/web/")

	hook := func(body []byte, token string) resp {
		req := httptest.NewRequest(http.MethodPost, "/api/v1.0/webhooks/xendit", strings.NewReader(string(body)))
		if token != "" {
			req.Header.Set("x-callback-token", token)
		}
		w := httptest.NewRecorder()
		a.router.ServeHTTP(w, req)
		out := resp{code: w.Code, body: w.Body.String()}
		return out
	}
	completed := xendittest.SessionEvent("payment_session.completed", session, "COMPLETED")

	if r := hook(completed, "wrong-token"); r.code != 401 || !strings.Contains(r.body, "not_verified") {
		t.Errorf("forged: %d %s", r.code, r.body)
	}
	if st := a.caller("GET", "/status/HOOK-1", ""); st.json["status"] != "PENDING" {
		t.Errorf("a forged webhook changed the status: %s", st.body)
	}

	srv.Complete(session, "IDR", "100000")
	if r := hook(completed, "route-token"); r.code != 200 || !strings.Contains(r.body, `"processed"`) {
		t.Errorf("genuine: %d %s", r.code, r.body)
	}
	if st := a.caller("GET", "/status/HOOK-1", ""); st.json["status"] != "PAID" {
		t.Errorf("status after webhook: %s", st.body)
	}
	if r := hook(completed, "route-token"); r.code != 200 || !strings.Contains(r.body, "duplicate") {
		t.Errorf("redelivery: %d %s", r.code, r.body)
	}

	// Other events: acknowledged (200), unknown provider (404), oversize (413), junk (401).
	if r := hook(xendittest.PaymentEvent("py-1"), "route-token"); r.code != 200 {
		t.Errorf("payment.capture: %d %s", r.code, r.body)
	}
	if r := hook([]byte("junk"), "route-token"); r.code != 401 {
		t.Errorf("junk: %d", r.code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1.0/webhooks/stripe", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	a.router.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Errorf("unknown provider: %d", w.Code)
	}
	big := httptest.NewRequest(http.MethodPost, "/api/v1.0/webhooks/xendit", strings.NewReader(strings.Repeat("x", 300<<10)))
	w = httptest.NewRecorder()
	a.router.ServeHTTP(w, big)
	if w.Code != 413 {
		t.Errorf("oversize: %d", w.Code)
	}
	// The webhook route needs no API key, but the API routes still do.
	if r := a.do("POST", "/api/v1.0/payments", "", `{}`); r.code != 401 {
		t.Errorf("payments without key: %d", r.code)
	}
}

func TestAdminStripeRules(t *testing.T) {
	a := newApp(t)
	base := func(name, extra string) string {
		return `{"provider":"stripe","environment":"sandbox","name":"` + name + `","allowed_currencies":["USD"],"credentials":{"api_key":"sk_test_X"}` + extra + `}`
	}
	cases := []struct {
		name, body string
		want       int
	}{
		{"valid with signing secret", base("s1", `,"webhook_secret":{"signing_secret":"whsec_x"},"settings":{"return_url":"https://x.example/r","cancel_url":"https://x.example/c","expiry_minutes":60}`), 201},
		{"IDR refused until confirmed", strings.Replace(base("s2", ""), `["USD"]`, `["USD","IDR"]`, 1), 400},
		{"expiry under 30 minutes", base("s3", `,"settings":{"expiry_minutes":10}`), 400},
		{"expiry over 24 hours", base("s4", `,"settings":{"expiry_minutes":2000}`), 400},
		{"bad return url", base("s5", `,"settings":{"return_url":"ftp://x"}`), 400},
		{"unknown webhook field", base("s6", `,"webhook_secret":{"callback_token":"x"}`), 400},
		{"missing api_key", `{"provider":"stripe","environment":"sandbox","name":"s7","allowed_currencies":["USD"],"credentials":{}}`, 400},
	}
	for _, c := range cases {
		if r := a.admin("POST", "/configs", c.body); r.code != c.want {
			t.Errorf("%s: got %d %s, want %d", c.name, r.code, r.body, c.want)
		}
	}
}

func TestAdminMidtransRules(t *testing.T) {
	a := newApp(t)
	base := func(name, extra string) string {
		return `{"provider":"midtrans","environment":"sandbox","name":"` + name + `","allowed_currencies":["IDR"],"credentials":{"server_key":"SB-Mid-server-X"}` + extra + `}`
	}
	cases := []struct {
		name, body string
		want       int
	}{
		{"valid", base("m1", `,"settings":{"return_url":"https://x.example/r","cancel_url":"https://x.example/c","expiry_minutes":60}`), 201},
		{"USD refused", strings.Replace(base("m2", ""), `["IDR"]`, `["IDR","USD"]`, 1), 400},
		{"webhook secret not accepted", base("m3", `,"webhook_secret":{"signing_secret":"x"}`), 400},
		{"missing server_key", `{"provider":"midtrans","environment":"sandbox","name":"m4","allowed_currencies":["IDR"],"credentials":{}}`, 400},
		{"expiry over 7 days", base("m5", `,"settings":{"expiry_minutes":20000}`), 400},
		{"bad url", base("m6", `,"settings":{"return_url":"javascript:x"}`), 400},
		{"production environment", strings.Replace(base("m7", ""), `"sandbox"`, `"production"`, 1), 201},
	}
	for _, c := range cases {
		if r := a.admin("POST", "/configs", c.body); r.code != c.want {
			t.Errorf("%s: got %d %s, want %d", c.name, r.code, r.body, c.want)
		}
	}
}
