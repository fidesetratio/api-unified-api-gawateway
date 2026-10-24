# Phase 4a Decisions (PayPal)

## Status (written when work stopped)

Written: PayPal adapter (`internal/provider/paypal`), a PayPal stand-in for tests (`paypaltest`), provider interface extensions (capture, locate, amount validation), automatic capture after a verified approval, `POST /api/v1.0/webhooks/{provider}`, PayPal currency and URL validation in the admin API, and a 5-hour cap on resuming `creating` payments.

**Verified by running tests:** the 14 adapter tests (create body and headers, idempotency key, token cache and refresh, errors, zero-decimal currencies, capture COMPLETED/DECLINED/PENDING, order status, webhook verification including the raw-body rule, tampering, outages, event mapping). The raw-body test found and fixed a real bug (`json.Marshal` compacts a `RawMessage`).

**Written but NOT run** because Windows Application Control (policy `{0283ac0f-fff1-49ae-ada1-8a933130cad6}`) started blocking the database test binaries (`service.test.exe`, `api.test.exe`, `cli.test.exe`, `store.test.exe`), which had run before: the end-to-end flow tests in `internal/service/paypal_flow_test.go`, and re-runs of the older service/api/cli/store tests after the Phase 4a changes. `go build` and `go vet` pass for everything. Nothing has been run against PayPal's real sandbox.

Based on `paypal-research.md`.

| # | Decision | Source |
|---|---|---|
| P1 | PayPal configs accept only currencies PayPal supports for payments; IDR is rejected for PayPal and goes to Xendit later. No currency conversion. | User |
| P2 | The gateway captures automatically: a verified `CHECKOUT.ORDER.APPROVED` makes it call capture. `PAID` only after a capture with status `COMPLETED` and matching amount and currency (from the capture response or a verified `PAYMENT.CAPTURE.COMPLETED`). `PENDING` and `DECLINED` never mark paid. | User |
| P3 | `return_url` and `cancel_url` come from the PayPal config `settings`, never from the payment request. | User |
| P4 | Webhook verification uses PayPal's postback endpoint with the raw body passed through unchanged. The webhook id is the config's webhook secret (`webhook_id`). | Plan, from research |
| P5 | A webhook finds its config through the PayPal order id in the payload (`resource.id` for approval events, `supplementary_data.related_ids.order_id` for capture events). That lookup only chooses which config's `webhook_id` to verify with; nothing changes before verification succeeds. | Plan |
| P6 | Gateway-side expiry for PayPal payments is a config setting (`expiry_minutes`, default 1440), because PayPal's order validity is not documented. | Research gap |
| P7 | The reconciler stops resuming a `creating` payment after about 5 hours (PayPal may keep idempotency keys for only 6 hours) and opens a review item. | Research |

# Phase 4b Decisions (Xendit)

Based on `xendit-research.md`.

| # | Decision | Source |
|---|---|---|
| X1 | The adapter uses **Xendit Payment Sessions** (`POST /sessions`, hosted checkout), not the legacy Invoice API. This supersedes the earlier Invoice API recommendation in `unified-api-design.md`. | Research: the Invoice API has no current documentation and the migration guide points to Payment Sessions |
| X2 | The outcome never comes from a webhook body. A webhook only triggers an authenticated read of the session and its payment; `PAID` requires session `COMPLETED`, payment `SUCCEEDED`, and captured amounts that sum to the stored amount in the stored currency. | Research: no signature, no event id, no paid-amount field on the session, ambiguous session-webhook authentication |
| X3 | When the config stores a `callback_token`, `x-callback-token` must match it (constant time) before anything else happens. Without one, the API read still protects the payment. | Research |
| X4 | De-duplication key: `session:<session id>:<status>[:<amount><currency>]`, because Xendit sends no event id. Different evidence gets a different key. | Research; found by a test |
| X5 | Subscribe only to `payment_session.completed` and `payment_session.expired`. Other deliveries (`payment.*`) are acknowledged with 200 and ignored. A valid JSON body that cannot be attributed to a payment is answered 200 `ignored`. | Research |
| X6 | `provider_ref` = `payment_session_id`; `payment_url` = `payment_link_url`; Xendit `reference_id` = gateway transaction id. | Research |
| X7 | Xendit documents no idempotency key for session creation. A retry after a timeout may create a second session; the first one's URL was never shown, so it cannot be paid and simply expires. A 409 `DUPLICATE_ERROR` is treated as an unknown outcome (retryable), not a failure. | Research |
| X8 | Return URLs (`return_url`, `cancel_url`) come from the config and must be HTTPS. `country` is derived from the currency (USD needs `settings.country`). `expiry_minutes` must be at least 10. Currencies: IDR, PHP, VND, THB, SGD, MYR, USD. | Research |

## Status (Xendit)

Written and verified by running tests: the adapter (13 tests against `xendittest`), the database-backed flow tests in `internal/service/xendit_flow_test.go`, admin validation and the webhook route (`internal/api/providers_test.go`). Nothing has been run against a real Xendit account; see `xendit-research.md` section 7 for the open points (session-webhook authentication, `reference_id` uniqueness, per-currency decimals).

