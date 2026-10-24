package service

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"unifiedgw/internal/auth"
	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider/midtrans"
	"unifiedgw/internal/provider/paypal"
	"unifiedgw/internal/provider/stripe"
	"unifiedgw/internal/provider/xendit"
	"unifiedgw/internal/secrets"
	"unifiedgw/internal/store"
)

// Admin manages provider configs and grants. Secrets are write-only: nothing
// here ever returns credential values.
type Admin struct {
	configs   *store.Configs
	clients   *store.Clients
	keys      *secrets.KeyRing
	allowFake bool
}

// NewAdmin wires the admin service. allowFake enables the "fake" provider.
func NewAdmin(c *store.Configs, cl *store.Clients, keys *secrets.KeyRing, allowFake bool) *Admin {
	return &Admin{configs: c, clients: cl, keys: keys, allowFake: allowFake}
}

// fieldSpec lists which credential keys a provider takes.
type fieldSpec struct {
	credRequired   []string
	credAllowed    []string
	webhookAllowed []string
}

var providerSpecs = map[string]fieldSpec{
	"paypal": {
		credRequired:   []string{"client_id", "client_secret"},
		credAllowed:    []string{"client_id", "client_secret"},
		webhookAllowed: []string{"webhook_id"},
	},
	"xendit": {
		credRequired:   []string{"api_key"},
		credAllowed:    []string{"api_key"},
		webhookAllowed: []string{"callback_token"},
	},
	"midtrans": {
		credRequired: []string{"server_key"},
		credAllowed:  []string{"server_key"},
		// No webhook secret: notifications are signed with the Server Key itself.
	},
	"stripe": {
		credRequired:   []string{"api_key"},
		credAllowed:    []string{"api_key"},
		webhookAllowed: []string{"signing_secret"},
	},
	"fake": {
		credAllowed:    []string{"client_secret", "token"},
		webhookAllowed: []string{"token"},
	},
}

const (
	maxFieldLen = 512
	maxNameLen  = 100
)

func invalid(format string, args ...any) *domain.Error {
	return domain.NewError(http.StatusBadRequest, "invalid_request", fmt.Sprintf(format, args...))
}

// CreateConfigInput is the admin request to add a provider configuration.
type CreateConfigInput struct {
	Provider          string
	Environment       string
	Name              string
	AllowedCurrencies []string
	Settings          map[string]any
	Credentials       map[string]string
	WebhookSecret     map[string]string
}

func (a *Admin) providerSpec(name string) (fieldSpec, error) {
	if name == "fake" && !a.allowFake {
		return fieldSpec{}, invalid("provider %q is not available in this environment", name)
	}
	spec, ok := providerSpecs[name]
	if !ok {
		return fieldSpec{}, invalid("provider must be one of paypal, xendit, stripe, midtrans")
	}
	return spec, nil
}

// validateSecrets checks field names and sizes without ever echoing values.
func validateSecrets(kind string, got map[string]string, required, allowed []string) error {
	for k, v := range got {
		if !slices.Contains(allowed, k) {
			return invalid("%s field %q is not accepted for this provider", kind, k)
		}
		if strings.TrimSpace(v) == "" || len(v) > maxFieldLen {
			return invalid("%s field %q must be 1-%d characters", kind, k, maxFieldLen)
		}
	}
	for _, k := range required {
		if got[k] == "" {
			return invalid("%s field %q is required", kind, k)
		}
	}
	return nil
}

func validateCurrencies(list []string) error {
	if len(list) == 0 || len(list) > 50 {
		return invalid("allowed_currencies must list 1-50 currencies")
	}
	for _, c := range list {
		if domain.ValidateCurrency(c) != nil {
			return invalid("allowed_currencies contains an invalid currency code")
		}
	}
	return nil
}

func actorOf(p auth.Principal) store.Actor {
	return store.Actor{Type: "admin", ID: fmt.Sprintf("%s#%d", p.Name, p.ID)}
}

