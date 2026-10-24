package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"unifiedgw/internal/auth"
	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
	"unifiedgw/internal/secrets"
	"unifiedgw/internal/store"
)

var uniquecodeRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// Options tunes the payment service. Zero values get safe defaults.
type Options struct {
	// Lease is how long an attempt owns the right to call the provider.
	Lease time.Duration
	// ProviderTimeout bounds one provider call (must be shorter than Lease).
	ProviderTimeout time.Duration
	// Owner identifies this process in attempt rows.
	Owner string
}

// Payments is the payment use-case service.
type Payments struct {
	payments *store.Payments
	configs  *store.Configs
	clients  *store.Clients
	keys     *secrets.KeyRing
	registry *provider.Registry
	log      *slog.Logger
	opt      Options
}

// NewPayments wires the service.
func NewPayments(p *store.Payments, c *store.Configs, cl *store.Clients, keys *secrets.KeyRing,
	reg *provider.Registry, log *slog.Logger, opt Options) *Payments {
	if opt.Lease <= 0 {
		opt.Lease = 30 * time.Second
	}
	if opt.ProviderTimeout <= 0 || opt.ProviderTimeout >= opt.Lease {
		opt.ProviderTimeout = opt.Lease * 2 / 3
	}
	if opt.Owner == "" {
		opt.Owner = "gateway"
	}
	return &Payments{payments: p, configs: c, clients: cl, keys: keys, registry: reg, log: log, opt: opt}
}

// CreateInput is the validated-on-entry request of POST /payments.
type CreateInput struct {
	ConfigID   int64
	UniqueCode string
	Amount     string
	Currency   string
}

// View is what callers see: exactly uniquecode, status and payment_url.
type View struct {
	UniqueCode string
	Status     domain.Status
	PaymentURL string // only while the payment is awaiting the payer
}

func viewOf(p store.Payment) View {
	v := View{UniqueCode: p.UniqueCode, Status: p.Status}
	if p.Status == domain.StatusCreating || p.Status == domain.StatusPending {
		v.PaymentURL = p.PaymentURL
	}
	return v
}

// Create starts a payment, or returns the existing one for an identical repeat
// of the same uniquecode (created=false). Provider calls happen outside any
// database transaction, fenced by an attempt lease.
func (s *Payments) Create(ctx context.Context, who auth.Principal, in CreateInput) (v View, created bool, err error) {
	if in.ConfigID <= 0 || !uniquecodeRe.MatchString(in.UniqueCode) {
		return View{}, false, domain.ErrInvalidRequest
	}
	amount, err := domain.ParseAmount(in.Amount, in.Currency)
	if err != nil {
		return View{}, false, domain.ErrInvalidRequest
	}

	// Authorization first, so an unknown id and a forbidden id look the same.
	ok, err := s.clients.HasAccess(ctx, who.ID, in.ConfigID)
	if err != nil {
		return View{}, false, err
	}
	if !ok {
		return View{}, false, domain.ErrConfigNotPermitted
	}
	cfg, err := s.configs.Get(ctx, in.ConfigID)
	if err != nil {
		return View{}, false, err
	}
	if cfg.Status != "active" {
		return View{}, false, domain.ErrConfigNotFound
	}
	if !slices.Contains(cfg.AllowedCurrencies, in.Currency) {
		return View{}, false, domain.ErrCurrencyUnsupported
	}

	if adapter, ok := s.registry.Get(cfg.Provider); ok {
		if v, ok := adapter.(provider.AmountValidator); ok {
			if err := v.ValidateAmount(amount); err != nil {
				return View{}, false, domain.NewError(http.StatusBadRequest, "invalid_request", err.Error())
			}
		}
	}

	fp := RequestFingerprint(in.ConfigID, amount)
	p, inserted, err := s.payments.InsertOrGet(ctx, store.NewPayment{
		ClientID: who.ID, ConfigID: in.ConfigID, UniqueCode: in.UniqueCode,
		Amount: amount, Fingerprint: fp, CredentialVersion: cfg.ActiveVersion,
	})
	if err != nil {
		return View{}, false, err
	}
	if !inserted && string(p.Fingerprint) != string(fp) {
		return View{}, false, domain.ErrUniquecodeConflict
	}
	if p.Status != domain.StatusCreating {
		return viewOf(p), inserted, nil // replay of a settled or pending payment
	}

	// Still creating: start (or take over) the provider attempt.
	att, err := s.payments.BeginAttempt(ctx, p.ID, s.opt.Owner, s.opt.Lease)
	switch {
	case errors.Is(err, store.ErrAttemptInFlight):
		return viewOf(p), inserted, nil // a concurrent identical request owns the call
	case errors.Is(err, store.ErrNotCreating):
		cur, gerr := s.payments.GetByID(ctx, p.ID)
		if gerr != nil {
			return View{}, false, gerr
		}
		return viewOf(cur), inserted, nil
	case err != nil:
		return View{}, false, err
	}

	v, err = s.runAttempt(ctx, p, cfg, amount, att)
	if err != nil {
		return View{}, false, err
	}
	return v, inserted, nil
}

