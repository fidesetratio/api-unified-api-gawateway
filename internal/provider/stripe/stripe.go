// Package stripe is the Stripe Checkout Sessions adapter (hosted page, payment
// mode). Behaviour follows Stripe's documentation as reviewed in
// docs/stripe-research.md. Webhooks are authenticated with Stripe's HMAC
// signature, and a completed session is still read back from the API before it
// can count as paid.
package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
)

// Name is the value stored in provider_configs.provider.
const Name = "stripe"

// BaseURL is Stripe's API host (test and live share it; the key decides).
const BaseURL = "https://api.stripe.com"

const (
	maxResponseBytes = 1 << 20
	minExpiryMin     = 30      // Stripe: expires_at is 30 minutes to 24 hours after creation
	maxExpiryMin     = 24 * 60 //
	defaultExpiryMin = maxExpiryMin
)

// Options customises the adapter. The zero value talks to Stripe.
type Options struct {
	BaseURL    string
	HTTPClient *http.Client
	Now        func() time.Time
}

// Adapter implements provider.Provider, Locator and AmountValidator.
type Adapter struct{ opt Options }

var (
	_ provider.Provider        = (*Adapter)(nil)
	_ provider.Locator         = (*Adapter)(nil)
	_ provider.AmountValidator = (*Adapter)(nil)
)

// New builds the adapter.
func New(opt Options) *Adapter {
	if opt.BaseURL == "" {
		opt.BaseURL = BaseURL
	}
	if opt.HTTPClient == nil {
		opt.HTTPClient = &http.Client{Timeout: 25 * time.Second}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Adapter{opt: opt}
}

func (a *Adapter) Name() string { return Name }

// Currencies whose decimal handling is certain. Stripe expects integer amounts
// in the smallest unit: two-decimal currencies are multiplied by 100, zero-decimal
// ones are sent as they are. Anything else (IDR, three-decimal currencies, special
// cases) is refused until the currency page is confirmed, because a wrong guess
// would charge 100 times too much or too little.
var (
	twoDecimal = map[string]bool{
		"USD": true, "EUR": true, "GBP": true, "CAD": true, "AUD": true, "NZD": true, "SGD": true,
		"CHF": true, "HKD": true, "SEK": true, "NOK": true, "DKK": true, "MYR": true,
	}
	zeroDecimal = map[string]bool{"JPY": true, "KRW": true}
)

// SupportsCurrency reports whether the adapter takes the currency.
func SupportsCurrency(code string) bool { return twoDecimal[code] || zeroDecimal[code] }

// minorUnits converts a canonical amount to Stripe's integer smallest unit.
func minorUnits(a domain.Amount) (int64, error) {
	cur := a.Currency()
	if !SupportsCurrency(cur) {
		return 0, fmt.Errorf("stripe adapter does not support %s", cur)
	}
	digits := strings.Replace(a.String(), ".", "", 1) // canonical: 2 decimals (or none for zero-decimal)
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n <= 0 {
		return 0, errors.New("amount out of range")
	}
	return n, nil
}

// fromMinor converts Stripe's integer smallest unit back to a domain amount.
func fromMinor(n int64, currency string) (domain.Amount, error) {
	cur := strings.ToUpper(currency)
	if !SupportsCurrency(cur) || n <= 0 {
		return domain.Amount{}, errors.New("unsupported currency or amount")
	}
	if zeroDecimal[cur] {
		return domain.ParseAmount(strconv.FormatInt(n, 10), cur)
	}
	return domain.ParseAmount(fmt.Sprintf("%d.%02d", n/100, n%100), cur)
}

// ValidateAmount implements provider.AmountValidator.
func (a *Adapter) ValidateAmount(amt domain.Amount) error {
	_, err := minorUnits(amt)
	return err
}

// ---- HTTP plumbing ---------------------------------------------------------

func (a *Adapter) call(ctx context.Context, cred provider.Credentials, method, path string, headers map[string]string, form url.Values) (int, []byte, error) {
	key := cred["api_key"].Reveal()
	if key == "" {
		return 0, nil, &provider.Error{Code: "credentials_incomplete"}
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, a.opt.BaseURL+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth(key, "") // secret key as username, empty password
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := a.opt.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %s", provider.ErrTimeout, errKind(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: reading response", provider.ErrTimeout)
	}
	return resp.StatusCode, raw, nil
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

// classify maps a non-2xx answer. Rate limits, server errors and 409 (a request
// that conflicts with one still executing) leave the outcome unknown and are
// retried with the same idempotency key; other 4xx are definite rejections.
func classify(status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	var e struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	code := e.Error.Code
	if code == "" {
		code = e.Error.Type
	}
	if status == http.StatusRequestTimeout || status == http.StatusConflict || status == http.StatusTooManyRequests || status >= 500 {
		return fmt.Errorf("%w: http %d %s", provider.ErrTimeout, status, sanitize(code, 0))
	}
	return &provider.Error{Code: sanitize(code, status)}
}

func sanitize(s string, status int) string {
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

// ---- create ----------------------------------------------------------------

type sessionJSON struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	Status        string `json:"status"`
	PaymentStatus string `json:"payment_status"`
	AmountTotal   int64  `json:"amount_total"`
	Currency      string `json:"currency"`
	ExpiresAt     int64  `json:"expires_at"`
}

// CreatePayment creates a hosted Checkout Session in payment mode.
func (a *Adapter) CreatePayment(ctx context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
	units, err := minorUnits(req.Amount)
	if err != nil {
		return provider.CreateResult{}, &provider.Error{Code: "amount_not_accepted"}
	}
	returnURL, cancelURL := strSetting(req.Settings, "return_url"), strSetting(req.Settings, "cancel_url")
	if !validURL(returnURL) || !validURL(cancelURL) {
		return provider.CreateResult{}, &provider.Error{Code: "config_incomplete_return_urls"}
	}
	minutes := intSetting(req.Settings, "expiry_minutes", defaultExpiryMin)
	if minutes < minExpiryMin {
		minutes = minExpiryMin
	}
	if minutes > maxExpiryMin {
		minutes = maxExpiryMin
	}
	expires := a.opt.Now().Add(time.Duration(minutes) * time.Minute).UTC()

	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("line_items[0][price_data][currency]", strings.ToLower(req.Amount.Currency()))
	form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(units, 10))
	form.Set("line_items[0][price_data][product_data][name]", "Payment "+req.Reference)
	form.Set("line_items[0][quantity]", "1")
	form.Set("success_url", returnURL)
	form.Set("cancel_url", cancelURL)
	form.Set("client_reference_id", req.Reference)
	form.Set("metadata[gateway_transaction_id]", req.Reference)
	form.Set("payment_intent_data[metadata][gateway_transaction_id]", req.Reference)
	form.Set("expires_at", strconv.FormatInt(expires.Unix(), 10))

	status, body, err := a.call(ctx, req.Credentials, http.MethodPost, "/v1/checkout/sessions",
		map[string]string{"Idempotency-Key": req.IdempotencyKey}, form)
	if err != nil {
		return provider.CreateResult{}, err
	}
	if err := classify(status, body); err != nil {
		return provider.CreateResult{}, err
	}
	var s sessionJSON
	if err := json.Unmarshal(body, &s); err != nil || s.ID == "" || !validURL(s.URL) {
		return provider.CreateResult{}, &provider.Error{Code: "session_response_invalid"}
	}
	exp := expires
	if s.ExpiresAt > 0 {
		exp = time.Unix(s.ExpiresAt, 0).UTC()
	}
	return provider.CreateResult{ProviderRef: s.ID, PaymentURL: s.URL, ExpiresAt: &exp, RawStatus: s.Status}, nil
}

