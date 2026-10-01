package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tanmayt124/seat-reservation/internal/auth"
)

// authenticate verifies the bearer token and stores the identity in the
// request context. Every protected route sits behind it. With optional set,
// a request without a token passes through anonymously; a token that is
// present must still be valid.
func authenticate(a *auth.Authenticator, log *slog.Logger, optional bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok && optional && r.Header.Get("Authorization") == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer`)
				writeError(w, r, http.StatusUnauthorized, "missing_token", "Authorization: Bearer <token> is required", nil)
				return
			}
			id, err := a.Verify(raw)
			if err != nil {
				code, msg := "invalid_token", "token is invalid"
				if errors.Is(err, auth.ErrExpiredToken) {
					code, msg = "token_expired", "token has expired"
				}
				log.Warn("auth_failed", "reason", code, "request_id", requestID(r), "path", r.URL.Path)
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				writeError(w, r, http.StatusUnauthorized, code, msg, nil)
				return
			}

			// Identity never comes from the request. If the caller also sent a
			// user id somewhere, it is ignored and recorded.
			noteSpoof(log, r, id, r.URL.Query().Get("user_id"), "query")
			noteSpoof(log, r, id, r.Header.Get("X-User-Id"), "header")

			recordIdentity(r, id)
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	}
}

func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())
		if !id.IsAdmin() {
			writeError(w, r, http.StatusForbidden, "forbidden", "admin role required", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	return tok, tok != ""
}

// noteSpoof logs when a request names a user other than the token's subject.
// The request still proceeds as the token's user.
func noteSpoof(log *slog.Logger, r *http.Request, id auth.Identity, claimed, source string) {
	if claimed == "" || claimed == id.UserID {
		return
	}
	log.Warn("spoof_attempt",
		"request_id", requestID(r),
		"token_user_id", id.UserID,
		"claimed_user_id", claimed,
		"source", source,
		"path", r.URL.Path,
	)
}

func identity(r *http.Request) auth.Identity {
	id, _ := auth.FromContext(r.Context())
	return id
}
