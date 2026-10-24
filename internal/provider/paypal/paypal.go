// Package paypal is the PayPal Orders v2 adapter. Behaviour follows PayPal's
// official documentation as reviewed in docs/paypal-research.md; points the
// documentation does not settle are marked there and covered by sandbox checks.
package paypal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
)

// Name is the value stored in provider_configs.provider.
const Name = "paypal"

// Official base URLs (api/rest/requests).
const (
	SandboxBase = "https://api-m.sandbox.paypal.com"
	LiveBase    = "https://api-m.paypal.com"
)

const (
	maxResponseBytes = 1 << 20
	tokenSafety      = 60 * time.Second
	defaultExpiryMin = 24 * 60
)

// Options customises the adapter. The zero value talks to PayPal.
type Options struct {
	// SandboxBase and LiveBase override the base URLs (tests point them at a stub).
	SandboxBase, LiveBase string
	HTTPClient            *http.Client
	Now                   func() time.Time
}

// Adapter implements provider.Provider, Capturer, Locator and AmountValidator.
type Adapter struct {
	opt    Options
	mu     sync.Mutex
	tokens map[string]token
}

type token struct {
	value   string
	expires time.Time
}

var (
	_ provider.Provider        = (*Adapter)(nil)
	_ provider.Capturer        = (*Adapter)(nil)
	_ provider.Locator         = (*Adapter)(nil)
	_ provider.AmountValidator = (*Adapter)(nil)
)

// New builds the adapter.
func New(opt Options) *Adapter {
	if opt.SandboxBase == "" {
		opt.SandboxBase = SandboxBase
	}
	if opt.LiveBase == "" {
		opt.LiveBase = LiveBase
	}
	if opt.HTTPClient == nil {
		opt.HTTPClient = &http.Client{Timeout: 25 * time.Second}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Adapter{opt: opt, tokens: map[string]token{}}
}

func (a *Adapter) Name() string { return Name }

// supportedCurrencies is PayPal's list for payments (paypal-supported-currencies
// and REST currency-codes pages). IDR is not on it.
var supportedCurrencies = map[string]bool{
	"AUD": true, "BRL": true, "CAD": true, "CNY": true, "CZK": true, "DKK": true, "EUR": true,
	"HKD": true, "HUF": true, "ILS": true, "JPY": true, "MYR": true, "MXN": true, "TWD": true,
	"NZD": true, "NOK": true, "PHP": true, "PLN": true, "GBP": true, "SGD": true, "SEK": true,
	"CHF": true, "THB": true, "USD": true, "RUB": true,
}

// zeroDecimal currencies reject any decimals ("an error occurs").
var zeroDecimal = map[string]bool{"HUF": true, "JPY": true, "TWD": true}

// SupportsCurrency reports whether PayPal takes payments in the currency.
func SupportsCurrency(code string) bool { return supportedCurrencies[code] }

// ValidateAmount implements provider.AmountValidator.
func (a *Adapter) ValidateAmount(amt domain.Amount) error {
	cur := amt.Currency()
	if !SupportsCurrency(cur) {
		return fmt.Errorf("PayPal does not support %s", cur)
	}
	if zeroDecimal[cur] {
		if _, frac, has := strings.Cut(amt.String(), "."); has && strings.Trim(frac, "0") != "" {
			return fmt.Errorf("%s has no decimals on PayPal", cur)
		}
	}
	return nil
}

// formatValue renders the amount the way PayPal wants it for the currency.
func formatValue(amt domain.Amount) string {
	if zeroDecimal[amt.Currency()] {
		whole, _, _ := strings.Cut(amt.String(), ".")
		return whole
	}
	return amt.String()
}

func (a *Adapter) base(env string) string {
	if env == "production" {
		return a.opt.LiveBase
	}
	return a.opt.SandboxBase
}

// ---- HTTP plumbing ---------------------------------------------------------

func (a *Adapter) tokenFor(ctx context.Context, env string, cred provider.Credentials) (string, error) {
	id, secret := cred["client_id"].Reveal(), cred["client_secret"].Reveal()
	if id == "" || secret == "" {
		return "", &provider.Error{Code: "credentials_incomplete"}
	}
	sum := sha256.Sum256([]byte(secret))
	key := env + "|" + id + "|" + hex.EncodeToString(sum[:8])

	a.mu.Lock()
	t, ok := a.tokens[key]
	a.mu.Unlock()
	if ok && a.opt.Now().Before(t.expires) {
		return t.value, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base(env)+"/v1/oauth2/token",
		strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(id, secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	status, body, err := a.send(req)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		if status >= 500 || status == http.StatusTooManyRequests {
			return "", fmt.Errorf("%w: token http %d", provider.ErrTimeout, status)
		}
		return "", &provider.Error{Code: "invalid_client"} // bad credentials; never echo the body
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return "", &provider.Error{Code: "token_response_invalid"}
	}
	ttl := time.Duration(tr.ExpiresIn)*time.Second - tokenSafety
	if ttl < time.Second {
		ttl = time.Second
	}
	a.mu.Lock()
	a.tokens[key] = token{value: tr.AccessToken, expires: a.opt.Now().Add(ttl)}
	a.mu.Unlock()
	return tr.AccessToken, nil
}

func (a *Adapter) forget(env string, cred provider.Credentials) {
	id, secret := cred["client_id"].Reveal(), cred["client_secret"].Reveal()
	sum := sha256.Sum256([]byte(secret))
	a.mu.Lock()
	delete(a.tokens, env+"|"+id+"|"+hex.EncodeToString(sum[:8]))
	a.mu.Unlock()
}

// send performs a request. Transport failures, timeouts and cancellations all
// mean "outcome unknown" and are reported as provider.ErrTimeout.
func (a *Adapter) send(req *http.Request) (int, []byte, error) {
	resp, err := a.opt.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %s", provider.ErrTimeout, errKind(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: reading response", provider.ErrTimeout)
	}
	return resp.StatusCode, body, nil
}

