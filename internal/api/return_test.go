package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/service"
)

func render(t *testing.T, accept string, view service.ReturnView) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1.0/return?ref=x", nil)
	if accept != "" {
		c.Request.Header.Set("Accept", accept)
	}
	respondReturn(c, http.StatusOK, pageFor(view), gin.H{"uniquecode": view.UniqueCode, "status": view.Status})
	return w
}

func TestReturnPageForBrowsers(t *testing.T) {
	w := render(t, "text/html,application/xhtml+xml,*/*;q=0.8", service.ReturnView{UniqueCode: "PP-1", Status: domain.StatusPaid})
	body := w.Body.String()
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(body, "Pembayaran berhasil") || !strings.Contains(body, "PP-1") {
		t.Fatalf("%s\n%s", w.Header().Get("Content-Type"), body)
	}
	if w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Error("security headers missing")
	}
}

func TestReturnPageEscapesTheCode(t *testing.T) {
	w := render(t, "", service.ReturnView{UniqueCode: `<script>alert(1)</script>`, Status: domain.StatusPending})
	if strings.Contains(w.Body.String(), "<script>alert") {
		t.Fatal("unescaped code in page")
	}
}

func TestReturnJSONOnRequest(t *testing.T) {
	w := render(t, "application/json", service.ReturnView{UniqueCode: "PP-1", Status: domain.StatusPaid})
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || !strings.Contains(w.Body.String(), `"status":"paid"`) {
		t.Fatalf("%s %s", w.Header().Get("Content-Type"), w.Body.String())
	}
}

func TestReturnEveryStatusHasAPage(t *testing.T) {
	for _, s := range []domain.Status{domain.StatusPending, domain.StatusPaid, domain.StatusFailed, domain.StatusExpired, domain.StatusCancelled} {
		if p := pageFor(service.ReturnView{Status: s}); p.Title == "" || p.Message == "" {
			t.Errorf("%s has no page text", s)
		}
	}
}
