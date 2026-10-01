package httpapi

import (
	"net/http"
	"time"

	"github.com/tanmayt124/seat-reservation/internal/metrics"
)

// admission caps the number of write requests in flight. A request that
// cannot get a slot within wait is answered 429 with Retry-After.
//
// It is a last resort for abnormal load (a runaway client, a flood far beyond
// the database's capacity), not a burst-shaping tool, and it is off unless
// ADMISSION_LIMIT is set. Under the on-sale burst the brief wants every loser
// to get 409, so requests queue on the database pool instead of being shed.
func admission(limit int, wait time.Duration, m *metrics.Metrics) func(http.Handler) http.Handler {
	if limit <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	slots := make(chan struct{}, limit)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
				next.ServeHTTP(w, r)
			case <-timer.C:
				m.Declined(metrics.DeclineOverloaded)
				w.Header().Set("Retry-After", "1")
				writeError(w, r, http.StatusTooManyRequests, "overloaded", "too many requests in flight, retry shortly", nil)
			case <-r.Context().Done():
				// Client gave up while queued; nothing to answer.
			}
		})
	}
}
