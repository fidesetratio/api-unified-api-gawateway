// Package paypaltest is an in-process stand-in for the parts of PayPal's REST API
// the adapter uses. It follows the shapes documented in docs/paypal-research.md.
// It is NOT PayPal: behaviour the documentation leaves open (for example the
// error name on a second capture) is a guess, to be confirmed in the sandbox.
package paypaltest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Server emulates PayPal for one merchant app.
type Server struct {
	*httptest.Server
	ClientID, Secret, WebhookID string

	mu            sync.Mutex
	orders        map[string]*Order
	byRequestID   map[string]string // PayPal-Request-Id -> order id
	next          int
	tokens        int
	issued        string
	CreateStatus  int    // when non-zero, create answers with this HTTP status
	CaptureMode   string // COMPLETED (default), DECLINED, PENDING
	VerifyStatus  int    // when non-zero, verify-webhook-signature answers with this status
	CaptureStatus int    // when non-zero, capture answers with this HTTP status (before touching the order)
	LastCreate    []byte
	LastCreateHdr http.Header
	Creates       int
	Captures      int
	Verifies      int
}

// Order is the server-side state of one order.
type Order struct {
	ID       string
	Status   string
	Amount   string
	Currency string
	Capture  *Capture
}

// Capture is a captured payment.
type Capture struct{ ID, Status string }

// New starts the server.
func New(clientID, secret, webhookID string) *Server {
	s := &Server{ClientID: clientID, Secret: secret, WebhookID: webhookID,
		orders: map[string]*Order{}, byRequestID: map[string]string{}, CaptureMode: "COMPLETED"}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Approve marks an order as approved by the buyer.
func (s *Server) Approve(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[id].Status = "APPROVED"
}

// OrderByID returns a copy of an order.
func (s *Server) OrderByID(id string) (Order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, false
	}
	return *o, true
}

// TokenRequests reports how many OAuth tokens were issued.
func (s *Server) TokenRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens
}

// RevokeToken makes the currently issued token invalid (the next call gets a 401).
func (s *Server) RevokeToken() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issued = ""
}

