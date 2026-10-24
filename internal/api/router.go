// Package api wires the Gin router, middleware and handlers.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"unifiedgw/internal/auth"
	"unifiedgw/internal/service"
)

const requestIDHeader = "X-Request-Id"

const (
	paymentBodyLimit = 64 << 10  // 64 KiB
	adminBodyLimit   = 16 << 10  // 16 KiB
	webhookBodyLimit = 256 << 10 // 256 KiB
)

// Deps are the collaborators the router needs. Nil members disable their routes,
// which keeps the health-only router usable in tests.
type Deps struct {
	Log      *slog.Logger
	Auth     *auth.Authenticator
	Payments *service.Payments
	Admin    *service.Admin
	// Settlement handles provider webhooks (unauthenticated routes; the provider adapter verifies each delivery).
	Settlement *service.Settlement
	// Ready reports whether dependencies (database, key ring) are usable.
	Ready func(context.Context) error
	// AllowInsecureAdmin permits admin routes over plain HTTP (dev only).
	AllowInsecureAdmin bool
}

// NewRouter builds the HTTP router.
func NewRouter(d Deps) *gin.Engine {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(requestID(), recoverer(d.Log))

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/readyz", func(c *gin.Context) {
		if d.Ready != nil {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
			defer cancel()
			if err := d.Ready(ctx); err != nil {
				d.Log.Warn("not ready", "error", err)
				c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	if d.Auth != nil && d.Payments != nil {
		v1 := r.Group("/api/v1.0", authenticate(d.Auth, d.Log), requireRole(auth.RoleCaller))
		v1.POST("/payments", bodyLimit(paymentBodyLimit), createPayment(d.Payments))
		v1.GET("/status/:uniquecode", getStatus(d.Payments))
	}
	if d.Settlement != nil {
		r.POST("/api/v1.0/webhooks/:provider", bodyLimit(webhookBodyLimit), receiveWebhook(d.Settlement, d.Log))
		r.GET("/api/v1.0/return", checkReturn(d.Settlement, d.Log))
	}
	if d.Auth != nil && d.Admin != nil {
		adm := r.Group("/api/v1.0/admin",
			requireTLS(d.AllowInsecureAdmin),
			authenticate(d.Auth, d.Log), requireRole(auth.RoleAdmin), bodyLimit(adminBodyLimit))
		registerAdmin(adm, d.Admin)
	}
	return r
}

func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := newRequestID()
		c.Set("request_id", id)
		c.Header(requestIDHeader, id)
		c.Next()
	}
}

// recoverer turns a panic into a 500 error envelope without leaking details.
func recoverer(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				id, _ := c.Get("request_id")
				log.Error("panic recovered", "request_id", id, "panic", rec)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{"code": "internal_error", "message": "internal error"},
				})
			}
		}()
		c.Next()
	}
}

func newRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b)
}
