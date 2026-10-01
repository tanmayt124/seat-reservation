package reserve

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestMapPgError(t *testing.T) {
	cases := []struct {
		sqlstate  string
		status    int
		code      string
		retryable bool
	}{
		{sqlLockNotAvailable, http.StatusConflict, "seat_contended", false},
		{sqlDeadlockDetected, http.StatusConflict, "seat_contended", true},
		{sqlSerializationFailure, http.StatusConflict, "seat_contended", true},
		{sqlQueryCanceled, http.StatusTooManyRequests, "busy_try_again", false},
		{sqlCheckViolation, http.StatusConflict, "seat_unavailable", false},
	}
	for _, c := range cases {
		t.Run(c.sqlstate, func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: c.sqlstate})
			out, ok := mapPgError(err)
			if !ok || out.Status != c.status || code(out) != c.code {
				t.Fatalf("got ok=%v %d %s, want %d %s", ok, out.Status, code(out), c.status, c.code)
			}
			if out.Status >= 500 {
				t.Fatal("mapped errors must never be 5xx")
			}
			if isRetryable(err) != c.retryable {
				t.Fatalf("retryable = %v, want %v", !c.retryable, c.retryable)
			}
		})
	}

	if _, ok := mapPgError(errors.New("boom")); ok {
		t.Fatal("unknown errors must not be mapped")
	}
	if _, ok := mapPgError(&pgconn.PgError{Code: "42P01"}); ok {
		t.Fatal("unlisted SQLSTATE must not be mapped")
	}
}
