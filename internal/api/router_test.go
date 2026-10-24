package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestHealthz(t *testing.T) {
	r := NewRouter(Deps{Log: discardLogger()})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"ok"`) {
		t.Errorf("body = %s", w.Body)
	}
	if w.Header().Get(requestIDHeader) == "" {
		t.Error("missing request id header")
	}
}

func TestPanicBecomesGenericError(t *testing.T) {
	var logs bytes.Buffer
	r := gin.New()
	r.Use(requestID(), recoverer(slog.New(slog.NewTextHandler(&logs, nil))))
	r.GET("/boom", func(*gin.Context) { panic("db password is hunter2") })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "hunter2") {
		t.Errorf("panic detail leaked to client: %s", w.Body)
	}
	if !strings.Contains(w.Body.String(), "internal_error") {
		t.Errorf("body = %s", w.Body)
	}
}