func errKind(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Timeout() {
		return "timeout"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "transport error"
}

// api calls an authenticated endpoint, refreshing the token once on a 401.
func (a *Adapter) api(ctx context.Context, env string, cred provider.Credentials, method, path string,
	headers map[string]string, payload []byte) (int, []byte, error) {

	for attempt := 0; attempt < 2; attempt++ {
		tok, err := a.tokenFor(ctx, env, cred)
		if err != nil {
			return 0, nil, err
		}
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, a.base(env)+path, rdr)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		status, body, err := a.send(req)
		if err != nil {
			return 0, nil, err
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			a.forget(env, cred)
			continue
		}
		return status, body, nil
	}
	return 0, nil, &provider.Error{Code: "unauthorized"}
}

// classify turns a non-2xx response into an adapter error: transient failures
// (429, 5xx) mean the outcome may be unknown; other 4xx are definite rejections.
func classify(status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 {
		return fmt.Errorf("%w: http %d", provider.ErrTimeout, status)
	}
	var pe struct {
		Name    string `json:"name"`
		DebugID string `json:"debug_id"`
		Details []struct {
			Issue string `json:"issue"`
		} `json:"details"`
	}
	_ = json.Unmarshal(body, &pe)
	code := pe.Name
	if len(pe.Details) > 0 && pe.Details[0].Issue != "" {
		code = pe.Name + "." + pe.Details[0].Issue
	}
	return &provider.Error{Code: sanitizeCode(code, status), DebugID: sanitizeCode(pe.DebugID, 0)}
}

// sanitizeCode keeps only characters that are safe to log and show.
func sanitizeCode(s string, status int) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' {
			b.WriteRune(r)
		}
		if b.Len() >= 80 {
			break
		}
	}
	if b.Len() == 0 && status != 0 {
		return fmt.Sprintf("http_%d", status)
	}
	return b.String()
}

// ---- payments --------------------------------------------------------------

