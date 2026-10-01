package httpapi

import (
	"bufio"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/tanmayt124/seat-reservation/internal/auth"
)

// scrape returns every sample on /metrics keyed by "name{labels}".
func (api *testAPI) scrape() map[string]float64 {
	api.t.Helper()
	rec := api.do("GET", "/metrics", "", nil)
	if rec.Code != http.StatusOK {
		api.t.Fatalf("/metrics: %d", rec.Code)
	}
	out := map[string]float64{}
	sc := bufio.NewScanner(rec.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			continue
		}
		out[line[:i]] = v
	}
	return out
}

func TestMetricsReconcileWithAPI(t *testing.T) {
	api := newTestAPI(t)
	show := api.newShow(1, 10)
	alice := api.token("alice", auth.RoleUser)
	bob := api.token("bob", auth.RoleUser)

	api.reserve(show, alice, "k1", map[string]any{"seats": []string{"A1", "A2"}})       // confirmed
	api.reserve(show, alice, "k1", map[string]any{"seats": []string{"A1", "A2"}})       // replay
	api.reserve(show, bob, "k1", map[string]any{"seats": []string{"A1"}})               // seat taken
	api.reserve(show, alice, "k2", map[string]any{"seats": []string{"A3", "A4", "A5"}}) // over limit (2 + 3 > 4)
	rec := api.reserve(show, bob, "k2", map[string]any{"seats": []string{"A9"}})        // confirmed
	rid := decode[map[string]any](t, rec)["reservation_id"].(string)
	api.do("POST", "/reservations/"+rid+"/cancel", bob, nil) // releases 1
	api.do("POST", "/reservations/"+rid+"/cancel", bob, nil) // repeat, not counted

	m := api.scrape()
	want := map[string]float64{
		`reservations_confirmed_total`:                            2,
		`seats_confirmed_total`:                                   3,
		`reservations_declined_total{reason="idempotent_replay"}`: 1,
		`reservations_declined_total{reason="seat_taken"}`:        1,
		`reservations_declined_total{reason="per_user_limit"}`:    1,
		`reservations_cancelled_total`:                            1,
		`seats_released_total`:                                    1,
		`seats{show_id="` + show + `",status="available"}`:        8,
		`seats{show_id="` + show + `",status="confirmed"}`:        2,
		`seats{show_id="` + show + `",status="held"}`:             0,
		`seats_total{show_id="` + show + `"}`:                     10,
		`seats_invariant_ok{show_id="` + show + `"}`:              1,
		`seats_scrape_error`:                                      0,
	}
	for k, v := range want {
		if got, ok := m[k]; !ok || got != v {
			t.Errorf("%s = %v (present=%v), want %v", k, got, ok, v)
		}
	}
	if m[`http_requests_total{code="201",method="POST",route="/shows/{showID}/reserve"}`] != 3 {
		t.Errorf("http_requests_total for reserve 201 = %v, want 3",
			m[`http_requests_total{code="201",method="POST",route="/shows/{showID}/reserve"}`])
	}
	if _, ok := m["db_pool_max_conns"]; !ok {
		t.Error("db pool metrics missing")
	}

	// The API agrees with the gauge.
	d := decode[map[string]any](t, api.do("GET", "/shows/"+show, "", nil))
	counts := d["counts"].(map[string]any)
	if counts["confirmed"].(float64) != 2 || counts["available"].(float64) != 8 {
		t.Fatalf("API counts %v disagree with metrics", counts)
	}
}
