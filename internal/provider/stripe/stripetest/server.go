// Package stripetest is an in-process stand-in for the parts of Stripe's API the
// adapter uses, following the shapes in docs/stripe-research.md. It is NOT Stripe:
// anything the documentation leaves open is a guess to confirm in test mode.
package stripetest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// Server emulates Stripe for one account.
type Server struct {
	*httptest.Server
	APIKey string

	mu          sync.Mutex
	sessions    map[string]*Session
	byKey       map[string]string     // Idempotency-Key -> session id
	keyParams   map[string]url.Values // Idempotency-Key -> original params
	next        int
	Creates     int
	LastForm    url.Values
	LastHeaders http.Header
	// CreateStatus makes POST /v1/checkout/sessions answer with this status/type.
	CreateStatus int
	CreateType   string
	ReadStatus   int
	Reads        int
}

// Session is the server-side state of a Checkout Session.
type Session struct {
	ID, Status, PaymentStatus, Currency string
	AmountTotal                         int64
}

// New starts the server.
func New(apiKey string) *Server {
	s := &Server{APIKey: apiKey, sessions: map[string]*Session{}, byKey: map[string]string{}, keyParams: map[string]url.Values{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Pay marks a session complete and paid.
func (s *Server) Pay(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id].Status, s.sessions[id].PaymentStatus = "complete", "paid"
}

// CompleteUnpaid marks a session complete but not yet paid (delayed payment method).
func (s *Server) CompleteUnpaid(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id].Status, s.sessions[id].PaymentStatus = "complete", "unpaid"
}

// Expire marks a session expired.
func (s *Server) Expire(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id].Status, s.sessions[id].PaymentStatus = "expired", "unpaid"
}

// SetAmountTotal changes the amount_total Stripe reports for a session.
func (s *Server) SetAmountTotal(id string, minor int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id].AmountTotal = minor
}

// Counts returns the number of creates and reads.
func (s *Server) Counts() (creates, reads int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Creates, s.Reads
}

// SessionIDs lists the sessions created so far.
func (s *Server) SessionIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		out = append(out, id)
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func serr(w http.ResponseWriter, code int, typ, errCode string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"type": typ, "code": errCode, "message": "stub error"}})
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != s.APIKey || pass != "" {
		serr(w, http.StatusUnauthorized, "invalid_request_error", "api_key_invalid")
		return
	}
	raw, _ := io.ReadAll(r.Body)
	switch {
	case r.URL.Path == "/v1/checkout/sessions" && r.Method == http.MethodPost:
		s.create(w, r, string(raw))
	case strings.HasPrefix(r.URL.Path, "/v1/checkout/sessions/") && r.Method == http.MethodGet:
		s.get(w, strings.TrimPrefix(r.URL.Path, "/v1/checkout/sessions/"))
	default:
		serr(w, http.StatusNotFound, "invalid_request_error", "resource_missing")
	}
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, raw string) {
	form, err := url.ParseQuery(raw)
	if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || err != nil {
		serr(w, http.StatusBadRequest, "invalid_request_error", "parameter_invalid_empty")
		return
	}
	s.mu.Lock()
	s.Creates++
	s.LastForm, s.LastHeaders = form, r.Header.Clone()
	status, typ := s.CreateStatus, s.CreateType
	s.mu.Unlock()
	if status != 0 {
		serr(w, status, typ, "stub")
		return
	}
	unit, perr := strconv.ParseInt(form.Get("line_items[0][price_data][unit_amount]"), 10, 64)
	if form.Get("mode") != "payment" || form.Get("success_url") == "" || perr != nil || unit <= 0 ||
		form.Get("line_items[0][price_data][currency]") == "" || form.Get("line_items[0][quantity]") != "1" {
		serr(w, http.StatusBadRequest, "invalid_request_error", "parameter_missing")
		return
	}

	key := r.Header.Get("Idempotency-Key")
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, seen := s.byKey[key]; seen && key != "" {
		if form.Encode() != s.keyParams[key].Encode() {
			serr(w, http.StatusBadRequest, "idempotency_error", "idempotency_key_in_use")
			return
		}
		writeJSON(w, http.StatusOK, s.view(s.sessions[id])) // saved response
		return
	}
	s.next++
	id := fmt.Sprintf("cs_test_%024d", s.next)
	ses := &Session{ID: id, Status: "open", PaymentStatus: "unpaid",
		Currency: form.Get("line_items[0][price_data][currency]"), AmountTotal: unit}
	s.sessions[id] = ses
	if key != "" {
		s.byKey[key], s.keyParams[key] = id, form
	}
	writeJSON(w, http.StatusOK, s.view(ses))
}

func (s *Server) view(ses *Session) map[string]any {
	v := map[string]any{
		"id": ses.ID, "object": "checkout.session", "status": ses.Status, "payment_status": ses.PaymentStatus,
		"amount_total": ses.AmountTotal, "currency": ses.Currency, "expires_at": 1893456000,
	}
	if ses.Status == "open" {
		v["url"] = "https://checkout.stripe.com/c/pay/" + ses.ID
	} else {
		v["url"] = nil
	}
	return v
}

func (s *Server) get(w http.ResponseWriter, id string) {
	s.mu.Lock()
	s.Reads++
	status := s.ReadStatus
	ses := s.sessions[id]
	var v map[string]any
	if ses != nil {
		v = s.view(ses)
	}
	s.mu.Unlock()
	if status != 0 {
		serr(w, status, "api_error", "stub")
		return
	}
	if ses == nil {
		serr(w, http.StatusNotFound, "invalid_request_error", "resource_missing")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ---- webhook helpers -------------------------------------------------------

// Sign builds a Stripe-Signature header for body at unix time ts with the given
// signing secrets (several secrets give several v1 entries, like a rotation).
func Sign(ts int64, body []byte, secrets ...string) http.Header {
	parts := []string{"t=" + strconv.FormatInt(ts, 10)}
	for _, sec := range secrets {
		mac := hmac.New(sha256.New, []byte(sec))
		mac.Write([]byte(strconv.FormatInt(ts, 10) + "."))
		mac.Write(body)
		parts = append(parts, "v1="+hex.EncodeToString(mac.Sum(nil)))
	}
	parts = append(parts, "v0=deadbeef") // test-mode extra scheme, must be ignored
	h := http.Header{}
	h.Set("Stripe-Signature", strings.Join(parts, ","))
	return h
}

// Event builds a webhook event body for a checkout session (with odd spacing so
// a client that re-serializes the body would break the signature).
func Event(eventID, eventType, sessionID string) []byte {
	return []byte(fmt.Sprintf(`{ "id" : %q,  "object":"event", "type":%q, "livemode":false,
  "data":{ "object":{ "id":%q, "object":"checkout.session", "payment_status":"paid", "amount_total":1 } } }`, eventID, eventType, sessionID))
}