type link struct {
	Href string `json:"href"`
	Rel  string `json:"rel"`
}

type amountJSON struct {
	CurrencyCode string `json:"currency_code"`
	Value        string `json:"value"`
}

type captureJSON struct {
	ID     string      `json:"id"`
	Status string      `json:"status"`
	Amount *amountJSON `json:"amount"`
}

type orderJSON struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	Links         []link `json:"links"`
	PurchaseUnits []struct {
		Payments struct {
			Captures []captureJSON `json:"captures"`
		} `json:"payments"`
	} `json:"purchase_units"`
}

func (o orderJSON) firstCapture() *captureJSON {
	for _, pu := range o.PurchaseUnits {
		if len(pu.Payments.Captures) > 0 {
			return &pu.Payments.Captures[0]
		}
	}
	return nil
}

// CreatePayment creates an Orders v2 order for a redirect flow.
func (a *Adapter) CreatePayment(ctx context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
	returnURL, cancelURL := strSetting(req.Settings, "return_url"), strSetting(req.Settings, "cancel_url")
	if !validURL(returnURL) || !validURL(cancelURL) {
		return provider.CreateResult{}, &provider.Error{Code: "config_incomplete_return_urls"}
	}
	if err := a.ValidateAmount(req.Amount); err != nil {
		return provider.CreateResult{}, &provider.Error{Code: "amount_not_accepted"}
	}
	payload, err := json.Marshal(map[string]any{
		"intent": "CAPTURE",
		"purchase_units": []any{map[string]any{
			"custom_id":  req.Reference,
			"invoice_id": req.Reference,
			"amount":     amountJSON{CurrencyCode: req.Amount.Currency(), Value: formatValue(req.Amount)},
		}},
		"payment_source": map[string]any{"paypal": map[string]any{"experience_context": map[string]any{
			"return_url":          returnURL,
			"cancel_url":          cancelURL,
			"user_action":         "PAY_NOW",
			"landing_page":        "LOGIN",
			"shipping_preference": "NO_SHIPPING",
		}}},
	})
	if err != nil {
		return provider.CreateResult{}, err
	}
	status, body, err := a.api(ctx, req.Environment, req.Credentials, http.MethodPost, "/v2/checkout/orders",
		map[string]string{"PayPal-Request-Id": req.IdempotencyKey, "Prefer": "return=representation"}, payload)
	if err != nil {
		return provider.CreateResult{}, err
	}
	if err := classify(status, body); err != nil {
		return provider.CreateResult{}, err
	}
	var o orderJSON
	if err := json.Unmarshal(body, &o); err != nil || o.ID == "" {
		return provider.CreateResult{}, &provider.Error{Code: "order_response_invalid"}
	}
	var approve string
	for _, l := range o.Links {
		if l.Rel == "payer-action" || (l.Rel == "approve" && approve == "") {
			approve = l.Href
		}
	}
	if !validURL(approve) {
		return provider.CreateResult{}, &provider.Error{Code: "no_approval_link"}
	}
	exp := a.opt.Now().Add(time.Duration(intSetting(req.Settings, "expiry_minutes", defaultExpiryMin)) * time.Minute).UTC()
	return provider.CreateResult{ProviderRef: o.ID, PaymentURL: approve, ExpiresAt: &exp, RawStatus: o.Status}, nil
}

// GetStatus reads the order and its first capture.
func (a *Adapter) GetStatus(ctx context.Context, req provider.StatusRequest) (provider.StatusResult, error) {
	status, body, err := a.api(ctx, req.Environment, req.Credentials, http.MethodGet,
		"/v2/checkout/orders/"+url.PathEscape(req.ProviderRef), nil, nil)
	if err != nil {
		return provider.StatusResult{}, err
	}
	if err := classify(status, body); err != nil {
		return provider.StatusResult{}, err
	}
	var o orderJSON
	if err := json.Unmarshal(body, &o); err != nil {
		return provider.StatusResult{}, &provider.Error{Code: "order_response_invalid"}
	}
	if c := o.firstCapture(); c != nil {
		st, amt := mapCapture(c)
		return provider.StatusResult{Status: st, RawStatus: c.Status, Amount: amt}, nil
	}
	st := domain.StatusPending
	if o.Status == "VOIDED" {
		st = domain.StatusCancelled
	}
	return provider.StatusResult{Status: st, RawStatus: o.Status}, nil
}

