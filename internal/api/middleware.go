package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"unifiedgw/internal/auth"
	"unifiedgw/internal/domain"
)

const principalKey = "principal"

// writeError sends the standard error envelope. Unknown errors become a generic
// 500 so internal detail never reaches a caller.
func writeError(c *gin.Context, err error) {
	var de *domain.Error
	if !errors.As(err, &de) {
		de = domain.ErrInternal
	}
	c.AbortWithStatusJSON(de.HTTP, gin.H{"error": gin.H{"code": de.Code, "message": de.Message}})
}

// authenticate requires a valid Bearer key. A directory failure is a 500, not a 401.
func authenticate(a *auth.Authenticator, log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := auth.BearerToken(c.GetHeader("Authorization"))
		if !ok {
			writeError(c, domain.ErrUnauthorized)
			return
		}
		p, err := a.Authenticate(c.Request.Context(), token)
		switch {
		case errors.Is(err, auth.ErrUnauthenticated):
			writeError(c, domain.ErrUnauthorized)
			return
		case err != nil:
			id, _ := c.Get("request_id")
			log.Error("authentication backend failed", "request_id", id, "error", err)
			writeError(c, domain.ErrInternal)
			return
		}
		c.Set(principalKey, p)
		c.Next()
	}
}

// requireRole allows only principals holding the role. Callers cannot reach admin
// routes and admin keys cannot create payments.
func requireRole(role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := principalFrom(c)
		if !ok || p.Role != role {
			writeError(c, domain.ErrForbidden)
			return
		}
		c.Next()
	}
}

func principalFrom(c *gin.Context) (auth.Principal, bool) {
	v, ok := c.Get(principalKey)
	if !ok {
		return auth.Principal{}, false
	}
	p, ok := v.(auth.Principal)
	return p, ok
}

// bodyLimit caps request bodies; larger ones fail with 413 when read.
func bodyLimit(max int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, max)
		c.Next()
	}
}
