// Package xendittest is an in-process stand-in for the parts of Xendit's Payment
// Session API the adapter uses, following the shapes documented in
// docs/xendit-research.md. It is NOT Xendit: anything the documentation leaves
// open is a guess to be confirmed with a real account.
package xendittest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Server emulates Xendit for one account.
type Server struct {
	*httptest.Server
	APIKey string

	mu         sync.Mutex
	sessions   map[string]*Session
	payments   map[string]*Payment
	next       int
	Creates    int
	LastCreate []byte
	// CreateStatus makes POST /sessions answer with this HTTP status (and CreateCode).
	CreateStatus int
	CreateCode   string
	// ReadStatus makes GET requests answer with this HTTP status.
	ReadStatus int
	// RejectDuplicateReference answers 409 DUPLICATE_ERROR for a repeated reference_id.
	RejectDuplicateReference bool
	Reads                    int
}

// Session is the server-side state of a session.
type Session struct {
	ID, Reference, Status, Currency, Amount, PaymentID, Link string
}

// Payment is the server-side state of a payment.
type Payment struct {
	ID, Status, Currency string
	Captures             []string // capture_amount values as JSON number text
}

// New starts the server.
func New(apiKey string) *Server {
	s := &Server{APIKey: apiKey, sessions: map[string]*Session{}, payments: map[string]*Payment{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func xerr(w http.ResponseWriter, code int, errorCode string) {
	writeJSON(w, code, map[string]any{"error_code": errorCode, "message": "stub error"})
}

// Complete marks a session paid: session COMPLETED, payment SUCCEEDED, with the
// given captured amounts (a partial capture is simply a smaller number).
func (s *Server) Complete(id, currency string, captures ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ses := s.sessions[id]
	ses.Status, ses.PaymentID = "COMPLETED", "py-"+id
	s.payments[ses.PaymentID] = &Payment{ID: ses.PaymentID, Status: "SUCCEEDED", Currency: currency, Captures: captures}
}

// SetPaymentStatus changes the status of a session's payment.
func (s *Server) SetPaymentStatus(id, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.payments["py-"+id]; p != nil {
		p.Status = status
	}
}

// SetSessionStatus changes a session's status (EXPIRED, CANCELED, ...).
func (s *Server) SetSessionStatus(id, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id].Status = status
}

// CompleteWithoutPayment marks a session COMPLETED before its payment exists.
func (s *Server) CompleteWithoutPayment(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id].Status = "COMPLETED"
}

// CreateCount reports POST /sessions calls.
func (s *Server) CreateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Creates
}

// ReadCount reports GET calls.
func (s *Server) ReadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Reads
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != s.APIKey || pass != "" {
		xerr(w, http.StatusUnauthorized, "INVALID_API_KEY")
		return
	}
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.URL.Path == "/sessions" && r.Method == http.MethodPost:
		s.create(w, body)
	case strings.HasPrefix(r.URL.Path, "/sessions/") && r.Method == http.MethodGet:
		s.getSession(w, strings.TrimPrefix(r.URL.Path, "/sessions/"))
	case strings.HasPrefix(r.URL.Path, "/v3/payments/") && r.Method == http.MethodGet:
		s.getPayment(w, r, strings.TrimPrefix(r.URL.Path, "/v3/payments/"))
	default:
		xerr(w, http.StatusNotFound, "DATA_NOT_FOUND")
	}
}

func (s *Server) create(w http.ResponseWriter, body []byte) {
	s.mu.Lock()
	s.Creates++
	s.LastCreate = body
	status, code := s.CreateStatus, s.CreateCode
	s.mu.Unlock()
	if status != 0 {
		xerr(w, status, code)
		return
	}
	var req struct {
		ReferenceID string          `json:"reference_id"`
		SessionType string          `json:"session_type"`
		Mode        string          `json:"mode"`
		Amount      json.RawMessage `json:"amount"`
		Currency    string          `json:"currency"`
		Country     string          `json:"country"`
		SuccessURL  string          `json:"success_return_url"`
		CancelURL   string          `json:"cancel_return_url"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.ReferenceID == "" || req.SessionType != "PAY" || req.Mode != "PAYMENT_LINK" ||
		len(req.Amount) == 0 || req.Amount[0] == '"' || req.Currency == "" || req.Country == "" {
		xerr(w, http.StatusBadRequest, "API_VALIDATION_ERROR")
		return
	}
	if !strings.HasPrefix(req.SuccessURL, "https://") || !strings.HasPrefix(req.CancelURL, "https://") {
		xerr(w, http.StatusBadRequest, "INVALID_URL")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.RejectDuplicateReference {
		for _, ses := range s.sessions {
			if ses.Reference == req.ReferenceID {
				xerr(w, http.StatusConflict, "DUPLICATE_ERROR")
				return
			}
		}
	}
	s.next++
	id := fmt.Sprintf("ps-stub%022d", s.next)
	ses := &Session{ID: id, Reference: req.ReferenceID, Status: "ACTIVE", Currency: req.Currency,
		Amount: string(req.Amount), Link: "https://checkout-staging.xendit.co/web/" + id}
	s.sessions[id] = ses
	writeJSON(w, http.StatusCreated, s.sessionView(ses))
}

func (s *Server) sessionView(ses *Session) map[string]any {
	v := map[string]any{
		"payment_session_id": ses.ID, "reference_id": ses.Reference, "status": ses.Status,
		"currency": ses.Currency, "payment_link_url": ses.Link, "expires_at": "2027-01-01T00:00:00Z",
	}
	if ses.PaymentID != "" {
		v["payment_id"] = ses.PaymentID
	}
	return v
}

func (s *Server) getSession(w http.ResponseWriter, id string) {
	s.mu.Lock()
	s.Reads++
	status := s.ReadStatus
	ses := s.sessions[id]
	var view map[string]any
	if ses != nil {
		view = s.sessionView(ses)
	}
	s.mu.Unlock()
	if status != 0 {
		xerr(w, status, "SERVER_ERROR")
		return
	}
	if ses == nil {
		xerr(w, http.StatusNotFound, "DATA_NOT_FOUND")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) getPayment(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	s.Reads++
	status := s.ReadStatus
	p := s.payments[id]
	s.mu.Unlock()
	if status != 0 {
		xerr(w, status, "SERVER_ERROR")
		return
	}
	if p == nil {
		xerr(w, http.StatusNotFound, "DATA_NOT_FOUND")
		return
	}
	caps := make([]json.RawMessage, 0, len(p.Captures))
	for _, c := range p.Captures {
		caps = append(caps, json.RawMessage(fmt.Sprintf(`{"capture_id":"cptr-%s","capture_amount":%s}`, id, c)))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"payment_id": p.ID, "status": p.Status, "currency": p.Currency, "captures": caps,
		"api_version_seen": r.Header.Get("api-version"),
	})
}

// SessionEvent builds a payment_session.* webhook body. claimedStatus and
// claimedAmount are what the (untrusted) body says; the adapter must ignore them.
func SessionEvent(event, sessionID, claimedStatus string) []byte {
	return []byte(fmt.Sprintf(`{ "event":%q,  "business_id":"biz-1", "created":"2026-10-23T10:00:00Z",
  "data":{ "payment_session_id":%q, "status":%q, "amount":1, "currency":"USD" } }`, event, sessionID, claimedStatus))
}

// PaymentEvent builds a payment.capture webhook body (no session id inside).
func PaymentEvent(paymentID string) []byte {
	return []byte(fmt.Sprintf(`{"event":"payment.capture","business_id":"biz-1","data":{"payment_id":%q,"status":"SUCCEEDED","request_amount":10}}`, paymentID))
}
