# seat-reservation

Seat reservation API for a single show, built for the Paytm engineering take-home.
The goal is correctness under a burst: no seat is ever sold twice, the seat
counts always add up, and overload is answered with 4xx, never 5xx.

Go, Postgres 16, chi, pgx. Design notes and evidence go in [WRITEUP.md](WRITEUP.md).

## Status

Work in progress. The API, burst script and live URL are added in later commits.

## Run locally

Requires Go 1.22+ and Docker.

```bash
make up          # app + Postgres via docker compose
curl localhost:8080/healthz
```

Without Docker for the app (Postgres still needed):

```bash
cp .env.example .env
make run
```

## Layout

```
cmd/server        entry point: config, wiring, graceful shutdown
cmd/burst         one-command load and correctness check
internal/config   environment parsing, fails fast on bad config
internal/httpapi  router, middleware, handlers, JSON error envelope
internal/auth     JWT verification, identity from the token only
internal/store    Postgres pool and migrations
internal/reserve  the reservation transaction
internal/metrics  Prometheus collectors
migrations        SQL migrations, embedded in the binary
```

## Make targets

| Target | What it does |
|---|---|
| `make up` / `down` / `reset` | Start, stop, or wipe the compose stack |
| `make run` | Run the server on the host using `.env` |
| `make test` | All tests with `-race` |
| `make lint` | `go vet` and `gofmt` check |
| `make burst BASE_URL=...` | Burst and correctness run against any URL |
