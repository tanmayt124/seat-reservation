package reserve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tanmayt124/seat-reservation/internal/store"
	"github.com/tanmayt124/seat-reservation/internal/testutil"
	"github.com/tanmayt124/seat-reservation/migrations"
)

const testPrice int64 = 25000 // paise

type fixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	svc    *Service
	showID string
}

// newFixture creates a fresh schema and one show with seats A1..A<n>.
func newFixture(t *testing.T, seats, limit int) *fixture {
	t.Helper()
	pool := testutil.Pool(t)
	ctx := context.Background()
	if err := store.Migrate(ctx, pool, migrations.FS, testutil.QuietLogger()); err != nil {
		t.Fatal(err)
	}
	labels := make([]string, seats)
	for i := range labels {
		labels[i] = fmt.Sprintf("A%d", i+1)
	}
	show, err := store.NewShows(pool).Create(ctx, store.NewShow{Name: "test", Labels: labels, PricePaise: testPrice, PerUserLimit: limit})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(pool, testutil.QuietLogger())
	// Correctness tests assert exact counts, so a slow machine must not turn
	// waits into 429s. Overload behaviour is covered by the burst script.
	svc.SetAcquireTimeout(30 * time.Second)
	return &fixture{t: t, pool: pool, svc: svc, showID: show.ID}
}

func (f *fixture) reserve(user, key string, seats ...string) Outcome {
	f.t.Helper()
	out, err := f.svc.Reserve(context.Background(), Request{ShowID: f.showID, UserID: user, Key: key, Seats: seats})
	if err != nil {
		f.t.Errorf("reserve(%s, %s, %v): unexpected error %v", user, key, seats, err)
	}
	return out
}

