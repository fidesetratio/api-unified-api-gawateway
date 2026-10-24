package api_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"unifiedgw/internal/api"
	"unifiedgw/internal/auth"
	"unifiedgw/internal/provider"
	"unifiedgw/internal/provider/fake"
	"unifiedgw/internal/secrets"
	"unifiedgw/internal/service"
	"unifiedgw/internal/store"
	"unifiedgw/internal/testutil"
)

const (
	pwSecret  = "PAYPAL-SECRET-DO-NOT-LEAK"
	pwSecret2 = "ROTATED-SECRET-DO-NOT-LEAK"
	whSecret  = "WEBHOOK-ID-DO-NOT-LEAK"
)

type app struct {
	t         *testing.T
	router    *gin.Engine
	logs      *bytes.Buffer
	pool      interface{ Close() }
	adminKey  string
	callerKey string
	callerID  int64
	clients   *store.Clients
	fake      *fake.Provider
	query     func(sql string, args ...any) string
}

func newApp(t *testing.T, customize ...func(*provider.Registry)) *app {
	t.Helper()
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()

	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	keys, err := secrets.ParseKeyRing("k1:"+base64.StdEncoding.EncodeToString(raw), "k1")
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	fk := fake.New()
	reg := provider.NewRegistry()
	_ = reg.Register(fk)
	if len(customize) == 0 {
		_ = reg.Register(provider.NewStub("paypal"))
		_ = reg.Register(provider.NewStub("xendit"))
	}
	for _, c := range customize {
		c(reg)
	}

	configs, clients, payments := store.NewConfigs(pool), store.NewClients(pool), store.NewPayments(pool)
	paySvc := service.NewPayments(payments, configs, clients, keys, reg, log, service.Options{})
	adminSvc := service.NewAdmin(configs, clients, keys, true)

	_, adminKey, err := clients.Create(ctx, "root-admin", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	caller, callerKey, err := clients.Create(ctx, "shop-1", auth.RoleCaller)
	if err != nil {
		t.Fatal(err)
	}
	r := api.NewRouter(api.Deps{
		Log: log, Auth: auth.New(clients), Payments: paySvc, Admin: adminSvc,
		Settlement: service.NewSettlement(payments, configs, keys, reg, log),
		Ready:      func(c context.Context) error { return pool.Ping(c) },
	})
	a := &app{t: t, router: r, logs: logs, adminKey: adminKey, callerKey: callerKey, callerID: caller.ID, clients: clients, fake: fk}
	a.query = func(sql string, args ...any) string {
		var s string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("query: %v", err)
		}
		return s
	}
	return a
}

type resp struct {
	code int
	body string
	json map[string]any
}

func (a *app) do(method, path, key string, body string, hdr ...string) resp {
	a.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	a.router.ServeHTTP(w, req)
	r := resp{code: w.Code, body: w.Body.String()}
	_ = json.Unmarshal(w.Body.Bytes(), &r.json)
	return r
}

func (a *app) admin(method, path, body string) resp {
	return a.do(method, "/api/v1.0/admin"+path, a.adminKey, body, "X-Forwarded-Proto", "https")
}

func (a *app) caller(method, path, body string) resp {
	return a.do(method, "/api/v1.0"+path, a.callerKey, body)
}

