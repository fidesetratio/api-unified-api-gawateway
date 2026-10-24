# Phase 3 Decisions (M0 sign-off)

Source: the approved Phase 3 plan and answers given in session.

## Status of the design base

The "Revision 3" documents named in the task are **not in the workspace**. Phase 3 is therefore built on the five Task 001 drafts in this folder. Everything marked [P] in the plan (attempt leases, fencing, review items, credential versions, operator CLI and audit, admin API, Fake Provider lifecycle) is a proposal that the user approved as the working design, and **must be reconciled with Revision 3 if those files are supplied later.** Missing files: `database-design.md`, `reliability-and-config-lifecycle.md`, `provider-integration.md`, `testing-guide.md`.

## Decisions

| # | Decision | Source |
|---|---|---|
| D1 | `uniquecode` is unique **per caller key**: `UNIQUE (client_id, uniquecode)`. A caller never sees another caller's codes. | User, M0 |
| D2 | Migrations are plain numbered `.sql` files applied by a small Go runner using pgx. No migration library. | User, M0 |
| D3 | Go dependencies may be installed inside `result/` (Gin, pgx/v5, google/uuid, a decimal library later). `gateway_test` may be created in the local PostgreSQL when M3 starts. | User, M0 |
| D4 | Create response is exactly `{uniquecode, status, payment_url}`, HTTP 201 (200 on identical replay). `GET /status/{uniquecode}` returns the same three fields. Statuses are uppercase: PENDING, PAID, FAILED, EXPIRED, CANCELLED. This supersedes the draft's `checkout_url` and lowercase statuses. | User |
| D5 | Provider configs and grants are managed through an admin HTTP API with separate admin keys. The CLI handles clients/bootstrap admin key, key re-encryption, review items and reconcile. | User |
| D6 | PayPal keys (client id/secret, webhook id) are stored encrypted and write-only. Gateway API keys (hash only) are a separate concept. | User |
| D7 | Webhook routes for PayPal/Xendit are not registered in Phase 3. | Plan |

## Defaults accepted with the plan (revisit if needed)

Credential versions in a separate table (Q3); operator identity by required flag/env, not authentication (Q5); concurrent duplicate create returns the current PENDING state immediately (Q6); internal `unknown` is shown as PENDING (Q7); Fake Provider events go through the service function, not an HTTP route (Q8); workers run in the gateway binary (Q11); rate limiting in-memory, later hardening (Q10).

## Implementation status (milestones M1-M12)

Implemented and tested: skeleton, domain, migrations and roles, credential encryption and versions, API-key auth, provider interface and Fake Provider, `POST /payments` and `GET /status`, attempts/leases/fencing, status/history/review/audit, settlement through verified events, expiry sweeper, reconciler, admin API, `gatewayctl`. See `testing-guide.md`.

Deviations from the approved plan, and things deliberately not done:

- No down migrations (up only).
- Rate limiting, admin IP allow-list / separate listener: not implemented (questions 10 and 14 remain open).
- Webhook HTTP routes are not registered; settlement is exercised through `Settlement.ProcessWebhook`.
- The server and CLI use whatever database login `GATEWAY_DATABASE_URL` provides; the `gateway_app`/`gateway_ops` roles exist and their grants are tested, but they have no login yet.
- `go test -race` could not be run (no C compiler for cgo on the development machine).
- The plan's `gatewayctl config/grant` commands were replaced by the admin API, as decided.
- `client list` was added to the CLI (needed to find client ids for grants).
- `GATEWAY_AUTO_MIGRATE=1` lets the server migrate itself outside production, for local development.

## Environment

PostgreSQL 18.6 at 127.0.0.1:5432 (superuser `postgres`, only database `postgres` at M0). The tool sandbox blocks loopback; Go tests that need the database must run with sandbox network access allowed. Test database to be created in M3: `gateway_test`.
