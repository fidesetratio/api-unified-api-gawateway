package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/service"
	"unifiedgw/internal/store"
)

// configResponse never contains secret values, only whether they exist.
type configResponse struct {
	ID                 int64          `json:"id"`
	Name               string         `json:"name"`
	Provider           string         `json:"provider"`
	Environment        string         `json:"environment"`
	Status             string         `json:"status"`
	AllowedCurrencies  []string       `json:"allowed_currencies"`
	Settings           map[string]any `json:"settings"`
	CredentialsVersion int            `json:"credentials_version"`
	HasWebhookSecret   bool           `json:"has_webhook_secret"`
}

func toConfigResponse(c store.Config) configResponse {
	v := 0
	if c.ActiveVersion != nil {
		v = *c.ActiveVersion
	}
	return configResponse{
		ID: c.ID, Name: c.Name, Provider: c.Provider, Environment: c.Environment, Status: c.Status,
		AllowedCurrencies: c.AllowedCurrencies, Settings: c.Settings,
		CredentialsVersion: v, HasWebhookSecret: c.HasWebhookSecret,
	}
}

type createConfigRequest struct {
	Provider          string            `json:"provider"`
	Environment       string            `json:"environment"`
	Name              string            `json:"name"`
	AllowedCurrencies []string          `json:"allowed_currencies"`
	Settings          map[string]any    `json:"settings"`
	Credentials       map[string]string `json:"credentials"`
	WebhookSecret     map[string]string `json:"webhook_secret"`
}

type rotateRequest struct {
	Credentials   map[string]string `json:"credentials"`
	WebhookSecret map[string]string `json:"webhook_secret"`
}

type updateConfigRequest struct {
	Name              *string        `json:"name"`
	AllowedCurrencies []string       `json:"allowed_currencies"`
	Settings          map[string]any `json:"settings"`
	Status            *string        `json:"status"`
}

func registerAdmin(g *gin.RouterGroup, svc *service.Admin) {
	g.POST("/configs", func(c *gin.Context) {
		var req createConfigRequest
		if err := decodeStrict(c, &req); err != nil {
			writeError(c, err)
			return
		}
		who, _ := principalFrom(c)
		cfg, err := svc.CreateConfig(c.Request.Context(), who, service.CreateConfigInput{
			Provider: req.Provider, Environment: req.Environment, Name: req.Name,
			AllowedCurrencies: req.AllowedCurrencies, Settings: req.Settings,
			Credentials: req.Credentials, WebhookSecret: req.WebhookSecret,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, toConfigResponse(cfg))
	})

	g.GET("/configs", func(c *gin.Context) {
		list, err := svc.ListConfigs(c.Request.Context())
		if err != nil {
			writeError(c, err)
			return
		}
		out := make([]configResponse, 0, len(list))
		for _, cfg := range list {
			out = append(out, toConfigResponse(cfg))
		}
		c.JSON(http.StatusOK, gin.H{"configs": out})
	})

	g.GET("/configs/:id", func(c *gin.Context) {
		id, ok := pathID(c, "id")
		if !ok {
			return
		}
		cfg, err := svc.GetConfig(c.Request.Context(), id)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, toConfigResponse(cfg))
	})

	g.PUT("/configs/:id/credentials", func(c *gin.Context) {
		id, ok := pathID(c, "id")
		if !ok {
			return
		}
		var req rotateRequest
		if err := decodeStrict(c, &req); err != nil {
			writeError(c, err)
			return
		}
		who, _ := principalFrom(c)
		version, err := svc.RotateCredentials(c.Request.Context(), who, id, req.Credentials, req.WebhookSecret)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": id, "credentials_version": version})
	})

	g.PATCH("/configs/:id", func(c *gin.Context) {
		id, ok := pathID(c, "id")
		if !ok {
			return
		}
		var req updateConfigRequest
		if err := decodeStrict(c, &req); err != nil {
			writeError(c, err)
			return
		}
		who, _ := principalFrom(c)
		cfg, err := svc.UpdateConfig(c.Request.Context(), who, id, service.UpdateConfigInput{
			Name: req.Name, AllowedCurrencies: req.AllowedCurrencies, Settings: req.Settings, Status: req.Status,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, toConfigResponse(cfg))
	})

	g.PUT("/configs/:id/grants/:client_id", func(c *gin.Context) {
		grant(c, svc, true)
	})
	g.DELETE("/configs/:id/grants/:client_id", func(c *gin.Context) {
		grant(c, svc, false)
	})
}

func grant(c *gin.Context, svc *service.Admin, add bool) {
	cfgID, ok := pathID(c, "id")
	if !ok {
		return
	}
	clientID, ok := pathID(c, "client_id")
	if !ok {
		return
	}
	who, _ := principalFrom(c)
	var err error
	if add {
		err = svc.Grant(c.Request.Context(), who, cfgID, clientID)
	} else {
		err = svc.Revoke(c.Request.Context(), who, cfgID, clientID)
	}
	if err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func pathID(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, domain.ErrInvalidRequest)
		return 0, false
	}
	return id, true
}

// requireTLS refuses admin calls over plain HTTP, because they carry provider
// secrets. TLS is detected from the connection or from X-Forwarded-Proto set by a
// trusted reverse proxy that strips client-supplied copies of that header.
func requireTLS(allowInsecure bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if allowInsecure || c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
			c.Next()
			return
		}
		writeError(c, domain.ErrTLSRequired)
	}
}
