package domain

import "net/http"

// Error is a domain error carrying a stable API code and HTTP status. The
// Message is safe to show to callers; provider detail never goes in it.
type Error struct {
	Code    string
	Message string
	HTTP    int
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// NewError builds a domain error.
func NewError(httpStatus int, code, message string) *Error {
	return &Error{Code: code, Message: message, HTTP: httpStatus}
}

// Predefined errors from the API design (unified-api-design.md section 2.1).
var (
	ErrInvalidRequest      = NewError(http.StatusBadRequest, "invalid_request", "request is invalid")
	ErrPayloadTooLarge     = NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "request body is too large")
	ErrTLSRequired         = NewError(http.StatusForbidden, "tls_required", "this endpoint must be called over HTTPS")
	ErrUnauthorized        = NewError(http.StatusUnauthorized, "unauthorized", "missing or invalid credentials")
	ErrConfigNotPermitted  = NewError(http.StatusForbidden, "config_not_permitted", "this key may not use that configuration")
	ErrForbidden           = NewError(http.StatusForbidden, "forbidden", "not allowed")
	ErrConfigNotFound      = NewError(http.StatusNotFound, "config_not_found", "configuration not found")
	ErrPaymentNotFound     = NewError(http.StatusNotFound, "payment_not_found", "payment not found")
	ErrUniquecodeConflict  = NewError(http.StatusConflict, "uniquecode_conflict", "uniquecode already used with different parameters")
	ErrCurrencyUnsupported = NewError(http.StatusUnprocessableEntity, "currency_not_supported", "currency is not supported by this configuration")
	ErrProviderError       = NewError(http.StatusBadGateway, "provider_error", "the payment provider returned an error")
	ErrProviderTimeout     = NewError(http.StatusGatewayTimeout, "provider_timeout", "the payment provider did not answer in time")
	ErrInternal            = NewError(http.StatusInternalServerError, "internal_error", "internal error")
)
