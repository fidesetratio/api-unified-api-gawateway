// Package xendit is the Xendit Payment Session adapter (hosted checkout).
// Behaviour follows Xendit's current documentation as reviewed in
// docs/xendit-research.md. The outcome of a payment is never taken from a webhook
// body: a webhook only triggers an authenticated read of the session and its
// payment, and that read decides.
package xendit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
)

// Name is the value stored in provider_configs.provider.
const Name = "xendit"

// BaseURL is Xendit's API host (test and live share it; the key decides the mode).
const BaseURL = "https://api.xendit.co"

const (
	maxResponseBytes = 1 << 20
	defaultExpiryMin = 24 * 60
	minExpiryMin     = 10 // Xendit: expires_at must be at least 10 minutes ahead
	paymentsVersion  = "2024-11-11"
)

// Options customises the adapter. The zero value talks to Xendit.
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

// currencyCountry maps each session currency to its country. USD belongs to no
// single country, so it needs settings.country.
var currencyCountry = map[string]string{
	"IDR": "ID", "PHP": "PH", "VND": "VN", "THB": "TH", "SGD": "SG", "MYR": "MY",
}

var supportedCountries = map[string]bool{"ID": true, "PH": true, "VN": true, "TH": true, "SG": true, "MY": true}

// SupportsCurrency reports whether Payment Sessions take the currency.
func SupportsCurrency(code string) bool {
	_, ok := currencyCountry[code]
	return ok || code == "USD"
}

// SupportedCountry reports whether Payment Sessions accept the country code.
func SupportedCountry(code string) bool { return supportedCountries[code] }

// ValidateAmount implements provider.AmountValidator.
func (a *Adapter) ValidateAmount(amt domain.Amount) error {
	if !SupportsCurrency(amt.Currency()) {
		return fmt.Errorf("Xendit does not support %s", amt.Currency())
	}
	return nil
}

// ---- HTTP plumbing ---------------------------------------------------------

