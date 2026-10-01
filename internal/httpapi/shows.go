package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/tanmayt124/seat-reservation/internal/store"
)

const (
	maxSeatsPerShow = 10000
	maxRows         = 26 // rows are lettered A..Z
	maxSeatsPerRow  = 500
	showsBodyLimit  = 256 << 10 // room for 10,000 explicit labels
)

var seatLabelPattern = regexp.MustCompile(`^[A-Z0-9-]{1,16}$`)

type createShowRequest struct {
	Name        string     `json:"name"`
	StartsAt    *time.Time `json:"starts_at"`
	Rows        int        `json:"rows"`
	SeatsPerRow int        `json:"seats_per_row"`
	SeatLabels  []string   `json:"seat_labels"`
}

// createShow handles POST /shows (admin only). The seat map is given either
// as rows x seats_per_row (labels A1..Z500) or as an explicit label list.
func (h *handlers) createShow(w http.ResponseWriter, r *http.Request) {
	var req createShowRequest
	if !decodeJSON(w, r, &req, showsBodyLimit) {
		return
	}

	labels, errs := validateCreateShow(&req)
	if errs.write(w, r) {
		return
	}

	show, err := h.shows.Create(r.Context(), req.Name, req.StartsAt, labels)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	h.log.Info("show_created",
		"request_id", requestID(r),
		"show_id", show.ID,
		"total_seats", show.TotalSeats,
		"admin_id", identity(r).UserID,
	)
	writeJSON(w, http.StatusCreated, show)
}

func validateCreateShow(req *createShowRequest) ([]string, fieldErrors) {
	errs := fieldErrors{}

	req.Name = strings.TrimSpace(req.Name)
	if n := len(req.Name); n == 0 || n > 200 {
		errs.add("name", "required, at most 200 characters")
	}

	grid := req.Rows != 0 || req.SeatsPerRow != 0
	explicit := len(req.SeatLabels) > 0
	switch {
	case grid && explicit:
		errs.add("seat_labels", "give either rows + seats_per_row or seat_labels, not both")
		return nil, errs
	case !grid && !explicit:
		errs.add("seat_labels", "give either rows + seats_per_row or seat_labels")
		return nil, errs
	}

	if grid {
		if req.Rows < 1 || req.Rows > maxRows {
			errs.add("rows", fmt.Sprintf("must be between 1 and %d", maxRows))
		}
		if req.SeatsPerRow < 1 || req.SeatsPerRow > maxSeatsPerRow {
			errs.add("seats_per_row", fmt.Sprintf("must be between 1 and %d", maxSeatsPerRow))
		}
		if len(errs) > 0 {
			return nil, errs
		}
		labels := make([]string, 0, req.Rows*req.SeatsPerRow)
		for row := 0; row < req.Rows; row++ {
			for n := 1; n <= req.SeatsPerRow; n++ {
				labels = append(labels, fmt.Sprintf("%c%d", 'A'+row, n))
			}
		}
		return labels, errs
	}

	if len(req.SeatLabels) > maxSeatsPerShow {
		errs.add("seat_labels", fmt.Sprintf("at most %d seats per show", maxSeatsPerShow))
		return nil, errs
	}
	labels, err := normalizeLabels(req.SeatLabels)
	if err != nil {
		errs.add("seat_labels", err.Error())
	}
	return labels, errs
}

// normalizeLabels upper-cases labels so "a1" and "A1" are the same seat,
// and rejects malformed or duplicate labels.
func normalizeLabels(in []string) ([]string, error) {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		l := strings.ToUpper(strings.TrimSpace(raw))
		if !seatLabelPattern.MatchString(l) {
			return nil, fmt.Errorf("invalid label %q: use 1-16 of A-Z, 0-9, '-'", raw)
		}
		if _, dup := seen[l]; dup {
			return nil, fmt.Errorf("duplicate label %q", l)
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	return out, nil
}

// getShow handles GET /shows/{showID}: every seat's status, the counts and
// a live invariant check. Other users' ids are never returned; the caller
// only sees which seats are theirs.
func (h *handlers) getShow(w http.ResponseWriter, r *http.Request) {
	showID, ok := parseShowID(w, r)
	if !ok {
		return
	}
	d, err := h.shows.Get(r.Context(), showID, identity(r).UserID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "show_not_found", "show not found", nil)
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	if !d.InvariantOK {
		// Should be impossible. Loud on purpose.
		h.log.Error("invariant_violation", "request_id", requestID(r), "show_id", showID,
			"total_seats", d.TotalSeats, "available", d.Counts.Available,
			"held", d.Counts.Held, "confirmed", d.Counts.Confirmed, "seat_rows", len(d.Seats))
	}
	writeJSON(w, http.StatusOK, d)
}

func parseShowID(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := chi.URLParam(r, "showID")
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_show_id", "show id must be a UUID", nil)
		return "", false
	}
	return id.String(), true
}
