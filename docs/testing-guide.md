# Testing Guide

## Running

From `result/` (Windows):

```
set GATEWAY_TEST_DATABASE_URL=postgres://postgres:PASSWORD@127.0.0.1:5432/gateway_test?sslmode=disable
build.bat test
```

Write `@` inside the password as `%40`. Without `GATEWAY_TEST_DATABASE_URL`, database tests are **skipped** (they print "skipping database test"); everything else still runs. `build.bat` sets `GOTMPDIR` to `result\.gotmp` because Windows Application Control blocked test executables in `%TEMP%` on the development machine.

Every database test creates its own throwaway schema (`t_<random>`), applies all migrations in it, and drops it afterwards, so tests are isolated, can run in any order, and never touch `public`. Use a dedicated database (`gateway_test`), not one with real data.

Not available on the development machine: `go test -race` (needs cgo and a C compiler). Concurrency tests use many goroutines and assert on database state instead.

## What is covered (all passing at the time of writing)

| Package | What the tests prove |
|---|---|
| `domain` | Money parsing (IDR vs USD vs KWD decimals, zero, negative, exponent, spaces, huge, currency shape); every ordered pair of statuses against the transition table; failed/refunded stay terminal; API names (creating shows as PENDING) |
| `secrets` | `Secret` never prints in `%s/%v/%+v/%#v/%q/%x`, JSON, or slog; AES-GCM round trip, fresh nonce, tampering, wrong AAD/version/slot/key, truncation; key rotation keeps old data readable; key-ring parsing errors never echo key material |
| `config` | All problems reported at once; bad environment; master keys required; Fake Provider not allowed in production |
| `auth` | Key shape and hash; wrong/empty/garbage/unknown/disabled keys all give one error; a database outage is not reported as a bad key; Bearer parsing |
| `provider`, `provider/fake` | Registry; stubs; Fake behaviours (success, timeout, reject, slow, context cancel); same idempotency key = one payment (also under 50 goroutines); webhook token verification, empty secret never authenticates |
| `store` (DB) | Migrations apply once and rerun as no-ops; audit/history are append-only (update, delete, truncate blocked); constraints (duplicate `uniquecode` per caller, other caller may reuse it, negative amount, bad status, bad uniquecode); `gateway_app` role cannot rewrite history, run DDL or update clients |
| `service` (DB) | Create, replay, conflict (amount, config, currency); authorization and validation matrix (no provider call, no row left behind); per-caller uniqueness and scoped status; timeout then retry creates one provider payment; definite rejection fails the payment; unimplemented provider gives `provider_error`; **30 concurrent identical creates: one transaction, one attempt, one provider call**; lease takeover after a "crashed" worker; **stale result fenced and a review item opened**; credentials never stored in plaintext; verified webhook marks paid with history; unverified webhooks change nothing and never block a real event; amount/currency mismatch never marks paid; duplicate/unknown/invalid-transition events; expired-then-paid is accepted and flagged; **25 concurrent deliveries apply exactly once**; reconciler finishes stuck payments and gives up after 5 attempts with a review item |
| `api` (DB) | Exact contract `{uniquecode,status,payment_url}` with 201 then 200 on replay; 401 matrix and uniform 401 body; admin and caller keys cannot use each other's routes; disabled client loses access; strict JSON (unknown field, number as amount, string id, trailing data, 413 body limit); admin validation never echoes secrets; admin needs TLS; config lifecycle, rotation, disable, grants, audit rows; **secrets never appear in responses, logs, audit details or the database** |
| `cli` (DB) | Operator identity required for every mutation; keys printed once on stdout only and stored hashed; env operator; rotate/disable/enable; audit rows with operator id and no keys; review list/resolve; key re-encryption under a new master key; reconcile; usage exit codes |
| `worker` | A failing or panicking job never stops the loop; stops on cancel |

## Manual end-to-end check

```
copy .env.example .env          (then fill DB_HOST, DB_PORT, DB_USER, DB_PASSWORD, DB_NAME)
build.bat build
bin\migrate.exe                (creates the database if missing and applies all migrations)
set GATEWAYCTL_OPERATOR=yourname
bin\gatewayctl.exe client create --name admin1 --role admin      (prints the admin key once)
bin\gatewayctl.exe client create --name shop1  --role caller     (prints the caller key once)
bin\gatewayctl.exe client list                                   (note shop1's id)
run.bat
```

Then, with `ADMIN` and `CALLER` set to the printed keys:

1. `POST /api/v1.0/admin/configs` with `{"provider":"fake","environment":"sandbox","name":"dev fake","allowed_currencies":["USD"],"credentials":{"client_secret":"x"}}` → note `id`.
2. `PUT /api/v1.0/admin/configs/{id}/grants/{shop1 id}` → 204.
3. `POST /api/v1.0/payments` with `{"id":{id},"uniquecode":"ORDER-1","amount":"10.00","currency":"USD"}` → 201 `{"uniquecode","status":"PENDING","payment_url"}`; repeat → 200 with the same body.
4. `GET /api/v1.0/status/ORDER-1`.

This sequence was run against the real binaries and gave the expected results.

## Phase 4 test status (PayPal, Xendit, Stripe, Midtrans)

- `internal/provider/paypal` (14 tests), `internal/provider/xendit` (13) and `internal/provider/stripe` (11) and `internal/provider/midtrans` (10) run against in-process stand-ins (`paypaltest`, `xendittest`, `stripetest`, `midtranstest`).
- `internal/service/paypal_flow_test.go` (12), `xendit_flow_test.go` (4) and `stripe_flow_test.go` (7) and `midtrans_flow_test.go` (6) run the complete flows through a real database: create, approve/capture or pay, webhook verification, forged and tampered deliveries, amount and currency mismatch, outages, duplicates, expiry.
- `internal/api/providers_test.go`: provider-specific admin rules and the `POST /api/v1.0/webhooks/{provider}` route (Xendit end to end).
- All of these passed in the last run. Windows Application Control still blocks some test binaries at random (most recently `secrets` and `store`); retry the same command, and if a package never runs, run it on another machine.
- The Stripe signature check has a known-answer test whose expected HMAC was computed independently with Node's crypto.
- The Midtrans signature has a known-answer test (SHA-512 computed independently with Node).
- Not done: sandbox smoke tests with real PayPal, Xendit, Stripe and Midtrans credentials (never committed).

## Known gaps

- No `-race` run (see above).
- No test against a real PayPal or Xendit adapter (Phase 4).
- The gateway process was exercised with a superuser database login; the least-privilege roles `gateway_app` / `gateway_ops` are tested for their grants but are not yet the login used by default (see `database-design.md`).
- No load or soak test.
