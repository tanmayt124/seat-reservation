// Command burst runs the correctness scenarios and a load burst against a
// running server and exits non-zero if any check fails or any 5xx is seen.
//
//	go run ./cmd/burst -base-url http://localhost:8080
//
// It needs the server's test token endpoint (ENABLE_TOKEN_ENDPOINT=true).
// Each run creates fresh shows, so runs are independent and repeatable.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	baseURL     string
	concurrency int
	stampede    int
	users       int
	hotRounds   int
	hotUsers    int
	limit       int
	price       int64
	timeout     time.Duration
	// run tags every user id and idempotency key, so repeated runs against
	// the same server never collide with earlier runs' keys.
	run string
}

func main() {
	var cfg config
	flag.StringVar(&cfg.baseURL, "base-url", "http://localhost:8080", "server to test")
	flag.IntVar(&cfg.concurrency, "concurrency", 500, "max requests in flight")
	flag.IntVar(&cfg.stampede, "stampede", 17000, "requests in the stampede scenario")
	flag.IntVar(&cfg.users, "users", 4000, "distinct users in the stampede")
	flag.IntVar(&cfg.hotRounds, "hot-rounds", 5, "hot-seat rounds (one seat each)")
	flag.IntVar(&cfg.hotUsers, "hot-users", 500, "users racing per hot-seat round")
	flag.IntVar(&cfg.limit, "limit", 4, "per-user seat limit set on the test shows")
	flag.Int64Var(&cfg.price, "price-paise", 25000, "seat price set on the test shows, in paise")
	flag.DurationVar(&cfg.timeout, "timeout", 30*time.Second, "per-request timeout")
	flag.Parse()
	cfg.baseURL = strings.TrimRight(cfg.baseURL, "/")
	cfg.run = fmt.Sprintf("r%06x", rand.Int63()&0xffffff)

	c := newClient(cfg)
	fmt.Printf("burst run %s against %s (concurrency %d)\n\n", cfg.run, cfg.baseURL, cfg.concurrency)

	if err := c.preflight(); err != nil {
		fmt.Fprintln(os.Stderr, "preflight failed:", err)
		os.Exit(2)
	}
	admin := c.mustToken(cfg.run+"-admin", "admin")

	start := time.Now()
	results := []scenario{
		hotSeat(c, cfg, admin),
		stampede(c, cfg, admin),
		idempotency(c, cfg, admin),
		perUserLimit(c, cfg, admin),
		cancelAndSpoof(c, cfg, admin),
	}
	results = append(results, metricsCheck(c))
	elapsed := time.Since(start)

	failed := report(c, results, elapsed)
	if failed {
		os.Exit(1)
	}
}

// ---------- scenarios ----------

type scenario struct {
	name    string
	pass    bool
	skipped bool
	notes   []string
}

func (s *scenario) check(ok bool, format string, args ...any) {
	mark := "ok  "
	if !ok {
		mark = "FAIL"
		s.pass = false
	}
	s.notes = append(s.notes, mark+" "+fmt.Sprintf(format, args...))
}

func newScenario(name string) scenario { return scenario{name: name, pass: true} }

// hotSeat: many users race for one seat, several rounds. Exactly one winner
// per round.
func hotSeat(c *client, cfg config, admin string) scenario {
	s := newScenario(fmt.Sprintf("Hot seat (%d users x %d rounds)", cfg.hotUsers, cfg.hotRounds))
	show := c.mustCreateShow(admin, "burst hot seat", 1, cfg.hotRounds)

	for r := 1; r <= cfg.hotRounds; r++ {
		seat := fmt.Sprintf("A%d", r)
		tokens := c.mustTokens(fmt.Sprintf("%s-hot%d-", cfg.run, r), cfg.hotUsers)
		res := c.parallel(cfg.hotUsers, func(i int) result {
			return c.reserve(show, tokens[i], fmt.Sprintf("%s-hot-%d-%d", cfg.run, r, i), []string{seat}, nil)
		})
		wins, losers409, other := 0, 0, map[int]int{}
		for _, x := range res {
			switch x.status {
			case http.StatusCreated:
				wins++
			case http.StatusConflict:
				losers409++
			default:
				other[x.status]++
			}
		}
		s.check(wins == 1 && losers409 == cfg.hotUsers-1,
			"round %d seat %s: %d x 201, %d x 409, other %v (want exactly 1 and %d)", r, seat, wins, losers409, other, cfg.hotUsers-1)
	}

	d := c.mustShow(admin, show)
	s.check(d.Counts.Confirmed == cfg.hotRounds && d.InvariantOK,
		"show: %d confirmed of %d, invariant_ok=%v", d.Counts.Confirmed, d.TotalSeats, d.InvariantOK)
	return s
}

