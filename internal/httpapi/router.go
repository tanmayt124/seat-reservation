// Package httpapi holds the HTTP surface: routing, middleware, handlers and
// the JSON error envelope. Business rules live in other packages.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type Deps struct {
	Logger *slog.Logger
}

func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "not_found", "route not found", nil)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed on this route", nil)
	})

	// Liveness only: the process is up and serving. Readiness with a DB
	// check is /readyz (KAN-27).
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	return r
}
