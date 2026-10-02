# Jira board export

I tracked every piece of this work on a Jira board (project KAN). The board is private, so this is a snapshot of it, exported from the Jira API on 2 Oct 2026 at about 14:30 IST. All times are IST.

Workflow: To Do → In Progress → In Review → In Test → Done. A story moved to In Progress before work on it started, and to Done only after its tests passed on my laptop (and, for the deployment stories, against the live URL). The "Delivered in" column points to the commits that did the work. Summaries are as written on the board; some routes in them changed later, when the API was aligned with the brief (KAN-39).

## Epics

| Epic | Summary | Created | Status |
|---|---|---|---|
| KAN-4 | Foundation: project scaffold, local environment and auth | 01 Oct 21:16 | Done |
| KAN-5 | Data model and show management | 01 Oct 21:17 | Done |
| KAN-6 | Seat reservation core: atomic, idempotent, limit-safe | 01 Oct 21:17 | Done |
| KAN-7 | Release flow: owner-only cancel and rebooking | 01 Oct 21:17 | Done |
| KAN-8 | Resilience under load: zero 5xx | 01 Oct 21:17 | Done |
| KAN-9 | Observability: health, metrics and structured logs | 01 Oct 21:17 | Done |
| KAN-10 | Deployment and operations | 01 Oct 21:17 | Done |
| KAN-11 | Verification: burst script and correctness tests | 01 Oct 21:17 | Done |
| KAN-12 | Documentation and submission | 01 Oct 21:17 | In Progress (the submission mail) |

## Stories planned up front (created 01 Oct, 21:19 to 21:22)

| Story | Epic | Summary | Done | Delivered in |
|---|---|---|---|---|
| KAN-13 | KAN-4 | Repo scaffold: Go module, chi router, project layout, Makefile | 01 Oct 22:14 | cce0b66 |
| KAN-14 | KAN-4 | Local environment: docker-compose (app + Postgres 16), env config, migrations on boot | 01 Oct 22:15 | 01a3c2e, 9e298a9 |
| KAN-15 | KAN-4 | JWT auth middleware: identity only from token, user/admin roles, test token endpoint | 01 Oct 22:36 | d295156, 3364b55 |
| KAN-16 | KAN-5 | Schema migrations: shows, seats, reservations, idempotency_keys with constraints | 01 Oct 22:15 | 9e298a9 |
| KAN-17 | KAN-5 | POST /shows (admin): create show with validated seat map in one transaction | 01 Oct 22:37 | 3364b55 |
| KAN-18 | KAN-5 | GET /shows/{id}: per-seat status, counts and live invariant check | 01 Oct 22:37 | 3364b55 |
| KAN-19 | KAN-6 | POST /shows/{id}/reservations: atomic all-or-nothing reserve, no double-sell | 01 Oct 22:37 | 579bef6, 894e098 |
| KAN-20 | KAN-6 | Idempotency-Key: replay stored response, 409 on key reuse with different body | 01 Oct 22:37 | 579bef6, 894e098, caafa25 |
| KAN-21 | KAN-6 | Per-user seat limit (default 4) enforced under concurrency via advisory lock | 01 Oct 22:58 | 579bef6 |
| KAN-22 | KAN-6 | Map Postgres errors to domain 4xx responses (never leak a 500 for contention) | 01 Oct 22:58 | 579bef6 |
| KAN-23 | KAN-7 | DELETE /reservations/{id}: owner-only cancel, seats become rebookable | 01 Oct 22:58 | d2d81be |
| KAN-24 | KAN-8 | Bounded DB pool and timeouts at every layer | 02 Oct 00:02 | 9e298a9, 579bef6, 7e19f22 |
| KAN-25 | KAN-8 | Admission limiter: shed excess load with 429 + Retry-After | 02 Oct 00:02 | 7e19f22 (kept off by default, see KAN-41) |
| KAN-26 | KAN-8 | Panic recovery and graceful shutdown (SIGTERM drain) | 02 Oct 00:03 | cce0b66, a5b4408, 7e19f22 |
| KAN-27 | KAN-9 | Liveness /healthz and readiness /readyz endpoints | 01 Oct 23:29 | a5b4408 |
| KAN-28 | KAN-9 | Prometheus /metrics: reservations, declines by reason, seat gauge, HTTP, pool | 01 Oct 23:47 | fe0a996 |
| KAN-29 | KAN-9 | Structured JSON logs (slog) with request_id propagation | 01 Oct 23:29 | a5b4408 |
| KAN-30 | KAN-10 | Multi-stage, non-root Dockerfile with compose parity | 02 Oct 14:04 * | 01a3c2e |
| KAN-31 | KAN-10 | Deploy to public URL on Railway (fallback: Render + Neon) | 02 Oct 14:04 * | 7e19f22 (railway.json); deployed 02 Oct 13:22 |
| KAN-32 | KAN-10 | Make logs and metrics viewable by reviewers (public dashboard or recording) | 02 Oct 14:04 * | f2f36ab, 47d7ad4 |
| KAN-33 | KAN-11 | One-command burst script: make burst BASE_URL=... (all correctness scenarios) | 02 Oct 14:03 * | 95aee4b, f1383e0 |
| KAN-34 | KAN-11 | Go integration and race tests against real Postgres | 02 Oct 14:05 * | 579bef6 onward (tests ship with each story) |
| KAN-35 | KAN-11 | Live verification run against public URL: all six correctness bars | 02 Oct 14:04 * | 47d7ad4 |
| KAN-36 | KAN-12 | README: run locally, API reference, config, run the burst | 02 Oct 14:18 | 7e19f22, 575d50a |
| KAN-37 | KAN-12 | WRITEUP.md: design decisions, trade-offs, evidence, AI usage, what's next | 02 Oct 14:18 | 7e19f22, 575d50a |
| KAN-38 | KAN-12 | Clean-clone check and submission email to Paytm | open at export | clean clone of b3c4bfb passed at 14:23 |

