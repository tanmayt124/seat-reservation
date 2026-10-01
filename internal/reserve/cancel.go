package reserve

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrReservationNotFound covers both "does not exist" and "not yours".
	// The HTTP layer answers 404 for both so reservation ids cannot be probed.
	ErrReservationNotFound = errors.New("reservation not found")
	// ErrNotOwner is answered exactly like ErrReservationNotFound; it exists
	// so the caller can log the attempt.
	ErrNotOwner = errors.New("reservation belongs to another user")
)

type Cancelled struct {
	ReservationID string    `json:"reservation_id"`
	ShowID        string    `json:"show_id"`
	Seats         []string  `json:"seats"`
	Status        string    `json:"status"`
	CancelledAt   time.Time `json:"cancelled_at"`
	// AlreadyCancelled is true when this call found the reservation already
	// cancelled. The response is the same either way.
	AlreadyCancelled bool `json:"-"`
	SeatsReleased    int  `json:"-"`
}

// Cancel releases a reservation's seats. Only the owner can cancel; seats are
// released by reservation_id, so a late or repeated cancel can never free a
// seat that has since been booked by someone else. Cancelling twice returns
// the same body.
func (s *Service) Cancel(ctx context.Context, reservationID, userID string) (Cancelled, error) {
	acqCtx, cancel := context.WithTimeout(ctx, s.acquireTimeout)
	conn, err := s.pool.Acquire(acqCtx)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			return Cancelled{}, ErrOverloaded
		}
		return Cancelled{}, err
	}
	defer conn.Release()

	var c Cancelled
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '"+lockTimeout+"'; SET LOCAL statement_timeout = '"+statementTimeout+"'"); err != nil {
			return err
		}

		var (
			owner, status string
			cancelledAt   *time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT show_id, user_id, seat_labels, status, cancelled_at
			FROM reservations WHERE id = $1
			FOR UPDATE`, reservationID,
		).Scan(&c.ShowID, &owner, &c.Seats, &status, &cancelledAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrReservationNotFound
		}
		if err != nil {
			return err
		}
		if owner != userID {
			return ErrNotOwner
		}
		c.ReservationID, c.Status = reservationID, "cancelled"

		if status == "cancelled" {
			c.AlreadyCancelled = true
			c.CancelledAt = *cancelledAt
			return nil
		}

		// Lock this reservation's seats in label order, the same order reserve
		// uses, so a concurrent reserve on the same seats queues instead of
		// deadlocking.
		if _, err := tx.Exec(ctx, `
			SELECT 1 FROM seats WHERE reservation_id = $1 ORDER BY label FOR UPDATE`,
			reservationID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE seats
			SET status = 'available', reservation_id = NULL, user_id = NULL, updated_at = now()
			WHERE reservation_id = $1`, reservationID)
		if err != nil {
			return err
		}
		c.SeatsReleased = int(tag.RowsAffected())

		return tx.QueryRow(ctx, `
			UPDATE reservations SET status = 'cancelled', cancelled_at = now()
			WHERE id = $1 RETURNING cancelled_at`, reservationID,
		).Scan(&c.CancelledAt)
	})
	if err != nil {
		if mapped, ok := mapPgError(err); ok && mapped.Status == 409 {
			// Seat locks are busy (a reserve is touching them): ask to retry.
			return Cancelled{}, ErrContended
		}
		return Cancelled{}, err
	}
	return c, nil
}

// ErrContended means locks could not be taken in time; safe to retry.
var ErrContended = errors.New("contended")
