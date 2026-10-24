// Package fake is a deterministic in-memory provider for development and tests.
// It is registered only when the environment is not production.
package fake

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
)

// Name is the provider name stored in provider_configs.provider.
const Name = "fake"

// Behaviors selectable through the config setting "behavior".
const (
	BehaviorSuccess = "success" // default
	BehaviorTimeout = "timeout" // returns provider.ErrTimeout
	BehaviorReject  = "reject"  // returns a definite *provider.Error
	BehaviorSlow    = "slow"    // sleeps settings.delay_ms, then succeeds
)

// Provider is the Fake Provider.
type Provider struct {
	mu       sync.Mutex
	creates  int
	byKey    map[string]provider.CreateResult // idempotency key -> result
	statuses map[string]domain.Status         // provider ref -> status
}

// New returns an empty Fake Provider.
func New() *Provider {
	return &Provider{byKey: map[string]provider.CreateResult{}, statuses: map[string]domain.Status{}}
}

func (p *Provider) Name() string { return Name }

// CreateCalls reports how many create calls reached the "provider" (including
// idempotent repeats), so tests can assert "one provider payment".
func (p *Provider) CreateCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.creates
}

// UniquePayments reports how many distinct provider payments exist.
func (p *Provider) UniquePayments() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byKey)
}

// SetStatus forces what GetStatus reports for a provider ref.
func (p *Provider) SetStatus(ref string, s domain.Status) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statuses[ref] = s
}

func (p *Provider) CreatePayment(ctx context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
	p.mu.Lock()
	p.creates++
	p.mu.Unlock()

	switch behavior(req.Settings) {
	case BehaviorTimeout:
		return provider.CreateResult{}, provider.ErrTimeout
	case BehaviorReject:
		return provider.CreateResult{}, &provider.Error{Code: "fake_rejected"}
	case BehaviorSlow:
		select {
		case <-time.After(time.Duration(intSetting(req.Settings, "delay_ms", 100)) * time.Millisecond):
		case <-ctx.Done():
			return provider.CreateResult{}, provider.ErrTimeout
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// Like a real provider honoring an idempotency key: same key, same payment.
	if res, ok := p.byKey[req.IdempotencyKey]; ok {
		return res, nil
	}
	sum := sha256.Sum256([]byte(req.IdempotencyKey))
	ref := "FAKE-" + hex.EncodeToString(sum[:6])
	exp := time.Now().Add(24 * time.Hour).UTC()
	res := provider.CreateResult{
		ProviderRef: ref,
		PaymentURL:  "https://fake.invalid/pay/" + ref,
		ExpiresAt:   &exp,
		RawStatus:   "CREATED",
	}
	p.byKey[req.IdempotencyKey] = res
	p.statuses[ref] = domain.StatusPending
	return res, nil
}

func (p *Provider) GetStatus(_ context.Context, req provider.StatusRequest) (provider.StatusResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.statuses[req.ProviderRef]
	if !ok {
		return provider.StatusResult{}, &provider.Error{Code: "fake_not_found"}
	}
	return provider.StatusResult{Status: s, RawStatus: string(s)}, nil
}

// webhookBody is the JSON shape the Fake Provider's "webhooks" use.
type webhookBody struct {
	EventID     string `json:"event_id"`
	ProviderRef string `json:"provider_ref"`
	Status      string `json:"status"`
	Amount      string `json:"amount"`
	Currency    string `json:"currency"`
}

// VerifyWebhook authenticates with the shared token in header X-Fake-Token
// (compared in constant time against the webhook secret "token").
func (p *Provider) VerifyWebhook(_ context.Context, req provider.WebhookRequest) (provider.Event, error) {
	want := req.WebhookSecret["token"].Reveal()
	got := req.Headers.Get("X-Fake-Token")
	if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return provider.Event{}, provider.ErrNotVerified
	}
	var b webhookBody
	if err := json.Unmarshal(req.Body, &b); err != nil || b.EventID == "" || b.ProviderRef == "" {
		return provider.Event{}, provider.ErrNotVerified
	}
	st := domain.Status(b.Status)
	if !st.Valid() {
		return provider.Event{}, provider.ErrNotVerified
	}
	ev := provider.Event{EventID: b.EventID, ProviderRef: b.ProviderRef, Status: st, RawStatus: b.Status}
	if b.Amount != "" {
		amt, err := domain.ParseAmount(b.Amount, b.Currency)
		if err != nil {
			return provider.Event{}, provider.ErrNotVerified
		}
		ev.Amount = &amt
	}
	return ev, nil
}

func behavior(settings map[string]any) string {
	if s, ok := settings["behavior"].(string); ok && s != "" {
		return s
	}
	return BehaviorSuccess
}

func intSetting(settings map[string]any, key string, def int) int {
	switch v := settings[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}
