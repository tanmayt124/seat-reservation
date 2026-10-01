# WRITEUP

<!-- DRAFT. Rewrite in your own words before submitting. Fill every <...>. -->

## 1. The atomic decision

The decision of who gets a seat lives in one Postgres transaction. Nothing outside it can change a seat.

**Mechanism.** For `POST /shows/{id}/reserve`:

1. Claim the idempotency key: `INSERT ... ON CONFLICT DO NOTHING`.
2. Take a per-user advisory lock for this show (`pg_advisory_xact_lock(hashtextextended(show:user))`) and count the seats the user already holds.
3. Lock the requested seat rows with `SELECT ... FOR UPDATE ORDER BY label`.
4. Check every locked seat is `available`.
5. Insert the reservation, then `UPDATE seats SET status='confirmed', ... WHERE show_id=$1 AND label = ANY($2) AND status='available'`, and require the rows affected to equal the number of seats asked for. Anything else rolls back.
6. Store the response on the key row and commit.

**Why it is race-free.** Two requests for A12 cannot both pass step 4: the second blocks on A12's row lock until the first commits, then reads the committed row (READ COMMITTED re-reads a locked row after the wait), sees `confirmed`, and declines with 409. The guarded `UPDATE ... AND status='available'` is a second, independent guard: even if the read were somehow wrong, the update would touch fewer rows than requested and the transaction would roll back. A third guard is in the schema: a `CHECK` makes "confirmed with no owner" and "available with an owner" impossible, and `(show_id, label)` is the primary key, so a seat exists once.

There is no "is it free? then take it" step anywhere. The lock-free pre-check described below only ever declines early; it never books.

**Multi-seat without deadlocks.** Every transaction that locks seats locks them in label order, both reserve and cancel. Two requests for {A12, A13} and {A13, A12} both lock A12 first, so one waits behind the other instead of each holding what the other needs. The advisory lock is taken before any seat lock, and it is per (show, user), so there is no cycle between it and the seat locks. The lock wait is capped at 3 seconds (`lock_timeout`); a timeout is a 409 `seat_contended`, never a 500. Deadlock and serialization errors, if they ever happen, are retried once.

**Partial requests are all-or-nothing.** If one of the requested seats is taken, the whole request is declined and nothing is booked. That holds under concurrency because all the seats are locked before any is written.

**Why not something else.**
- *SERIALIZABLE isolation*: correct, but under a hot-seat storm almost every transaction aborts with a serialization error and has to retry. Explicit row locks make losers wait a few milliseconds and then decline cleanly.
- *Optimistic version column*: same storm, same retries, more code.
- *Redis or an in-memory lock*: a second source of truth that can disagree with the database. One Postgres is enough at this scale and the brief encourages it.

**Making losers cheap.** Before the transaction, one lock-free round trip reads the requested seats' status and the idempotency key. If a seat is already taken, the request is declined with 409 without opening a transaction. Once a hot seat is sold, almost all of the storm stops here.

## 2. Idempotency

**Where the key lives.** Table `idempotency_keys`, primary key `(user_id, key)`. Keys are per user, so two users can send the same key safely. Each row stores `request_hash` (SHA-256 of show id plus the sorted seat list, so the same seats in a different order count as the same request), `response_status` and `response_body`.

**How exactly-once is enforced.** The key is claimed with `INSERT ... ON CONFLICT DO NOTHING` *inside the same transaction that books the seats*, and the response is written to that row before commit. Either the booking and the stored response commit together, or neither exists. When 50 copies of one request arrive at once, one wins the insert; the other 49 block on the unique index until it commits, find the row, roll back their own transaction and return the stored response. Tested with exactly that: 50 concurrent duplicates, one reservation, 50 byte-identical bodies.

**Replays are byte-identical.** The first response is read back from the database after it is stored, so the original answer and every replay are the same bytes. Replays carry `Idempotent-Replay: true`.

**Same key, different body.** If the stored `request_hash` differs, the answer is 409 `idempotency_key_reused`.

