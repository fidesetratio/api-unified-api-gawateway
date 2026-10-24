package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
)

// signatureTolerance is Stripe's documented library default.
const signatureTolerance = 5 * time.Minute

type eventJSON struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object struct {
			ID string `json:"id"`
		} `json:"object"`
	} `json:"data"`
}

func parseEvent(body []byte) (eventJSON, bool) {
	var e eventJSON
	if err := json.Unmarshal(body, &e); err != nil || e.Type == "" {
		return eventJSON{}, false
	}
	return e, true
}

// LocateWebhook implements provider.Locator. The body is unverified here: the
// result only chooses which configuration's signing secret to verify with.
func (a *Adapter) LocateWebhook(body []byte) (string, bool) {
	e, ok := parseEvent(body)
	if !ok || !strings.HasPrefix(e.Type, "checkout.session.") || e.Data.Object.ID == "" {
		return "", false
	}
	return e.Data.Object.ID, true
}

// verifySignature implements Stripe's documented manual check: the header holds
// t=<timestamp> and one or more v1=<hex hmac>; the signed payload is
// "<t>.<raw body>"; the key is the endpoint signing secret; any matching v1 passes
// (several appear while a secret is being rotated); other schemes are ignored;
// the timestamp must be within the tolerance in both directions.
func verifySignature(secret, header string, body []byte, now time.Time) bool {
	if secret == "" || header == "" {
		return false
	}
	var tsText string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			tsText = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	ts, err := strconv.ParseInt(tsText, 10, 64)
	if err != nil || len(sigs) == 0 {
		return false
	}
	age := now.Sub(time.Unix(ts, 0))
	if age > signatureTolerance || age < -signatureTolerance {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(tsText))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := mac.Sum(nil)
	for _, s := range sigs {
		got, err := hex.DecodeString(s)
		if err == nil && hmac.Equal(expected, got) { // constant time
			return true
		}
	}
	return false
}

// VerifyWebhook authenticates the delivery with the Stripe-Signature header, then
// maps the event. For payment events the session is read back from the API so the
// outcome and amount come from an authenticated call, as Stripe's fulfillment
// guide advises.
func (a *Adapter) VerifyWebhook(ctx context.Context, req provider.WebhookRequest) (provider.Event, error) {
	secret := req.WebhookSecret["signing_secret"].Reveal()
	if !verifySignature(secret, req.Headers.Get("Stripe-Signature"), req.Body, a.opt.Now()) {
		return provider.Event{}, provider.ErrNotVerified
	}
	e, ok := parseEvent(req.Body)
	if !ok || e.ID == "" {
		return provider.Event{}, provider.ErrNotVerified
	}
	ref := e.Data.Object.ID
	ev := provider.Event{EventID: e.ID, ProviderRef: ref, RawStatus: e.Type}

	switch e.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded":
		res, err := a.resolve(ctx, req.Credentials, ref)
		if err != nil {
			return provider.Event{}, err // outage or unknown: Stripe will redeliver
		}
		if res.Status != domain.StatusPaid {
			// completed but unpaid: a delayed payment method; async_payment_succeeded settles it.
			ev.Ignore = true
			return ev, nil
		}
		ev.Status, ev.Amount, ev.RawStatus = domain.StatusPaid, res.Amount, e.Type+" ("+res.RawStatus+")"
	case "checkout.session.async_payment_failed":
		ev.Status = domain.StatusFailed
	case "checkout.session.expired":
		ev.Status = domain.StatusExpired
	default:
		ev.Ignore = true
		return ev, nil
	}
	if ref == "" {
		return provider.Event{}, provider.ErrNotVerified
	}
	return ev, nil
}
