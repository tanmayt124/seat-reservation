// Package httpapi holds the HTTP surface: routing, middleware, handlers and
// the JSON error envelope. Business rules live in other packages.
package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tanmayt124/seat-reservation/internal/auth"
	"github.com/tanmayt124/seat-reservation/internal/metrics"
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
	// Ready backs /readyz. Nil means always ready (tests).
	Ready ReadyFunc
	// Metrics backs /metrics and the outcome counters. Nil disables both.
	Metrics *metrics.Metrics
	// AdmissionLimit caps in-flight reserve/cancel requests; 0 disables it.
	AdmissionLimit int
	AdmissionWait  time.Duration
}

type handlers struct {
	log          *slog.Logger
	auth         *auth.Authenticator
	shows        *store.Shows
	reserve      *reserve.Service
	defaultLimit int
	m            *metrics.Metrics
}

func NewRouter(d Deps) http.Handler {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.DefaultPerUserLimit <= 0 {
		d.DefaultPerUserLimit = 4
	}
	h := &handlers{log: d.Logger, auth: d.Auth, shows: d.Shows, reserve: d.Reserve, defaultLimit: d.DefaultPerUserLimit, m: d.Metrics}

	r := chi.NewRouter()
	r.Use(withRequestID, accessLog(d.Logger, d.Metrics), recoverPanics(d.Logger))

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "not_found", "route not found", nil)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed on this route", nil)
	})

	// Liveness: the process is up and serving. No dependencies are checked,
	// so a slow database never gets the instance restarted.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Readiness: the database answers and migrations are applied. Fails closed.
	r.Get("/readyz", readyz(d.Ready))
	if d.Metrics != nil {
		// Public on purpose: the brief asks for metrics access to watch the burst.
		r.Method(http.MethodGet, "/metrics", d.Metrics.Handler())
		// A live view of /metrics for people; the root URL lands there.
		r.Get("/dashboard", serveDashboard)
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/dashboard", http.StatusFound)
		})
	}

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
			r.Group(func(r chi.Router) {
				r.Use(admission(d.AdmissionLimit, d.AdmissionWait, d.Metrics))
				r.Post("/shows/{showID}/reserve", h.createReservation)
				r.Post("/shows/{showID}/reservations", h.createReservation)
				r.Post("/reservations/{reservationID}/cancel", h.cancelReservation)
				r.Delete("/reservations/{reservationID}", h.cancelReservation)
			})

			r.With(requireAdmin).Post("/shows", h.createShow)
		})
	}

	return r
}

// statusClientClosed is nginx's convention for "the client went away before
// we answered". It is not a server error and must not count as a 5xx.
const statusClientClosed = 499

// internalError is for failures the client cannot fix. Contention and
// overload are mapped to 4xx elsewhere and never reach here. If the request's
// own context was cancelled (client disconnected, or a keep-alive connection
// closed during shutdown) nobody is listening, so it is logged as 499.
func (h *handlers) internalError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		h.log.Info("client_closed_request", "request_id", requestID(r), "path", r.URL.Path, "err", err)
		w.WriteHeader(statusClientClosed)
		return
	}
	h.log.Error("internal_error", "request_id", requestID(r), "path", r.URL.Path, "err", err)
	writeError(w, r, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
}