**Only successful bookings are stored.** A decline (seat taken, over the limit) rolls back and leaves no key row. Nothing changed, so a retry with the same key is evaluated again and gets the truthful current answer. This was a deliberate change after a test run: originally every in-transaction decline was stored and committed, which meant a 500-user hot-seat storm queued about 500 WAL flushes on a slow disk. Now it costs one (the winner's).

**A race the tests found.** A duplicate request could commit between another copy's key lookup and its seat pre-check, so the second copy saw its own seats as taken and returned 409 instead of the replay. The fix was ordering: the pre-check reads seats first and the key second, in one batch, so a committed duplicate is always visible to the key read.

## 3. Holds and expiry

I chose **explicit cancel by the owner** (`POST /reservations/{id}/cancel`) over timed holds. Seats confirm immediately; there is no payment step in this exercise, so a hold would only add a state that can expire without adding behaviour the brief tests.

- Only the owner can cancel. Anyone else gets the same 404 as a reservation that does not exist, so ids cannot be probed, and the attempt is logged as `cancel_denied`.
- Seats are released **by reservation id**: `UPDATE seats ... WHERE reservation_id = $id`. A late or repeated cancel can never free a seat that has since been booked by someone else. Tested: Alice cancels, Bob books the same seat, Alice's cancel is retried, the seat stays Bob's.
- Cancelling twice returns the same body.

**If holds were needed** (for example a payment window): seats would go `available -> held` with an `expires_at`, and `held -> confirmed` on payment. Expiry can be lazy, by letting the guarded update also accept `status = 'held' AND expires_at < now()`, so no sweeper is needed for correctness; a periodic sweeper would only keep the counts tidy. The invariant already counts `held`, and the schema already allows it.

## 4. Consistency vs availability under a partition

This service chooses **consistency**. There is one Postgres primary and every booking decision is made there. If the app cannot reach the database, it does not guess: `/readyz` goes to 503 so the platform stops sending traffic, and reserve requests fail rather than risk selling a seat twice. Selling the same seat to two people is far worse than asking a buyer to retry.

What I would keep available during a partition: read-only show state could be served from a replica or a short cache, clearly marked as possibly stale. Booking must never be served from a replica.

## 5. Observability: what would page me at 2am

Page immediately:
- `seats_invariant_ok == 0` for any show. Should be impossible; means data corruption or a bug.
- Any `constraint_blocked_bad_write` log line. The database blocked a write the application thought was fine; the guard worked, but the code has a bug.
- 5xx rate above zero on reserve or cancel.
- `/readyz` failing for more than a minute, or `seats_scrape_error == 1`.

Page if sustained:
- `reservations_declined_total{reason="overloaded"}` or `{reason="seat_contended"}` rising: the database is the bottleneck.
- Reserve p99 latency high together with `db_pool_empty_acquires_total` climbing: requests are queueing for connections.

Never page: a spike in `seat_taken`. That is an on-sale working as intended.

Every log line carries a `request_id` (from `X-Request-Id` or generated), and the same id is in the response header and every error body, so one booking can be traced from the client to the database outcome.

## 6. AI usage

<!-- Write this yourself. Be specific and honest: they will ask you to extend the service live. Some prompts: -->

**Directed (what I asked the AI to do):**
- <e.g. turn Karan's mail into Jira epics and stories, then implement them one story at a time>
- <e.g. write the code and tests for each story; I ran every test and burst on my own machine and pasted the output back>

**Decided (what I decided or changed):**
- <your decisions: scope, order, when to stop, which trade-offs you accepted and why>
- <e.g. re-checking the build against the mail found the API did not match the brief's routes and fields; we aligned them>

**What the tests caught, and how it was fixed:** the duplicate-key race in the pre-check; hot-seat losers timing out on a slow disk because declines were being committed; a shutdown race logged as 500; the burst script colliding with its own earlier runs.

**What I verified by hand:** <list>

## 7. What I would do next

- Expire idempotency keys after 24h (a cleanup job), and add a per-user rate limit.
- Timed holds with a payment step, as described in section 3.
- For much bigger on-sales: keep a per-show in-memory set of sold seats in front of the database so losers are declined without a query, partition seats by show, and put a queue in front of the hottest shows.
- Run more than one app instance (the app is stateless; all state is in Postgres).
- Drop the `show_id` label from the seat gauges, or keep only the top shows, once there are thousands of shows.

## Evidence

- Local burst (laptop, Docker Postgres, `<DATE>`): about 19,500 requests, `confirmed 785 | seat_taken 18,721, per_user_limit_exceeded 18, idempotent_replay 24 | 5xx 0`; metrics reconciled exactly.
- Live burst against `<LIVE_URL>`: `<PASTE>`.
- Logs and metrics during the live run: `<LINK>`.
