package api

import (
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/service"
)

// checkReturn handles GET /api/v1.0/return?ref={payment id}, the page a buyer's
// browser lands on after paying. It is unauthenticated and its input is not
// evidence: the gateway asks the provider for the real status (see
// Settlement.CheckReturn) and reports only that. Browsers get a small HTML page;
// a client that asks for JSON (Accept: application/json) gets the same facts as JSON.
func checkReturn(svc *service.Settlement, log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		view, err := svc.CheckReturn(c.Request.Context(), c.Query("ref"))
		switch {
		case errors.Is(err, domain.ErrPaymentNotFound):
			respondReturn(c, http.StatusNotFound, returnPage{Tone: "bad", Title: "Pembayaran tidak ditemukan",
				Message: "Tautan ini tidak dikenali. Periksa kembali tautan dari toko Anda."}, gin.H{
				"error": gin.H{"code": "payment_not_found", "message": "payment not found"}})
		case err != nil:
			id, _ := c.Get("request_id")
			log.Error("return check failed", "request_id", id, "error", err)
			respondReturn(c, http.StatusInternalServerError, returnPage{Tone: "bad", Title: "Terjadi kesalahan",
				Message: "Kami belum bisa memeriksa pembayaran Anda. Silakan muat ulang halaman ini sebentar lagi."}, gin.H{
				"error": gin.H{"code": "internal_error", "message": "internal error"}})
		default:
			respondReturn(c, http.StatusOK, pageFor(view), gin.H{"uniquecode": view.UniqueCode, "status": view.Status})
		}
	}
}

// returnPage is everything the HTML page shows.
type returnPage struct {
	Tone       string // "ok", "wait" or "bad": picks the colour
	Title      string
	Message    string
	UniqueCode string
	Status     string
}

func pageFor(v service.ReturnView) returnPage {
	p := returnPage{UniqueCode: v.UniqueCode, Status: strings.ToUpper(string(v.Status))}
	switch v.Status {
	case domain.StatusPaid:
		p.Tone, p.Title, p.Message = "ok", "Pembayaran berhasil", "Terima kasih. Pembayaran Anda sudah kami terima."
	case domain.StatusPending, domain.StatusCreating:
		p.Tone, p.Title, p.Message = "wait", "Pembayaran sedang diproses",
			"Kami belum menerima konfirmasi dari penyedia pembayaran. Muat ulang halaman ini dalam beberapa saat."
	case domain.StatusExpired:
		p.Tone, p.Title, p.Message = "bad", "Pembayaran kedaluwarsa", "Waktu pembayaran sudah habis. Silakan buat pesanan baru."
	case domain.StatusCancelled:
		p.Tone, p.Title, p.Message = "bad", "Pembayaran dibatalkan", "Pembayaran ini dibatalkan dan tidak ada dana yang diambil."
	default:
		p.Tone, p.Title, p.Message = "bad", "Pembayaran gagal", "Pembayaran tidak berhasil. Silakan coba lagi atau hubungi toko."
	}
	return p
}

func respondReturn(c *gin.Context, code int, page returnPage, body gin.H) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	if wantsJSON(c.GetHeader("Accept")) {
		c.AbortWithStatusJSON(code, body)
		return
	}
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'")
	c.Header("Referrer-Policy", "no-referrer")
	c.Status(code)
	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := returnTemplate.Execute(c.Writer, page); err != nil {
		_ = c.Error(err)
	}
	c.Abort()
}

// wantsJSON is true when the client prefers JSON over HTML; browsers send
// text/html first, so they always get the page.
func wantsJSON(accept string) bool {
	j := strings.Index(accept, "application/json")
	if j < 0 {
		return false
	}
	h := strings.Index(accept, "text/html")
	return h < 0 || j < h
}

var returnTemplate = template.Must(template.New("return").Parse(`<!doctype html>
<html lang="id">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}}</title>
<style>
:root { --bg:#f5f7f6; --card:#fff; --fg:#1b2420; --muted:#5d6a64; --line:#d8dfdb; --ok:#13795b; --wait:#9a6a00; --bad:#b3261e; color-scheme: light dark; }
@media (prefers-color-scheme: dark) { :root { --bg:#111714; --card:#18201c; --fg:#e8eeea; --muted:#9aa8a1; --line:#2b3530; --ok:#4cc79d; --wait:#e2b34b; --bad:#f08a82; } }
body { margin:0; min-height:100vh; display:flex; align-items:center; justify-content:center; padding:16px; box-sizing:border-box;
  background:var(--bg); color:var(--fg); font:16px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif; }
main { width:100%; max-width:440px; background:var(--card); border:1px solid var(--line); border-radius:8px; padding:28px 24px; }
.badge { display:inline-block; font-size:.75rem; font-weight:600; letter-spacing:.06em; padding:3px 10px; border-radius:99px; border:1px solid currentColor; }
.ok .badge, .ok h1 { color:var(--ok); } .wait .badge, .wait h1 { color:var(--wait); } .bad .badge, .bad h1 { color:var(--bad); }
h1 { font-size:1.4rem; margin:14px 0 8px; } p { margin:0 0 16px; color:var(--muted); }
dl { margin:0; padding-top:14px; border-top:1px solid var(--line); font-size:.9rem; } dt { color:var(--muted); } dd { margin:0 0 8px; font-family:ui-monospace,Consolas,monospace; word-break:break-all; }
</style>
</head>
<body>
<main class="{{.Tone}}">
{{if .Status}}<span class="badge">{{.Status}}</span>{{end}}
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
{{if .UniqueCode}}<dl><dt>Kode pesanan</dt><dd>{{.UniqueCode}}</dd></dl>{{end}}
</main>
</body>
</html>
`))
