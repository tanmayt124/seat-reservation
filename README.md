# seat-reservation

A small service that sells assigned seats for a show and stays correct when thousands of buyers hit the same seats at once. Built for the Paytm Money backend take-home ("Seat Reservation at Scale").

- **Live URL:** `<LIVE_URL>`
- **Metrics:** `<LIVE_URL>/metrics`
- **Logs:** `<LOGS_LINK_OR_RECORDING>`
- **Design notes:** [WRITEUP.md](WRITEUP.md)

Go 1.23, Postgres 16, chi, pgx. One database, no cache, no queue.

## What it guarantees

- A seat is never confirmed to two users. In a hot-seat storm exactly one request gets `201`, everyone else gets `409`.
- Zero 5xx under burst. Every decline is a 4xx with a reason code.
- `available + held + confirmed == total_seats` for every show, at all times. `GET /shows/{id}` and `/metrics` both report it.
- The same idempotency key books once. Same key with different seats is `409`.
- A user never holds more than the show's `per_user_limit` (default 4), even with parallel requests.
- Identity comes only from the token. A `user_id` in the body, query or headers is ignored and logged as `spoof_attempt`.

## Run it locally

Needs Docker. Go 1.23+ only if you want to run tests or the burst script outside Docker.

```bash
docker compose up --build -d      # or: make up
curl localhost:8080/readyz        # {"status":"ready"}
```

Get tokens (test helper, enabled in compose), create a show, book a seat:

```bash
ADMIN=$(curl -s -XPOST localhost:8080/auth/token -d '{"user_id":"admin1","role":"admin"}' | jq -r .token)
ME=$(curl -s -XPOST localhost:8080/auth/token -d '{"user_id":"tanmay"}' | jq -r .token)

SHOW=$(curl -s -XPOST localhost:8080/shows -H "Authorization: Bearer $ADMIN" \
  -d '{"name":"friday-night","seats":["A1","A2","A12","A13"],"price_paise":25000}' | jq -r .id)

curl -s -XPOST localhost:8080/shows/$SHOW/reserve -H "Authorization: Bearer $ME" \
  -d '{"seats":["A12"],"idempotency_key":"order-1"}'

curl -s localhost:8080/shows/$SHOW | jq .counts
```

## One-command burst

```bash
./burst.sh <BASE_URL>             # e.g. ./burst.sh http://localhost:8080
make burst BASE_URL=<BASE_URL>    # same thing
```

The script uses Go if it is installed. Without Go it runs the same program inside the `golang:1.23-alpine` image, so Docker alone is enough (`BURST_DOCKER=1 ./burst.sh <BASE_URL>` forces this). For a `localhost` URL it uses host networking on Linux and `host.docker.internal` on macOS and Windows.

It creates fresh shows, then runs about 20,000 requests:

| Scenario | What must hold |
|---|---|
| Hot seat: 500 users race for one seat, 5 rounds | exactly 1 x 201 and 499 x 409 per round |
| Stampede: 17,000 requests from 4,000 users on 1,000 seats | no seat in two 201s, nobody over the limit, amounts correct, invariant holds |
| Idempotent retries: one key sent 20x at once and 5x in a row | one booking, all 25 responses byte-identical; same key with other seats is 409 |
| Per-user limit: one user, 20 parallel requests | exactly 4 x 201, 16 x 409 |
| Identity, cancel, rebook | another user's cancel is 404; body `user_id` ignored; double cancel is safe; freed seats rebook |
| Metrics reconcile | counter deltas equal what the burst saw; seat gauges equal `GET /shows` |

It prints the outcome distribution (confirmed / declined by reason / 5xx) and latency percentiles. It ends with a reconciliation block: seats in 201 responses minus seats released by cancel must equal the confirmed count in the database, the money must equal confirmed seats × price, the invariant must hold on every show, and the seat counters must match. It exits non-zero if any check fails or any 5xx is seen. Flags: `./burst.sh <URL> -concurrency 500 -stampede 17000 -users 4000`.

The server must run with `ENABLE_TOKEN_ENDPOINT=true` so the script can mint tokens.

Sample output (local, `<DATE>`):

```
<PASTE ./burst.sh OUTPUT>
```

## API

All bodies are JSON. Money is integer paise.

