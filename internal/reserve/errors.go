package reserve

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Postgres SQLSTATE codes this package handles.
const (
	sqlLockNotAvailable     = "55P03" // lock_timeout hit
	sqlQueryCanceled        = "57014" // statement_timeout hit
	sqlDeadlockDetected     = "40P01"
	sqlSerializationFailure = "40001"
	sqlCheckViolation       = "23514"
)

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func isRetryable(err error) bool {
	switch pgCode(err) {
	case sqlDeadlockDetected, sqlSerializationFailure:
		return true
	}
	return false
}

// mapPgError turns expected database failures into client answers. Anything
// not listed here is a real server error. Contention and timeouts are never
// 5xx: the brief requires zero 5xx under burst, and these are load
// conditions the client can retry, not server faults.
func mapPgError(err error) (Outcome, bool) {
	switch pgCode(err) {
	case sqlLockNotAvailable, sqlDeadlockDetected, sqlSerializationFailure:
		out := errorOutcome(http.StatusConflict, "seat_contended",
			"seats are being booked by someone else right now, try again", nil)
		out.RetryAfter = time.Second
		return out, true
	case sqlQueryCanceled:
		out := errorOutcome(http.StatusTooManyRequests, "busy_try_again",
			"the system is busy, try again shortly", nil)
		out.RetryAfter = time.Second
		return out, true
	case sqlCheckViolation:
		out := errorOutcome(http.StatusConflict, "seat_unavailable",
			"seats are no longer available", nil)
		out.Reason = "check_violation"
		return out, true
	}
	return Outcome{}, false
}
