package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tanmayt124/seat-reservation/internal/auth"
	"github.com/tanmayt124/seat-reservation/internal/store"
)

func (api *testAPI) reserve(showID, token, key string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/shows/"+showID+"/reserve", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	api.h.ServeHTTP(rec, req)
	return rec
}

func (api *testAPI) newShow(rows, perRow int) string {
	rec := api.do("POST", "/shows", api.token("admin1", auth.RoleAdmin),
		map[string]any{"name": "s", "rows": rows, "seats_per_row": perRow})
	if rec.Code != http.StatusCreated {
		api.t.Fatalf("create show: %d %s", rec.Code, rec.Body)
	}
	return decode[store.Show](api.t, rec).ID
}

func TestReserveHTTPFlow(t *testing.T) {
	api := newTestAPI(t)
	show := api.newShow(1, 10)
	alice := api.token("alice", auth.RoleUser)

	rec := api.reserve(show, alice, "k1", map[string]any{"seats": []string{"a1", "A2"}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("reserve: %d %s", rec.Code, rec.Body)
	}
	first := rec.Body.String()
	if !strings.Contains(first, `"seats": ["A1", "A2"]`) {
		t.Fatalf("labels not normalised/sorted: %s", first)
	}

	rec = api.reserve(show, alice, "k1", map[string]any{"seats": []string{"A2", "A1"}})
	if rec.Code != http.StatusCreated || rec.Header().Get("Idempotent-Replay") != "true" || rec.Body.String() != first {
		t.Fatalf("replay: %d replay=%q body=%s", rec.Code, rec.Header().Get("Idempotent-Replay"), rec.Body)
	}

	rec = api.reserve(show, api.token("bob", auth.RoleUser), "k1", map[string]any{"seats": []string{"A1"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("taken seat: %d %s", rec.Code, rec.Body)
	}

	d := decode[store.ShowDetail](t, api.do("GET", "/shows/"+show, alice, nil))
	if !d.InvariantOK || d.Counts.Confirmed != 2 || !d.Seats[0].Mine || d.Seats[2].Mine {
		t.Fatalf("show after reserve: %+v", d.Counts)
	}
}

func TestReserveHTTPValidation(t *testing.T) {
	api := newTestAPI(t)
	show := api.newShow(1, 10)
	alice := api.token("alice", auth.RoleUser)

	cases := map[string]struct {
		key    string
		body   any
		status int
		code   string
	}{
		"missing key":     {"", map[string]any{"seats": []string{"A1"}}, 400, "invalid_idempotency_key"},
		"key with spaces": {"a b", map[string]any{"seats": []string{"A1"}}, 400, "invalid_idempotency_key"},
		"no seats":        {"k", map[string]any{"seats": []string{}}, 422, "validation_failed"},
		"over the limit":  {"k3", map[string]any{"seats": []string{"A1", "A2", "A3", "A4", "A5"}}, 409, "per_user_limit_exceeded"},
		"header vs body":  {"k4", map[string]any{"seats": []string{"A1"}, "idempotency_key": "other"}, 400, "invalid_idempotency_key"},
		"duplicate seats": {"k", map[string]any{"seats": []string{"A1", "a1"}}, 422, "validation_failed"},
		"unknown seat":    {"k2", map[string]any{"seats": []string{"Q1"}}, 422, "unknown_seats"},
		"not json":        {"k", "nope", 400, "invalid_json"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := api.reserve(show, alice, c.key, c.body)
			env := decode[errorBody](t, rec)
			if rec.Code != c.status || env.Error.Code != c.code {
				t.Fatalf("got %d %s, want %d %s", rec.Code, env.Error.Code, c.status, c.code)
			}
		})
	}

	if rec := api.reserve("not-a-uuid", alice, "k", map[string]any{"seats": []string{"A1"}}); rec.Code != 400 {
		t.Fatalf("bad show id: %d", rec.Code)
	}
}

func TestReserveIgnoresAndLogsBodyUserID(t *testing.T) {
	api := newTestAPI(t)
	show := api.newShow(1, 5)

	rec := api.reserve(show, api.token("alice", auth.RoleUser), "k", map[string]any{"seats": []string{"A1"}, "user_id": "bob"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("reserve: %d %s", rec.Code, rec.Body)
	}
	// The seat belongs to alice (the token), not bob (the body).
	d := decode[store.ShowDetail](t, api.do("GET", "/shows/"+show, api.token("alice", auth.RoleUser), nil))
	if !d.Seats[0].Mine {
		t.Fatal("seat should belong to the token's user")
	}
	if logs := api.logs.String(); !strings.Contains(logs, `"msg":"spoof_attempt"`) || !strings.Contains(logs, `"source":"body"`) {
		t.Fatalf("body spoof not logged: %s", logs)
	}
}

// The exact request shapes from the brief: seats + price_paise on create,
// idempotency_key in the body, POST /reserve, POST .../cancel, public GET.
func TestBriefContract(t *testing.T) {
	api := newTestAPI(t)
	admin := api.token("admin1", auth.RoleAdmin)
	alice := api.token("alice", auth.RoleUser)

	rec := api.do("POST", "/shows", admin, map[string]any{
		"name": "friday-night", "seats": []string{"A1", "A2", "A12", "A13"}, "price_paise": 25000,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	created := decode[store.ShowDetail](t, rec)
	if created.PricePaise != 25000 || created.PerUserLimit != 4 || len(created.Seats) != 4 || created.Seats[0].Status != "available" {
		t.Fatalf("create body: %+v", created)
	}

	rec = api.reserve(created.ID, alice, "", map[string]any{"seats": []string{"A12"}, "idempotency_key": "key-1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("reserve with body key: %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"reservation_id", "show_id", "user_id", "seats", "amount_paise", "status"} {
		if _, ok := body[f]; !ok {
			t.Fatalf("201 body missing %q: %s", f, rec.Body)
		}
	}
	if body["user_id"] != "alice" || body["amount_paise"].(float64) != 25000 || body["status"] != "confirmed" {
		t.Fatalf("201 body: %s", rec.Body)
	}

	// Same key in the header now: same request, so a replay.
	rec = api.reserve(created.ID, alice, "key-1", map[string]any{"seats": []string{"A12"}})
	if rec.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("header key did not replay the body key: %d %s", rec.Code, rec.Body)
	}

	// Public show state, no token.
	rec = api.do("GET", "/shows/"+created.ID, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous GET: %d %s", rec.Code, rec.Body)
	}
	d := decode[store.ShowDetail](t, rec)
	if d.Counts.Confirmed != 1 || !d.InvariantOK || d.Seats[2].Mine {
		t.Fatalf("anonymous view: %+v", d.Counts)
	}
	// A present but bad token is still rejected.
	if rec := api.do("GET", "/shows/"+created.ID, "garbage", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token on GET: %d", rec.Code)
	}

	// Cancel as named in the brief.
	rid := body["reservation_id"].(string)
	if rec := api.do("POST", "/reservations/"+rid+"/cancel", alice, nil); rec.Code != http.StatusOK {
		t.Fatalf("POST cancel: %d %s", rec.Code, rec.Body)
	}
}
