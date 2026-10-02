package httpapi

import (
	_ "embed"
	"net/http"
)

// dashboardHTML is a single self-contained page (no external scripts or
// fonts) that polls /metrics and /readyz from the browser and draws them. It
// reads nothing the server does not already expose publicly.
//
//go:embed dashboard.html
var dashboardHTML []byte

func serveDashboard(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	// Same-origin fetches only; inline script and style are the page itself.
	h.Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(dashboardHTML)
}