// Counts returns call counters.
func (s *Server) Counts() (creates, captures, verifies int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Creates, s.Captures, s.Verifies
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func paypalError(w http.ResponseWriter, code int, name, issue string) {
	writeJSON(w, code, map[string]any{
		"name": name, "message": "stub error", "debug_id": "dbg-" + name,
		"details": []map[string]string{{"issue": issue}},
	})
}

func (s *Server) authorized(r *http.Request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issued != "" && r.Header.Get("Authorization") == "Bearer "+s.issued
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/oauth2/token" && r.Method == http.MethodPost {
		s.token(w, r)
		return
	}
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.URL.Path == "/v2/checkout/orders" && r.Method == http.MethodPost:
		s.create(w, r, body)
	case strings.HasPrefix(r.URL.Path, "/v2/checkout/orders/") && strings.HasSuffix(r.URL.Path, "/capture") && r.Method == http.MethodPost:
		s.capture(w, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/checkout/orders/"), "/capture"))
	case strings.HasPrefix(r.URL.Path, "/v2/checkout/orders/") && r.Method == http.MethodGet:
		s.show(w, strings.TrimPrefix(r.URL.Path, "/v2/checkout/orders/"))
	case r.URL.Path == "/v1/notifications/verify-webhook-signature" && r.Method == http.MethodPost:
		s.verify(w, body)
	default:
		paypalError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "UNKNOWN_ROUTE")
	}
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok || id != s.ClientID || secret != s.Secret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client", "error_description": "Client Authentication failed"})
		return
	}
	s.mu.Lock()
	s.tokens++
	s.issued = fmt.Sprintf("A21-token-%d", s.tokens)
	tok := s.issued
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": 32400, "scope": "x"})
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, body []byte) {
	s.mu.Lock()
	s.Creates++
	s.LastCreate, s.LastCreateHdr = body, r.Header.Clone()
	status := s.CreateStatus
	s.mu.Unlock()
	if status != 0 {
		paypalError(w, status, "INTERNAL_SERVER_ERROR", "STUB")
		return
	}
	var req struct {
		PurchaseUnits []struct {
			Amount struct {
				CurrencyCode string `json:"currency_code"`
				Value        string `json:"value"`
			} `json:"amount"`
		} `json:"purchase_units"`
		PaymentSource map[string]any `json:"payment_source"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.PurchaseUnits) != 1 || req.PurchaseUnits[0].Amount.Value == "" {
		paypalError(w, http.StatusBadRequest, "INVALID_REQUEST", "MISSING_REQUIRED_PARAMETER")
		return
	}
	reqID := r.Header.Get("PayPal-Request-Id")

	s.mu.Lock()
	if id, dup := s.byRequestID[reqID]; dup && reqID != "" {
		o := s.orders[id]
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, s.orderView(o)) // idempotent repeat: 200
		return
	}
	s.next++
	o := &Order{ID: fmt.Sprintf("5O190127TN36471%d", s.next), Status: "PAYER_ACTION_REQUIRED",
		Amount: req.PurchaseUnits[0].Amount.Value, Currency: req.PurchaseUnits[0].Amount.CurrencyCode}
	s.orders[o.ID] = o
	if reqID != "" {
		s.byRequestID[reqID] = o.ID
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, s.orderView(o))
}

func (s *Server) capture(w http.ResponseWriter, id string) {
	s.mu.Lock()
	s.Captures++
	if s.CaptureStatus != 0 {
		code := s.CaptureStatus
		s.mu.Unlock()
		paypalError(w, code, "INTERNAL_SERVER_ERROR", "STUB")
		return
	}
	o, ok := s.orders[id]
	if !ok {
		s.mu.Unlock()
		paypalError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "INVALID_RESOURCE_ID")
		return
	}
	if o.Capture != nil {
		s.mu.Unlock()
		paypalError(w, http.StatusUnprocessableEntity, "UNPROCESSABLE_ENTITY", "ORDER_ALREADY_CAPTURED")
		return
	}
	if o.Status != "APPROVED" {
		s.mu.Unlock()
		paypalError(w, http.StatusUnprocessableEntity, "UNPROCESSABLE_ENTITY", "ORDER_NOT_APPROVED")
		return
	}
	o.Capture = &Capture{ID: "CAP-" + o.ID, Status: s.CaptureMode}
	if o.Capture.Status == "COMPLETED" {
		o.Status = "COMPLETED"
	}
	view := s.orderView(o)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, view) // PayPal answers 201 even for a DECLINED capture
}

func (s *Server) show(w http.ResponseWriter, id string) {
	s.mu.Lock()
	o, ok := s.orders[id]
	s.mu.Unlock()
	if !ok {
		paypalError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "INVALID_RESOURCE_ID")
		return
	}
	writeJSON(w, http.StatusOK, s.orderView(o))
}

// orderView renders an order; callers hold no lock on o (read-only here).
func (s *Server) orderView(o *Order) map[string]any {
	v := map[string]any{
		"id": o.ID, "status": o.Status,
		"links": []map[string]string{
			{"href": s.URL + "/v2/checkout/orders/" + o.ID, "rel": "self", "method": "GET"},
			{"href": s.URL + "/checkoutnow?token=" + o.ID, "rel": "payer-action", "method": "GET"},
		},
	}
	if o.Capture != nil {
		v["purchase_units"] = []map[string]any{{"payments": map[string]any{"captures": []map[string]any{{
			"id": o.Capture.ID, "status": o.Capture.Status,
			"amount": map[string]string{"currency_code": o.Currency, "value": o.Amount},
		}}}}}
	}
	return v
}

// Signature is what the stub considers a valid transmission signature: a hash
// over the webhook id and the EXACT bytes of the event. A client that parses and
// re-serializes the event changes the bytes and fails verification, like PayPal.
func Signature(webhookID string, rawEvent []byte) string {
	sum := sha256.Sum256([]byte(webhookID + "|" + string(rawEvent)))
	return hex.EncodeToString(sum[:])
}

// Sign returns the PayPal headers for a delivery of rawEvent to webhookID.
func Sign(webhookID string, rawEvent []byte) http.Header {
	h := http.Header{}
	h.Set("PAYPAL-AUTH-ALGO", "SHA256withRSA")
	h.Set("PAYPAL-CERT-URL", "https://api.sandbox.paypal.com/v1/notifications/certs/CERT-360caa42")
	h.Set("PAYPAL-TRANSMISSION-ID", "tx-1")
	h.Set("PAYPAL-TRANSMISSION-TIME", "2026-10-23T10:00:00Z")
	h.Set("PAYPAL-TRANSMISSION-SIG", Signature(webhookID, rawEvent))
	return h
}

func (s *Server) verify(w http.ResponseWriter, body []byte) {
	s.mu.Lock()
	s.Verifies++
	status := s.VerifyStatus
	s.mu.Unlock()
	if status != 0 {
		paypalError(w, status, "INTERNAL_SERVER_ERROR", "STUB")
		return
	}
	var req struct {
		AuthAlgo         string          `json:"auth_algo"`
		CertURL          string          `json:"cert_url"`
		TransmissionID   string          `json:"transmission_id"`
		TransmissionSig  string          `json:"transmission_sig"`
		TransmissionTime string          `json:"transmission_time"`
		WebhookID        string          `json:"webhook_id"`
		WebhookEvent     json.RawMessage `json:"webhook_event"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.AuthAlgo == "" || req.CertURL == "" ||
		req.TransmissionID == "" || req.TransmissionSig == "" || req.TransmissionTime == "" || len(req.WebhookEvent) == 0 {
		paypalError(w, http.StatusBadRequest, "INVALID_REQUEST", "MISSING_REQUIRED_PARAMETER")
		return
	}
	result := "FAILURE"
	if req.WebhookID == s.WebhookID && req.TransmissionSig == Signature(s.WebhookID, req.WebhookEvent) {
		result = "SUCCESS"
	}
	writeJSON(w, http.StatusOK, map[string]string{"verification_status": result})
}

// ---- event builders (the bytes keep deliberate odd spacing) ----------------

// ApprovedEvent builds a CHECKOUT.ORDER.APPROVED notification.
func ApprovedEvent(eventID, orderID string) []byte {
	return []byte(fmt.Sprintf(`{ "id" : %q,  "event_version":"1.0", "event_type":"CHECKOUT.ORDER.APPROVED",
  "resource_type":"checkout-order", "resource":{"id":%q, "status":"APPROVED"} }`, eventID, orderID))
}

// CaptureEvent builds a PAYMENT.CAPTURE.* notification for an order.
func CaptureEvent(eventID, eventType, orderID, value, currency string) []byte {
	return []byte(fmt.Sprintf(`{ "id":%q, "event_version":"1.0", "event_type":%q, "resource_type":"capture",
  "resource":{ "id":"CAP-%s", "status":"COMPLETED", "amount":{"currency_code":%q,"value":%q},
  "supplementary_data":{"related_ids":{"order_id":%q}} } }`, eventID, eventType, orderID, currency, value, orderID))
}