// ---- reading the truth -----------------------------------------------------

func (a *Adapter) getSession(ctx context.Context, cred provider.Credentials, id string) (sessionJSON, error) {
	status, body, err := a.call(ctx, cred, http.MethodGet, "/v1/checkout/sessions/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return sessionJSON{}, err
	}
	if err := classify(status, body); err != nil {
		return sessionJSON{}, err
	}
	var s sessionJSON
	if err := json.Unmarshal(body, &s); err != nil || s.ID == "" {
		return sessionJSON{}, &provider.Error{Code: "session_response_invalid"}
	}
	return s, nil
}

// resolve reads the session. Money has arrived only when the session is complete
// AND payment_status is paid; complete+unpaid is a delayed payment still processing.
func (a *Adapter) resolve(ctx context.Context, cred provider.Credentials, id string) (provider.StatusResult, error) {
	s, err := a.getSession(ctx, cred, id)
	if err != nil {
		return provider.StatusResult{}, err
	}
	raw := s.Status + "/" + s.PaymentStatus
	switch s.Status {
	case "expired":
		return provider.StatusResult{Status: domain.StatusExpired, RawStatus: raw}, nil
	case "complete":
		if s.PaymentStatus != "paid" {
			return provider.StatusResult{Status: domain.StatusPending, RawStatus: raw}, nil
		}
		amt, aerr := fromMinor(s.AmountTotal, s.Currency)
		if aerr != nil {
			return provider.StatusResult{}, &provider.Error{Code: "amount_total_unreadable"}
		}
		return provider.StatusResult{Status: domain.StatusPaid, RawStatus: raw, Amount: &amt}, nil
	default: // open, unknown
		return provider.StatusResult{Status: domain.StatusPending, RawStatus: raw}, nil
	}
}

// GetStatus reads the current state of a session.
func (a *Adapter) GetStatus(ctx context.Context, req provider.StatusRequest) (provider.StatusResult, error) {
	return a.resolve(ctx, req.Credentials, req.ProviderRef)
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

// ValidSettingURL is used by the admin API.
func ValidSettingURL(raw string) bool { return validURL(raw) }