// stampede: lots of users, random 1-2 seat requests over a 1,000 seat show.
// No seat may be sold twice and the show's counts must match the 201s.
func stampede(c *client, cfg config, admin string) scenario {
	s := newScenario(fmt.Sprintf("Stampede (%d requests, %d users)", cfg.stampede, cfg.users))
	show := c.mustCreateShow(admin, "burst stampede", 10, 100)
	tokens := c.mustTokens(cfg.run+"-st-", cfg.users)

	type ask struct {
		user  int
		seats []string
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	asks := make([]ask, cfg.stampede)
	for i := range asks {
		row, n := 'A'+rng.Intn(10), 1+rng.Intn(99)
		seats := []string{fmt.Sprintf("%c%d", row, n)}
		if rng.Intn(2) == 0 {
			seats = append(seats, fmt.Sprintf("%c%d", row, n+1))
		}
		asks[i] = ask{user: rng.Intn(cfg.users), seats: seats}
	}

	t0 := time.Now()
	res := c.parallel(len(asks), func(i int) result {
		return c.reserve(show, tokens[asks[i].user], fmt.Sprintf("%s-st-%d", cfg.run, i), asks[i].seats, nil)
	})
	dur := time.Since(t0)

	sold := map[string]int{}
	perUser := map[int]int{}
	badAmounts := 0
	codes := map[int]int{}
	for i, x := range res {
		codes[x.status]++
		if x.status != http.StatusCreated {
			continue
		}
		var body struct {
			Seats       []string `json:"seats"`
			AmountPaise int64    `json:"amount_paise"`
		}
		_ = json.Unmarshal(x.body, &body)
		for _, seat := range body.Seats {
			sold[seat]++
		}
		if body.AmountPaise != cfg.price*int64(len(body.Seats)) {
			badAmounts++
		}
		perUser[asks[i].user] += len(body.Seats)
	}
	doubles := 0
	for _, n := range sold {
		if n > 1 {
			doubles++
		}
	}
	overLimit := 0
	for _, n := range perUser {
		if n > cfg.limit {
			overLimit++
		}
	}

	s.notes = append(s.notes, fmt.Sprintf("     %d requests in %s (%.0f req/s), codes %v",
		len(asks), dur.Round(time.Millisecond), float64(len(asks))/dur.Seconds(), codes))
	s.check(doubles == 0, "no seat sold twice (%d seats sold, %d doubles)", len(sold), doubles)
	s.check(overLimit == 0, "no user above %d seats (%d over)", cfg.limit, overLimit)
	s.check(badAmounts == 0, "every 201 has amount_paise = %d x seats (%d wrong)", cfg.price, badAmounts)

	d := c.mustShow(admin, show)
	s.check(d.InvariantOK, "invariant: %d available + %d held + %d confirmed = %d total",
		d.Counts.Available, d.Counts.Held, d.Counts.Confirmed, d.TotalSeats)
	s.check(d.Counts.Confirmed == len(sold), "server confirmed %d = seats in 201 responses %d", d.Counts.Confirmed, len(sold))
	return s
}

// idempotency: one key fired concurrently and sequentially makes one booking
// with identical responses; reusing the key with other seats is 409.
func idempotency(c *client, cfg config, admin string) scenario {
	s := newScenario("Idempotent retries")
	show := c.mustCreateShow(admin, "burst idempotency", 1, 10)
	tok := c.mustToken(cfg.run+"-idem", "user")
	key := cfg.run + "-same-key"
	seats := []string{"A1", "A2"}

	res := c.parallel(20, func(int) result { return c.reserve(show, tok, key, seats, nil) })
	for i := 0; i < 5; i++ {
		res = append(res, c.reserve(show, tok, key, []string{"A2", "A1"}, nil))
	}
	fresh, same, ok := 0, true, true
	for _, x := range res {
		if x.status != http.StatusCreated {
			ok = false
		}
		if x.header.Get("Idempotent-Replay") != "true" {
			fresh++
		}
		if !bytes.Equal(x.body, res[0].body) {
			same = false
		}
	}
	s.check(ok, "all %d attempts answered 201", len(res))
	s.check(fresh == 1, "exactly one fresh booking, %d replays (got %d fresh)", len(res)-fresh, fresh)
	s.check(same, "every response body is byte-identical")

	mm := c.reserve(show, tok, key, []string{"A3"}, nil)
	s.check(mm.status == http.StatusConflict && errCode(mm.body) == "idempotency_key_reused",
		"same key, different seats -> %d %s", mm.status, errCode(mm.body))

	d := c.mustShow(admin, show)
	s.check(d.Counts.Confirmed == 2 && d.InvariantOK, "show: %d confirmed (want 2), invariant_ok=%v", d.Counts.Confirmed, d.InvariantOK)
	return s
}

// perUserLimit: one user fires many parallel single-seat requests; exactly
// the limit succeed.
func perUserLimit(c *client, cfg config, admin string) scenario {
	s := newScenario(fmt.Sprintf("Per-user limit (%d)", cfg.limit))
	show := c.mustCreateShow(admin, "burst limit", 1, 30)
	tok := c.mustToken(cfg.run+"-limit", "user")

	res := c.parallel(20, func(i int) result {
		return c.reserve(show, tok, fmt.Sprintf("%s-lim-%d", cfg.run, i), []string{fmt.Sprintf("A%d", i+1)}, nil)
	})
	wins, limited, other := 0, 0, map[int]int{}
	for _, x := range res {
		switch {
		case x.status == http.StatusCreated:
			wins++
		case x.status == http.StatusConflict && errCode(x.body) == "per_user_limit_exceeded":
			limited++
		default:
			other[x.status]++
		}
	}
	s.check(wins == cfg.limit && limited == 20-cfg.limit,
		"20 parallel requests: %d x 201, %d x 409 per_user_limit_exceeded, other %v", wins, limited, other)

	d := c.mustShow(admin, show)
	s.check(d.Counts.Confirmed == cfg.limit && d.InvariantOK, "show: %d confirmed, invariant_ok=%v", d.Counts.Confirmed, d.InvariantOK)
	return s
}

// cancelAndSpoof: identity comes only from the token; only the owner can
// cancel; cancelled seats can be rebooked.
func cancelAndSpoof(c *client, cfg config, admin string) scenario {
	s := newScenario("Identity, cancel and rebook")
	show := c.mustCreateShow(admin, "burst cancel", 1, 5)
	aliceID := cfg.run + "-alice"
	alice := c.mustToken(aliceID, "user")
	bob := c.mustToken(cfg.run+"-bob", "user")

	r := c.reserve(show, alice, cfg.run+"-a1", []string{"A1", "A2"}, nil)
	var booked struct {
		ReservationID string `json:"reservation_id"`
	}
	_ = json.Unmarshal(r.body, &booked)
	s.check(r.status == http.StatusCreated, "alice books A1,A2 -> %d", r.status)

	x := c.do("POST", "/reservations/"+booked.ReservationID+"/cancel", bob, "", nil, true)
	s.check(x.status == http.StatusNotFound, "bob cancels alice's reservation -> %d (want 404)", x.status)
	d := c.mustShow(alice, show)
	s.check(d.seat("A1").Mine && d.seat("A2").Mine, "A1,A2 still alice's after bob's attempt")

	x = c.reserve(show, bob, cfg.run+"-b1", []string{"A3"}, map[string]any{"user_id": aliceID})
	d = c.mustShow(bob, show)
	s.check(x.status == http.StatusCreated && d.seat("A3").Mine, "bob sends user_id=alice in body -> seat booked as bob (%d)", x.status)

	x = c.do("POST", "/reservations/"+booked.ReservationID+"/cancel", alice, "", nil, true)
	y := c.do("POST", "/reservations/"+booked.ReservationID+"/cancel", alice, "", nil, true)
	s.check(x.status == http.StatusOK && y.status == http.StatusOK && bytes.Equal(x.body, y.body),
		"alice cancels -> %d, cancels again -> %d with same body", x.status, y.status)

	x = c.reserve(show, bob, cfg.run+"-b2", []string{"A1", "A2"}, nil)
	d = c.mustShow(bob, show)
	s.check(x.status == http.StatusCreated && d.seat("A1").Mine && d.seat("A2").Mine, "bob rebooks A1,A2 -> %d", x.status)
	s.check(d.Counts.Confirmed == 3 && d.InvariantOK, "show: %d confirmed (want 3), invariant_ok=%v", d.Counts.Confirmed, d.InvariantOK)
	return s
}

// metricsCheck confirms /metrics is served. Detailed reconciliation against
// the counters is added with the metrics story.
func metricsCheck(c *client) scenario {
	s := newScenario("Metrics endpoint")
	x := c.do("GET", "/metrics", "", "", nil, false)
	if x.status == http.StatusNotFound {
		s.skipped = true
		s.notes = append(s.notes, "skip /metrics not served yet")
		return s
	}
	s.check(x.status == http.StatusOK, "/metrics -> %d", x.status)
	return s
}

// ---------- report ----------

func report(c *client, results []scenario, elapsed time.Duration) (failed bool) {
	fmt.Println()
	for _, s := range results {
		status := "PASS"
		switch {
		case s.skipped:
			status = "SKIP"
		case !s.pass:
			status = "FAIL"
			failed = true
		}
		fmt.Printf("[%s] %s\n", status, s.name)
		for _, n := range s.notes {
			fmt.Println("       " + n)
		}
	}

	st := c.stats.snapshot()
	fmt.Println()
	fmt.Printf("Requests counted: %d in %s\n", st.total, elapsed.Round(time.Millisecond))
	fmt.Printf("Outcomes:         %s\n", formatOutcomes(st.outcomes))
	fmt.Printf("Status codes:     %s\n", formatCodes(st.codes))
	fmt.Printf("Latency:          p50 %s  p95 %s  p99 %s  max %s\n", st.p(0.50), st.p(0.95), st.p(0.99), st.p(1))
	fmt.Printf("429 retries:      %d (client honoured Retry-After)\n", st.retries)

	fiveXX := 0
	for code, n := range st.codes {
		if code >= 500 {
			fiveXX += n
		}
	}
	if fiveXX > 0 {
		fmt.Printf("FAIL: %d responses were 5xx\n", fiveXX)
		failed = true
	}
	if st.transportErrors > 0 {
		fmt.Printf("FAIL: %d requests got no HTTP response (timeouts or connection errors)\n", st.transportErrors)
		failed = true
	}
	if failed {
		fmt.Println("\nRESULT: FAIL")
	} else {
		fmt.Println("\nRESULT: PASS (zero 5xx, all checks green)")
	}
	return failed
}

// formatOutcomes prints confirmed first, then declines by reason, then 5xx.
func formatOutcomes(m map[string]int) string {
	parts := []string{fmt.Sprintf("confirmed %d", m["confirmed"])}
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "confirmed" && k != "5xx" && k != "ok" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	declines := make([]string, 0, len(keys))
	for _, k := range keys {
		declines = append(declines, fmt.Sprintf("%s %d", k, m[k]))
	}
	parts = append(parts, "declined: "+strings.Join(declines, ", "))
	parts = append(parts, fmt.Sprintf("5xx %d", m["5xx"]))
	return strings.Join(parts, " | ")
}

func formatCodes(m map[int]int) string {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d x %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// ---------- HTTP client ----------

type result struct {
	status int
	body   []byte
	header http.Header
	err    error
}

type client struct {
	cfg   config
	http  *http.Client
	stats *stats
}

func newClient(cfg config) *client {
	tr := &http.Transport{
		MaxIdleConns:        cfg.concurrency * 2,
		MaxIdleConnsPerHost: cfg.concurrency * 2,
		MaxConnsPerHost:     cfg.concurrency,
		IdleConnTimeout:     90 * time.Second,
	}
	return &client{cfg: cfg, http: &http.Client{Timeout: cfg.timeout, Transport: tr}, stats: &stats{codes: map[int]int{}, outcomes: map[string]int{}}}
}

// do sends one request. Reservation and cancel calls are recorded in the
// stats; setup calls (tokens, shows) are not. A 429 is retried up to three
// times after Retry-After, as a well-behaved client would.
func (c *client) do(method, path, token, idemKey string, body any, record bool) result {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	var res result
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(method, c.cfg.baseURL+path, bytes.NewReader(payload))
		if err != nil {
			return result{err: err}
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if idemKey != "" {
			req.Header.Set("Idempotency-Key", idemKey)
		}
		t0 := time.Now()
		resp, err := c.http.Do(req)
		lat := time.Since(t0)
		if err != nil {
			res = result{err: err}
		} else {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			res = result{status: resp.StatusCode, body: b, header: resp.Header}
		}
		if record {
			c.stats.add(res, lat)
		}
		if res.status != http.StatusTooManyRequests || attempt == 3 {
			return res
		}
		if record {
			c.stats.retry()
		}
		wait := time.Second
		if n, err := strconv.Atoi(res.header.Get("Retry-After")); err == nil && n > 0 {
			wait = time.Duration(n) * time.Second
		}
		time.Sleep(wait/2 + time.Duration(rand.Int63n(int64(wait))))
	}
}

// reserve calls POST /shows/{id}/reserve. The key goes in the body
// (idempotency_key) or the header depending on its length's parity, so a run
// exercises both ways the brief allows.
func (c *client) reserve(show, token, key string, seats []string, extra map[string]any) result {
	body := map[string]any{"seats": seats}
	for k, v := range extra {
		body[k] = v
	}
	header := key
	if len(key)%2 == 1 {
		body["idempotency_key"] = key
		header = ""
	}
	return c.do("POST", "/shows/"+show+"/reserve", token, header, body, true)
}

// parallel runs n calls with at most cfg.concurrency in flight.
func (c *client) parallel(n int, fn func(i int) result) []result {
	out := make([]result, n)
	sem := make(chan struct{}, c.cfg.concurrency)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = fn(i)
		}(i)
	}
	wg.Wait()
	return out
}