| Method and path | Auth | Purpose |
|---|---|---|
| `POST /shows` | admin | Create a show: `{"name", "seats": [...], "price_paise", "per_user_limit"?}`, up to 100,000 seats. Returns the show with every seat `available`. |
| `GET /shows/{id}` | none (token optional) | Per-seat status, counts, `invariant_ok`. With a token, your seats are marked `mine`. |
| `POST /shows/{id}/reserve` | user | `{"seats": [...], "idempotency_key": "..."}`. Key may also go in the `Idempotency-Key` header. |
| `POST /reservations/{id}/cancel` | owner | Releases the seats. Anyone else gets 404. |
| `POST /auth/token` | none | Test helper, only when `ENABLE_TOKEN_ENDPOINT=true`. |
| `GET /healthz` | none | Liveness. |
| `GET /readyz` | none | Readiness: DB reachable and migrations applied. 503 otherwise. |
| `GET /metrics` | none | Prometheus metrics. |

Reserve outcomes:

| Status | Code | Meaning |
|---|---|---|
| 201 | | Booked. Body: `reservation_id, show_id, user_id, seats, amount_paise, status`. |
| 201 + `Idempotent-Replay: true` | | Same key and seats as an earlier booking; the original response, byte for byte. |
| 409 | `seat_taken` | One or more seats are held or confirmed by someone else. |
| 409 | `per_user_limit_exceeded` | Would take the user over the show's limit. |
| 409 | `idempotency_key_reused` | Key already used for a different request. |
| 409 | `seat_contended` | Lock wait timed out; safe to retry. |
| 422 | `unknown_seats` / `validation_failed` | Seats not in the show, or a malformed request. |
| 400 / 401 / 404 | | Bad key or JSON, missing or bad token, unknown show. |
| 429 | `overloaded` | Last resort only (no DB connection within 10s). |

**Partial requests are all-or-nothing.** If you ask for A12 and A13 and only one is free, you get 409 and nothing is booked.

**Hold model: explicit cancel.** Seats confirm immediately; the owner can cancel. There are no timed holds (see WRITEUP).

Errors look like `{"error": {"code", "message", "request_id", "details"?}}`. A replayed response is the original body, so its `request_id` is in the `X-Request-Id` header instead.

## Observability

- **Logs:** JSON on stdout. Every line has `request_id`; send `X-Request-Id` to set it. One `http_request` line per request plus domain events: `reservation_confirmed`, `reservation_declined`, `idempotent_replay`, `reservation_cancelled`, `cancel_denied`, `spoof_attempt`.
- **Metrics:** `reservations_confirmed_total`, `reservations_declined_total{reason}` (`seat_taken`, `per_user_limit`, `idempotent_replay`, ...), `seats{show_id,status}` and `seats_invariant_ok{show_id}` read from the database at scrape time, HTTP latency by route, DB pool stats.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | required | Postgres connection string |
| `JWT_SECRET` | required | HS256 secret, 32+ characters |
| `PORT` | 8080 | HTTP port |
| `DB_MAX_CONNS` | 16 | Pool size |
| `DB_ACQUIRE_TIMEOUT` | 10s | Wait for a pool connection before 429 |
| `PER_USER_LIMIT` | 4 | Default limit for shows created without one |
| `ADMISSION_LIMIT` | 0 (off) | Optional cap on in-flight writes |
| `ENABLE_TOKEN_ENDPOINT` | false | Expose `POST /auth/token` (test helper) |
| `LOG_LEVEL` | info | debug, info, warn, error |
| `SHUTDOWN_TIMEOUT` | 20s | Drain time on SIGTERM |

## Tests

```bash
make up      # tests use the compose Postgres
make test    # go test -race ./... (database tests run in throwaway schemas)
```

Concurrency tests run against real Postgres: 200 goroutines on one seat, overlapping multi-seat requests, 50 duplicate keys at once, parallel requests against the per-user limit, cancel racing reserve.

## Layout

```
cmd/server         wiring, graceful shutdown
cmd/burst          the burst and correctness script
internal/reserve   the reservation and cancel transactions
internal/httpapi   routes, handlers, auth middleware, request ids, access logs
internal/store     pool, migrations, show queries, readiness
internal/metrics   Prometheus metrics
internal/auth      JWT verification
migrations         SQL, embedded in the binary
```
