package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/service"
)

// createRequest is the body of POST /payments. amount is a string on purpose: a
// JSON number fails to decode, so floats can never reach money handling.
type createRequest struct {
	ID         int64  `json:"id"`
	UniqueCode string `json:"uniquecode"`
	Amount     string `json:"amount"`
	Currency   string `json:"currency"`
}

// paymentResponse is the whole body returned for a payment: exactly these fields.
type paymentResponse struct {
	UniqueCode string `json:"uniquecode"`
	Status     string `json:"status"`
	PaymentURL string `json:"payment_url,omitempty"`
}

func toResponse(v service.View) paymentResponse {
	return paymentResponse{UniqueCode: v.UniqueCode, Status: v.Status.APIName(), PaymentURL: v.PaymentURL}
}

func createPayment(svc *service.Payments) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req createRequest
		if err := decodeStrict(c, &req); err != nil {
			writeError(c, err)
			return
		}
		who, _ := principalFrom(c)
		v, created, err := svc.Create(c.Request.Context(), who, service.CreateInput{
			ConfigID: req.ID, UniqueCode: req.UniqueCode, Amount: req.Amount, Currency: req.Currency,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		code := http.StatusOK
		if created {
			code = http.StatusCreated
		}
		c.JSON(code, toResponse(v))
	}
}

func getStatus(svc *service.Payments) gin.HandlerFunc {
	return func(c *gin.Context) {
		who, _ := principalFrom(c)
		v, err := svc.Status(c.Request.Context(), who, c.Param("uniquecode"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, toResponse(v))
	}
}

// decodeStrict decodes exactly one JSON object, rejecting unknown fields,
// wrong types, trailing data and oversized bodies.
func decodeStrict(c *gin.Context, dst any) error {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return domain.ErrPayloadTooLarge
		}
		return domain.ErrInvalidRequest
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return domain.ErrPayloadTooLarge
		}
		return domain.ErrInvalidRequest // trailing data after the object
	}
	return nil
}