func (c *client) preflight() error {
	x := c.do("GET", "/healthz", "", "", nil, false)
	if x.err != nil || x.status != http.StatusOK {
		return fmt.Errorf("GET /healthz: status %d err %v", x.status, x.err)
	}
	x = c.do("POST", "/auth/token", "", "", map[string]string{"user_id": "preflight"}, false)
	if x.status == http.StatusNotFound {
		return fmt.Errorf("POST /auth/token is not enabled; start the server with ENABLE_TOKEN_ENDPOINT=true")
	}
	if x.status != http.StatusOK {
		return fmt.Errorf("POST /auth/token: status %d %s", x.status, x.body)
	}
	return nil
}

func (c *client) mustToken(user, role string) string {
	x := c.do("POST", "/auth/token", "", "", map[string]string{"user_id": user, "role": role}, false)
	var v struct{ Token string }
	if x.status != http.StatusOK || json.Unmarshal(x.body, &v) != nil || v.Token == "" {
		fmt.Fprintf(os.Stderr, "token for %s: %d %s %v\n", user, x.status, x.body, x.err)
		os.Exit(2)
	}
	return v.Token
}

func (c *client) mustTokens(prefix string, n int) []string {
	out := make([]string, n)
	var next atomic.Int64
	var wg sync.WaitGroup
	workers := min(64, n)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				out[i] = c.mustToken(fmt.Sprintf("%s%d", prefix, i), "user")
			}
		}()
	}
	wg.Wait()
	return out
}

