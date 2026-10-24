# Reliability and Config Lifecycle (as implemented)

Everything here is covered by tests listed in `testing-guide.md`. These mechanisms were designed in Phase 3 and replace the earlier proposals; the "Revision 3" documents named in the task were not in the workspace.

## Creating a payment

1. Validate input (strict JSON, uniquecode shape, decimal-string amount with the currency's decimals).
2. Authorize: the caller must hold a grant for the config id (otherwise `403 config_not_permitted`, which is also what an unknown id gives, so ids cannot be probed). A disabled config answers `404 config_not_found`. A currency outside `allowed_currencies` is `422`.
3. **Insert-or-get** the transaction in `creating` under `UNIQUE (client_id, uniquecode)`. If it already exists: a different fingerprint is `409`, an identical one is a replay.
4. If the transaction is still `creating`, **begin an attempt**: lock the transaction row, refuse if another attempt holds an unexpired lease, mark older in-flight attempts `unknown`, increment `current_fence`, insert the attempt with `fence_token = current_fence` and a lease (default 30 s).
5. Call the provider **outside any database transaction**, with a timeout shorter than the lease and an idempotency key equal to the transaction id (the same for every retry, so a retry cannot make a second provider payment where the provider honours the key).
6. Record the outcome with a **fence-checked** update (`WHERE current_fence = $attempt_fence AND status = 'creating'`), using a context that survives the caller disconnecting.

| Outcome | Effect |
|---|---|
| Success | `creating` to `pending`, `payment_url`/`provider_ref`/`expires_at` saved, history row |
| Definite rejection (or provider not implemented) | `creating` to `failed`, `provider_error`; an identical repeat reports FAILED without calling the provider again |
| Timeout or unclassified error | attempt marked `unknown`, transaction stays `creating`, caller gets `504`/`502`; a retry or the reconciler continues with the same idempotency key |
| Stale result (lease expired and a newer attempt started) | **not applied**; a `stale_result` review item records the provider reference, because a provider payment may exist that we did not record |

A concurrent identical request that finds a live lease returns the current state (PENDING, without `payment_url`) immediately instead of waiting.

## Reconciliation and expiry (worker, every 30 s; also `gatewayctl reconcile run`)

- **Reconciler** (`ResumeCreating`): picks `creating` transactions idle for two lease periods with no live lease, takes a new attempt and finishes the call with the same idempotency key. After 5 attempts it stops and opens `unknown_outcome_exhausted`.
- **Expiry sweeper**: `pending` transactions past `expires_at` become `expired` using `FOR UPDATE SKIP LOCKED`, so several sweepers can run at once.

## Settlement (verified evidence only)

`Settlement.ProcessWebhook(configID, headers, body)`:

1. The provider adapter verifies authenticity using the config's webhook secret. A failure is recorded as an unverified event and **changes nothing**.
2. In one transaction: insert the event (duplicate `(provider, event_id)` for a verified event is a no-op `duplicate`); find the transaction by `(config, provider_ref)` and lock it; **a `paid` event must carry an amount and currency equal to the stored values**, otherwise `amount_mismatch` and a review item, no status change; check the transition table; update status, `paid_at`, history, event outcome.
3. Rules: a verified `paid` after `expired`/`cancelled` is accepted and opens `paid_after_expiry`; `paid` can only go to `refunded`; `failed` and `refunded` are terminal; a repeated fact is `ignored`.

No HTTP route calls this yet (Phase 3 registers no webhook routes). Phase 4 adds `POST /api/v1.0/webhooks/{provider}` and decides how a delivery finds its config (by provider reference in the payload, or per-config URLs).

## Config and credential lifecycle

- Created through the admin API (admin keys only, TLS required unless `GATEWAY_ENV=dev`). Credentials are validated per provider (PayPal: `client_id`, `client_secret`, optional webhook `webhook_id`; Xendit: `api_key`, optional webhook `callback_token`), encrypted with AES-256-GCM using the active master key, with additional data binding config id, version, provider and slot.
- Secrets are **write-only**: no endpoint, log line, audit detail or error message returns them.
- **Rotation** (`PUT /admin/configs/{id}/credentials`) adds version N+1, retires N, flips the pointer, same `id`. Old versions stay in the table.
- **Master key rotation**: add the new key to `GATEWAY_MASTER_KEYS`, make it `GATEWAY_ACTIVE_KEY_ID`, run `gatewayctl key reencrypt`; the old key can be removed afterwards.
- Disabling a config makes its id answer `404`; grants can be added and revoked at any time; all changes write an audit row with the admin or operator identity.

## Operator identity

Every mutating `gatewayctl` command requires `--operator NAME` or `GATEWAYCTL_OPERATOR`. It is recorded in `audit_log` with the OS user and host. This is attribution, not authentication: the CLI already runs with database credentials.

## Not done yet

Rate limiting; admin IP allow-list or separate listener; real PayPal/Xendit adapters; webhook HTTP routes; refunds; least-privilege database logins as the default; `-race` verification.
