package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/tanmayt124/seat-reservation/internal/store"
)

const (
	maxPricePaise   = 1_000_000_000_00 // 1 crore rupees per seat, a sanity cap
	maxSeatsPerShow = 10000
	maxRows         = 26 // rows are lettered A..Z
	maxSeatsPerRow  = 500
	showsBodyLimit  = 256 << 10 // room for 10,000 explicit labels
)

var seatLabelPattern = regexp.MustCompile(`^[A-Z0-9-]{1,16}$`)

type createShowRequest struct {
	Name     string     `json:"name"`
	StartsAt *time.Time `json:"starts_at"`
	// Seats is the seat list, as in the brief: ["A1","A2",...].
	Seats []string `json:"seats"`
	// SeatLabels is accepted as an alias of Seats.
	SeatLabels []string `json:"seat_labels"`
	// Rows and SeatsPerRow generate A1..Z500 as a convenience.
	Rows        int `json:"rows"`
	SeatsPerRow int `json:"seats_per_row"`
	// PricePaise is raw so a float or a string is a field error, not a
	// silent truncation. Money is integer paise only.
	PricePaise   json.RawMessage `json:"price_paise"`
	PerUserLimit *int            `json:"per_user_limit"`
}

// createShow handles POST /shows (admin only). The brief's shape is
// {"name", "seats": [...], "price_paise"}; rows + seats_per_row is a
// convenience for large halls. Returns the show with every seat available.
func (h *handlers) createShow(w http.ResponseWriter, r *http.Request) {
	var req createShowRequest
	if !decodeJSON(w, r, &req, showsBodyLimit) {
		return
	}

	in, errs := validateCreateShow(&req, h.defaultLimit)
	if errs.write(w, r) {
		return
	}

	show, err := h.shows.Create(r.Context(), in)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	h.log.Info("show_created",
		"request_id", requestID(r),
		"show_id", show.ID,
		"total_seats", show.TotalSeats,
		"price_paise", show.PricePaise,
		"per_user_limit", show.PerUserLimit,
		"admin_id", identity(r).UserID,
	)

	d := store.ShowDetail{
		Show:        show,
		Counts:      store.SeatCounts{Available: show.TotalSeats},
		InvariantOK: true,
		Seats:       make([]store.SeatView, len(in.Labels)),
	}
	for i, l := range in.Labels {
		d.Seats[i] = store.SeatView{Label: l, Status: "available"}
	}
	store.SortLabels(d.Seats)
	writeJSON(w, http.StatusCreated, d)
}

func validateCreateShow(req *createShowRequest, defaultLimit int) (store.NewShow, fieldErrors) {
	errs := fieldErrors{}
	in := store.NewShow{StartsAt: req.StartsAt, PerUserLimit: defaultLimit}

	in.Name = strings.TrimSpace(req.Name)
	if n := len(in.Name); n == 0 || n > 200 {
		errs.add("name", "required, at most 200 characters")
	}

	if len(req.PricePaise) > 0 && string(req.PricePaise) != "null" {
		p, err := strconv.ParseInt(string(req.PricePaise), 10, 64)
		if err != nil || p < 0 || p > maxPricePaise {
			errs.add("price_paise", "must be a non-negative whole number of paise (no decimals, no quotes)")
		}
		in.PricePaise = p
	}

	if req.PerUserLimit != nil {
		if *req.PerUserLimit < 1 || *req.PerUserLimit > 100 {
			errs.add("per_user_limit", "must be between 1 and 100")
		}
		in.PerUserLimit = *req.PerUserLimit
	}

	explicit := req.Seats
	if len(explicit) == 0 {
		explicit = req.SeatLabels
	}
	grid := req.Rows != 0 || req.SeatsPerRow != 0
	switch {
	case grid && len(explicit) > 0:
		errs.add("seats", "give either seats or rows + seats_per_row, not both")
		return in, errs
	case !grid && len(explicit) == 0:
		errs.add("seats", "required: a list of seat labels, e.g. [\"A1\",\"A2\"]")
		return in, errs
	}

	if grid {
		if req.Rows < 1 || req.Rows > maxRows {
			errs.add("rows", fmt.Sprintf("must be between 1 and %d", maxRows))
		}
		if req.SeatsPerRow < 1 || req.SeatsPerRow > maxSeatsPerRow {
			errs.add("seats_per_row", fmt.Sprintf("must be between 1 and %d", maxSeatsPerRow))
		}
		if len(errs) > 0 {
			return in, errs
		}
		in.Labels = make([]string, 0, req.Rows*req.SeatsPerRow)
		for row := 0; row < req.Rows; row++ {
			for n := 1; n <= req.SeatsPerRow; n++ {
				in.Labels = append(in.Labels, fmt.Sprintf("%c%d", 'A'+row, n))
			}
		}
		return in, errs
	}

	if len(explicit) > maxSeatsPerShow {
		errs.add("seats", fmt.Sprintf("at most %d seats per show", maxSeatsPerShow))
		return in, errs
	}
	labels, err := normalizeLabels(explicit)
	if err != nil {
		errs.add("seats", err.Error())
	}
	in.Labels = labels
	return in, errs
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
