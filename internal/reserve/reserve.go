// Package reserve implements the seat reservation transaction: all-or-nothing,
// idempotent per (user, key), and safe for the per-user seat limit under
// concurrency. See Service.Reserve for the exact steps.
package reserve

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	DefaultAcquireTimeout = 10 * time.Second
	lockTimeout           = "3s"
	statementTimeout      = "5s"
)

// ErrOverloaded means no database connection was free in time. The caller
// answers 429. With the default wait this is a last resort, not a normal
// outcome of a burst.
var ErrOverloaded = errors.New("overloaded")

// Outcome reasons. These are the error codes clients see and the reason
// labels used in logs and metrics.
const (
	ReasonConfirmed      = "confirmed"
	ReasonReplay         = "idempotent_replay"
	ReasonSeatTaken      = "seat_taken"
	ReasonPerUserLimit   = "per_user_limit_exceeded"
	ReasonKeyReused      = "idempotency_key_reused"
	ReasonSeatContended  = "seat_contended"
	ReasonBusy           = "busy_try_again"
	ReasonUnknownSeats   = "unknown_seats"
	ReasonShowNotFound   = "show_not_found"
	reasonCheckViolation = "check_violation"
)

type Request struct {
	ShowID string
	UserID string
	Key    string
	Seats  []string // normalised, unique, non-empty
}

// Outcome is a complete HTTP answer. Body is exactly what the client gets,
// including on replay.
type Outcome struct {
	Status   int
	Body     []byte
	Replayed bool
	// Reason is the outcome label for logs and metrics (see Reason*).
	Reason string
	// RetryAfter, when set, is sent as the Retry-After header.
	RetryAfter time.Duration
}

type Service struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	// acquireTimeout bounds the wait for a pool connection. Bursts queue
	// rather than shed: a hot-seat loser should get 409, not 429.
	acquireTimeout time.Duration
}

func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{pool: pool, log: log, acquireTimeout: DefaultAcquireTimeout}
}

// SetAcquireTimeout overrides the pool wait.
func (s *Service) SetAcquireTimeout(d time.Duration) { s.acquireTimeout = d }

