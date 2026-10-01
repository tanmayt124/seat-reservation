package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/tanmayt124/seat-reservation/internal/auth"
)

type requestIDKey struct{}

// An inbound X-Request-Id is reused only if it looks like an id, so a client
// cannot inject arbitrary text into our logs.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// withRequestID gives every request a correlation id: the caller's
// X-Request-Id when it is well formed, otherwise a new UUIDv7 (time-ordered,
// so ids sort by arrival). The id is echoed in the response header, in every
// log line and in every error body.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !requestIDPattern.MatchString(id) {
			if v7, err := uuid.NewV7(); err == nil {
				id = v7.String()
			} else {
				id = uuid.NewString()
			}
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func requestID(r *http.Request) string {
	if id, ok := r.Context().Value(requestIDKey{}).(string); ok {
		return id
	}
	return ""
}

// quietPaths are probed constantly by the platform and scrapers; logging
// them would bury the requests that matter.
var quietPaths = map[string]bool{"/healthz": true, "/readyz": true, "/metrics": true}

// accessLog writes one structured line per request after it completes.
func accessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if quietPaths[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			// The identity is set deeper in the chain; carry a pointer down so
			// the access line can include the token's user.
			holder := &identityHolder{}
			next.ServeHTTP(ww, r.WithContext(context.WithValue(r.Context(), identityHolderKey{}, holder)))

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			level := slog.LevelInfo
			if status >= 500 {
				level = slog.LevelError
			}
			log.LogAttrs(r.Context(), level, "http_request",
				slog.String("request_id", requestID(r)),
				slog.String("method", r.Method),
				slog.String("route", routePattern(r)),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
				slog.Int("bytes", ww.BytesWritten()),
				slog.String("user_id", holder.userID),
			)
		})
	}
}

type identityHolderKey struct{}

type identityHolder struct{ userID string }

// recordIdentity lets the auth middleware report the user to the access log.
func recordIdentity(r *http.Request, id auth.Identity) {
	if h, ok := r.Context().Value(identityHolderKey{}).(*identityHolder); ok {
		h.userID = id.UserID
	}
}

func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
		return rc.RoutePattern()
	}
	return "unmatched"
}

// recoverPanics turns a panic into a logged 500 instead of a dropped
// connection. A panic is a bug; the burst must never trigger one.
func recoverPanics(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if v == http.ErrAbortHandler {
						panic(v)
					}
					log.Error("panic", "request_id", requestID(r), "path", r.URL.Path,
						"panic", v, "stack", string(debug.Stack()))
					writeError(w, r, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// ReadyFunc reports whether the service can take traffic, and why not.
type ReadyFunc func(ctx context.Context) error

// readyz checks real dependencies and fails closed: any error, including a
// slow database, is 503.
func readyz(check ReadyFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if check == nil {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := check(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "reason": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}