// checkInvariant asserts available + held + confirmed == total_seats and that
// every non-available seat belongs to a confirmed reservation of the same user.
func (f *fixture) checkInvariant() (confirmed int) {
	f.t.Helper()
	ctx := context.Background()
	var total, avail, held, conf int
	err := f.pool.QueryRow(ctx, `
		SELECT s.total_seats,
		       count(*) FILTER (WHERE st.status = 'available'),
		       count(*) FILTER (WHERE st.status = 'held'),
		       count(*) FILTER (WHERE st.status = 'confirmed')
		FROM shows s JOIN seats st ON st.show_id = s.id
		WHERE s.id = $1 GROUP BY s.total_seats`, f.showID).Scan(&total, &avail, &held, &conf)
	if err != nil {
		f.t.Fatal(err)
	}
	if avail+held+conf != total {
		f.t.Fatalf("invariant broken: %d + %d + %d != %d", avail, held, conf, total)
	}
	var orphans int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM seats st
		LEFT JOIN reservations r ON r.id = st.reservation_id
		WHERE st.show_id = $1 AND st.status <> 'available'
		  AND (r.id IS NULL OR r.status <> 'confirmed' OR r.user_id <> st.user_id
		       OR NOT (st.label = ANY (r.seat_labels)))`, f.showID).Scan(&orphans); err != nil {
		f.t.Fatal(err)
	}
	if orphans != 0 {
		f.t.Fatalf("%d seats are held without a matching confirmed reservation", orphans)
	}
	return conf
}

func code(o Outcome) string {
	var e errorEnvelope
	_ = json.Unmarshal(o.Body, &e)
	return e.Error.Code
}

func TestHotSeatHasExactlyOneWinner(t *testing.T) {
	f := newFixture(t, 10, 4)
	const n = 200

	var wg sync.WaitGroup
	outs := make([]Outcome, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i] = f.reserve(fmt.Sprintf("u%d", i), "k", "A1")
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, o := range outs {
		switch {
		case o.Status == http.StatusCreated:
			wins++
		case o.Status == http.StatusConflict && (code(o) == ReasonSeatTaken || code(o) == ReasonSeatContended):
		default:
			t.Fatalf("unexpected outcome %d %s", o.Status, o.Body)
		}
	}
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins)
	}
	if got := f.checkInvariant(); got != 1 {
		t.Fatalf("confirmed seats = %d, want 1", got)
	}
}

func TestOverlappingMultiSeatIsAllOrNothing(t *testing.T) {
	f := newFixture(t, 50, 4)

	// User i wants A<i> and A<i+1>: every request overlaps its neighbours.
	var wg sync.WaitGroup
	outs := make([]Outcome, 49)
	for i := 1; i <= 49; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i-1] = f.reserve(fmt.Sprintf("u%d", i), "k", fmt.Sprintf("A%d", i+1), fmt.Sprintf("A%d", i))
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, o := range outs {
		if o.Status == http.StatusCreated {
			wins++
			var c Confirmed
			if err := json.Unmarshal(o.Body, &c); err != nil || len(c.Seats) != 2 {
				t.Fatalf("bad 201 body: %s", o.Body)
			}
		} else if o.Status != http.StatusConflict {
			t.Fatalf("unexpected outcome %d %s", o.Status, o.Body)
		}
	}
	if wins == 0 {
		t.Fatal("no request succeeded")
	}
	if got := f.checkInvariant(); got != 2*wins {
		t.Fatalf("confirmed seats = %d, want %d (2 per success, no partial bookings)", got, 2*wins)
	}
}

func TestIdempotencySequential(t *testing.T) {
	f := newFixture(t, 10, 4)

	first := f.reserve("alice", "key-1", "A1", "A2")
	if first.Status != http.StatusCreated || first.Replayed {
		t.Fatalf("first: %d replayed=%v %s", first.Status, first.Replayed, first.Body)
	}

	again := f.reserve("alice", "key-1", "A2", "A1") // same seats, different order
	if !again.Replayed || again.Status != first.Status || string(again.Body) != string(first.Body) {
		t.Fatalf("replay differs:\nfirst %s\nagain %s", first.Body, again.Body)
	}

	mismatch := f.reserve("alice", "key-1", "A3")
	if mismatch.Status != http.StatusConflict || code(mismatch) != "idempotency_key_reused" {
		t.Fatalf("mismatch: %d %s", mismatch.Status, mismatch.Body)
	}

	// Same key, different user: independent.
	if bob := f.reserve("bob", "key-1", "A3"); bob.Status != http.StatusCreated {
		t.Fatalf("bob: %d %s", bob.Status, bob.Body)
	}

	if got := f.checkInvariant(); got != 3 {
		t.Fatalf("confirmed seats = %d, want 3", got)
	}
}

func TestIdempotencyConcurrentDuplicates(t *testing.T) {
	f := newFixture(t, 10, 4)
	const n = 50

	var wg sync.WaitGroup
	outs := make([]Outcome, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i] = f.reserve("alice", "same-key", "A5", "A6")
		}(i)
	}
	wg.Wait()

	fresh := 0
	for _, o := range outs {
		if o.Status != http.StatusCreated {
			t.Fatalf("unexpected outcome %d %s", o.Status, o.Body)
		}
		if string(o.Body) != string(outs[0].Body) {
			t.Fatalf("bodies differ:\n%s\n%s", o.Body, outs[0].Body)
		}
		if !o.Replayed {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("fresh (non-replayed) responses = %d, want 1", fresh)
	}
	var reservations int
	if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM reservations").Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 1 {
		t.Fatalf("reservations created = %d, want 1", reservations)
	}
	f.checkInvariant()
}

func TestPerUserLimitUnderConcurrency(t *testing.T) {
	f := newFixture(t, 30, 4)
	const n = 20

	var wg sync.WaitGroup
	outs := make([]Outcome, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i] = f.reserve("alice", fmt.Sprintf("k%d", i), fmt.Sprintf("A%d", i+1))
		}(i)
	}
	wg.Wait()

	wins, limited := 0, 0
	for _, o := range outs {
		switch {
		case o.Status == http.StatusCreated:
			wins++
		case o.Status == http.StatusConflict && code(o) == ReasonPerUserLimit:
			limited++
		default:
			t.Fatalf("unexpected outcome %d %s", o.Status, o.Body)
		}
	}
	if wins != 4 || limited != 16 {
		t.Fatalf("wins=%d limited=%d, want 4 and 16", wins, limited)
	}
	if got := f.checkInvariant(); got != 4 {
		t.Fatalf("confirmed seats = %d, want 4", got)
	}

	// Another user is unaffected.
	if o := f.reserve("bob", "k", "A25"); o.Status != http.StatusCreated {
		t.Fatalf("bob: %d %s", o.Status, o.Body)
	}
}

func TestPerUserLimitAcrossRequests(t *testing.T) {
	f := newFixture(t, 10, 4)
	if o := f.reserve("alice", "k1", "A1", "A2", "A3"); o.Status != http.StatusCreated {
		t.Fatalf("first: %d %s", o.Status, o.Body)
	}
	o := f.reserve("alice", "k2", "A4", "A5")
	if o.Status != http.StatusConflict || code(o) != ReasonPerUserLimit {
		t.Fatalf("second: %d %s", o.Status, o.Body)
	}
	// Rejections are not stored: the same key is re-evaluated, not replayed.
	if again := f.reserve("alice", "k2", "A4", "A5"); again.Replayed || again.Status != http.StatusConflict {
		t.Fatalf("retry of limit decline: %d replayed=%v", again.Status, again.Replayed)
	}
	var keys int
	if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM idempotency_keys WHERE key = 'k2'").Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if keys != 0 {
		t.Fatalf("rejected request left %d key rows, want 0", keys)
	}
	if o := f.reserve("alice", "k3", "A4"); o.Status != http.StatusCreated {
		t.Fatalf("fourth seat: %d %s", o.Status, o.Body)
	}
}

func TestUnknownSeatsAndShow(t *testing.T) {
	f := newFixture(t, 5, 4)

	if o := f.reserve("alice", "k1", "A1", "Z99"); o.Status != http.StatusUnprocessableEntity || code(o) != "unknown_seats" {
		t.Fatalf("unknown seat: %d %s", o.Status, o.Body)
	}
	out, err := f.svc.Reserve(context.Background(), Request{
		ShowID: "00000000-0000-0000-0000-000000000000", UserID: "alice", Key: "k2", Seats: []string{"A1"},
	})
	if err != nil || out.Status != http.StatusNotFound {
		t.Fatalf("unknown show: %d %v", out.Status, err)
	}
	if got := f.checkInvariant(); got != 0 {
		t.Fatalf("confirmed seats = %d, want 0", got)
	}
}

func TestAmountIsIntegerPaiseAndReplayed(t *testing.T) {
	f := newFixture(t, 10, 4)

	o := f.reserve("alice", "k1", "A1", "A2")
	var c Confirmed
	if err := json.Unmarshal(o.Body, &c); err != nil || o.Status != http.StatusCreated {
		t.Fatalf("reserve: %d %s", o.Status, o.Body)
	}
	if c.AmountPaise != 2*testPrice || c.UserID != "alice" {
		t.Fatalf("amount_paise=%d user_id=%q, want %d and alice", c.AmountPaise, c.UserID, 2*testPrice)
	}
	var stored int64
	if err := f.pool.QueryRow(context.Background(), "SELECT amount_paise FROM reservations WHERE id = $1", c.ReservationID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 2*testPrice {
		t.Fatalf("stored amount = %d", stored)
	}
	if again := f.reserve("alice", "k1", "A1", "A2"); !again.Replayed || string(again.Body) != string(o.Body) {
		t.Fatalf("replay differs: %s", again.Body)
	}
}

func TestRequestAboveLimitIsLimitDecline(t *testing.T) {
	f := newFixture(t, 10, 2)
	o := f.reserve("alice", "k1", "A1", "A2", "A3")
	if o.Status != http.StatusConflict || code(o) != ReasonPerUserLimit {
		t.Fatalf("3 seats on a limit-2 show: %d %s", o.Status, o.Body)
	}
	if got := f.checkInvariant(); got != 0 {
		t.Fatalf("confirmed = %d, want 0", got)
	}
}
