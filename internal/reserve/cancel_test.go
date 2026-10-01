package reserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
)

func (f *fixture) mustReserve(user, key string, seats ...string) string {
	f.t.Helper()
	o := f.reserve(user, key, seats...)
	if o.Status != http.StatusCreated {
		f.t.Fatalf("reserve %v for %s: %d %s", seats, user, o.Status, o.Body)
	}
	var c Confirmed
	if err := json.Unmarshal(o.Body, &c); err != nil {
		f.t.Fatal(err)
	}
	return c.ReservationID
}

func TestCancelReleasesSeatsForRebooking(t *testing.T) {
	f := newFixture(t, 10, 4)
	ctx := context.Background()
	rid := f.mustReserve("alice", "k1", "A1", "A2")

	c, err := f.svc.Cancel(ctx, rid, "alice")
	if err != nil || c.SeatsReleased != 2 || c.AlreadyCancelled {
		t.Fatalf("cancel: %+v %v", c, err)
	}
	if got := f.checkInvariant(); got != 0 {
		t.Fatalf("confirmed after cancel = %d, want 0", got)
	}

	// Freed seats are bookable by someone else, and alice's quota is back.
	f.mustReserve("bob", "k1", "A1", "A2")
	f.mustReserve("alice", "k2", "A3", "A4", "A5", "A6")
	f.checkInvariant()
}

func TestCancelByNonOwnerIsRejected(t *testing.T) {
	f := newFixture(t, 10, 4)
	ctx := context.Background()
	rid := f.mustReserve("alice", "k1", "A1")

	if _, err := f.svc.Cancel(ctx, rid, "mallory"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("got %v, want ErrNotOwner", err)
	}
	if _, err := f.svc.Cancel(ctx, "00000000-0000-0000-0000-000000000000", "alice"); !errors.Is(err, ErrReservationNotFound) {
		t.Fatalf("got %v, want ErrReservationNotFound", err)
	}
	if got := f.checkInvariant(); got != 1 {
		t.Fatalf("seat should still be alice's; confirmed = %d", got)
	}
}

func TestStaleCancelNeverFreesSomeoneElsesSeat(t *testing.T) {
	f := newFixture(t, 10, 4)
	ctx := context.Background()

	ridA := f.mustReserve("alice", "k1", "A1")
	first, err := f.svc.Cancel(ctx, ridA, "alice")
	if err != nil {
		t.Fatal(err)
	}
	f.mustReserve("bob", "k1", "A1")

	// Alice retries her cancel (a client retry after a timeout, say).
	again, err := f.svc.Cancel(ctx, ridA, "alice")
	if err != nil || !again.AlreadyCancelled || again.SeatsReleased != 0 || !again.CancelledAt.Equal(first.CancelledAt) {
		t.Fatalf("second cancel: %+v %v", again, err)
	}
	var holder string
	if err := f.pool.QueryRow(ctx, "SELECT user_id FROM seats WHERE show_id = $1 AND label = 'A1'", f.showID).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	if holder != "bob" {
		t.Fatalf("A1 holder = %q, want bob", holder)
	}
	f.checkInvariant()
}

// Cancels and reserves on the same seats race repeatedly. Nothing may
// deadlock, error, or break the invariant.
func TestCancelAndReserveRace(t *testing.T) {
	f := newFixture(t, 4, 4)
	ctx := context.Background()

	for round := 0; round < 30; round++ {
		rid := f.mustReserve("alice", fmt.Sprintf("a%d", round), "A1", "A2", "A3")

		var wg sync.WaitGroup
		var cancelErr error
		outs := make([]Outcome, 5)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, cancelErr = f.svc.Cancel(ctx, rid, "alice")
		}()
		for i := range outs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				outs[i] = f.reserve(fmt.Sprintf("u%d", i), fmt.Sprintf("r%d", round), "A3", "A2", "A1")
			}(i)
		}
		wg.Wait()

		if cancelErr != nil {
			t.Fatalf("round %d: cancel error %v", round, cancelErr)
		}
		winner := ""
		for i, o := range outs {
			if o.Status == http.StatusCreated {
				if winner != "" {
					t.Fatalf("round %d: two winners", round)
				}
				winner = fmt.Sprintf("u%d", i)
			} else if o.Status != http.StatusConflict {
				t.Fatalf("round %d: unexpected %d %s", round, o.Status, o.Body)
			}
		}
		f.checkInvariant()

		// Reset: release whatever the winner got.
		if winner != "" {
			var wrid string
			if err := f.pool.QueryRow(ctx, "SELECT reservation_id FROM seats WHERE show_id = $1 AND label = 'A1'", f.showID).Scan(&wrid); err != nil {
				t.Fatal(err)
			}
			if _, err := f.svc.Cancel(ctx, wrid, winner); err != nil {
				t.Fatal(err)
			}
		}
	}
}