func (a *Adapter) call(ctx context.Context, cred provider.Credentials, method, path string, headers map[string]string, payload []byte) (int, []byte, error) {
	key := cred["api_key"].Reveal()
	if key == "" {
		return 0, nil, &provider.Error{Code: "credentials_incomplete"}
	}
	var rdr io.Reader
	if payload != nil {
		rdr = strings.NewReader(string(payload))
	}
	req, err := http.NewRequestWithContext(ctx, method, a.opt.BaseURL+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth(key, "") // secret key as username, empty password
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
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

// classify maps a non-2xx answer: transient failures and a 409 DUPLICATE_ERROR
// (a retried create whose first attempt may exist) mean the outcome is unknown;
// other 4xx are definite rejections.
func classify(status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	var e struct {
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(body, &e)
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 || status == http.StatusConflict {
		return fmt.Errorf("%w: http %d %s", provider.ErrTimeout, status, sanitize(e.ErrorCode, 0))
	}
	return &provider.Error{Code: sanitize(e.ErrorCode, status)}
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
	ID          string      `json:"payment_session_id"`
	Status      string      `json:"status"`
	PaymentID   string      `json:"payment_id"`
	LinkURL     string      `json:"payment_link_url"`
	ExpiresAt   string      `json:"expires_at"`
	Currency    string      `json:"currency"`
	Amount      json.Number `json:"amount"`
	ReferenceID string      `json:"reference_id"`
}

// CreatePayment creates a hosted-checkout session.
func (a *Adapter) CreatePayment(ctx context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
	if err := a.ValidateAmount(req.Amount); err != nil {
		return provider.CreateResult{}, &provider.Error{Code: "amount_not_accepted"}
	}
	returnURL, cancelURL := strSetting(req.Settings, "return_url"), strSetting(req.Settings, "cancel_url")
	if !httpsURL(returnURL) || !httpsURL(cancelURL) {
		return provider.CreateResult{}, &provider.Error{Code: "config_incomplete_return_urls"}
	}
	country := currencyCountry[req.Amount.Currency()]
	if country == "" {
		country = strings.ToUpper(strSetting(req.Settings, "country"))
	}
	if !SupportedCountry(country) {
		return provider.CreateResult{}, &provider.Error{Code: "config_incomplete_country"}
	}
	minutes := intSetting(req.Settings, "expiry_minutes", defaultExpiryMin)
	if minutes < minExpiryMin {
		minutes = minExpiryMin
	}
	expires := a.opt.Now().Add(time.Duration(minutes) * time.Minute).UTC()

	payload, err := json.Marshal(map[string]any{
		"reference_id":       req.Reference,
		"session_type":       "PAY",
		"mode":               "PAYMENT_LINK",
		"amount":             json.Number(req.Amount.String()), // exact decimal text, never a float
		"currency":           req.Amount.Currency(),
		"country":            country,
		"capture_method":     "AUTOMATIC",
		"description":        "Payment " + req.Reference,
		"success_return_url": returnURL,
		"cancel_return_url":  cancelURL,
		"expires_at":         expires.Format(time.RFC3339),
	})
	if err != nil {
		return provider.CreateResult{}, err
	}
	status, body, err := a.call(ctx, req.Credentials, http.MethodPost, "/sessions", nil, payload)
	if err != nil {
		return provider.CreateResult{}, err
	}
	if err := classify(status, body); err != nil {
		return provider.CreateResult{}, err
	}
	var s sessionJSON
	if err := json.Unmarshal(body, &s); err != nil || s.ID == "" || !httpsURL(s.LinkURL) {
		return provider.CreateResult{}, &provider.Error{Code: "session_response_invalid"}
	}
	exp := expires
	if t, perr := time.Parse(time.RFC3339, s.ExpiresAt); perr == nil {
		exp = t.UTC()
	}
	return provider.CreateResult{ProviderRef: s.ID, PaymentURL: s.LinkURL, ExpiresAt: &exp, RawStatus: s.Status}, nil
}

// ---- reading the truth -----------------------------------------------------

type paymentJSON struct {
	Status   string `json:"status"`
	Currency string `json:"currency"`
	Captures []struct {
		Amount json.Number `json:"capture_amount"`
	} `json:"captures"`
}

func (a *Adapter) getSession(ctx context.Context, cred provider.Credentials, id string) (sessionJSON, error) {
	status, body, err := a.call(ctx, cred, http.MethodGet, "/sessions/"+url.PathEscape(id), nil, nil)
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

func (a *Adapter) getPayment(ctx context.Context, cred provider.Credentials, id string) (paymentJSON, error) {
	status, body, err := a.call(ctx, cred, http.MethodGet, "/v3/payments/"+url.PathEscape(id),
		map[string]string{"api-version": paymentsVersion}, nil)
	if err != nil {
		return paymentJSON{}, err
	}
	if err := classify(status, body); err != nil {
		return paymentJSON{}, err
	}
	var p paymentJSON
	if err := json.Unmarshal(body, &p); err != nil {
		return paymentJSON{}, &provider.Error{Code: "payment_response_invalid"}
	}
	return p, nil
}

// capturedAmount sums what Xendit actually captured, exactly (no floats).
func capturedAmount(p paymentJSON, fallbackCurrency string) (*domain.Amount, error) {
	total := new(big.Rat)
	for _, c := range p.Captures {
		r, ok := new(big.Rat).SetString(c.Amount.String())
		if !ok || r.Sign() < 0 {
			return nil, errors.New("unreadable capture amount")
		}
		total.Add(total, r)
	}
	currency := p.Currency
	if currency == "" {
		currency = fallbackCurrency
	}
	amt, err := domain.ParseAmount(total.FloatString(4), currency)
	if err != nil {
		return nil, err
	}
	return &amt, nil
}

// resolve reads the session and, when it is completed, its payment, and returns
// the gateway's view. retryable=true means "not settled yet, ask again later".
func (a *Adapter) resolve(ctx context.Context, cred provider.Credentials, id string) (res provider.StatusResult, retryable bool, err error) {
	s, err := a.getSession(ctx, cred, id)
	if err != nil {
		return provider.StatusResult{}, false, err
	}
	switch s.Status {
	case "ACTIVE":
		return provider.StatusResult{Status: domain.StatusPending, RawStatus: s.Status}, false, nil
	case "EXPIRED":
		return provider.StatusResult{Status: domain.StatusExpired, RawStatus: s.Status}, false, nil
	case "CANCELED":
		return provider.StatusResult{Status: domain.StatusCancelled, RawStatus: s.Status}, false, nil
	case "COMPLETED":
	default:
		return provider.StatusResult{Status: domain.StatusPending, RawStatus: s.Status}, false, nil
	}
	if s.PaymentID == "" {
		return provider.StatusResult{Status: domain.StatusPending, RawStatus: s.Status}, true, nil
	}
	p, err := a.getPayment(ctx, cred, s.PaymentID)
	if err != nil {
		return provider.StatusResult{}, false, err
	}
	switch p.Status {
	case "SUCCEEDED":
		amt, aerr := capturedAmount(p, s.Currency)
		if aerr != nil {
			return provider.StatusResult{}, false, &provider.Error{Code: "capture_amount_unreadable"}
		}
		return provider.StatusResult{Status: domain.StatusPaid, RawStatus: "COMPLETED/SUCCEEDED", Amount: amt}, false, nil
	case "FAILED", "CANCELED", "EXPIRED":
		return provider.StatusResult{Status: domain.StatusFailed, RawStatus: "COMPLETED/" + p.Status}, false, nil
	default: // PENDING, AUTHORIZED, unknown: money has not moved (yet)
		return provider.StatusResult{Status: domain.StatusPending, RawStatus: "COMPLETED/" + p.Status}, true, nil
	}
}

// GetStatus reads the current state of a session.
func (a *Adapter) GetStatus(ctx context.Context, req provider.StatusRequest) (provider.StatusResult, error) {
	res, _, err := a.resolve(ctx, req.Credentials, req.ProviderRef)
	return res, err
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

func httpsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && len(raw) <= 500
}

// ValidSettingURL is used by the admin API: Xendit requires HTTPS return URLs.
func ValidSettingURL(raw string) bool { return httpsURL(raw) }
