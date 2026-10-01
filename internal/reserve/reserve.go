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
	acquireTimeout   = 2 * time.Second
	lockTimeout      = "3s"
	statementTimeout = "5s"
)

// ErrOverloaded means no database connection was free in time. The caller
// should answer 429 so the client backs off.
var ErrOverloaded = errors.New("overloaded")

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
	// Reason is a short label for logs and metrics, e.g. "confirmed",
	// "seat_unavailable", "per_user_limit".
	Reason string
	// RetryAfter, when set, is sent as the Retry-After header.
	RetryAfter time.Duration
}

type Service struct {
	pool  *pgxpool.Pool
	limit int
	log   *slog.Logger
}

func NewService(pool *pgxpool.Pool, perUserLimit int, log *slog.Logger) *Service {
	return &Service{pool: pool, limit: perUserLimit, log: log}
}

func (s *Service) Limit() int { return s.limit }

// Confirmed is the 201 body.
type Confirmed struct {
	ReservationID string    `json:"reservation_id"`
	ShowID        string    `json:"show_id"`
	Seats         []string  `json:"seats"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

// Reserve runs the whole flow:
//
//  1. Replay check (no locks): a finished (user, key) returns its stored response.
//  2. Fast pre-check (no locks): unknown seats -> 422, taken seats -> 409.
//  3. Transaction: claim the key, per-user advisory lock + limit check,
//     lock seats in label order, guarded update, store the response, commit.
//
// Deadlocks and serialization failures are retried once.
func (s *Service) Reserve(ctx context.Context, req Request) (Outcome, error) {
	seats := append([]string(nil), req.Seats...)
	sort.Strings(seats)
	req.Seats = seats
	hash := requestHash(req.ShowID, seats)

	// The lock-free reads share one short deadline. Under burst, waiting
	// longer for a pool connection only grows the queue; 429 is better.
	pctx, cancel := context.WithTimeout(ctx, acquireTimeout+time.Second)
	defer cancel()
	if out, done, err := s.lookupKey(pctx, req, hash); err != nil || done {
		return out, err
	}
	if out, done, err := s.precheck(pctx, req); err != nil || done {
		if err == nil && out.Status != http.StatusNotFound {
			// A duplicate of this request may have committed between the key
			// lookup and the pre-check, making its own seats look taken.
			// Check the key once more so the client gets the replay, not 409.
			if replayed, ok, lerr := s.lookupKey(pctx, req, hash); lerr == nil && ok {
				return replayed, nil
			}
		}
		return out, err
	}

	out, err := s.runTx(ctx, req, hash)
	if isRetryable(err) {
		s.log.Warn("reserve_retry", "show_id", req.ShowID, "user_id", req.UserID, "err", err)
		out, err = s.runTx(ctx, req, hash)
	}
	if err != nil {
		if mapped, ok := mapPgError(err); ok {
			if mapped.Reason == "check_violation" {
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

func (s *Service) lookupKey(ctx context.Context, req Request, hash []byte) (Outcome, bool, error) {
	var (
		storedHash []byte
		status     *int
		body       []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT request_hash, response_status, response_body::text
		FROM idempotency_keys WHERE user_id = $1 AND key = $2`,
		req.UserID, req.Key,
	).Scan(&storedHash, &status, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return Outcome{}, false, nil
	}
	if err != nil {
		return s.wrapAcquire(err)
	}
	out, ok := replay(storedHash, hash, status, body)
	return out, ok, nil
}

// replay turns a stored key row into an outcome. A row without a stored
// status cannot be committed, so it is treated as not found.
func replay(storedHash, hash []byte, status *int, body []byte) (Outcome, bool) {
	if string(storedHash) != string(hash) {
		return errorOutcome(http.StatusConflict, "idempotency_key_reused",
			"this Idempotency-Key was already used with a different request", nil), true
	}
	if status == nil {
		return Outcome{}, false
	}
	return Outcome{Status: *status, Body: body, Replayed: true, Reason: "replay"}, true
}

func (s *Service) precheck(ctx context.Context, req Request) (Outcome, bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT label, status FROM seats WHERE show_id = $1 AND label = ANY($2)`,
		req.ShowID, req.Seats)
	if err != nil {
		return s.wrapAcquire(err)
	}
	found := map[string]string{}
	var label, status string
	_, err = pgx.ForEachRow(rows, []any{&label, &status}, func() error {
		found[label] = status
		return nil
	})
	if err != nil {
		return s.wrapAcquire(err)
	}

	if len(found) == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM shows WHERE id = $1)", req.ShowID).Scan(&exists); err != nil {
			return s.wrapAcquire(err)
		}
		if !exists {
			return errorOutcome(http.StatusNotFound, "show_not_found", "show not found", nil), true, nil
		}
	}
	if out, bad := checkSeats(req.Seats, found); bad {
		out.Reason += "_precheck"
		return out, true, nil
	}
	return Outcome{}, false, nil
}

// checkSeats reports unknown seats (422) first, then unavailable seats (409).
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
		return errorOutcome(http.StatusUnprocessableEntity, "unknown_seats",
			"some seats do not exist in this show", map[string]any{"seats": unknown}), true
	}
	if len(taken) > 0 {
		return errorOutcome(http.StatusConflict, "seat_unavailable",
			"some seats are no longer available", map[string]any{"seats": taken}), true
	}
	return Outcome{}, false
}

// errDuplicateInFlight signals that another request with the same key won the
// insert; the caller replays its stored response.
var errDuplicateInFlight = errors.New("duplicate key in flight")

func (s *Service) runTx(ctx context.Context, req Request, hash []byte) (Outcome, error) {
	acqCtx, cancel := context.WithTimeout(ctx, acquireTimeout)
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
		out, err = s.reserveInTx(ctx, tx, req, hash)
		return err
	})

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

func (s *Service) reserveInTx(ctx context.Context, tx pgx.Tx, req Request, hash []byte) (Outcome, error) {
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
	if held+len(req.Seats) > s.limit {
		out := errorOutcome(http.StatusUnprocessableEntity, "per_user_limit_exceeded",
			fmt.Sprintf("at most %d seats per user for this show", s.limit),
			map[string]any{"limit": s.limit, "current": held, "requested": len(req.Seats)})
		return s.store(ctx, tx, req, out)
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
		return s.store(ctx, tx, req, out)
	}

	var c Confirmed
	if err := tx.QueryRow(ctx, `
		INSERT INTO reservations (show_id, user_id, seat_labels)
		VALUES ($1, $2, $3)
		RETURNING id, created_at`,
		req.ShowID, req.UserID, req.Seats).Scan(&c.ReservationID, &c.CreatedAt); err != nil {
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

	c.ShowID, c.Seats, c.Status = req.ShowID, req.Seats, "confirmed"
	body, err := json.Marshal(c)
	if err != nil {
		return Outcome{}, err
	}
	return s.store(ctx, tx, req, Outcome{Status: http.StatusCreated, Body: body, Reason: "confirmed"})
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

func (s *Service) wrapAcquire(err error) (Outcome, bool, error) {
	if errors.Is(err, context.DeadlineExceeded) {
		return Outcome{}, false, ErrOverloaded
	}
	if mapped, ok := mapPgError(err); ok {
		return mapped, true, nil
	}
	return Outcome{}, false, err
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