// runAttempt performs one fenced provider call for a transaction whose lease the
// caller already holds, and records the outcome. It is shared by Create and the
// reconciler.
func (s *Payments) runAttempt(ctx context.Context, p store.Payment, cfg store.Config, amount domain.Amount, att store.Attempt) (View, error) {
	res, perr := s.callProvider(ctx, cfg, amount, att)
	// Recording the outcome must survive a caller that disconnected meanwhile;
	// otherwise a payment created at the provider would never be recorded.
	ctx = context.WithoutCancel(ctx)
	switch {
	case perr == nil:
		applied, err := s.payments.CompleteSuccess(ctx, att, store.ProviderResult{
			ProviderRef: res.ProviderRef, PaymentURL: res.PaymentURL, ExpiresAt: res.ExpiresAt, RawStatus: res.RawStatus,
		})
		if err != nil {
			return View{}, err
		}
		if !applied {
			s.log.Warn("stale provider result discarded", "transaction_id", p.ID, "fence", att.Fence)
		}
		cur, err := s.payments.GetByID(ctx, p.ID)
		if err != nil {
			return View{}, err
		}
		return viewOf(cur), nil

	case errors.Is(perr, provider.ErrTimeout) || errors.Is(perr, context.DeadlineExceeded):
		if err := s.payments.CompleteUnknown(ctx, att, "timeout"); err != nil {
			s.log.Error("record unknown attempt failed", "transaction_id", p.ID, "error", err)
		}
		return View{}, domain.ErrProviderTimeout

	default:
		code := "provider_error"
		var pe *provider.Error
		switch {
		case errors.As(perr, &pe):
			code = pe.Code
		case errors.Is(perr, provider.ErrNotImplemented):
			code = "not_implemented"
		default:
			// Unclassified failure: the outcome is unknown, so do not burn the uniquecode.
			s.log.Error("provider call failed", "transaction_id", p.ID, "error", perr)
			_ = s.payments.CompleteUnknown(ctx, att, "unclassified")
			return View{}, domain.ErrProviderError
		}
		s.log.Warn("provider rejected payment", "transaction_id", p.ID, "code", code)
		if _, err := s.payments.CompleteFailure(ctx, att, code); err != nil {
			return View{}, err
		}
		return View{}, domain.ErrProviderError
	}
}

func (s *Payments) callProvider(ctx context.Context, cfg store.Config, amount domain.Amount, att store.Attempt) (provider.CreateResult, error) {
	adapter, ok := s.registry.Get(cfg.Provider)
	if !ok {
		return provider.CreateResult{}, provider.ErrNotImplemented
	}
	cred, _, _, err := loadCredentials(ctx, s.configs, s.keys, cfg)
	if err != nil {
		return provider.CreateResult{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, s.opt.ProviderTimeout)
	defer cancel()
	return adapter.CreatePayment(callCtx, provider.CreateRequest{
		Reference:      att.TransactionID,
		IdempotencyKey: att.IdempotencyKey,
		Amount:         amount,
		Environment:    cfg.Environment,
		Credentials:    cred,
		Settings:       withReference(cfg.Settings, att.TransactionID),
	})
}

// referencePlaceholder in a config's return_url or cancel_url is replaced by the
// payment's id, so the buyer comes back to the gateway's return endpoint
// carrying the id (for example http://host/api/v1.0/return?ref={reference}).
const referencePlaceholder = "{reference}"

// withReference returns a copy of the settings with the placeholder filled in.
func withReference(settings map[string]any, reference string) map[string]any {
	out := make(map[string]any, len(settings))
	for k, v := range settings {
		if s, ok := v.(string); ok && (k == "return_url" || k == "cancel_url") {
			v = strings.ReplaceAll(s, referencePlaceholder, reference)
		}
		out[k] = v
	}
	return out
}

// Status returns the caller's payment by uniquecode. Another caller's code is
// reported as not found, never as forbidden.
func (s *Payments) Status(ctx context.Context, who auth.Principal, code string) (View, error) {
	if !uniquecodeRe.MatchString(code) {
		return View{}, domain.ErrPaymentNotFound
	}
	p, err := s.payments.GetByCode(ctx, who.ID, code)
	if err != nil {
		return View{}, err
	}
	return viewOf(p), nil
}

// RequestFingerprint identifies the parameters of a create request (config,
// amount, currency) so an identical repeat is a replay and anything else a conflict.
func RequestFingerprint(configID int64, a domain.Amount) []byte {
	sum := sha256.Sum256([]byte(strconv.FormatInt(configID, 10) + "|" + a.String() + "|" + a.Currency()))
	return sum[:]
}