// CreateConfig validates and stores a config with encrypted credentials.
func (a *Admin) CreateConfig(ctx context.Context, who auth.Principal, in CreateConfigInput) (store.Config, error) {
	spec, err := a.providerSpec(in.Provider)
	if err != nil {
		return store.Config{}, err
	}
	if in.Environment != "sandbox" && in.Environment != "production" {
		return store.Config{}, invalid("environment must be sandbox or production")
	}
	if n := strings.TrimSpace(in.Name); n == "" || len(n) > maxNameLen {
		return store.Config{}, invalid("name must be 1-%d characters", maxNameLen)
	}
	if err := validateCurrencies(in.AllowedCurrencies); err != nil {
		return store.Config{}, err
	}
	if err := validateForProvider(in.Provider, in.AllowedCurrencies, in.Settings); err != nil {
		return store.Config{}, err
	}
	if err := validateSecrets("credentials", in.Credentials, spec.credRequired, spec.credAllowed); err != nil {
		return store.Config{}, err
	}
	if err := validateSecrets("webhook_secret", in.WebhookSecret, nil, spec.webhookAllowed); err != nil {
		return store.Config{}, err
	}
	return a.configs.Create(ctx, store.NewConfig{
		Name: strings.TrimSpace(in.Name), Provider: in.Provider, Environment: in.Environment,
		AllowedCurrencies: in.AllowedCurrencies, Settings: in.Settings,
	}, NewSealer(a.keys, in.Provider, in.Credentials, in.WebhookSecret), actorOf(who))
}

// RotateCredentials stores a new credential version for an existing config.
func (a *Admin) RotateCredentials(ctx context.Context, who auth.Principal, id int64, cred, webhook map[string]string) (int, error) {
	cfg, err := a.configs.Get(ctx, id)
	if err != nil {
		return 0, err
	}
	spec, err := a.providerSpec(cfg.Provider)
	if err != nil {
		return 0, err
	}
	if err := validateSecrets("credentials", cred, spec.credRequired, spec.credAllowed); err != nil {
		return 0, err
	}
	if err := validateSecrets("webhook_secret", webhook, nil, spec.webhookAllowed); err != nil {
		return 0, err
	}
	return a.configs.RotateCredentials(ctx, id, NewSealer(a.keys, cfg.Provider, cred, webhook), actorOf(who))
}

// UpdateConfigInput carries optional changes.
type UpdateConfigInput struct {
	Name              *string
	AllowedCurrencies []string
	Settings          map[string]any
	Status            *string
}

// UpdateConfig changes non-secret fields or enables/disables a config.
func (a *Admin) UpdateConfig(ctx context.Context, who auth.Principal, id int64, in UpdateConfigInput) (store.Config, error) {
	if in.Name != nil {
		if n := strings.TrimSpace(*in.Name); n == "" || len(n) > maxNameLen {
			return store.Config{}, invalid("name must be 1-%d characters", maxNameLen)
		}
	}
	if in.AllowedCurrencies != nil {
		if err := validateCurrencies(in.AllowedCurrencies); err != nil {
			return store.Config{}, err
		}
	}
	if in.Status != nil && *in.Status != "active" && *in.Status != "disabled" {
		return store.Config{}, invalid("status must be active or disabled")
	}
	if in.AllowedCurrencies != nil || in.Settings != nil {
		cfg, err := a.configs.Get(ctx, id)
		if err != nil {
			return store.Config{}, err
		}
		if err := validateForProvider(cfg.Provider, in.AllowedCurrencies, in.Settings); err != nil {
			return store.Config{}, err
		}
	}
	return a.configs.Update(ctx, id, store.ConfigUpdate{
		Name: in.Name, AllowedCurrencies: in.AllowedCurrencies, Settings: in.Settings, Status: in.Status,
	}, actorOf(who))
}

// GetConfig returns a config without secrets.
func (a *Admin) GetConfig(ctx context.Context, id int64) (store.Config, error) {
	return a.configs.Get(ctx, id)
}

// ListConfigs returns all configs without secrets.
func (a *Admin) ListConfigs(ctx context.Context) ([]store.Config, error) { return a.configs.List(ctx) }

// Grant lets a caller client use a config.
func (a *Admin) Grant(ctx context.Context, who auth.Principal, configID, clientID int64) error {
	if err := a.checkGrantTargets(ctx, configID, clientID); err != nil {
		return err
	}
	return a.configs.Grant(ctx, configID, clientID, actorOf(who))
}

// Revoke removes a caller client's access to a config.
func (a *Admin) Revoke(ctx context.Context, who auth.Principal, configID, clientID int64) error {
	if err := a.checkGrantTargets(ctx, configID, clientID); err != nil {
		return err
	}
	return a.configs.Revoke(ctx, configID, clientID, actorOf(who))
}

