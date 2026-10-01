package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tanmayt124/seat-reservation/internal/auth"
	"github.com/tanmayt124/seat-reservation/internal/reserve"
	"github.com/tanmayt124/seat-reservation/internal/store"
	"github.com/tanmayt124/seat-reservation/internal/testutil"
	"github.com/tanmayt124/seat-reservation/migrations"
)

const testSecret = "httpapi-test-secret-httpapi-test-secret"

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type testAPI struct {
	t    *testing.T
	h    http.Handler
	auth *auth.Authenticator
	logs *syncBuffer
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	pool := testutil.Pool(t)
	if err := store.Migrate(context.Background(), pool, migrations.FS, testutil.QuietLogger()); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	a := auth.New(testSecret)
	svc := reserve.NewService(pool, 4, testutil.QuietLogger())
	svc.SetAcquireTimeout(30 * time.Second)
	return &testAPI{
		t:    t,
		auth: a,
		logs: logs,
		h: NewRouter(Deps{
			Logger:              slog.New(slog.NewJSONHandler(logs, nil)),
			Auth:                a,
			Shows:               store.NewShows(pool),
			Reserve:             svc,
			EnableTokenEndpoint: true,
		}),
	}
}

func (api *testAPI) token(user string, role auth.Role) string {
	tok, _, err := api.auth.Issue(user, role, time.Hour)
	if err != nil {
		api.t.Fatal(err)
	}
	return tok
}

func (api *testAPI) do(method, path, token string, body any) *httptest.ResponseRecorder {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	api.h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestTokenEndpoint(t *testing.T) {
	api := newTestAPI(t)

	rec := api.do("POST", "/auth/token", "", map[string]string{"user_id": "alice"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := decode[map[string]any](t, rec)
	id, err := api.auth.Verify(got["token"].(string))
	if err != nil || id.UserID != "alice" || id.Role != auth.RoleUser {
		t.Fatalf("token does not verify as alice/user: %+v %v", id, err)
	}

	rec = api.do("POST", "/auth/token", "", map[string]string{"user_id": "bad id", "role": "root"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", rec.Code)
	}
}

func TestShowsRequireAuth(t *testing.T) {
	api := newTestAPI(t)
	body := map[string]any{"name": "x", "rows": 1, "seats_per_row": 1}

	if rec := api.do("POST", "/shows", "", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", rec.Code)
	}
	if rec := api.do("POST", "/shows", "garbage", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d, want 401", rec.Code)
	}
	if rec := api.do("POST", "/shows", api.token("bob", auth.RoleUser), body); rec.Code != http.StatusForbidden {
		t.Fatalf("user role: %d, want 403", rec.Code)
	}
}

func TestCreateAndGetShow(t *testing.T) {
	api := newTestAPI(t)
	admin := api.token("admin1", auth.RoleAdmin)

	rec := api.do("POST", "/shows", admin, map[string]any{"name": "Evening", "rows": 10, "seats_per_row": 100})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	show := decode[store.Show](t, rec)
	if show.TotalSeats != 1000 {
		t.Fatalf("total_seats = %d, want 1000", show.TotalSeats)
	}

	rec = api.do("GET", "/shows/"+show.ID, api.token("alice", auth.RoleUser), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	d := decode[store.ShowDetail](t, rec)
	if !d.InvariantOK || d.Counts.Available != 1000 || len(d.Seats) != 1000 {
		t.Fatalf("unexpected detail: invariant=%v counts=%+v seats=%d", d.InvariantOK, d.Counts, len(d.Seats))
	}
	// Natural order: A1, A2, ... A10, not A1, A10, A100.
	if d.Seats[0].Label != "A1" || d.Seats[9].Label != "A10" || d.Seats[100].Label != "B1" {
		t.Fatalf("unexpected order: %s %s %s", d.Seats[0].Label, d.Seats[9].Label, d.Seats[100].Label)
	}
}

func TestCreateShowExplicitLabels(t *testing.T) {
	api := newTestAPI(t)
	admin := api.token("admin1", auth.RoleAdmin)

	rec := api.do("POST", "/shows", admin, map[string]any{"name": "Small", "seat_labels": []string{"vip-1", "VIP-2"}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	show := decode[store.Show](t, rec)
	d := decode[store.ShowDetail](t, api.do("GET", "/shows/"+show.ID, admin, nil))
	if d.Seats[0].Label != "VIP-1" || d.Seats[1].Label != "VIP-2" {
		t.Fatalf("labels not normalised: %+v", d.Seats)
	}
}

func TestCreateShowValidation(t *testing.T) {
	api := newTestAPI(t)
	admin := api.token("admin1", auth.RoleAdmin)

	cases := map[string]struct {
		body  map[string]any
		field string
	}{
		"missing name":    {map[string]any{"rows": 1, "seats_per_row": 1}, "name"},
		"no layout":       {map[string]any{"name": "x"}, "seat_labels"},
		"both layouts":    {map[string]any{"name": "x", "rows": 1, "seats_per_row": 1, "seat_labels": []string{"A1"}}, "seat_labels"},
		"too many rows":   {map[string]any{"name": "x", "rows": 27, "seats_per_row": 1}, "rows"},
		"row too long":    {map[string]any{"name": "x", "rows": 1, "seats_per_row": 501}, "seats_per_row"},
		"duplicate label": {map[string]any{"name": "x", "seat_labels": []string{"A1", "a1"}}, "seat_labels"},
		"bad label":       {map[string]any{"name": "x", "seat_labels": []string{"A 1"}}, "seat_labels"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := api.do("POST", "/shows", admin, c.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422: %s", rec.Code, rec.Body)
			}
			env := decode[errorBody](t, rec)
			details, _ := env.Error.Details.(map[string]any)
			if _, ok := details[c.field]; !ok {
				t.Fatalf("details %v missing field %q", env.Error.Details, c.field)
			}
		})
	}

	rec := api.do("POST", "/shows", admin, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body: %d, want 400", rec.Code)
	}
}

func TestGetShowErrors(t *testing.T) {
	api := newTestAPI(t)
	tok := api.token("alice", auth.RoleUser)

	if rec := api.do("GET", "/shows/not-a-uuid", tok, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d, want 400", rec.Code)
	}
	if rec := api.do("GET", "/shows/00000000-0000-0000-0000-000000000000", tok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d, want 404", rec.Code)
	}
}

func TestSpoofedUserIDIsLoggedAndIgnored(t *testing.T) {
	api := newTestAPI(t)
	admin := api.token("admin1", auth.RoleAdmin)
	show := decode[store.Show](t, api.do("POST", "/shows", admin, map[string]any{"name": "x", "rows": 1, "seats_per_row": 2}))

	rec := api.do("GET", "/shows/"+show.ID+"?user_id=bob", api.token("alice", auth.RoleUser), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	logs := api.logs.String()
	if !strings.Contains(logs, `"msg":"spoof_attempt"`) || !strings.Contains(logs, `"claimed_user_id":"bob"`) {
		t.Fatalf("spoof attempt not logged; logs: %s", logs)
	}
}
