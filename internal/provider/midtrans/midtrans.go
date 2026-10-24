// Package midtrans is the Midtrans Snap adapter (hosted payment page, IDR only).
// Behaviour follows Midtrans's documentation as reviewed in
// docs/midtrans-research.md. A notification's signature proves it came from
// Midtrans, and the outcome is then read back from the status API, so a payment
// never depends on what a notification body says.
package midtrans

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
)

// Name is the value stored in provider_configs.provider.
const Name = "midtrans"

// Official hosts (Snap and Core API differ, and so do sandbox and production).
const (
	SandboxSnap = "https://app.sandbox.midtrans.com"
	LiveSnap    = "https://app.midtrans.com"
	SandboxCore = "https://api.sandbox.midtrans.com"
	LiveCore    = "https://api.midtrans.com"
)

const (
	maxResponseBytes = 1 << 20
	defaultExpiryMin = 24 * 60
	maxExpiryMin     = 7 * 24 * 60 // Midtrans: the maximum expiry for the Snap token is 7 days
)

// orderIDRe is Midtrans's order_id rule: max 50 of alphanumerics, - _ ~ .
var orderIDRe = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,50}$`)

// Options customises the adapter. The zero value talks to Midtrans. Setting
// SnapBase/CoreBase overrides the host for both environments (tests).
type Options struct {
	SnapBase, CoreBase string
	HTTPClient         *http.Client
	Now                func() time.Time
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
	if opt.HTTPClient == nil {
		opt.HTTPClient = &http.Client{Timeout: 25 * time.Second}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Adapter{opt: opt}
}

func (a *Adapter) Name() string { return Name }

func (a *Adapter) snapBase(env string) string {
	if a.opt.SnapBase != "" {
		return a.opt.SnapBase
	}
	if env == "production" {
		return LiveSnap
	}
	return SandboxSnap
}

func (a *Adapter) coreBase(env string) string {
	if a.opt.CoreBase != "" {
		return a.opt.CoreBase
	}
	if env == "production" {
		return LiveCore
	}
	return SandboxCore
}

// SupportsCurrency: Snap amounts are in IDR.
func SupportsCurrency(code string) bool { return code == "IDR" }

// grossAmount converts a gateway amount into Snap's integer gross_amount.
func grossAmount(a domain.Amount) (int64, error) {
	if a.Currency() != "IDR" {
		return 0, fmt.Errorf("Midtrans takes IDR only, not %s", a.Currency())
	}
	n, err := strconv.ParseInt(a.String(), 10, 64) // canonical IDR has no decimals
	if err != nil || n <= 0 {
		return 0, errors.New("amount out of range")
	}
	return n, nil
}

// ValidateAmount implements provider.AmountValidator.
func (a *Adapter) ValidateAmount(amt domain.Amount) error {
	_, err := grossAmount(amt)
	return err
}

// ---- HTTP plumbing ---------------------------------------------------------

func (a *Adapter) do(ctx context.Context, cred provider.Credentials, method, fullURL string, payload []byte) (int, []byte, error) {
	key := cred["server_key"].Reveal()
	if key == "" {
		return 0, nil, &provider.Error{Code: "credentials_incomplete"}
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth(key, "") // Server Key as username, empty password
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
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

// ---- create ----------------------------------------------------------------

type snapError struct {
	ErrorMessages []string `json:"error_messages"`
}

// duplicateOrder recognises a "this order_id was already used" rejection. The
// exact text and HTTP code are not documented, so this matches loosely; a wrong
// match only makes a definite error retryable, never the other way round.
func duplicateOrder(messages []string) bool {
	for _, m := range messages {
		l := strings.ToLower(m)
		if strings.Contains(l, "order_id") && (strings.Contains(l, "sudah") || strings.Contains(l, "already") ||
			strings.Contains(l, "utilized") || strings.Contains(l, "duplicate") || strings.Contains(l, "taken") || strings.Contains(l, "used")) {
			return true
		}
	}
	return false
}

// CreatePayment creates a Snap transaction and returns its redirect URL.
func (a *Adapter) CreatePayment(ctx context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
	gross, err := grossAmount(req.Amount)
	if err != nil {
		return provider.CreateResult{}, &provider.Error{Code: "amount_not_accepted"}
	}
	if !orderIDRe.MatchString(req.Reference) {
		return provider.CreateResult{}, &provider.Error{Code: "invalid_order_reference"}
	}
	minutes := intSetting(req.Settings, "expiry_minutes", defaultExpiryMin)
	if minutes > maxExpiryMin {
		minutes = maxExpiryMin
	}
	body := map[string]any{
		"transaction_details": map[string]any{"order_id": req.Reference, "gross_amount": gross},
		"expiry":              map[string]any{"unit": "minutes", "duration": minutes},
	}
	cb := map[string]any{}
	if u := strSetting(req.Settings, "return_url"); validURL(u) {
		cb["finish"] = u
	}
	if u := strSetting(req.Settings, "cancel_url"); validURL(u) {
		cb["error"] = u
	}
	if len(cb) > 0 {
		body["callbacks"] = cb
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return provider.CreateResult{}, err
	}

	status, raw, err := a.do(ctx, req.Credentials, http.MethodPost, a.snapBase(req.Environment)+"/snap/v1/transactions", payload)
	if err != nil {
		return provider.CreateResult{}, err
	}
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
		return provider.CreateResult{}, fmt.Errorf("%w: http %d", provider.ErrTimeout, status)
	case status == http.StatusUnauthorized:
		return provider.CreateResult{}, &provider.Error{Code: "invalid_server_key"}
	default:
		var e snapError
		_ = json.Unmarshal(raw, &e)
		if duplicateOrder(e.ErrorMessages) {
			return provider.CreateResult{}, fmt.Errorf("%w: order_id already used", provider.ErrTimeout)
		}
		return provider.CreateResult{}, &provider.Error{Code: fmt.Sprintf("snap_http_%d", status)}
	}
	var out struct {
		Token       string `json:"token"`
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" || !httpsURL(out.RedirectURL) {
		return provider.CreateResult{}, &provider.Error{Code: "snap_response_invalid"}
	}
	exp := a.opt.Now().Add(time.Duration(minutes) * time.Minute).UTC()
	return provider.CreateResult{ProviderRef: req.Reference, PaymentURL: out.RedirectURL, ExpiresAt: &exp, RawStatus: "token_created"}, nil
}

// ---- reading the truth -----------------------------------------------------

// flexString accepts a JSON string or number (Midtrans is not consistent).
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*f = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*f = flexString(v)
		return nil
	}
	*f = flexString(s)
	return nil
}

type statusJSON struct {
	StatusCode        flexString `json:"status_code"`
	OrderID           string     `json:"order_id"`
	TransactionStatus string     `json:"transaction_status"`
	FraudStatus       string     `json:"fraud_status"`
	GrossAmount       flexString `json:"gross_amount"`
	Currency          string     `json:"currency"`
}

// readStatus fetches /v2/{order_id}/status. found=false means Midtrans has no
// payment for the order yet (a Snap token nobody has paid on).
func (a *Adapter) readStatus(ctx context.Context, cred provider.Credentials, env, orderID string) (statusJSON, bool, error) {
	if !orderIDRe.MatchString(orderID) {
		return statusJSON{}, false, &provider.Error{Code: "invalid_order_reference"}
	}
	status, raw, err := a.do(ctx, cred, http.MethodGet, a.coreBase(env)+"/v2/"+url.PathEscape(orderID)+"/status", nil)
	if err != nil {
		return statusJSON{}, false, err
	}
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 {
		return statusJSON{}, false, fmt.Errorf("%w: http %d", provider.ErrTimeout, status)
	}
	if status == http.StatusUnauthorized {
		return statusJSON{}, false, &provider.Error{Code: "invalid_server_key"}
	}
	if status == http.StatusNotFound {
		return statusJSON{}, false, nil
	}
	var s statusJSON
	if err := json.Unmarshal(raw, &s); err != nil {
		return statusJSON{}, false, &provider.Error{Code: "status_response_invalid"}
	}
	// The Core API reports its own code in the body (and may do so with HTTP 200).
	switch code := string(s.StatusCode); {
	case code == "404":
		return statusJSON{}, false, nil
	case code == "401" || code == "403":
		return statusJSON{}, false, &provider.Error{Code: "invalid_server_key"}
	case strings.HasPrefix(code, "5"):
		return statusJSON{}, false, fmt.Errorf("%w: status_code %s", provider.ErrTimeout, code)
	}
	return s, true, nil
}

// outcome maps a status read onto the gateway's model. ignore=true marks states
// the gateway deliberately leaves alone (refunds, chargebacks).
func outcome(s statusJSON) (res provider.StatusResult, ignore bool, err error) {
	raw := s.TransactionStatus
	if s.FraudStatus != "" {
		raw += "/" + s.FraudStatus
	}
	switch s.TransactionStatus {
	case "settlement":
		return paid(s, raw)
	case "capture":
		if s.FraudStatus == "accept" {
			return paid(s, raw)
		}
		return provider.StatusResult{Status: domain.StatusPending, RawStatus: raw}, false, nil // challenge: awaiting review
	case "deny", "failure":
		return provider.StatusResult{Status: domain.StatusFailed, RawStatus: raw}, false, nil
	case "cancel":
		return provider.StatusResult{Status: domain.StatusCancelled, RawStatus: raw}, false, nil
	case "expire":
		return provider.StatusResult{Status: domain.StatusExpired, RawStatus: raw}, false, nil
	case "refund", "partial_refund", "chargeback", "partial_chargeback":
		return provider.StatusResult{Status: domain.StatusPending, RawStatus: raw}, true, nil
	default: // pending, authorize, unknown: the money has not been received
		return provider.StatusResult{Status: domain.StatusPending, RawStatus: raw}, false, nil
	}
}

func paid(s statusJSON, raw string) (provider.StatusResult, bool, error) {
	if s.Currency != "" && s.Currency != "IDR" {
		return provider.StatusResult{}, false, &provider.Error{Code: "unexpected_currency"}
	}
	amt, err := domain.ParseAmount(string(s.GrossAmount), "IDR")
	if err != nil {
		return provider.StatusResult{}, false, &provider.Error{Code: "gross_amount_unreadable"}
	}
	return provider.StatusResult{Status: domain.StatusPaid, RawStatus: raw, Amount: &amt}, false, nil
}

// GetStatus reads the current state of an order.
func (a *Adapter) GetStatus(ctx context.Context, req provider.StatusRequest) (provider.StatusResult, error) {
	s, found, err := a.readStatus(ctx, req.Credentials, req.Environment, req.ProviderRef)
	if err != nil {
		return provider.StatusResult{}, err
	}
	if !found {
		return provider.StatusResult{Status: domain.StatusPending, RawStatus: "no_payment_yet"}, nil
	}
	res, _, err := outcome(s)
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
		if v >= 1 {
			return int(v)
		}
	case int:
		if v >= 1 {
			return v
		}
	}
	return def
}

func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && len(raw) <= 255
}

func httpsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// ValidSettingURL is used by the admin API (Midtrans callbacks are at most 255 chars).
func ValidSettingURL(raw string) bool { return validURL(raw) }
