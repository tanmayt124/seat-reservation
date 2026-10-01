package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tanmayt124/seat-reservation/internal/auth"
	"github.com/tanmayt124/seat-reservation/internal/reserve"
	"github.com/tanmayt124/seat-reservation/internal/store"
)

func TestCancelHTTP(t *testing.T) {
	api := newTestAPI(t)
	show := api.newShow(1, 5)
	alice := api.token("alice", auth.RoleUser)
	bob := api.token("bob", auth.RoleUser)

	rec := api.reserve(show, alice, "k1", map[string]any{"seats": []string{"A1", "A2"}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("reserve: %d %s", rec.Code, rec.Body)
	}
	rid := decode[reserve.Confirmed](t, rec).ReservationID

	// Bob cannot cancel alice's reservation, and cannot tell it exists.
	rec = api.do("POST", "/reservations/"+rid+"/cancel", bob, nil)
	if rec.Code != http.StatusNotFound || decode[errorBody](t, rec).Error.Code != "reservation_not_found" {
		t.Fatalf("non-owner: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(api.logs.String(), `"msg":"cancel_denied"`) {
		t.Fatal("cancel_denied not logged")
	}

	rec = api.do("POST", "/reservations/"+rid+"/cancel", alice, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner cancel: %d %s", rec.Code, rec.Body)
	}
	first := decode[reserve.Cancelled](t, rec)
	if first.Status != "cancelled" || len(first.Seats) != 2 {
		t.Fatalf("cancel body: %+v", first)
	}

	// Repeat cancel: same answer.
	rec = api.do("DELETE", "/reservations/"+rid, alice, nil)
	if again := decode[reserve.Cancelled](t, rec); rec.Code != http.StatusOK || !again.CancelledAt.Equal(first.CancelledAt) {
		t.Fatalf("repeat cancel: %d %s", rec.Code, rec.Body)
	}

	// Bob can now book the seats.
	if rec := api.reserve(show, bob, "k1", map[string]any{"seats": []string{"A1", "A2"}}); rec.Code != http.StatusCreated {
		t.Fatalf("rebook: %d %s", rec.Code, rec.Body)
	}
	d := decode[store.ShowDetail](t, api.do("GET", "/shows/"+show, bob, nil))
	if !d.InvariantOK || d.Counts.Confirmed != 2 || !d.Seats[0].Mine {
		t.Fatalf("after rebook: %+v", d.Counts)
	}

	if rec := api.do("DELETE", "/reservations/nope", alice, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", rec.Code)
	}
	if rec := api.do("DELETE", "/reservations/"+rid, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
}
