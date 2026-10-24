package xendit

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"strings"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
)

// Session events the gateway reacts to (docs: the only session webhooks).
const (
	eventCompleted = "payment_session.completed"
	eventExpired   = "payment_session.expired"
)

type webhookBody struct {
	Event string `json:"event"`
	Data  struct {
		SessionID string `json:"payment_session_id"`
	} `json:"data"`
}

func parseWebhook(body []byte) (webhookBody, bool) {
	var w webhookBody
	if err := json.Unmarshal(body, &w); err != nil || w.Event == "" {
		return webhookBody{}, false
	}
	return w, true
}

// LocateWebhook implements provider.Locator. Only session events carry a session
// id; payment.* deliveries cannot be attributed and are acknowledged by the caller.
func (a *Adapter) LocateWebhook(body []byte) (string, bool) {
	w, ok := parseWebhook(body)
	if !ok || !strings.HasPrefix(w.Event, "payment_session.") || w.Data.SessionID == "" {
		return "", false
	}
	return w.Data.SessionID, true
}

// VerifyWebhook checks the optional static x-callback-token and then reads the
// truth from Xendit's API with the secret key.
//
// Xendit documents no signature and no event id, and it is unclear whether
// session webhooks even carry x-callback-token. So: when the config stores a
// callback_token the header must match it (constant time); and in every case the
// reported outcome comes from the authenticated API read, never from the body. A
// forged delivery can at most cause one extra read of a real session.
func (a *Adapter) VerifyWebhook(ctx context.Context, req provider.WebhookRequest) (provider.Event, error) {
	if want := req.WebhookSecret["callback_token"].Reveal(); want != "" {
		got := req.Headers.Get("x-callback-token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			return provider.Event{}, provider.ErrNotVerified
		}
	}
	w, ok := parseWebhook(req.Body)
	if !ok {
		return provider.Event{}, provider.ErrNotVerified
	}
	if (w.Event != eventCompleted && w.Event != eventExpired) || w.Data.SessionID == "" {
		return provider.Event{Ignore: true, EventID: "ignored:" + w.Event, RawStatus: w.Event}, nil
	}

	res, retryable, err := a.resolve(ctx, req.Credentials, w.Data.SessionID)
	if err != nil {
		return provider.Event{}, err // outage or unknown: Xendit will redeliver
	}
	if retryable {
		return provider.Event{}, fmt.Errorf("%w: session completed but payment not settled yet", provider.ErrTimeout)
	}
	if res.Status == domain.StatusPending {
		// The session is still open (a stale or early delivery): nothing to apply.
		return provider.Event{Ignore: true, EventID: "ignored:" + w.Event + ":" + w.Data.SessionID, ProviderRef: w.Data.SessionID, RawStatus: res.RawStatus}, nil
	}
	// No delivery id exists, so the key is the session, its final state and, for a
	// payment, the amount read. A redelivery of the same evidence repeats the key
	// (a duplicate), while different evidence (say, more money captured later)
	// gets its own key instead of being swallowed by an earlier mismatch.
	eventID := "session:" + w.Data.SessionID + ":" + string(res.Status)
	if res.Amount != nil {
		eventID += ":" + res.Amount.String() + res.Amount.Currency()
	}
	return provider.Event{
		EventID:     eventID,
		ProviderRef: w.Data.SessionID,
		Status:      res.Status,
		RawStatus:   res.RawStatus,
		Amount:      res.Amount,
	}, nil
}