\* Verified between 13:20 and 13:50 (live deploy and bursts) and closed together in a board cleanup at 14:04.

## Stories added during the work

Each of these came from a check against the brief or from a run on my laptop, after the plan above was made.

| Story | Epic | Summary | Created | Done | Delivered in |
|---|---|---|---|---|---|
| KAN-39 | KAN-6 | Align API with the Paytm contract (routes, request and response fields) | 01 Oct 23:05 | 01 Oct 23:22 | fdc297e |
| KAN-40 | KAN-5 | Money in integer paise: price_paise on shows, amount_paise on reservations | 01 Oct 23:06 | 01 Oct 23:22 | fdc297e |
| KAN-41 | KAN-8 | Hot-seat losers always get 409: per-show limit, 409 declines, no 429 for losers | 01 Oct 23:06 | 01 Oct 23:22 | fdc297e, 7e19f22 |
| KAN-42 | KAN-5 | Raise the per-show seat cap above 10,000 (brief says "a hall of N seats") | 02 Oct 00:45 | 02 Oct 12:47 | f1383e0 |
| KAN-43 | KAN-11 | burst.sh works without Go installed (Docker fallback) and prints a final reconciliation block | 02 Oct 00:45 | 02 Oct 12:47 | f1383e0 |
| KAN-44 | KAN-9 | Live dashboard page at /dashboard | 02 Oct 12:47 | 02 Oct 13:00 | f2f36ab |
| KAN-45 | KAN-8 | Make the HTTP write timeout follow DB_ACQUIRE_TIMEOUT | 02 Oct 13:05 | 02 Oct 13:13 | d07d5c9 |
| KAN-46 | KAN-12 | AI usage document: how AI was used from requirements to submission | 02 Oct 13:57 | 02 Oct 14:18 | 575d50a, b3c4bfb |

How these came about is described in [AI_USAGE.md](../AI_USAGE.md).