func (a *Admin) checkGrantTargets(ctx context.Context, configID, clientID int64) error {
	if _, err := a.configs.Get(ctx, configID); err != nil {
		return err
	}
	role, ok, err := a.clients.RoleOf(ctx, clientID)
	if err != nil {
		return err
	}
	if !ok {
		return domain.NewError(http.StatusNotFound, "client_not_found", "client not found")
	}
	if role != auth.RoleCaller {
		return invalid("only caller clients can be granted configurations")
	}
	return nil
}

// validateForProvider applies provider-specific rules to currencies and settings.
// Nil arguments are skipped (partial updates).
func validateForProvider(providerName string, currencies []string, settings map[string]any) error {
	if providerName == "xendit" {
		return validateXendit(currencies, settings)
	}
	if providerName == "stripe" {
		return validateStripe(currencies, settings)
	}
	if providerName == "midtrans" {
		return validateMidtrans(currencies, settings)
	}
	if providerName != "paypal" {
		return nil
	}
	for _, c := range currencies {
		if !paypal.SupportsCurrency(c) {
			return invalid("PayPal does not accept %s for payments; remove it from allowed_currencies", c)
		}
	}
	for _, key := range []string{"return_url", "cancel_url"} {
		if v, present := settings[key]; present {
			s, _ := v.(string)
			if !paypal.ValidSettingURL(s) {
				return invalid("settings.%s must be an absolute http(s) URL", key)
			}
		}
	}
	if v, present := settings["expiry_minutes"]; present {
		n, ok := v.(float64)
		if !ok || n < 1 || n > 60*24*30 {
			return invalid("settings.expiry_minutes must be a number of minutes between 1 and 43200")
		}
	}
	return nil
}

func validateXendit(currencies []string, settings map[string]any) error {
	for _, c := range currencies {
		if !xendit.SupportsCurrency(c) {
			return invalid("Xendit Payment Sessions do not accept %s; remove it from allowed_currencies", c)
		}
	}
	for _, key := range []string{"return_url", "cancel_url"} {
		if v, present := settings[key]; present {
			s, _ := v.(string)
			if !xendit.ValidSettingURL(s) {
				return invalid("settings.%s must be an absolute https URL (Xendit requires HTTPS)", key)
			}
		}
	}
	if v, present := settings["country"]; present {
		s, _ := v.(string)
		if !xendit.SupportedCountry(s) {
			return invalid("settings.country must be one of ID, PH, VN, TH, SG, MY")
		}
	}
	if v, present := settings["expiry_minutes"]; present {
		n, ok := v.(float64)
		if !ok || n < 10 || n > 60*24*30 {
			return invalid("settings.expiry_minutes must be between 10 and 43200 (Xendit needs at least 10 minutes)")
		}
	}
	return nil
}

func validateStripe(currencies []string, settings map[string]any) error {
	for _, c := range currencies {
		if !stripe.SupportsCurrency(c) {
			return invalid("the Stripe adapter does not accept %s yet; remove it from allowed_currencies", c)
		}
	}
	for _, key := range []string{"return_url", "cancel_url"} {
		if v, present := settings[key]; present {
			s, _ := v.(string)
			if !stripe.ValidSettingURL(s) {
				return invalid("settings.%s must be an absolute http(s) URL", key)
			}
		}
	}
	if v, present := settings["expiry_minutes"]; present {
		n, ok := v.(float64)
		if !ok || n < 30 || n > 24*60 {
			return invalid("settings.expiry_minutes must be between 30 and 1440 (Stripe allows 30 minutes to 24 hours)")
		}
	}
	return nil
}

func validateMidtrans(currencies []string, settings map[string]any) error {
	for _, c := range currencies {
		if !midtrans.SupportsCurrency(c) {
			return invalid("Midtrans Snap takes IDR only; remove %s from allowed_currencies", c)
		}
	}
	for _, key := range []string{"return_url", "cancel_url"} {
		if v, present := settings[key]; present {
			s, _ := v.(string)
			if !midtrans.ValidSettingURL(s) {
				return invalid("settings.%s must be an absolute http(s) URL of at most 255 characters", key)
			}
		}
	}
	if v, present := settings["expiry_minutes"]; present {
		n, ok := v.(float64)
		if !ok || n < 1 || n > 7*24*60 {
			return invalid("settings.expiry_minutes must be between 1 and 10080 (Midtrans allows at most 7 days)")
		}
	}
	return nil
}
