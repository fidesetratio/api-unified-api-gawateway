package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
	"unifiedgw/internal/secrets"
	"unifiedgw/internal/store"
)

// Settlement applies provider evidence to payments: verified webhook events and
// expiry. It is the only code path that can move a payment to paid.
type Settlement struct {
	payments *store.Payments
	configs  *store.Configs
	keys     *secrets.KeyRing
	registry *provider.Registry
	log      *slog.Logger
}

// NewSettlement wires the settlement service.
func NewSettlement(p *store.Payments, c *store.Configs, keys *secrets.KeyRing, reg *provider.Registry, log *slog.Logger) *Settlement {
	return &Settlement{payments: p, configs: c, keys: keys, registry: reg, log: log}
}

// ProcessWebhook verifies a raw delivery with the adapter and the config's
// webhook secret, and only then applies it. A delivery that fails verification
// is recorded and changes nothing. The returned error is for failures the
// sender should retry; every definite answer is an outcome string.
func (s *Settlement) ProcessWebhook(ctx context.Context, configID int64, headers http.Header, body []byte) (string, error) {
	cfg, err := s.configs.Get(ctx, configID)
	if err != nil {
		return "", err
	}
	adapter, ok := s.registry.Get(cfg.Provider)
	if !ok {
		return "", provider.ErrNotImplemented
	}
	cred, webhook, _, err := loadCredentials(ctx, s.configs, s.keys, cfg)
	if err != nil {
		return "", err
	}
	ev, err := adapter.VerifyWebhook(ctx, provider.WebhookRequest{
		Headers: headers, Body: body, Environment: cfg.Environment,
		Credentials: cred, WebhookSecret: webhook, Settings: cfg.Settings,
	})
	if errors.Is(err, provider.ErrNotVerified) {
		if rerr := s.payments.RecordUnverified(ctx, cfg.Provider, cfg.ID); rerr != nil {
			s.log.Error("record unverified webhook failed", "error", rerr)
		}
		return store.OutcomeNotVerified, nil
	}
	if err != nil {
		return "", err
	}
	outcome, err := s.payments.ApplyEvent(ctx, store.EventInput{
		Provider: cfg.Provider, ConfigID: cfg.ID, EventID: ev.EventID, ProviderRef: ev.ProviderRef,
		Status: ev.Status, Amount: ev.Amount, Ignore: ev.Ignore,
	})
	if err != nil {
		return "", err
	}
	if ev.Capturable {
		// Attempted even when the event itself was a duplicate: the first delivery
		// may have been recorded and then died before the capture happened.
		return s.captureApproved(ctx, cfg, adapter, ev, outcome)
	}
	return outcome, nil
}

// ErrUnknownProvider is returned when no webhook-capable adapter exists for a name.
var ErrUnknownProvider = errors.New("unknown provider")

// ProcessProviderWebhook handles a delivery that arrived at a provider-level URL.
// The adapter peeks at the unverified body only to learn which payment (and so
// which configuration) it concerns; verification then uses that configuration's
// secret, and nothing changes before it succeeds.
func (s *Settlement) ProcessProviderWebhook(ctx context.Context, providerName string, headers http.Header, body []byte) (string, error) {
	adapter, ok := s.registry.Get(providerName)
	if !ok {
		return "", ErrUnknownProvider
	}
	loc, canLocate := adapter.(provider.Locator)
	if !canLocate {
		return "", ErrUnknownProvider
	}
	ref, ok := loc.LocateWebhook(body)
	if !ok {
		if json.Valid(body) {
			return store.OutcomeIgnored, nil // a real event of a kind we do not track (nothing recorded)
		}
		return store.OutcomeNotVerified, nil // junk: nothing to attribute, nothing recorded
	}
	configID, found, err := s.payments.FindConfigByProviderRef(ctx, providerName, ref)
	if err != nil {
		return "", err
	}
	if !found {
		return store.OutcomeUnknownReference, nil
	}
	return s.ProcessWebhook(ctx, configID, headers, body)
}

// captureApproved moves the money for an approved payment. It captures only a
// payment that is still pending, with a stable idempotency key so a retry never
// charges twice. PAID is applied only when the provider reports the capture as
// completed with the stored amount and currency.
func (s *Settlement) captureApproved(ctx context.Context, cfg store.Config, adapter provider.Provider, ev provider.Event, prior string) (string, error) {
	capturer, ok := adapter.(provider.Capturer)
	if !ok {
		return prior, nil
	}
	p, err := s.payments.GetByProviderRef(ctx, cfg.ID, ev.ProviderRef)
	if errors.Is(err, domain.ErrPaymentNotFound) {
		return prior, nil
	}
	if err != nil {
		return "", err
	}
	if p.Status != domain.StatusPending {
		return prior, nil // already settled, expired or cancelled: do not take money
	}
	cred, _, _, err := loadCredentials(ctx, s.configs, s.keys, cfg)
	if err != nil {
		return "", err
	}
	res, err := capturer.Capture(ctx, provider.CaptureRequest{
		ProviderRef: ev.ProviderRef, IdempotencyKey: "capture-" + p.ID,
		Environment: cfg.Environment, Credentials: cred, Settings: cfg.Settings,
	})
	if err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) {
			// A definite refusal will not change on retry: log it, keep the payment
			// pending, and acknowledge the webhook.
			s.log.Warn("capture refused", "transaction_id", p.ID, "code", pe.Code, "debug_id", pe.DebugID)
			return "capture_refused", nil
		}
		return "", err // unknown outcome: fail the webhook so the provider retries
	}
	if res.Status == domain.StatusPending {
		return prior, nil // a verified capture webhook will follow
	}
	return s.payments.ApplyEvent(ctx, store.EventInput{
		Provider: cfg.Provider, ConfigID: cfg.ID, EventID: "capture:" + res.CaptureID,
		ProviderRef: ev.ProviderRef, Status: res.Status, Amount: res.Amount,
	})
}

var transactionIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ReturnView is what the return endpoint reports back to the buyer's browser.
type ReturnView struct {
	UniqueCode string
	Status     domain.Status
}

// CheckReturn runs when the buyer's browser comes back from the provider. The
// request is NOT evidence of payment: the id in it only tells us which payment
// to look at. The gateway then asks the provider directly, with its own
// credentials, and applies the answer through the same amount, currency and
// transition checks as a verified webhook. Two-step providers (PayPal) are
// captured here once the provider reports the order as approved.
func (s *Settlement) CheckReturn(ctx context.Context, txID string) (ReturnView, error) {
	if !transactionIDRe.MatchString(txID) {
		return ReturnView{}, domain.ErrPaymentNotFound
	}
	p, err := s.payments.GetByID(ctx, txID)
	if err != nil {
		return ReturnView{}, err
	}
	if p.Status != domain.StatusPending || p.ProviderRef == "" {
		return returnViewOf(p), nil
	}
	cfg, err := s.configs.Get(ctx, p.ConfigID)
	if err != nil {
		return ReturnView{}, err
	}
	adapter, ok := s.registry.Get(cfg.Provider)
	if !ok {
		return returnViewOf(p), nil
	}
	cred, _, _, err := loadCredentials(ctx, s.configs, s.keys, cfg)
	if err != nil {
		return ReturnView{}, err
	}
	res, err := adapter.GetStatus(ctx, provider.StatusRequest{
		ProviderRef: p.ProviderRef, Environment: cfg.Environment, Credentials: cred, Settings: cfg.Settings,
	})
	if err != nil {
		s.log.Warn("return status check failed", "transaction_id", p.ID, "provider", cfg.Provider, "error", err)
		return returnViewOf(p), nil // the buyer still gets an answer; a webhook or the next check settles it
	}

	switch {
	case res.Status == domain.StatusPending && res.RawStatus == "APPROVED":
		if _, err := s.captureApproved(ctx, cfg, adapter, provider.Event{ProviderRef: p.ProviderRef}, ""); err != nil {
			s.log.Warn("capture on return failed", "transaction_id", p.ID, "error", err)
		}
	case res.Status != domain.StatusPending:
		if _, err := s.payments.ApplyEvent(ctx, store.EventInput{
			Provider: cfg.Provider, ConfigID: cfg.ID, EventID: "return:" + p.ProviderRef + ":" + string(res.Status),
			ProviderRef: p.ProviderRef, Status: res.Status, Amount: res.Amount,
		}); err != nil {
			return ReturnView{}, err
		}
	}
	p, err = s.payments.GetByID(ctx, txID)
	if err != nil {
		return ReturnView{}, err
	}
	return returnViewOf(p), nil
}

func returnViewOf(p store.Payment) ReturnView {
	return ReturnView{UniqueCode: p.UniqueCode, Status: p.Status}
}

// ExpireDue expires pending payments whose time has passed.
func (s *Settlement) ExpireDue(ctx context.Context) (int, error) {
	return s.payments.ExpireDue(ctx, 100)
}

const (
	maxAttempts = 5
	// maxResumeAge: PayPal may keep idempotency keys for only 6 hours, so a retry
	// after that could create a second order. Older payments go to a human instead.
	maxResumeAge = 5 * time.Hour
)

// ResumeCreating finishes payments stuck in creating (a crash, or a timeout whose
// outcome is unknown). It retries with the same provider idempotency key, so it
// can never create a second provider payment. After maxAttempts it stops and
// opens a review item for a human.
func (s *Payments) ResumeCreating(ctx context.Context) (int, error) {
	stuck, err := s.payments.StuckCreating(ctx, (2 * s.opt.Lease).Seconds(), 20)
	if err != nil {
		return 0, err
	}
	resumed := 0
	for _, p := range stuck {
		n, err := s.payments.AttemptCount(ctx, p.ID)
		if err != nil {
			return resumed, err
		}
		if n >= maxAttempts || time.Since(p.CreatedAt) > maxResumeAge {
			if err := s.payments.OpenReview(ctx, p.ID, "unknown_outcome_exhausted", map[string]any{"attempts": n}); err != nil {
				return resumed, err
			}
			continue
		}
		cfg, err := s.configs.Get(ctx, p.ConfigID)
		if err != nil || cfg.Status != "active" {
			continue
		}
		amount, err := domain.ParseAmount(p.Amount, p.Currency)
		if err != nil {
			s.log.Error("stored amount invalid", "transaction_id", p.ID, "error", err)
			continue
		}
		att, err := s.payments.BeginAttempt(ctx, p.ID, s.opt.Owner, s.opt.Lease)
		if errors.Is(err, store.ErrAttemptInFlight) || errors.Is(err, store.ErrNotCreating) {
			continue // somebody else got there first
		}
		if err != nil {
			return resumed, err
		}
		if _, err := s.runAttempt(ctx, p, cfg, amount, att); err != nil {
			s.log.Warn("resume attempt did not complete", "transaction_id", p.ID, "error", err)
		}
		resumed++
	}
	return resumed, nil
}
