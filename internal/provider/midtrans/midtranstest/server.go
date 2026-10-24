// Package midtranstest is an in-process stand-in for the parts of Midtrans's Snap
// and Core APIs the adapter uses, following docs/midtrans-research.md. It is NOT
// Midtrans: anything the documentation leaves open is a guess to confirm in the
// sandbox.
package midtranstest

import (
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
)

var orderRe = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,50}$`)

// Server emulates Midtrans (Snap and Core share one test host).
type Server struct {
	*httptest.Server
	ServerKey string

	mu         sync.Mutex
	orders     map[string]*Order
	Creates    int
	Reads      int
	LastCreate []byte
	// CreateStatus makes POST /snap/v1/transactions answer with this HTTP status.
	CreateStatus int
	// ReadStatus makes GET /v2/{id}/status answer with this HTTP status.
	ReadStatus int
	// BodyStatusCodeOn200 makes "not found" come as HTTP 200 with status_code "404"
	// in the body, as the Core API reportedly does.
	BodyStatusCodeOn200 bool
}

// Order is the server-side state of a Snap order.
type Order struct {
	ID          string
	Gross       int64
	HasPayment  bool
	Status      string // transaction_status
	Fraud       string // fraud_status
	GrossString string // gross_amount as Midtrans prints it, e.g. "10000.00"
	Currency    string
}

// New starts the server.
func New(serverKey string) *Server {
	s := &Server{ServerKey: serverKey, orders: map[string]*Order{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Pay records a payment attempt with the given state (for example
// "settlement", "pending", "capture"+"accept", "deny", "expire").
func (s *Server) Pay(orderID, status, fraud, gross string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.orders[orderID]
	o.HasPayment, o.Status, o.Fraud, o.GrossString, o.Currency = true, status, fraud, gross, "IDR"
}

// Counts returns the number of creates and reads.
func (s *Server) Counts() (creates, reads int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Creates, s.Reads
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != s.ServerKey || pass != "" {
		if strings.HasPrefix(r.URL.Path, "/snap/") {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error_messages": []string{"Access denied due to unauthorized transaction, please check client or server key"}})
		} else {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"status_code": "401", "status_message": "Transaction cannot be authorized with the current client/server key."})
		}
		return
	}
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.URL.Path == "/snap/v1/transactions" && r.Method == http.MethodPost:
		s.create(w, r, body)
	case strings.HasPrefix(r.URL.Path, "/v2/") && strings.HasSuffix(r.URL.Path, "/status") && r.Method == http.MethodGet:
		s.status(w, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/"), "/status"))
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"status_code": "404", "status_message": "not found"})
	}
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, body []byte) {
	s.mu.Lock()
	s.Creates++
	s.LastCreate = body
	status := s.CreateStatus
	s.mu.Unlock()
	if status != 0 {
		writeJSON(w, status, map[string]any{"error_messages": []string{"Sorry, we encountered internal server error."}})
		return
	}
	var req struct {
		TransactionDetails struct {
			OrderID     string          `json:"order_id"`
			GrossAmount json.RawMessage `json:"gross_amount"`
		} `json:"transaction_details"`
	}
	if r.Header.Get("Content-Type") != "application/json" || json.Unmarshal(body, &req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error_messages": []string{"invalid body"}})
		return
	}
	td := req.TransactionDetails
	if !orderRe.MatchString(td.OrderID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error_messages": []string{"transaction_details.order_id is invalid"}})
		return
	}
	var gross int64
	if _, err := fmt.Sscan(string(td.GrossAmount), &gross); err != nil || strings.ContainsAny(string(td.GrossAmount), ".\"") || gross <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error_messages": []string{"transaction_details.gross_amount must be an integer"}})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, dup := s.orders[td.OrderID]; dup && o.HasPayment {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error_messages": []string{"transaction_details.order_id sudah digunakan"}})
		return
	}
	s.orders[td.OrderID] = &Order{ID: td.OrderID, Gross: gross}
	token := "tok-" + td.OrderID
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "redirect_url": "https://app.sandbox.midtrans.com/snap/v2/vtweb/" + token})
}

func (s *Server) status(w http.ResponseWriter, id string) {
	s.mu.Lock()
	s.Reads++
	status := s.ReadStatus
	o := s.orders[id]
	s.mu.Unlock()
	if status != 0 {
		writeJSON(w, status, map[string]any{"status_code": fmt.Sprint(status), "status_message": "stub error"})
		return
	}
	if o == nil || !o.HasPayment {
		code := http.StatusNotFound
		if s.BodyStatusCodeOn200 {
			code = http.StatusOK
		}
		writeJSON(w, code, map[string]any{"status_code": "404", "status_message": "Transaction doesn't exist."})
		return
	}
	v := map[string]any{
		"status_code": "200", "status_message": "Success, transaction is found", "order_id": o.ID,
		"transaction_id": "tx-" + o.ID, "payment_type": "bank_transfer", "transaction_status": o.Status,
		"gross_amount": o.GrossString, "currency": o.Currency,
	}
	if o.Fraud != "" {
		v["fraud_status"] = o.Fraud
	}
	writeJSON(w, http.StatusOK, v)
}

// ---- notification helpers --------------------------------------------------

// Signature is SHA512(order_id + status_code + gross_amount + serverKey), hex.
func Signature(orderID, statusCode, gross, serverKey string) string {
	sum := sha512.Sum512([]byte(orderID + statusCode + gross + serverKey))
	return hex.EncodeToString(sum[:])
}

// Notification builds a signed notification body. claimedStatus is what the
// (untrusted) body says; the adapter must ignore it.
func Notification(orderID, statusCode, gross, serverKey, claimedStatus string) []byte {
	return []byte(fmt.Sprintf(`{ "transaction_time":"2026-10-23 17:00:00", "transaction_status":%q,  "transaction_id":"tx-%s",
  "status_code":%q, "signature_key":%q, "payment_type":"bank_transfer", "order_id":%q, "gross_amount":%q, "fraud_status":"accept", "currency":"IDR" }`,
		claimedStatus, orderID, statusCode, Signature(orderID, statusCode, gross, serverKey), orderID, gross))
}