// Capture moves the money for an approved order. "Paid" is reported only when
// PayPal says the capture is COMPLETED; HTTP 201 alone proves nothing, because
// PayPal answers 201 with a DECLINED capture too.
func (a *Adapter) Capture(ctx context.Context, req provider.CaptureRequest) (provider.CaptureResult, error) {
	status, body, err := a.api(ctx, req.Environment, req.Credentials, http.MethodPost,
		"/v2/checkout/orders/"+url.PathEscape(req.ProviderRef)+"/capture",
		map[string]string{"PayPal-Request-Id": req.IdempotencyKey, "Prefer": "return=representation"}, []byte("{}"))
	if err != nil {
		return provider.CaptureResult{}, err
	}
	if status == http.StatusUnprocessableEntity {
		// Possibly already captured (the error name is not documented): read the order instead.
		return a.captureFromOrder(ctx, req)
	}
	if err := classify(status, body); err != nil {
		return provider.CaptureResult{}, err
	}
	var o orderJSON
	if err := json.Unmarshal(body, &o); err != nil {
		return provider.CaptureResult{}, &provider.Error{Code: "capture_response_invalid"}
	}
	c := o.firstCapture()
	if c == nil {
		return provider.CaptureResult{Status: domain.StatusPending, RawStatus: o.Status}, nil
	}
	st, amt := mapCapture(c)
	return provider.CaptureResult{CaptureID: c.ID, Status: st, RawStatus: c.Status, Amount: amt}, nil
}

func (a *Adapter) captureFromOrder(ctx context.Context, req provider.CaptureRequest) (provider.CaptureResult, error) {
	status, body, err := a.api(ctx, req.Environment, req.Credentials, http.MethodGet,
		"/v2/checkout/orders/"+url.PathEscape(req.ProviderRef), nil, nil)
	if err != nil {
		return provider.CaptureResult{}, err
	}
	if err := classify(status, body); err != nil {
		return provider.CaptureResult{}, err
	}
	var o orderJSON
	if err := json.Unmarshal(body, &o); err != nil {
		return provider.CaptureResult{}, &provider.Error{Code: "order_response_invalid"}
	}
	c := o.firstCapture()
	if c == nil {
		return provider.CaptureResult{}, &provider.Error{Code: "capture_rejected"}
	}
	st, amt := mapCapture(c)
	return provider.CaptureResult{CaptureID: c.ID, Status: st, RawStatus: c.Status, Amount: amt}, nil
}

func mapCapture(c *captureJSON) (domain.Status, *domain.Amount) {
	var amt *domain.Amount
	if c.Amount != nil {
		if parsed, err := domain.ParseAmount(c.Amount.Value, c.Amount.CurrencyCode); err == nil {
			amt = &parsed
		}
	}
	switch c.Status {
	case "COMPLETED":
		return domain.StatusPaid, amt
	case "DECLINED", "FAILED", "DENIED":
		return domain.StatusFailed, amt
	default: // PENDING and anything unknown never count as paid
		return domain.StatusPending, amt
	}
}

// ---- settings helpers ------------------------------------------------------

func strSetting(s map[string]any, key string) string {
	v, _ := s[key].(string)
	return strings.TrimSpace(v)
}

func intSetting(s map[string]any, key string, def int) int {
	switch v := s[key].(type) {
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	}
	return def
}

func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && len(raw) <= 500
}

// ValidSettingURL is used by the admin API to validate return/cancel URLs.
func ValidSettingURL(raw string) bool { return validURL(raw) }
