package midtrans

import (
	"context"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"strings"

	"unifiedgw/internal/provider"
)

type notificationJSON struct {
	OrderID      string     `json:"order_id"`
	StatusCode   flexString `json:"status_code"`
	GrossAmount  flexString `json:"gross_amount"`
	SignatureKey string     `json:"signature_key"`
}

func parseNotification(body []byte) (notificationJSON, bool) {
	var n notificationJSON
	if err := json.Unmarshal(body, &n); err != nil || n.OrderID == "" {
		return notificationJSON{}, false
	}
	return n, true
}

// LocateWebhook implements provider.Locator. The body is unverified: the result
// only chooses which configuration's Server Key the notification is checked with.
func (a *Adapter) LocateWebhook(body []byte) (string, bool) {
	n, ok := parseNotification(body)
	if !ok || !orderIDRe.MatchString(n.OrderID) {
		return "", false
	}
	return n.OrderID, true
}

// signatureMatches checks signature_key = SHA512(order_id + status_code +
// gross_amount + ServerKey) in constant time. The pieces are used exactly as
// they appear in the notification (gross_amount is often "10000.00").
func signatureMatches(n notificationJSON, serverKey string) bool {
	if serverKey == "" || n.SignatureKey == "" {
		return false
	}
	sum := sha512.Sum512([]byte(n.OrderID + string(n.StatusCode) + string(n.GrossAmount) + serverKey))
	want := hex.EncodeToString(sum[:])
	got := strings.ToLower(strings.TrimSpace(n.SignatureKey))
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// VerifyWebhook authenticates the notification with its signature, then reads
// the order from the status API; the outcome and the amount come from that read.
func (a *Adapter) VerifyWebhook(ctx context.Context, req provider.WebhookRequest) (provider.Event, error) {
	n, ok := parseNotification(req.Body)
	if !ok || !orderIDRe.MatchString(n.OrderID) || n.StatusCode == "" || n.GrossAmount == "" {
		return provider.Event{}, provider.ErrNotVerified
	}
	if !signatureMatches(n, req.Credentials["server_key"].Reveal()) {
		return provider.Event{}, provider.ErrNotVerified
	}

	s, found, err := a.readStatus(ctx, req.Credentials, req.Environment, n.OrderID)
	if err != nil {
		return provider.Event{}, err // outage or unknown: Midtrans will retry
	}
	ev := provider.Event{ProviderRef: n.OrderID}
	if !found {
		ev.Ignore, ev.EventID, ev.RawStatus = true, "ignored:nopayment:"+n.OrderID, "no_payment_yet"
		return ev, nil
	}
	res, ignore, err := outcome(s)
	if err != nil {
		return provider.Event{}, err
	}
	// No notification id exists, so the key is the order and the state that was read.
	ev.EventID = "order:" + n.OrderID + ":" + s.TransactionStatus + ":" + s.FraudStatus + ":" + string(s.GrossAmount)
	ev.RawStatus = res.RawStatus
	if ignore || res.Status == "pending" {
		ev.Ignore = true // nothing to apply (pending, challenge, authorize, refunds)
		return ev, nil
	}
	ev.Status, ev.Amount = res.Status, res.Amount
	return ev, nil
}
