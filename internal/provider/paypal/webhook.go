package paypal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
)

// eventJSON is the notification envelope (webhooks/rest sample).
type eventJSON struct {
	ID        string          `json:"id"`
	EventType string          `json:"event_type"`
	Resource  json.RawMessage `json:"resource"`
}

type resourceJSON struct {
	ID                string      `json:"id"`
	Status            string      `json:"status"`
	Amount            *amountJSON `json:"amount"`
	SupplementaryData struct {
		RelatedIDs struct {
			OrderID string `json:"order_id"`
		} `json:"related_ids"`
	} `json:"supplementary_data"`
}

func parseEnvelope(body []byte) (eventJSON, resourceJSON, bool) {
	var ev eventJSON
	if err := json.Unmarshal(body, &ev); err != nil || ev.EventType == "" {
		return eventJSON{}, resourceJSON{}, false
	}
	var res resourceJSON
	_ = json.Unmarshal(ev.Resource, &res)
	return ev, res, true
}

// orderRef is the PayPal order id an event is about: the resource itself for
// order events, the related order for capture events.
func orderRef(eventType string, res resourceJSON) string {
	if eventType == "CHECKOUT.ORDER.APPROVED" || eventType == "CHECKOUT.PAYMENT-APPROVAL.REVERSED" {
		return res.ID
	}
	return res.SupplementaryData.RelatedIDs.OrderID
}

// LocateWebhook implements provider.Locator. The body is not trusted: the result
// only selects which configuration's webhook id the delivery is verified with.
func (a *Adapter) LocateWebhook(body []byte) (string, bool) {
	ev, res, ok := parseEnvelope(body)
	if !ok {
		return "", false
	}
	ref := orderRef(ev.EventType, res)
	return ref, ref != ""
}

// VerifyWebhook authenticates the delivery with PayPal's verify-webhook-signature
// endpoint (postback), passing the original bytes through untouched, and then
// normalizes the event. Failed authenticity is provider.ErrNotVerified; an
// outage of the verify call is a different error so PayPal retries the delivery.
func (a *Adapter) VerifyWebhook(ctx context.Context, req provider.WebhookRequest) (provider.Event, error) {
	webhookID := req.WebhookSecret["webhook_id"].Reveal()
	h := req.Headers
	fields := map[string]string{
		"auth_algo":         h.Get("PAYPAL-AUTH-ALGO"),
		"cert_url":          h.Get("PAYPAL-CERT-URL"),
		"transmission_id":   h.Get("PAYPAL-TRANSMISSION-ID"),
		"transmission_sig":  h.Get("PAYPAL-TRANSMISSION-SIG"),
		"transmission_time": h.Get("PAYPAL-TRANSMISSION-TIME"),
	}
	if webhookID == "" || !json.Valid(req.Body) {
		return provider.Event{}, provider.ErrNotVerified
	}
	for _, v := range fields {
		if v == "" {
			return provider.Event{}, provider.ErrNotVerified
		}
	}
	ev, res, ok := parseEnvelope(req.Body)
	if !ok || ev.ID == "" {
		return provider.Event{}, provider.ErrNotVerified
	}

	payload, err := verifyBody(fields, webhookID, req.Body)
	if err != nil {
		return provider.Event{}, err
	}

	status, body, err := a.api(ctx, req.Environment, req.Credentials, http.MethodPost,
		"/v1/notifications/verify-webhook-signature", nil, payload)
	if err != nil {
		return provider.Event{}, err // outage: not a verdict, let PayPal retry
	}
	if cerr := classify(status, body); cerr != nil {
		if errors.Is(cerr, provider.ErrTimeout) {
			return provider.Event{}, cerr
		}
		return provider.Event{}, provider.ErrNotVerified
	}
	var vr struct {
		VerificationStatus string `json:"verification_status"`
	}
	if err := json.Unmarshal(body, &vr); err != nil || vr.VerificationStatus != "SUCCESS" {
		return provider.Event{}, provider.ErrNotVerified
	}
	return normalize(ev, res)
}

// verifyBody builds the verify-webhook-signature request. PayPal requires
// webhook_event to be the exact bytes received, and json.Marshal would compact a
// json.RawMessage (dropping whitespace), so the body is assembled by hand and the
// event bytes are copied in untouched.
func verifyBody(fields map[string]string, webhookID string, rawEvent []byte) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for _, k := range []string{"auth_algo", "cert_url", "transmission_id", "transmission_sig", "transmission_time"} {
		v, err := json.Marshal(fields[k])
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "%q:%s,", k, v)
	}
	wid, err := json.Marshal(webhookID)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(&b, `"webhook_id":%s,"webhook_event":`, wid)
	b.Write(rawEvent)
	b.WriteByte('}')
	return b.Bytes(), nil
}

// normalize maps a verified PayPal event onto the gateway's model.
func normalize(ev eventJSON, res resourceJSON) (provider.Event, error) {
	out := provider.Event{EventID: ev.ID, ProviderRef: orderRef(ev.EventType, res), RawStatus: ev.EventType}
	switch ev.EventType {
	case "CHECKOUT.ORDER.APPROVED":
		out.Status = domain.StatusPending
		out.Capturable = true
	case "PAYMENT.CAPTURE.COMPLETED":
		out.Status = domain.StatusPaid
		out.Amount = captureAmount(res)
	case "PAYMENT.CAPTURE.DECLINED", "PAYMENT.CAPTURE.DENIED":
		out.Status = domain.StatusFailed
	default:
		// PENDING, REFUNDED, REVERSED and everything else: acknowledged, no effect yet.
		out.Ignore = true
	}
	if out.ProviderRef == "" && !out.Ignore {
		return provider.Event{}, fmt.Errorf("%w: event without order reference", provider.ErrNotVerified)
	}
	return out, nil
}

func captureAmount(res resourceJSON) *domain.Amount {
	if res.Amount == nil {
		return nil
	}
	amt, err := domain.ParseAmount(res.Amount.Value, res.Amount.CurrencyCode)
	if err != nil {
		return nil
	}
	return &amt
}