// mustCreateShow creates a show the way the brief does: an explicit seat
// list, a price in paise and the per-user limit.
func (c *client) mustCreateShow(admin, name string, rows, perRow int) string {
	seats := make([]string, 0, rows*perRow)
	for r := 0; r < rows; r++ {
		for n := 1; n <= perRow; n++ {
			seats = append(seats, fmt.Sprintf("%c%d", 'A'+r, n))
		}
	}
	x := c.do("POST", "/shows", admin, "", map[string]any{
		"name": name, "seats": seats, "price_paise": c.cfg.price, "per_user_limit": c.cfg.limit,
	}, false)
	var v struct{ ID string }
	if x.status != http.StatusCreated || json.Unmarshal(x.body, &v) != nil {
		fmt.Fprintf(os.Stderr, "create show: %d %s %v\n", x.status, x.body, x.err)
		os.Exit(2)
	}
	return v.ID
}

type seatView struct {
	Label  string `json:"label"`
	Status string `json:"status"`
	Mine   bool   `json:"mine"`
}

type showDetail struct {
	TotalSeats int `json:"total_seats"`
	Counts     struct {
		Available int `json:"available"`
		Held      int `json:"held"`
		Confirmed int `json:"confirmed"`
	} `json:"counts"`
	InvariantOK bool       `json:"invariant_ok"`
	Seats       []seatView `json:"seats"`
}

