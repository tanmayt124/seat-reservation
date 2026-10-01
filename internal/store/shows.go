package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

type Show struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	StartsAt   *time.Time `json:"starts_at,omitempty"`
	TotalSeats int        `json:"total_seats"`
	CreatedAt  time.Time  `json:"created_at"`
}

type SeatCounts struct {
	Available int `json:"available"`
	Held      int `json:"held"`
	Confirmed int `json:"confirmed"`
}

func (c SeatCounts) Sum() int { return c.Available + c.Held + c.Confirmed }

type SeatView struct {
	Label  string `json:"label"`
	Status string `json:"status"`
	Mine   bool   `json:"mine,omitempty"`
}

type ShowDetail struct {
	Show
	Counts SeatCounts `json:"counts"`
	// InvariantOK is available + held + confirmed == total_seats, and the
	// number of seat rows equals total_seats. Computed on every read.
	InvariantOK bool       `json:"invariant_ok"`
	Seats       []SeatView `json:"seats"`
}

type Shows struct {
	pool *pgxpool.Pool
}

func NewShows(pool *pgxpool.Pool) *Shows { return &Shows{pool: pool} }

// Create inserts the show and all of its seats in one transaction, so a show
// is never visible with only some of its seats.
func (s *Shows) Create(ctx context.Context, name string, startsAt *time.Time, labels []string) (Show, error) {
	var show Show
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO shows (name, starts_at, total_seats)
			VALUES ($1, $2, $3)
			RETURNING id, name, starts_at, total_seats, created_at`,
			name, startsAt, len(labels),
		).Scan(&show.ID, &show.Name, &show.StartsAt, &show.TotalSeats, &show.CreatedAt)
		if err != nil {
			return fmt.Errorf("insert show: %w", err)
		}

		rows := make([][]any, len(labels))
		for i, l := range labels {
			rows[i] = []any{show.ID, l}
		}
		n, err := tx.CopyFrom(ctx, pgx.Identifier{"seats"}, []string{"show_id", "label"}, pgx.CopyFromRows(rows))
		if err != nil {
			return fmt.Errorf("insert seats: %w", err)
		}
		if int(n) != len(labels) {
			return fmt.Errorf("inserted %d seats, want %d", n, len(labels))
		}
		return nil
	})
	return show, err
}

// Get reads the show, its counts and every seat from one snapshot, so the
// counts and the seat list always agree with each other. It takes no locks.
func (s *Shows) Get(ctx context.Context, showID, viewerID string) (ShowDetail, error) {
	var d ShowDetail
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	}, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT id, name, starts_at, total_seats, created_at
			FROM shows WHERE id = $1`, showID,
		).Scan(&d.ID, &d.Name, &d.StartsAt, &d.TotalSeats, &d.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}

		rows, err := tx.Query(ctx, `
			SELECT label, status, COALESCE(user_id = $2, false)
			FROM seats WHERE show_id = $1`, showID, viewerID)
		if err != nil {
			return err
		}
		d.Seats, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (SeatView, error) {
			var v SeatView
			err := r.Scan(&v.Label, &v.Status, &v.Mine)
			return v, err
		})
		if err != nil {
			return err
		}

		for _, v := range d.Seats {
			switch v.Status {
			case "available":
				d.Counts.Available++
			case "held":
				d.Counts.Held++
			case "confirmed":
				d.Counts.Confirmed++
			}
		}
		return nil
	})
	if err != nil {
		return ShowDetail{}, err
	}

	d.InvariantOK = d.Counts.Sum() == d.TotalSeats && len(d.Seats) == d.TotalSeats
	SortLabels(d.Seats)
	return d, nil
}

// SortLabels orders seats the way a seat map reads: by row prefix, then by
// seat number numerically (A2 before A10), then by the raw label.
func SortLabels(seats []SeatView) {
	sort.Slice(seats, func(i, j int) bool { return labelLess(seats[i].Label, seats[j].Label) })
}

func labelLess(a, b string) bool {
	pa, na := splitLabel(a)
	pb, nb := splitLabel(b)
	if pa != pb {
		return pa < pb
	}
	if na != nb {
		return na < nb
	}
	return a < b
}

func splitLabel(l string) (string, int) {
	i := strings.LastIndexFunc(l, func(r rune) bool { return r < '0' || r > '9' })
	num, err := strconv.Atoi(l[i+1:])
	if err != nil {
		return l, -1
	}
	return l[:i+1], num
}
