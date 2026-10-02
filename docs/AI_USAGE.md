# How I used AI on this assignment

The brief says AI tools are allowed and expected, and asks for an honest account of what I directed and what I decided. This is that account, in the order things happened. Times are IST.

**Short version.** Claude (Anthropic's assistant, in the Claude desktop app) wrote most of the code, the tests, the burst script and the first drafts of the documents. I ran the project: I set the plan and the rules for how we worked, made the scope and trade-off calls, ran every test and every burst myself, and pushed back when something did not match the brief. Several of the fixes that matter most came from my machine failing a test or from me asking it to re-check its work against the mail.

## Tools

- **Claude**, with access to my Gmail (to read the brief), my Jira and Confluence, my Google Calendar, and my `~/develop` folder on my laptop. It could read and write files there but could not run commands on my machine, so every `make test`, `docker compose` and `./burst.sh` in this project was run by me.
- **Jira** (project KAN) for every piece of work, **Confluence** for my own notes while learning the code.
- **Railway** for hosting, **GoDaddy** for the `seats.tanmaythakur.co.in` subdomain.

<!-- Tanmay: if you used any other AI tool at any point (Copilot, Cursor, Gemini...), add it here. -->

## 1. Understanding the brief (Thu 1 Oct, evening)

The mail from Karan arrived on Thursday afternoon. I asked Claude to read it and break it into work I could track. I gave it a picture of the workflow I wanted and my Jira board, which already had a few dummy issues and my own workflow (To Do, In Progress, In Review, In Test, Done).

What it produced, between 21:16 and 21:22: 9 epics (foundation, data model, reservation core, release flow, resilience, observability, deployment, verification, documentation) and 26 stories under them, written from the brief.

What I decided:
- No assignees. I assign myself.
- No story points. The board is for tracking, not estimating.
- The board is updated **before** we move to the next piece of work, every time, so the board always shows the truth.
- How my time was split. Coding that night (the last commit was at midnight); on Friday the morning stayed free and the remaining work went into a 1 PM to 5 PM block, which Claude put on my calendar.

<!-- Tanmay: add one line on why Go + Postgres (for example, you are learning Go and a single Postgres is what the brief encourages). -->

## 2. Building it (Thu 21:56 to Fri 00:00)

We worked one story at a time, in this loop:

1. I said which story to start. Claude moved it to In Progress.
2. Claude wrote the code and tests in its own workspace, ran them against a local Postgres there, then wrote the files into my repo.
3. I ran `make up`, `make test`, curl calls and later `./burst.sh` on my laptop and pasted the output back.
4. When it was green I committed and pushed. Claude suggested the commit messages; the commits are mine. That is why the history is incremental: 15 commits that night, one or two per story.

The design choices in WRITEUP.md (row locks in label order plus a guarded update, idempotency keys claimed inside the booking transaction, a per-user advisory lock, all-or-nothing for multi-seat requests, explicit cancel instead of timed holds) were proposed by Claude with the alternatives it rejected. I accepted them after it explained each one; I did not design them from scratch.

## 3. What my runs and the tests caught

These were found by running things, not by reading code:

- **Hot-seat test failing on my laptop.** On my machine the 200-goroutine hot-seat test returned `overloaded` instead of 409 for some losers. The cause was that every decline was being committed, so 200 losers queued 200 disk flushes. The fix: only successful bookings are stored; a decline rolls back and costs nothing. (22:56 commit.)
- **Tests not running at all.** My first `make test` silently skipped the database tests because the test database URL was not set. Fixed in the Makefile.
- **A race in the idempotency pre-check**, found by the 50-duplicates test: a duplicate could see its own seats as taken and return 409 instead of the replay. Fixed by reading seats first and the key second.
- **The burst script colliding with its own earlier runs** when I ran it twice. Every run now tags its users and keys.
- **A shutdown race counted as a 500.** A request cut off by shutdown is now logged as 499 (client gone), not a server error.

## 4. Checking against the brief (Thu ~23:00)

While a test run was going I asked Claude to go back to Karan's mail and check we had not drifted. That check found real gaps: our routes and field names did not match the brief (`/reserve`, `seats`, `price_paise`, key in header or body, `POST .../cancel`, public show state), money was not yet in paise, and some declines could come back as 429 instead of 409. Three stories were added (KAN-39, 40, 41) and fixed, mostly in the 23:20 commit with a new migration. Without that re-check the service would have been correct but would not have matched the contract they test against.

Before stopping for the night I asked for the same check once more. It added KAN-42 (a 10,000-seat cap was smaller than "a hall of N seats" suggests) and KAN-43 (the burst script should work for a reviewer without Go installed).

## 5. Learning the code (Thu night and Fri)

I had not written this code myself, and the brief says they will ask me to extend it live. So I changed how we worked: Claude first explains every important line, file by file, and then I get asked questions. I keep the notes in Confluence (a page per stop of the code tour).

One example of why this matters. Asked why two users can never get the same seat, I first said the database CHECK constraint stops it. That was wrong: the race is decided by the row lock and the guarded `UPDATE ... WHERE status = 'available'`; the CHECK is only a backstop against a bug writing an impossible row. The tour continues before the interview.

## 6. Friday: improvements and deployment

- **Seat cap and Docker-only burst** (KAN-42, KAN-43). Claude implemented them. While doing it, it found that a 26 by 500 grid passed the API's check but broke the database's, which would have been a 500.
- **Dashboard** (KAN-44). I asked whether there was any view where I could watch things happening. Claude gave two options, a small page built into the app or Prometheus plus Grafana in compose. I picked the built-in page because it works on the live URL with nothing extra to deploy.
- **A last check before deploying** (KAN-45). I asked for one final verification. It confirmed GitHub held exactly the code we had tested, then flagged a risk: if the reviewers' burst queues longer than our database wait, hot-seat losers would get 429 instead of 409. The setting to raise the wait existed, but the server's write timeout was fixed, so raising it was unsafe. I approved a small fix: the write timeout now follows the wait, plus a test for the overload path, which had no test before.
- **Deploying.** I did the Railway setup myself, following steps Claude gave me, and sent screenshots of the logs back. The first deploy crashed because the database and secret were not set yet, which we expected; the second came up clean. Using my own subdomain was my idea; I added the GoDaddy records.
- **Live bursts.** Claude could not run these: its sandbox cannot reach Railway, and it cannot type into my terminal. I ran two full bursts against the live URL. Both passed with zero 5xx and zero 429, and the books balanced.
- **Reviewing the recording.** I recorded the second run and asked Claude to check it. It noticed something I had missed: Railway had dropped about 23,000 log lines because it keeps at most 500 lines per second. It suggested log sampling and a re-recording. I decided to explain it instead of changing the code on submission day: the metrics stayed exact, and screenshots from the recording show the request-id correlation. It is written up in WRITEUP.md section 5 as a known limit, with the fix I would make.

## 7. Documentation

Claude drafted README.md, WRITEUP.md and this file, using the Jira history, the git log and our conversation as the record. I read all three and edited them before submitting.

<!-- Tanmay: this line is only true once you have done it. Read all three, rewrite anything that is not how you would say it, then delete this comment. -->

## Directed vs decided, in one table

| | Claude | Me |
|---|---|---|
| Reading the brief, Jira epics and stories | wrote them | set the workflow and the rules (no assignees, no points, board first) |
| Design of the reservation path | proposed, with alternatives | accepted after explanations; now learning it line by line |
| Code, tests, burst script, dashboard | wrote them | chose order and scope; asked for the dashboard; picked its form |
| Testing | ran tests in its workspace | ran every test and burst on my laptop and live; found the slow-disk failure |
| Checking against the brief | did the checks | asked for them, three times |
| Deployment | gave steps, read logs | did the deploy, the domain, the live runs |
| Trade-offs | laid out options | decided: built-in dashboard, timeout fix, no log sampling today, no video |
| Commits | suggested messages | made every commit |

## What I would do differently

Ask for the check against the brief before writing any code, not after the first few stories. It was cheap, and it found the biggest gap of the whole project.
