package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDGeneratedEchoedAndInErrors(t *testing.T) {
	h := NewRouter(Deps{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))
	id := rec.Header().Get("X-Request-Id")
	if len(id) != 36 {
		t.Fatalf("expected a generated UUID, got %q", id)
	}
	if body := decode[errorBody](t, rec); body.Error.RequestID != id {
		t.Fatalf("error body request_id %q != header %q", body.Error.RequestID, id)
	}

	// A well-formed inbound id is kept; a hostile one is replaced.
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set("X-Request-Id", "client-abc_1.2")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); got != "client-abc_1.2" {
		t.Fatalf("inbound id not kept: %q", got)
	}

	req = httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set("X-Request-Id", "bad id\nINJECTED")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); strings.Contains(got, "INJECTED") {
		t.Fatalf("hostile id was kept: %q", got)
	}
}

func TestAccessLogLine(t *testing.T) {
	var buf bytes.Buffer
	h := NewRouter(Deps{Logger: slog.New(slog.NewJSONHandler(&buf, nil))})

	req := httptest.NewRequest("GET", "/nope", nil)
	req.Header.Set("X-Request-Id", "trace-1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly one access line (healthz is quiet), got %d: %s", len(lines), buf.String())
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["msg"] != "http_request" || entry["request_id"] != "trace-1" || entry["status"].(float64) != 404 {
		t.Fatalf("unexpected access line: %v", entry)
	}
	for _, k := range []string{"method", "route", "duration_ms", "bytes", "user_id"} {
		if _, ok := entry[k]; !ok {
			t.Fatalf("access line missing %q: %v", k, entry)
		}
	}
}

func TestReadyzFailsClosed(t *testing.T) {
	var fail error
	h := NewRouter(Deps{Ready: func(context.Context) error { return fail }})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready: %d", rec.Code)
	}

	fail = errors.New("database_unreachable")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "database_unreachable") {
		t.Fatalf("not ready: %d %s", rec.Code, rec.Body)
	}

	// Liveness does not depend on the database.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
}

func TestPanicBecomesLogged500(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h := withRequestID(recoverPanics(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusInternalServerError || decode[errorBody](t, rec).Error.RequestID == "" {
		t.Fatalf("panic response: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(buf.String(), `"msg":"panic"`) || !strings.Contains(buf.String(), "boom") {
		t.Fatalf("panic not logged: %s", buf.String())
	}
}

func TestClientGoneIsNot5xx(t *testing.T) {
	h := &handlers{log: slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "/x", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.internalError(rec, req, context.Canceled)
	if rec.Code != statusClientClosed {
		t.Fatalf("cancelled request answered %d, want 499", rec.Code)
	}
	// A real failure on a live request is still a 500.
	rec = httptest.NewRecorder()
	h.internalError(rec, httptest.NewRequest("POST", "/x", nil), errors.New("boom"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("live failure answered %d, want 500", rec.Code)
	}
}