## Status (PayPal flow tests)

The database-backed PayPal flow tests (`paypal_flow_test.go`) that could not run earlier now pass.

## Environment note

Windows Application Control still blocks some Go test binaries at random (most recently `secrets` and `store`, whose code did not change). Retrying the same command usually, but not always, gets through.

# Phase 4c Decisions (Stripe)

Based on `stripe-research.md`.

| # | Decision | Source |
|---|---|---|
| S1 | The adapter uses **Stripe Checkout Sessions** (hosted page, `mode=payment`). Requests are form-encoded; the secret key is the Basic-auth username. | Research |
| S2 | Webhooks are authenticated with Stripe's documented HMAC: `Stripe-Signature` `t=…,v1=…`, signed payload `t.rawBody`, HMAC-SHA256 with the endpoint signing secret, constant-time compare, any matching `v1` (rotation), other schemes ignored, 5-minute tolerance in both directions. A missing secret never verifies. A known-answer vector computed independently with Node is in the tests. | Research |
| S3 | After the signature passes, `completed` and `async_payment_succeeded` still **read the session from the API**; `PAID` needs `status=complete`, `payment_status=paid` and `amount_total` equal to the stored amount and currency. `completed` with `unpaid` (a delayed payment method) waits for `async_payment_succeeded`. | Research (Stripe's fulfillment guide) |
| S4 | `Idempotency-Key` = the gateway's attempt key on create (Stripe keeps keys 24 h, inside the 5 h resume cap). 409 and 429/5xx are unknown outcomes (retried); other 4xx, including an `idempotency_error`, are definite. | Research |
| S5 | **Only currencies whose decimal handling is certain are accepted**: USD EUR GBP CAD AUD NZD SGD CHF HKD SEK NOK DKK MYR (×100) and JPY KRW (as is). IDR and three-decimal currencies are refused rather than risk a 100x error, until the currency page is confirmed. | Research gap |
| S6 | `provider_ref` = session id, `payment_url` = session `url`, `client_reference_id` and `metadata[gateway_transaction_id]` = gateway transaction id, `expiry_minutes` clamped to Stripe's 30 min–24 h. | Research |
| S7 | Migration `0012_provider_stripe.sql` extends the `provider_configs.provider` check to allow `stripe`. | Needed by S1 |

## Status (Stripe)

Written and verified by running tests: the adapter (11 tests against `stripetest`, including the known-answer signature vector), the database-backed flows in `stripe_flow_test.go` (8), the admin rules (`providers_test.go`), and migration 0012 (the whole suite creates Stripe configs). Nothing has been run against a Stripe account; see `stripe-research.md` section 9.

# Phase 4d Decisions (Midtrans)

Based on `midtrans-research.md`.

| # | Decision | Source |
|---|---|---|
| M1 | The adapter uses **Midtrans Snap** (`POST /snap/v1/transactions`, hosted page). The Server Key is the Basic-auth username. Sandbox and production use **different hosts** (Snap `app.…`, Core `api.…`), selected by the config's `environment`. | Research |
| M2 | **IDR only**; `gross_amount` is sent as an integer. Any other currency is refused before a call. | Research |
| M3 | `order_id` = the gateway transaction id (valid characters, 36 chars); `provider_ref` = that same `order_id`; `callbacks.finish`/`error` from the config's `return_url`/`cancel_url`; `expiry` from `expiry_minutes` (1 to 10080, Midtrans maximum 7 days). | Research |
| M4 | A notification is authenticated with `signature_key = SHA512(order_id + status_code + gross_amount + ServerKey)` (constant time, hex case ignored). There is **no separate webhook secret**; the admin API rejects one for Midtrans. A known-answer vector computed independently with Node is in the tests. | Research (formula from a page summary, to confirm in the sandbox) |
| M5 | After the signature passes, the outcome is **read from the status API** (`GET /v2/{order_id}/status`), never taken from the body. `PAID` = `settlement`, or `capture` with `fraud_status = accept`; the amount comes from the API read and is compared by the generic code. `pending`, `authorize`, card `challenge` and refunds/chargebacks do not change the payment. | Research |
| M6 | De-duplication key (no notification id exists): `order:<order_id>:<transaction_status>:<fraud_status>:<gross_amount>`. | Research |
| M7 | A duplicate-`order_id` rejection on a retried create is treated as an unknown outcome (retried), because the first attempt may have succeeded. The exact error code and text are undocumented, so detection is loose. | Research gap |
| M8 | Known limitation: Midtrans may reverse a `settlement` to `deny` within minutes for Permata VA, Mandiri bill and Indomaret. `paid → failed` is not an allowed transition, so such a reversal is recorded as `invalid_transition` on the event row and a human must notice. Revisit with refunds and reversals. | Research |
| M9 | Migration `0013_provider_midtrans.sql` allows `midtrans` in `provider_configs.provider`. | Needed by M1 |

## Status (Midtrans)

Written and verified by running tests: the adapter (10 tests against `midtranstest`, including the known-answer signature), the database-backed flows in `midtrans_flow_test.go` (6), the admin rules, and migration 0013. Nothing has been run against a Midtrans sandbox account; see `midtrans-research.md` section 6.
