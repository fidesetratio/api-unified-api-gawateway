// Package provider defines the contract every payment provider adapter fulfils.
// Adapters know their provider's HTTP API and signature scheme. They never touch
// the database and never decide status transitions; they return normalized values.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/secrets"
)

// Credentials are decrypted provider secrets. Values print as [REDACTED].
type Credentials map[string]secrets.Secret

// CreateRequest asks a provider to start a payment.
type CreateRequest struct {
	// Reference is the gateway's own unique reference for this payment (the
	// transaction id). Adapters send it to the provider as the merchant reference.
	Reference string
	// IdempotencyKey is stable across retries of the same payment, so a retry
	// cannot create a second provider payment where the provider supports it.
	IdempotencyKey string
	Amount         domain.Amount
	// Environment is "sandbox" or "production"; it selects the provider base URL.
	Environment string
	Credentials Credentials
	Settings    map[string]any
}

// CreateResult is the provider's answer to a create call.
type CreateResult struct {
	ProviderRef string
	PaymentURL  string
	ExpiresAt   *time.Time
	RawStatus   string
}

// StatusRequest asks a provider for the current state of a payment.
type StatusRequest struct {
	ProviderRef string
	Environment string
	Credentials Credentials
	Settings    map[string]any
}

// StatusResult is a provider's current view of a payment.
type StatusResult struct {
	Status    domain.Status
	RawStatus string
	// Amount is set when the provider reports what was charged.
	Amount *domain.Amount
}

// WebhookRequest carries a raw webhook delivery for verification.
type WebhookRequest struct {
	Headers       http.Header
	Body          []byte
	Environment   string
	Credentials   Credentials
	WebhookSecret Credentials
	Settings      map[string]any
}

// Event is a verified, normalized webhook event.
type Event struct {
	EventID     string
	ProviderRef string
	Status      domain.Status
	RawStatus   string
	Amount      *domain.Amount
	// Capturable marks an approval event: the payer approved and the gateway must
	// now capture the payment. It carries no status change.
	Capturable bool
	// Ignore marks a verified event the gateway has no use for (acknowledge only).
	Ignore bool
}

// CaptureRequest asks a provider to move the money for an approved payment.
type CaptureRequest struct {
	ProviderRef    string
	IdempotencyKey string
	Environment    string
	Credentials    Credentials
	Settings       map[string]any
}

// CaptureResult is the answer to a capture.
type CaptureResult struct {
	CaptureID string
	// Status is paid only when the provider reports the capture as completed.
	Status    domain.Status
	RawStatus string
	Amount    *domain.Amount
}

// Optional capabilities. An adapter implements only what its provider needs.
type (
	// Capturer is implemented by two-step providers (approve, then capture).
	Capturer interface {
		Capture(ctx context.Context, req CaptureRequest) (CaptureResult, error)
	}
	// Locator extracts the provider reference from an UNVERIFIED webhook body,
	// only to decide which configuration's secret to verify it with.
	Locator interface {
		LocateWebhook(body []byte) (providerRef string, ok bool)
	}
	// AmountValidator rejects amounts the provider cannot take (for example
	// decimals in a zero-decimal currency).
	AmountValidator interface {
		ValidateAmount(a domain.Amount) error
	}
)

// Provider is implemented by every adapter.
type Provider interface {
	// Name is the value stored in provider_configs.provider.
	Name() string
	CreatePayment(ctx context.Context, req CreateRequest) (CreateResult, error)
	GetStatus(ctx context.Context, req StatusRequest) (StatusResult, error)
	// VerifyWebhook must authenticate the delivery before returning an Event.
	// A failed verification returns ErrNotVerified.
	VerifyWebhook(ctx context.Context, req WebhookRequest) (Event, error)
}

// Errors adapters return. They carry no provider secrets.
var (
	// ErrTimeout means the provider did not answer; the outcome is unknown.
	ErrTimeout = errors.New("provider timeout")
	// ErrNotVerified means webhook authenticity could not be established.
	ErrNotVerified = errors.New("webhook not verified")
	// ErrNotImplemented is returned by providers whose adapter is not built yet.
	ErrNotImplemented = errors.New("provider not implemented")
)

// Error is a definite provider rejection (the payment was not created).
type Error struct {
	Code    string // provider or adapter error code, safe to log
	DebugID string // provider support reference (e.g. PayPal debug_id), safe to log
}

func (e *Error) Error() string {
	if e.DebugID != "" {
		return "provider error: " + e.Code + " (debug_id " + e.DebugID + ")"
	}
	return "provider error: " + e.Code
}

// Registry maps provider names to adapters.
type Registry struct{ providers map[string]Provider }

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{providers: map[string]Provider{}} }

// Register adds an adapter. Registering a name twice is a programming error.
func (r *Registry) Register(p Provider) error {
	if _, dup := r.providers[p.Name()]; dup {
		return fmt.Errorf("provider %q registered twice", p.Name())
	}
	r.providers[p.Name()] = p
	return nil
}

// Get returns the adapter for a name.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

// Stub is a placeholder for providers whose adapter is built in Phase 4. Every
// call fails with ErrNotImplemented, which the service reports as provider_error.
type Stub struct{ name string }

// NewStub returns a stub for the named provider.
func NewStub(name string) Stub { return Stub{name: name} }

func (s Stub) Name() string { return s.name }
func (s Stub) CreatePayment(context.Context, CreateRequest) (CreateResult, error) {
	return CreateResult{}, ErrNotImplemented
}
func (s Stub) GetStatus(context.Context, StatusRequest) (StatusResult, error) {
	return StatusResult{}, ErrNotImplemented
}
func (s Stub) VerifyWebhook(context.Context, WebhookRequest) (Event, error) {
	return Event{}, ErrNotImplemented
}
