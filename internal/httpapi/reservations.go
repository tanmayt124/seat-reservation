package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/tanmayt124/seat-reservation/internal/reserve"
)

const (
	maxIdempotencyKeyLen = 128
	// maxSeatsPerRequest is a sanity cap on request size. The per-user limit
	// is separate and per show: asking for more than the limit is a 409
	// limit decline, not a validation error.
	maxSeatsPerRequest = 50
)

type reserveRequest struct {
	Seats []string `json:"seats"`
	// IdempotencyKey may come in the body or in the Idempotency-Key header.
	IdempotencyKey string `json:"idempotency_key"`
	// UserID is never used for identity. It is read only so a mismatch with
	// the token can be logged as a spoof attempt.
	UserID string `json:"user_id"`
}

// createReservation handles POST /shows/{showID}/reserve (and the older
// /reservations alias).
func (h *handlers) createReservation(w http.ResponseWriter, r *http.Request) {
	showID, ok := parseShowID(w, r)
	if !ok {
		return
	}

	var body reserveRequest
	if !decodeJSON(w, r, &body, defaultBodyLimit) {
		return
	}
	id := identity(r)
	noteSpoof(h.log, r, id, body.UserID, "body")

	key, ok := idempotencyKey(w, r, body.IdempotencyKey)
	if !ok {
		return
	}

	errs := fieldErrors{}
	if n := len(body.Seats); n < 1 || n > maxSeatsPerRequest {
		errs.add("seats", fmt.Sprintf("give between 1 and %d seats", maxSeatsPerRequest))
	}
	seats, err := normalizeLabels(body.Seats)
	if err != nil {
		errs.add("seats", err.Error())
	}
	if errs.write(w, r) {
		return
	}

	out, err := h.reserve.Reserve(r.Context(), reserve.Request{
		ShowID: showID,
		UserID: id.UserID,
		Key:    key,
		Seats:  seats,
	})
	if errors.Is(err, reserve.ErrOverloaded) {
		h.log.Warn("reservation_declined", "request_id", requestID(r), "reason", "overloaded",
			"show_id", showID, "user_id", id.UserID)
		w.Header().Set("Retry-After", "1")
		writeError(w, r, http.StatusTooManyRequests, "overloaded", "too many requests in flight, retry shortly", nil)
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}

	attrs := []any{"request_id", requestID(r), "show_id", showID, "user_id", id.UserID,
		"seats", seats, "status", out.Status, "reason", out.Reason}
	switch {
	case out.Replayed:
		h.log.Info("idempotent_replay", attrs...)
	case out.Status == http.StatusCreated:
		h.log.Info("reservation_confirmed", attrs...)
	default:
		h.log.Info("reservation_declined", attrs...)
	}

	if out.Replayed {
		w.Header().Set("Idempotent-Replay", "true")
	}
	if out.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(out.RetryAfter.Seconds())))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(out.Status)
	_, _ = w.Write(out.Body)
}

// idempotencyKey takes the key from the Idempotency-Key header or the
// idempotency_key body field. Both may be sent, but they must agree.
func idempotencyKey(w http.ResponseWriter, r *http.Request, fromBody string) (string, bool) {
	fromHeader := r.Header.Get("Idempotency-Key")
	key := fromHeader
	if key == "" {
		key = fromBody
	}
	switch {
	case fromHeader != "" && fromBody != "" && fromHeader != fromBody:
		writeError(w, r, http.StatusBadRequest, "invalid_idempotency_key",
			"Idempotency-Key header and idempotency_key body field differ", nil)
		return "", false
	case key == "" || len(key) > maxIdempotencyKeyLen || !printableASCII(key):
		writeError(w, r, http.StatusBadRequest, "invalid_idempotency_key",
			fmt.Sprintf("an idempotency key is required (Idempotency-Key header or idempotency_key field): 1-%d printable ASCII characters", maxIdempotencyKeyLen), nil)
		return "", false
	}
	return key, true
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
