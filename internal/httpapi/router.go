// Package httpapi holds the HTTP surface: routing, middleware, handlers and
// the JSON error envelope. Business rules live in other packages.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/tanmayt124/seat-reservation/internal/auth"
	"github.com/tanmayt124/seat-reservation/internal/reserve"
	"github.com/tanmayt124/seat-reservation/internal/store"
)

type Deps struct {
	Logger              *slog.Logger
	Auth                *auth.Authenticator
	Shows               *store.Shows
	Reserve             *reserve.Service
	EnableTokenEndpoint bool
	// DefaultPerUserLimit applies to shows created without per_user_limit.
	DefaultPerUserLimit int
}

type handlers struct {
	log          *slog.Logger
	auth         *auth.Authenticator
	shows        *store.Shows
	reserve      *reserve.Service
	defaultLimit int
}

func NewRouter(d Deps) http.Handler {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.DefaultPerUserLimit <= 0 {
		d.DefaultPerUserLimit = 4
	}
	h := &handlers{log: d.Logger, auth: d.Auth, shows: d.Shows, reserve: d.Reserve, defaultLimit: d.DefaultPerUserLimit}

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

	if d.EnableTokenEndpoint && d.Auth != nil {
		r.Post("/auth/token", h.issueTestToken)
	}

	if d.Auth != nil {
		// Show state is public. A token is optional; with one, the caller's
		// own seats are marked "mine".
		r.With(authenticate(d.Auth, d.Logger, true)).Get("/shows/{showID}", h.getShow)

		r.Group(func(r chi.Router) {
			r.Use(authenticate(d.Auth, d.Logger, false))

			// Routes as named in the brief, plus earlier names kept as aliases.
			r.Post("/shows/{showID}/reserve", h.createReservation)
			r.Post("/shows/{showID}/reservations", h.createReservation)
			r.Post("/reservations/{reservationID}/cancel", h.cancelReservation)
			r.Delete("/reservations/{reservationID}", h.cancelReservation)

			r.With(requireAdmin).Post("/shows", h.createShow)
		})
	}

	return r
}

// internalError is for failures the client cannot fix. Contention and
// overload are mapped to 4xx elsewhere (KAN-22) and never reach here.
func (h *handlers) internalError(w http.ResponseWriter, r *http.Request, err error) {
	h.log.Error("internal_error", "request_id", requestID(r), "path", r.URL.Path, "err", err)
	writeError(w, r, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
}
