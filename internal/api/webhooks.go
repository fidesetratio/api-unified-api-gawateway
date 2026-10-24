package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"unifiedgw/internal/service"
	"unifiedgw/internal/store"
)

// receiveWebhook handles POST /api/v1.0/webhooks/{provider}. It is unauthenticated
// by API key: authenticity is established by the provider adapter. Answers:
// 401 when verification fails (a forged or damaged delivery), 5xx only when the
// provider should retry, and 200 for everything handled or deliberately ignored.
func receiveWebhook(svc *service.Settlement, log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body) // raw bytes: verification needs them untouched
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"status": "payload_too_large"})
				return
			}
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"status": "unreadable"})
			return
		}
		outcome, err := svc.ProcessProviderWebhook(c.Request.Context(), c.Param("provider"), c.Request.Header, body)
		switch {
		case errors.Is(err, service.ErrUnknownProvider):
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"status": "unknown_provider"})
		case err != nil:
			id, _ := c.Get("request_id")
			log.Error("webhook processing failed", "request_id", id, "provider", c.Param("provider"), "error", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"status": "error"})
		case outcome == store.OutcomeNotVerified:
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"status": outcome})
		default:
			c.JSON(http.StatusOK, gin.H{"status": outcome})
		}
	}
}