func errCode(r resp) string {
	e, _ := r.json["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func (a *app) mustStatus(r resp, want int) resp {
	a.t.Helper()
	if r.code != want {
		a.t.Fatalf("status = %d, want %d, body %s", r.code, want, r.body)
	}
	return r
}

// setupFakeConfig creates a fake provider config through the admin API and grants the caller.
func (a *app) setupFakeConfig() int64 {
	a.t.Helper()
	r := a.mustStatus(a.admin("POST", "/configs", `{"provider":"fake","environment":"sandbox","name":"Fake A",
		"allowed_currencies":["USD","IDR"],"credentials":{"client_secret":"`+pwSecret+`"}}`), 201)
	id := int64(r.json["id"].(float64))
	a.mustStatus(a.admin("PUT", fmt.Sprintf("/configs/%d/grants/%d", id, a.callerID), ""), 204)
	return id
}

func TestHealth(t *testing.T) {
	a := newApp(t)
	if r := a.do("GET", "/healthz", "", ""); r.code != 200 {
		t.Errorf("healthz %d", r.code)
	}
	if r := a.do("GET", "/readyz", "", ""); r.code != 200 {
		t.Errorf("readyz %d", r.code)
	}
}

func TestAuthenticationMatrix(t *testing.T) {
	a := newApp(t)
	body := `{"id":1,"uniquecode":"A","amount":"1.00","currency":"USD"}`

	for name, r := range map[string]resp{
		"no header":      a.do("POST", "/api/v1.0/payments", "", body),
		"garbage token":  a.do("POST", "/api/v1.0/payments", "garbage", body),
		"unknown key":    a.do("POST", "/api/v1.0/payments", "gw_0123456789abcdef_"+strings.Repeat("a", 64), body),
		"status no auth": a.do("GET", "/api/v1.0/status/A", "", ""),
	} {
		if r.code != 401 || errCode(r) != "unauthorized" {
			t.Errorf("%s: %d %s", name, r.code, r.body)
		}
	}
	// Uniform message: a bad key is indistinguishable from an unknown one.
	if x, y := a.do("GET", "/api/v1.0/status/A", "garbage", ""), a.do("GET", "/api/v1.0/status/A", "gw_0123456789abcdef_"+strings.Repeat("a", 64), ""); x.body != y.body {
		t.Errorf("distinguishable 401 bodies: %s vs %s", x.body, y.body)
	}

	// Roles are separate: admin keys cannot pay, caller keys cannot administer.
	if r := a.do("POST", "/api/v1.0/payments", a.adminKey, body); r.code != 403 {
		t.Errorf("admin key on /payments: %d", r.code)
	}
	if r := a.do("GET", "/api/v1.0/admin/configs", a.callerKey, "", "X-Forwarded-Proto", "https"); r.code != 403 {
		t.Errorf("caller key on /admin: %d", r.code)
	}
	if r := a.do("GET", "/api/v1.0/admin/configs", "", "", "X-Forwarded-Proto", "https"); r.code != 401 {
		t.Errorf("no key on /admin: %d", r.code)
	}

	// Disabled client loses access immediately.
	if err := a.clients.SetStatus(context.Background(), "shop-1", "disabled"); err != nil {
		t.Fatal(err)
	}
	if r := a.caller("GET", "/status/A", ""); r.code != 401 {
		t.Errorf("disabled client: %d", r.code)
	}
}

func TestCreatePaymentContract(t *testing.T) {
	a := newApp(t)
	id := a.setupFakeConfig()
	req := fmt.Sprintf(`{"id":%d,"uniquecode":"ORDER-10001","amount":"10.00","currency":"USD"}`, id)

	r := a.mustStatus(a.caller("POST", "/payments", req), 201)
	if len(r.json) != 3 || r.json["uniquecode"] != "ORDER-10001" || r.json["status"] != "PENDING" ||
		!strings.HasPrefix(r.json["payment_url"].(string), "https://fake.invalid/pay/") {
		t.Fatalf("response must be exactly uniquecode/status/payment_url: %s", r.body)
	}

	// An identical repeat is a 200 with the same body and no second provider call.
	again := a.mustStatus(a.caller("POST", "/payments", req), 200)
	if again.body != r.body {
		t.Errorf("replay differs: %s vs %s", again.body, r.body)
	}
	if a.fake.CreateCalls() != 1 {
		t.Errorf("provider calls = %d", a.fake.CreateCalls())
	}

	st := a.mustStatus(a.caller("GET", "/status/ORDER-10001", ""), 200)
	if st.body != r.body {
		t.Errorf("status body %s vs %s", st.body, r.body)
	}

	conflict := a.caller("POST", "/payments", fmt.Sprintf(`{"id":%d,"uniquecode":"ORDER-10001","amount":"11.00","currency":"USD"}`, id))
	if conflict.code != 409 || errCode(conflict) != "uniquecode_conflict" {
		t.Errorf("conflict: %d %s", conflict.code, conflict.body)
	}
	if nf := a.caller("GET", "/status/NOPE", ""); nf.code != 404 || errCode(nf) != "payment_not_found" {
		t.Errorf("unknown code: %d %s", nf.code, nf.body)
	}

	// IDR sample from the design: decimals equal to zero are accepted.
	idr := a.mustStatus(a.caller("POST", "/payments", fmt.Sprintf(`{"id":%d,"uniquecode":"ORDER-10002","amount":"100000.00","currency":"IDR"}`, id)), 201)
	if idr.json["status"] != "PENDING" {
		t.Error(idr.body)
	}
}

func TestCreatePaymentErrors(t *testing.T) {
	a := newApp(t)
	id := a.setupFakeConfig()
	ok := func(extra string) string {
		return fmt.Sprintf(`{"id":%d,"uniquecode":"E1","amount":"1.00","currency":"USD"%s}`, id, extra)
	}
	cases := []struct {
		name, body string
		code       int
		err        string
	}{
		{"empty body", ``, 400, "invalid_request"},
		{"not json", `hello`, 400, "invalid_request"},
		{"unknown field", ok(`,"extra":1`), 400, "invalid_request"},
		{"amount as number", fmt.Sprintf(`{"id":%d,"uniquecode":"E1","amount":1.5,"currency":"USD"}`, id), 400, "invalid_request"},
		{"id as string", `{"id":"5","uniquecode":"E1","amount":"1.00","currency":"USD"}`, 400, "invalid_request"},
		{"trailing data", ok(``) + `{}`, 400, "invalid_request"},
		{"array", `[1]`, 400, "invalid_request"},
		{"zero id", `{"id":0,"uniquecode":"E1","amount":"1.00","currency":"USD"}`, 400, "invalid_request"},
		{"bad amount", fmt.Sprintf(`{"id":%d,"uniquecode":"E1","amount":"-5","currency":"USD"}`, id), 400, "invalid_request"},
		{"lowercase currency", fmt.Sprintf(`{"id":%d,"uniquecode":"E1","amount":"1.00","currency":"usd"}`, id), 400, "invalid_request"},
		{"not granted / unknown id", `{"id":99999,"uniquecode":"E1","amount":"1.00","currency":"USD"}`, 403, "config_not_permitted"},
		{"currency not allowed", fmt.Sprintf(`{"id":%d,"uniquecode":"E1","amount":"1.00","currency":"EUR"}`, id), 422, "currency_not_supported"},
		{"too large", fmt.Sprintf(`{"id":%d,"uniquecode":"E1","amount":"1.00","currency":"USD","pad":"%s"}`, id, strings.Repeat("x", 70000)), 413, "payload_too_large"},
	}
	for _, c := range cases {
		r := a.caller("POST", "/payments", c.body)
		if r.code != c.code || errCode(r) != c.err {
			t.Errorf("%s: got %d %s, want %d %s", c.name, r.code, r.body, c.code, c.err)
		}
	}
	if a.fake.CreateCalls() != 0 {
		t.Error("an invalid request reached the provider")
	}
}

func TestPaypalConfigCanBeStoredButPaymentsReportProviderError(t *testing.T) {
	a := newApp(t)
	r := a.mustStatus(a.admin("POST", "/configs", `{"provider":"paypal","environment":"sandbox","name":"PayPal sandbox A",
		"allowed_currencies":["USD"],"credentials":{"client_id":"abc","client_secret":"`+pwSecret+`"},
		"webhook_secret":{"webhook_id":"`+whSecret+`"}}`), 201)
	id := int64(r.json["id"].(float64))
	if r.json["has_webhook_secret"] != true || r.json["credentials_version"] != float64(1) || r.json["provider"] != "paypal" {
		t.Errorf("response: %s", r.body)
	}
	a.mustStatus(a.admin("PUT", fmt.Sprintf("/configs/%d/grants/%d", id, a.callerID), ""), 204)

	pay := a.caller("POST", "/payments", fmt.Sprintf(`{"id":%d,"uniquecode":"PP-1","amount":"10.00","currency":"USD"}`, id))
	if pay.code != 502 || errCode(pay) != "provider_error" {
		t.Errorf("got %d %s", pay.code, pay.body)
	}
}

func TestAdminValidation(t *testing.T) {
	a := newApp(t)
	cases := []struct{ name, body string }{
		{"unknown provider", `{"provider":"stripe","environment":"sandbox","name":"x","allowed_currencies":["USD"],"credentials":{}}`},
		{"bad environment", `{"provider":"paypal","environment":"prod","name":"x","allowed_currencies":["USD"],"credentials":{"client_id":"a","client_secret":"` + pwSecret + `"}}`},
		{"missing client_secret", `{"provider":"paypal","environment":"sandbox","name":"x","allowed_currencies":["USD"],"credentials":{"client_id":"a"}}`},
		{"extra credential field", `{"provider":"paypal","environment":"sandbox","name":"x","allowed_currencies":["USD"],"credentials":{"client_id":"a","client_secret":"` + pwSecret + `","password":"p"}}`},
		{"empty currencies", `{"provider":"paypal","environment":"sandbox","name":"x","allowed_currencies":[],"credentials":{"client_id":"a","client_secret":"` + pwSecret + `"}}`},
		{"bad currency", `{"provider":"paypal","environment":"sandbox","name":"x","allowed_currencies":["usd"],"credentials":{"client_id":"a","client_secret":"` + pwSecret + `"}}`},
		{"blank name", `{"provider":"paypal","environment":"sandbox","name":" ","allowed_currencies":["USD"],"credentials":{"client_id":"a","client_secret":"` + pwSecret + `"}}`},
		{"unknown top-level field", `{"provider":"paypal","environment":"sandbox","name":"x","allowed_currencies":["USD"],"credentials":{"client_id":"a","client_secret":"` + pwSecret + `"},"id":1}`},
		{"xendit field on paypal", `{"provider":"paypal","environment":"sandbox","name":"x","allowed_currencies":["USD"],"credentials":{"api_key":"` + pwSecret + `"}}`},
	}
	for _, c := range cases {
		r := a.admin("POST", "/configs", c.body)
		if r.code != 400 || errCode(r) != "invalid_request" {
			t.Errorf("%s: %d %s", c.name, r.code, r.body)
		}
		if strings.Contains(r.body, pwSecret) {
			t.Errorf("%s: error response echoed a secret: %s", c.name, r.body)
		}
	}
	if n := a.query(`SELECT count(*)::text FROM provider_configs`); n != "0" {
		t.Errorf("invalid requests stored %s configs", n)
	}
}

func TestAdminRequiresTLS(t *testing.T) {
	a := newApp(t)
	r := a.do("GET", "/api/v1.0/admin/configs", a.adminKey, "")
	if r.code != 403 || errCode(r) != "tls_required" {
		t.Errorf("plain http: %d %s", r.code, r.body)
	}
	if r := a.do("GET", "/api/v1.0/admin/configs", a.adminKey, "", "X-Forwarded-Proto", "https"); r.code != 200 {
		t.Errorf("https: %d %s", r.code, r.body)
	}
}

func TestAdminLifecycleAndGrants(t *testing.T) {
	a := newApp(t)
	id := a.setupFakeConfig()
	path := fmt.Sprintf("/configs/%d", id)

	got := a.mustStatus(a.admin("GET", path, ""), 200)
	if got.json["credentials_version"] != float64(1) || got.json["name"] != "Fake A" {
		t.Errorf("get: %s", got.body)
	}
	list := a.mustStatus(a.admin("GET", "/configs", ""), 200)
	if cs, _ := list.json["configs"].([]any); len(cs) != 1 {
		t.Errorf("list: %s", list.body)
	}

	rot := a.mustStatus(a.admin("PUT", path+"/credentials", `{"credentials":{"client_secret":"`+pwSecret2+`"}}`), 200)
	if rot.json["credentials_version"] != float64(2) {
		t.Errorf("rotate: %s", rot.body)
	}
	if v := a.query(`SELECT count(*)::text FROM provider_credentials WHERE provider_config_id=$1 AND status='active'`, id); v != "1" {
		t.Errorf("active versions = %s", v)
	}

	// Disabling the config makes its id answer 404 for payments.
	a.mustStatus(a.admin("PATCH", path, `{"status":"disabled"}`), 200)
	pay := a.caller("POST", "/payments", fmt.Sprintf(`{"id":%d,"uniquecode":"D1","amount":"1.00","currency":"USD"}`, id))
	if pay.code != 404 || errCode(pay) != "config_not_found" {
		t.Errorf("disabled config: %d %s", pay.code, pay.body)
	}
	a.mustStatus(a.admin("PATCH", path, `{"status":"active","allowed_currencies":["USD","EUR"],"name":"Fake A2"}`), 200)
	a.mustStatus(a.caller("POST", "/payments", fmt.Sprintf(`{"id":%d,"uniquecode":"D2","amount":"1.00","currency":"EUR"}`, id)), 201)

	// Revoking the grant stops the caller.
	a.mustStatus(a.admin("DELETE", fmt.Sprintf("%s/grants/%d", path, a.callerID), ""), 204)
	if r := a.caller("POST", "/payments", fmt.Sprintf(`{"id":%d,"uniquecode":"D3","amount":"1.00","currency":"USD"}`, id)); r.code != 403 {
		t.Errorf("after revoke: %d", r.code)
	}

	// Grant targets are validated.
	if r := a.admin("PUT", fmt.Sprintf("%s/grants/%d", path, 99999), ""); r.code != 404 || errCode(r) != "client_not_found" {
		t.Errorf("unknown client: %d %s", r.code, r.body)
	}
	if r := a.admin("PUT", "/configs/99999/grants/1", ""); r.code != 404 {
		t.Errorf("unknown config: %d", r.code)
	}
	if r := a.admin("GET", "/configs/abc", ""); r.code != 400 {
		t.Errorf("bad id: %d", r.code)
	}
	if r := a.admin("GET", "/configs/99999", ""); r.code != 404 {
		t.Errorf("missing config: %d", r.code)
	}
	adminID := a.query(`SELECT id::text FROM api_clients WHERE name='root-admin'`)
	if r := a.admin("PUT", fmt.Sprintf("%s/grants/%s", path, adminID), ""); r.code != 400 {
		t.Errorf("granting to an admin client: %d", r.code)
	}

	// Every mutation is audited with the admin as actor.
	if n := a.query(`SELECT count(*)::text FROM audit_log WHERE actor_type='admin' AND actor_id LIKE 'root-admin#%'`); n == "0" {
		t.Error("no audit rows for admin actions")
	}
	for _, action := range []string{"config.create", "config.rotate_credentials", "config.update", "grant.add", "grant.remove"} {
		if n := a.query(`SELECT count(*)::text FROM audit_log WHERE action=$1`, action); n == "0" {
			t.Errorf("no audit row for %s", action)
		}
	}
}

// Secrets are write-only: they never come back, never reach logs or the audit trail.
func TestSecretsNeverLeak(t *testing.T) {
	a := newApp(t)
	r := a.mustStatus(a.admin("POST", "/configs", `{"provider":"paypal","environment":"sandbox","name":"P",
		"allowed_currencies":["USD"],"credentials":{"client_id":"id-1","client_secret":"`+pwSecret+`"},
		"webhook_secret":{"webhook_id":"`+whSecret+`"}}`), 201)
	id := int64(r.json["id"].(float64))
	a.mustStatus(a.admin("PUT", fmt.Sprintf("/configs/%d/credentials", id), `{"credentials":{"client_id":"id-2","client_secret":"`+pwSecret2+`"}}`), 200)

	var seen []string
	seen = append(seen, r.body)
	seen = append(seen, a.admin("GET", fmt.Sprintf("/configs/%d", id), "").body)
	seen = append(seen, a.admin("GET", "/configs", "").body)
	seen = append(seen, a.query(`SELECT coalesce(string_agg(details::text, ' '), '') FROM audit_log`))
	seen = append(seen, a.logs.String())
	// Failure paths: bad requests and panics must not echo bodies either.
	seen = append(seen, a.admin("POST", "/configs", `{"provider":"paypal","bogus":"`+pwSecret+`"}`).body)
	seen = append(seen, a.admin("PUT", fmt.Sprintf("/configs/%d/credentials", id), `{"credentials":{"nope":"`+pwSecret+`"}}`).body)

	for i, s := range seen {
		for _, secret := range []string{pwSecret, pwSecret2, whSecret} {
			if strings.Contains(s, secret) {
				t.Errorf("output #%d contains secret %q", i, secret)
			}
		}
	}
	// Ciphertext in the database must not contain them either.
	for _, secret := range []string{pwSecret, pwSecret2, whSecret} {
		if n := a.query(`SELECT count(*)::text FROM provider_credentials WHERE position($1::bytea in ciphertext) > 0 OR position($1::bytea in coalesce(webhook_ciphertext,''::bytea)) > 0`, []byte(secret)); n != "0" {
			t.Errorf("plaintext %q found in database", secret)
		}
	}
}
