package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/tanmayt124/seat-reservation/internal/reserve"
)

// cancelReservation handles DELETE /reservations/{reservationID}. Only the
// owner can cancel. Someone else's reservation gets the same 404 as a
// missing one, so ids cannot be probed, and the attempt is logged.
func (h *handlers) cancelReservation(w http.ResponseWriter, r *http.Request) {
	rid, err := uuid.Parse(chi.URLParam(r, "reservationID"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_reservation_id", "reservation id must be a UUID", nil)
		return
	}
	id := identity(r)

	c, err := h.reserve.Cancel(r.Context(), rid.String(), id.UserID)
	switch {
	case errors.Is(err, reserve.ErrNotOwner):
		h.log.Warn("cancel_denied", "request_id", requestID(r), "reservation_id", rid.String(),
			"user_id", id.UserID, "reason", "not_owner")
		fallthrough
	case errors.Is(err, reserve.ErrReservationNotFound):
		writeError(w, r, http.StatusNotFound, "reservation_not_found", "reservation not found", nil)
		return
	case errors.Is(err, reserve.ErrOverloaded):
		w.Header().Set("Retry-After", "1")
		writeError(w, r, http.StatusTooManyRequests, "overloaded", "too many requests in flight, retry shortly", nil)
		return
	case errors.Is(err, reserve.ErrContended):
		w.Header().Set("Retry-After", "1")
		writeError(w, r, http.StatusConflict, "seat_contended", "seats are busy right now, retry shortly", nil)
		return
	case err != nil:
		h.internalError(w, r, err)
		return
	}

	h.log.Info("reservation_cancelled", "request_id", requestID(r), "reservation_id", c.ReservationID,
		"show_id", c.ShowID, "user_id", id.UserID, "seats_released", c.SeatsReleased,
		"already_cancelled", c.AlreadyCancelled)
	writeJSON(w, http.StatusOK, c)
}