// Confirmed is the 201 body.
type Confirmed struct {
	ReservationID string    `json:"reservation_id"`
	ShowID        string    `json:"show_id"`
	UserID        string    `json:"user_id"`
	Seats         []string  `json:"seats"`
	AmountPaise   int64     `json:"amount_paise"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

// showTerms are the show's booking terms, read in the pre-check. They never
// change after the show is created.
type showTerms struct {
	pricePaise int64
	limit      int
}

// Reserve runs the whole flow:
//
//  1. One round trip, no locks: the seats' current status, then the key.
//     A key that already booked returns that booking (replay). Otherwise
//     unknown seats -> 422, taken seats -> 409, over the limit -> 409.
//  2. Transaction: claim the key, per-user advisory lock + limit check,
//     lock seats in label order, guarded update, store the response, commit.
//
// Only successful bookings are stored against the key. A rejection rolls the
// transaction back and leaves nothing behind: nothing changed, so a retry
// with the same key simply re-evaluates. This also keeps rejections off the
// WAL, so a hot-seat storm costs one disk flush (the winner), not hundreds.
//
// Deadlocks and serialization failures are retried once.
func (s *Service) Reserve(ctx context.Context, req Request) (Outcome, error) {
	seats := append([]string(nil), req.Seats...)
	sort.Strings(seats)
	req.Seats = seats
	hash := requestHash(req.ShowID, seats)

	pctx, cancel := context.WithTimeout(ctx, s.acquireTimeout+time.Second)
	terms, out, done, err := s.precheck(pctx, req, hash)
	cancel()
	if err != nil || done {
		return out, err
	}

	out, err = s.runTx(ctx, req, hash, terms)
	if isRetryable(err) {
		s.log.Warn("reserve_retry", "show_id", req.ShowID, "user_id", req.UserID, "err", err)
		out, err = s.runTx(ctx, req, hash, terms)
	}
	if err != nil {
		if mapped, ok := mapPgError(err); ok {
			if mapped.Reason == reasonCheckViolation {
				s.log.Error("constraint_blocked_bad_write", "show_id", req.ShowID, "user_id", req.UserID, "err", err)
			}
			return mapped, nil
		}
		return Outcome{}, err
	}
	return out, nil
}

// requestHash identifies "the same request": same show and same seats, in
// any order.
func requestHash(showID string, sortedSeats []string) []byte {
	h := sha256.Sum256([]byte(showID + "\n" + strings.Join(sortedSeats, ",")))
	return h[:]
}

// precheck reads the seats and then the key in one batch (one network round
// trip). The order matters: if a duplicate of this request commits between
// the two reads, the seats look taken but the key read, which runs after,
// sees the committed booking, so the client gets the replay rather than 409.
func (s *Service) precheck(ctx context.Context, req Request, hash []byte) (showTerms, Outcome, bool, error) {
	var (
		terms      showTerms
		showFound  bool
		found      = map[string]string{}
		storedHash []byte
		status     *int
		body       []byte
		keyFound   bool
	)

	batch := &pgx.Batch{}
	batch.Queue(`
		SELECT s.price_paise, s.per_user_limit, st.label, st.status
		FROM shows s
		LEFT JOIN seats st ON st.show_id = s.id AND st.label = ANY($2)
		WHERE s.id = $1`, req.ShowID, req.Seats).Query(func(rows pgx.Rows) error {
		var label, st *string
		_, err := pgx.ForEachRow(rows, []any{&terms.pricePaise, &terms.limit, &label, &st}, func() error {
			showFound = true
			if label != nil {
				found[*label] = *st
			}
			return nil
		})
		return err
	})
	batch.Queue(`
		SELECT request_hash, response_status, response_body::text
		FROM idempotency_keys WHERE user_id = $1 AND key = $2`,
		req.UserID, req.Key).QueryRow(func(row pgx.Row) error {
		err := row.Scan(&storedHash, &status, &body)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		keyFound = err == nil
		return err
	})
	if err := s.pool.SendBatch(ctx, batch).Close(); err != nil {
		if ctx.Err() != nil && errors.Is(err, context.DeadlineExceeded) {
			return terms, Outcome{}, false, ErrOverloaded
		}
		if mapped, ok := mapPgError(err); ok {
			return terms, mapped, true, nil
		}
		return terms, Outcome{}, false, err
	}

	if keyFound {
		if out, ok := replay(storedHash, hash, status, body); ok {
			return terms, out, true, nil
		}
	}
	if !showFound {
		return terms, errorOutcome(http.StatusNotFound, ReasonShowNotFound, "show not found", nil), true, nil
	}
	if out, bad := checkSeats(req.Seats, found); bad {
		return terms, out, true, nil
	}
	if len(req.Seats) > terms.limit {
		return terms, limitOutcome(terms.limit, 0, len(req.Seats)), true, nil
	}
	return terms, Outcome{}, false, nil
}

// replay turns a stored key row into an outcome. A row without a stored
// status cannot be committed, so it is treated as not found.
func replay(storedHash, hash []byte, status *int, body []byte) (Outcome, bool) {
	if string(storedHash) != string(hash) {
		return errorOutcome(http.StatusConflict, ReasonKeyReused,
			"this idempotency key was already used with a different request", nil), true
	}
	if status == nil {
		return Outcome{}, false
	}
	return Outcome{Status: *status, Body: body, Replayed: true, Reason: ReasonReplay}, true
}

// checkSeats reports unknown seats (422) first, then taken seats (409).
func checkSeats(want []string, found map[string]string) (Outcome, bool) {
	var unknown, taken []string
	for _, l := range want {
		st, ok := found[l]
		switch {
		case !ok:
			unknown = append(unknown, l)
		case st != "available":
			taken = append(taken, l)
		}
	}
	if len(unknown) > 0 {
		return errorOutcome(http.StatusUnprocessableEntity, ReasonUnknownSeats,
			"some seats do not exist in this show", map[string]any{"seats": unknown}), true
	}
	if len(taken) > 0 {
		return errorOutcome(http.StatusConflict, ReasonSeatTaken,
			"some seats are already taken", map[string]any{"seats": taken}), true
	}
	return Outcome{}, false
}

func limitOutcome(limit, current, requested int) Outcome {
	return errorOutcome(http.StatusConflict, ReasonPerUserLimit,
		fmt.Sprintf("at most %d seats per user for this show", limit),
		map[string]any{"limit": limit, "current": current, "requested": requested})
}

// rejection carries a 4xx outcome out of the transaction function so the
// transaction rolls back instead of committing.
type rejection struct{ out Outcome }

func (r rejection) Error() string { return "rejected: " + r.out.Reason }

// errDuplicateInFlight signals that another request with the same key won the
// insert; the caller replays its stored response.
var errDuplicateInFlight = errors.New("duplicate key in flight")

func (s *Service) runTx(ctx context.Context, req Request, hash []byte, terms showTerms) (Outcome, error) {
	acqCtx, cancel := context.WithTimeout(ctx, s.acquireTimeout)
	conn, err := s.pool.Acquire(acqCtx)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			return Outcome{}, ErrOverloaded
		}
		return Outcome{}, err
	}
	defer conn.Release()

	var out Outcome
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		var err error
		out, err = s.reserveInTx(ctx, tx, req, hash, terms)
		return err
	})
	var rej rejection
	if errors.As(err, &rej) {
		return rej.out, nil
	}

	if errors.Is(err, errDuplicateInFlight) {
		// The other request has committed by now (the insert waited for it).
		var (
			storedHash []byte
			status     *int
			body       []byte
		)
		err := conn.QueryRow(ctx, `
			SELECT request_hash, response_status, response_body::text
			FROM idempotency_keys WHERE user_id = $1 AND key = $2`,
			req.UserID, req.Key,
		).Scan(&storedHash, &status, &body)
		if err != nil {
			return Outcome{}, err
		}
		if o, ok := replay(storedHash, hash, status, body); ok {
			return o, nil
		}
		return Outcome{}, fmt.Errorf("idempotency row for %s has no response", req.Key)
	}
	return out, err
}

func (s *Service) reserveInTx(ctx context.Context, tx pgx.Tx, req Request, hash []byte, terms showTerms) (Outcome, error) {
	// One round trip; no arguments, so pgx sends both statements together.
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '"+lockTimeout+"'; SET LOCAL statement_timeout = '"+statementTimeout+"'"); err != nil {
		return Outcome{}, err
	}

	// Claim the key. If a concurrent request holds it uncommitted, Postgres
	// makes this insert wait until that transaction ends.
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (user_id, key, request_hash)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		req.UserID, req.Key, hash)
	if err != nil {
		return Outcome{}, err
	}
	if tag.RowsAffected() == 0 {
		return Outcome{}, errDuplicateInFlight
	}

	// Serialise this user's reservations for this show only, so the count
	// below cannot be raced by a parallel request from the same user.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))",
		req.ShowID+":"+req.UserID); err != nil {
		return Outcome{}, err
	}
	var held int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM seats
		WHERE show_id = $1 AND user_id = $2 AND status <> 'available'`,
		req.ShowID, req.UserID).Scan(&held); err != nil {
		return Outcome{}, err
	}
	if held+len(req.Seats) > terms.limit {
		return Outcome{}, rejection{limitOutcome(terms.limit, held, len(req.Seats))}
	}

	// Lock the requested seats in label order. Every transaction locks in the
	// same order, so overlapping requests queue instead of deadlocking.
	rows, err := tx.Query(ctx, `
		SELECT label, status FROM seats
		WHERE show_id = $1 AND label = ANY($2)
		ORDER BY label
		FOR UPDATE`, req.ShowID, req.Seats)
	if err != nil {
		return Outcome{}, err
	}
	found := map[string]string{}
	var label, status string
	if _, err := pgx.ForEachRow(rows, []any{&label, &status}, func() error {
		found[label] = status
		return nil
	}); err != nil {
		return Outcome{}, err
	}
	if out, bad := checkSeats(req.Seats, found); bad {
		return Outcome{}, rejection{out}
	}

	c := Confirmed{
		ShowID:      req.ShowID,
		UserID:      req.UserID,
		Seats:       req.Seats,
		AmountPaise: terms.pricePaise * int64(len(req.Seats)),
		Status:      "confirmed",
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO reservations (show_id, user_id, seat_labels, amount_paise)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at`,
		req.ShowID, req.UserID, req.Seats, c.AmountPaise).Scan(&c.ReservationID, &c.CreatedAt); err != nil {
		return Outcome{}, err
	}

	// Guarded update: only seats still available change. With the rows locked
	// above this always matches, but if it ever does not, nothing is written.
	tag, err = tx.Exec(ctx, `
		UPDATE seats
		SET status = 'confirmed', reservation_id = $3, user_id = $4, updated_at = now()
		WHERE show_id = $1 AND label = ANY($2) AND status = 'available'`,
		req.ShowID, req.Seats, c.ReservationID, req.UserID)
	if err != nil {
		return Outcome{}, err
	}
	if int(tag.RowsAffected()) != len(req.Seats) {
		return Outcome{}, fmt.Errorf("guarded update changed %d rows, want %d", tag.RowsAffected(), len(req.Seats))
	}

	body, err := json.Marshal(c)
	if err != nil {
		return Outcome{}, err
	}
	return s.store(ctx, tx, req, Outcome{Status: http.StatusCreated, Body: body, Reason: ReasonConfirmed})
}

// store saves the outcome on the key row and returns the body as Postgres
// stored it, so the first response and every replay are identical bytes.
func (s *Service) store(ctx context.Context, tx pgx.Tx, req Request, out Outcome) (Outcome, error) {
	var stored []byte
	err := tx.QueryRow(ctx, `
		UPDATE idempotency_keys
		SET response_status = $3, response_body = $4
		WHERE user_id = $1 AND key = $2
		RETURNING response_body::text`,
		req.UserID, req.Key, out.Status, string(out.Body)).Scan(&stored)
	if err != nil {
		return Outcome{}, err
	}
	out.Body = stored
	return out, nil
}

type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details any    `json:"details,omitempty"`
	} `json:"error"`
}

func errorOutcome(status int, code, msg string, details any) Outcome {
	var e errorEnvelope
	e.Error.Code, e.Error.Message, e.Error.Details = code, msg, details
	b, _ := json.Marshal(e)
	return Outcome{Status: status, Body: b, Reason: code}
}
