# Database Design (Proposal)

**Status: proposal. No migrations written or run.** PostgreSQL. Derived from `unified-api-design.md`. Reference input: Ref2 `order_transactions` (`Sales/src/Database/Migrations/2021_03_11_212124_create_order_transactions_table.php`) and Ref1 `orders` show what *not* to do (raw provider strings as status, no unique provider ref). Ref3 shows an indexed provider transaction id.

## 1. Principles

- Provider configuration is separate from transactions (CLAUDE.md §4.4). A transaction references a config by FK and never copies credentials.
- Money is `NUMERIC`, never float. Currency stored with every amount.
- Normalized `status` plus a separate `provider_status` (raw text) for diagnostics.
- Every status change is recorded in a history table.
- Webhook events are stored once, keyed by provider event id.
- Secrets are stored encrypted. They are never selected into API DTOs.

## 2. Tables

### 2.1 `api_clients` (pending Decision 1)
| Column | Type | Notes |
|---|---|---|
| id | bigserial PK | |
| name | text not null | |
| api_key_hash | bytea not null unique | Hash of the key (e.g. SHA-256 of a random 256-bit key); the plain key is shown once |
| status | text check in (`active`,`disabled`) | |
| created_at, updated_at | timestamptz | |

### 2.2 `provider_configs`
| Column | Type | Notes |
|---|---|---|
| id | bigserial PK | The `id` in `POST /payments` |
| name | text not null | Human label |
| provider | text not null check in (`paypal`,`xendit`) | Adapter selector |
| environment | text not null check in (`sandbox`,`production`) | |
| status | text not null check in (`active`,`disabled`) | |
| credentials_enc | bytea not null | AES-GCM ciphertext of a JSON blob (client id/secret, API key, ...) |
| webhook_secret_enc | bytea null | Callback token / PayPal webhook id / public key set, depending on provider |
| enc_key_version | smallint not null | For key rotation |
| allowed_currencies | text[] not null | |
| settings | jsonb not null default '{}' | Non-secret: return URLs, default expiry, method list. Validated per provider |
| created_at, updated_at | timestamptz | |

Constraint idea: `UNIQUE (provider, environment, name)`. Non-secret identifiers needed for webhook routing (e.g. PayPal merchant id, Xendit business id) as separate indexed columns **if** the chosen resolution strategy needs them (design issue 5). No credentials in `settings`.

### 2.3 `client_config_access` (pending Decision 1)
`client_id` FK → `api_clients`, `provider_config_id` FK → `provider_configs`, PK on both. Defines which client may use which config.

### 2.4 `payment_transactions`
| Column | Type | Notes |
|---|---|---|
| id | uuid PK | Gateway reference; sent to the provider as our reference |
| client_id | bigint FK null | Scope of `uniquecode` (Decision 1) |
| provider_config_id | bigint FK not null → provider_configs | `ON DELETE RESTRICT` |
| uniquecode | text not null | Caller's reference |
| amount | numeric(20,4) not null check (amount > 0) | Gross amount |
| currency | char(3) not null | |
| status | text not null | `creating`, `pending`, `paid`, `failed`, `expired`, `cancelled`, `partially_refunded`, `refunded` (`creating` is internal, shown as `pending`) |
| provider_status | text null | Raw provider status, diagnostic |
| provider_ref | text null | Provider's payment/invoice/order id |
| checkout_url | text null | |
| expires_at | timestamptz null | |
| paid_at | timestamptz null | Set only on verified evidence |
| failure_code, failure_message | text null | Normalized code; message scrubbed |
| request_fingerprint | bytea not null | Hash of (config id, amount, currency) for replay vs conflict |
| created_at, updated_at | timestamptz | |
| version | int not null default 0 | Optimistic locking if used |

Constraints and indexes:
- `UNIQUE (client_id, uniquecode)`. If there is no client concept, `UNIQUE (uniquecode)`.
- `UNIQUE (provider_config_id, provider_ref) WHERE provider_ref IS NOT NULL`. Prevents one provider payment mapping to two transactions.
- Index `(status, expires_at)` for the expiry sweeper; index `(status, updated_at)` for the reconciler of `creating` rows.
- `CHECK (paid_at IS NULL OR status IN ('paid','partially_refunded','refunded'))`.

### 2.5 `payment_status_history`
`id bigserial`, `transaction_id uuid FK`, `from_status`, `to_status`, `reason text` (e.g. `webhook`, `reconcile`, `expiry`), `webhook_event_id bigint null FK`, `created_at`. Append-only.

### 2.6 `webhook_events`
| Column | Type | Notes |
|---|---|---|
| id | bigserial PK | |
| provider | text not null | |
| event_id | text not null | Provider's event/notification id. If a provider has none, a hash of the raw body |
| provider_config_id | bigint null FK | Resolved config |
| transaction_id | uuid null FK | |
| verified | boolean not null | Set only after successful verification |
| outcome | text null | `processed`, `ignored`, `duplicate`, `amount_mismatch`, `unknown_reference`, `invalid_transition`, `error` (mirrors Ref2's `IpnStatus`) |
| payload | jsonb null | Scrubbed, size limited; retention policy TBD |
| received_at, processed_at | timestamptz | |

`UNIQUE (provider, event_id)` is the duplicate guard. For unverified deliveries, store with `verified=false` and a nullable `event_id` (partial unique index `WHERE verified`) so an attacker cannot poison the dedup key.

### 2.7 `refunds` (later phase)
`id uuid`, `transaction_id`, `amount numeric(20,4)`, `currency`, `status`, `provider_refund_ref` (unique per config), `reason`, `created_at`. Constraint: sum of non-failed refunds ≤ transaction amount, enforced in the service with a row lock.

### 2.8 `idempotency_keys`
Not needed while `uniquecode` is the key. Revisit if an `Idempotency-Key` header is adopted.

## 3. Concurrency and integrity rules

- Settlement path: `BEGIN; SELECT … FROM payment_transactions WHERE id=$1 FOR UPDATE; validate transition; UPDATE; INSERT history; INSERT/UPDATE webhook_events; COMMIT`. This replaces Ref2's cache lock with a database-native one.
- Provider HTTP calls happen **outside** DB transactions.
- A migration tool is needed, but it is not chosen here and **no dependency will be installed in this phase**. **[Decision]**

## 4. Reference-to-design mapping

| Our element | Reference inspiration | Reference weakness we avoid |
|---|---|---|
| `provider_ref` unique index | Ref3 `sales_order.xendit_transaction_id` index (`etc/db_schema.xml`) | Ref2 `transaction_id` not unique |
| `payment_status_history` | none | Ref1 overwrites status, with no history |
| `webhook_events` | Ref2 `IpnStatus` outcomes | Ref1 and Ref3 have no event log |
| Encrypted credentials | Ref3 `Encrypted` backend model (`etc/adminhtml/system.xml`) | Ref1 env-only, one global key |
| Raw + normalized status | Ref2 `PaypalPaymentStatus` enum | Ref1 stores raw only |
| `jsonb` raw data | Ref2 `order_transactions.data` | Ref3 logs payloads to files, with PII |

## 5. Open questions

- Retention for `webhook_events.payload` and PII.
- Multi-tenancy scope (`client_id`) and whether configs are shared across clients.
- Table partitioning for events (probably unnecessary initially).
- Whether `provider_configs` need soft-delete/versioning to keep audit trails when credentials rotate.