// seat returns the named seat, or a zero value if it is missing.
func (d showDetail) seat(label string) seatView {
	for _, s := range d.Seats {
		if s.Label == label {
			return s
		}
	}
	return seatView{}
}

func (c *client) mustShow(token, id string) showDetail {
	x := c.do("GET", "/shows/"+id, token, "", nil, false)
	var d showDetail
	if x.status != http.StatusOK || json.Unmarshal(x.body, &d) != nil {
		fmt.Fprintf(os.Stderr, "get show: %d %s %v\n", x.status, x.body, x.err)
		os.Exit(2)
	}
	return d
}

// outcome names what a response meant: confirmed, idempotent_replay, the
// decline reason from the error body, or 5xx.
func outcome(r result) string {
	switch {
	case r.status >= 500:
		return "5xx"
	case r.header.Get("Idempotent-Replay") == "true":
		return "idempotent_replay"
	case r.status == http.StatusCreated:
		return "confirmed"
	case r.status == http.StatusOK:
		return "ok"
	}
	if code := errCode(r.body); code != "" {
		return code
	}
	return fmt.Sprintf("http_%d", r.status)
}

func errCode(body []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return e.Error.Code
}

// ---------- stats ----------

type stats struct {
	mu              sync.Mutex
	codes           map[int]int
	outcomes        map[string]int
	lat             []time.Duration
	transportErrors int
	retries         int
}

func (s *stats) add(r result, lat time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.err != nil {
		s.transportErrors++
		return
	}
	s.codes[r.status]++
	s.outcomes[outcome(r)]++
	s.lat = append(s.lat, lat)
}

func (s *stats) retry() {
	s.mu.Lock()
	s.retries++
	s.mu.Unlock()
}

type snapshot struct {
	codes           map[int]int
	outcomes        map[string]int
	lat             []time.Duration
	total           int
	transportErrors int
	retries         int
}

func (s *stats) snapshot() snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	lat := append([]time.Duration(nil), s.lat...)
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	codes := map[int]int{}
	total := s.transportErrors
	for k, v := range s.codes {
		codes[k] = v
		total += v
	}
	outcomes := map[string]int{}
	for k, v := range s.outcomes {
		outcomes[k] = v
	}
	return snapshot{codes: codes, outcomes: outcomes, lat: lat, total: total, transportErrors: s.transportErrors, retries: s.retries}
}

func (s snapshot) p(q float64) time.Duration {
	if len(s.lat) == 0 {
		return 0
	}
	return s.lat[int(q*float64(len(s.lat)-1))].Round(time.Millisecond)
}
