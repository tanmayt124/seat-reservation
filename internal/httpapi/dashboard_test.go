package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestDashboard(t *testing.T) {
	api := newTestAPI(t)

	rec := api.do("GET", "/dashboard", "", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("/dashboard: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	// It reads the same public endpoints anyone can, and loads nothing external.
	for _, want := range []string{`fetch("metrics"`, `fetch("readyz"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard does not call %s", want)
		}
	}
	for _, bad := range []string{"<script src", "<link ", "https://", "http://"} {
		if strings.Contains(body, bad) {
			t.Fatalf("dashboard references something external: %q", bad)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("missing CSP: %q", csp)
	}

	rec = api.do("GET", "/", "", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/dashboard" {
		t.Fatalf("/ -> %d %q, want 302 to /dashboard", rec.Code, rec.Header().Get("Location"))
	}

	// The page polls every second; that must not show up in the request
	// counters it is displaying.
	before := api.scrape()
	for i := 0; i < 5; i++ {
		api.do("GET", "/dashboard", "", nil)
	}
	after := api.scrape()
	for k, v := range after {
		if strings.HasPrefix(k, "http_requests_total") && v != before[k] {
			t.Fatalf("dashboard traffic was counted: %s %v -> %v", k, before[k], v)
		}
	}
}
