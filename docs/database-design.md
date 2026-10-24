# Database Design (as implemented)

PostgreSQL 18 on the development machine; nothing used is newer than PostgreSQL 13 (`gen_random_uuid()` is built in from 13). Migrations are plain numbered SQL files in `result/migrations/`, applied in filename order by `store.Migrate` (one transaction per file, a `pg_advisory_lock` serializes runners, applied versions are in `schema_migrations`). There are no `down` migrations. This supersedes `database-design-proposal.md`.

| # | File | Creates |
|---|---|---|
| 0001 | `roles` | `gateway_app`, `gateway_ops` roles (`NOLOGIN`, no passwords in SQL) |
| 0002 | `api_clients` | `api_clients`: `role` caller/admin, `key_id` (public, unique), `key_hash` (sha-256, 32 bytes), `status` |
| 0003 | `provider_configs` | `provider_configs` (provider paypal/xendit/fake, environment, status, `allowed_currencies text[]`, `settings jsonb`, `active_credential_version`), `client_config_access` (client to config grants) |
| 0004 | `provider_credentials` | Encrypted credentials per `(config, version)`: `ciphertext`, `webhook_ciphertext`, `key_id` (master key id), `status` active/retired. Unique partial index: one active version per config. Deferred FK from `provider_configs.active_credential_version` |
| 0005 | `payment_transactions` | See below |
| 0006 | `payment_attempts` | One row per provider call with lease and fence token |
| 0007 | `payment_status_history` | Append-only status changes with actor |
| 0008 | `webhook_events` | Every webhook delivery; unique `(provider, event_id) WHERE verified` |
| 0009 | `review_items` | Things a human must check; one open item per `(transaction, kind)` |
| 0010 | `audit_log` | Append-only administrative actions, never holds secrets |
| 0011 | `triggers_grants` | Immutability triggers and role grants |

## `payment_transactions`

`id uuid`, `client_id` (FK, **not null**), `provider_config_id` (FK, restrict), `uniquecode`, `amount numeric(20,4) > 0`, `currency char(3)`, `status` (creating, pending, paid, failed, expired, cancelled, refunded), `provider_status`, `provider_ref`, `payment_url`, `expires_at`, `paid_at`, `failure_code`, `request_fingerprint` (sha-256 of config id, canonical amount, currency), `credential_version`, `current_fence`, timestamps.

Constraints that carry the design:

- `UNIQUE (client_id, uniquecode)`: `uniquecode` is unique **per caller** (decision D1).
- `UNIQUE (provider_config_id, provider_ref) WHERE provider_ref IS NOT NULL`: one provider payment, one transaction.
- `CHECK (paid_at IS NULL OR status IN ('paid','refunded'))`: `paid_at` can only exist on a paid payment.
- `CHECK` on `uniquecode` shape, `amount > 0`, status list, currency shape.
- `current_fence` is incremented each time an attempt starts; see `reliability-and-config-lifecycle.md`.

## Roles and transactions

- `gateway_app` (the running server, including the admin API): read `api_clients`; read/write configs, credentials, transactions, attempts, events, reviews; **insert-only** on `payment_status_history` and `audit_log`; no DDL.
- `gateway_ops` (CLI): additionally writes `api_clients`.
- History and audit tables are protected by triggers that reject `UPDATE`, `DELETE` and `TRUNCATE` for every role, superusers included. A superuser can still defeat them deliberately (drop the trigger, or `SET session_replication_role = replica`), which is one more reason the server must not run as a superuser in production.
- **Today the server and CLI log in with whatever `GATEWAY_DATABASE_URL` says.** The roles exist and their grants are tested, but giving them logins (and pointing production at them) is a deployment step still to be done.
- Provider HTTP calls never happen inside a database transaction. Writes that must be atomic are one transaction each: insert-or-get, begin attempt (row lock), complete attempt (fence-checked update + attempt + history), apply webhook event (dedup insert + row lock + checks + status + history), admin mutations (change + audit).

## Differences from the proposal

- Credentials are in their own versioned table instead of a single column on the config.
- `current_fence` on the transaction and a `payment_attempts` table were added.
- `client_id` is mandatory and part of the unique key (the proposal left this open).
- No `refunds` table yet (refunds are a later phase).
- No down migrations.
