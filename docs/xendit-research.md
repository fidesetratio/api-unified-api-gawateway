# Xendit Documentation Review (Phase 2b, Xendit)

Read on 2026-10-23 from Xendit's official documentation (`docs.xendit.co`, including `.md` versions of its pages) by helper agents. Pages longer than ~39K characters were partly summarized, so **[not found]** means "not on the pages that could be read". Nothing was tested against a Xendit account.

Legend: **[V]** stated on an official page. **[I]** my inference. **[not found]**.

## 1. Which Xendit product: Payment Sessions, not Invoices [V]

- The Invoice API (`POST /v2/invoices`, `invoice_url`, `x-callback-token`) is **legacy**. The current documentation has no Invoice reference pages ("invoices-overview" is a 404); its Invoice section survives only in an archive site whose bodies could not be read. `migrate-to-payment-session.md` (updated 2026-10-22) maps legacy `POST /v2/invoices` to **Payment Sessions `POST /sessions`**, `external_id` to `reference_id`, `invoice_duration` (seconds) to `expires_at` (timestamp), `GET /v2/invoices/{id}` to `GET /sessions/{id}`.
- Decision: build the adapter on **Payment Sessions** (the earlier recommendation of the Invoice API in `unified-api-design.md` is superseded). The Magento module in `opensource3/` also uses a "payment session", but through a Xendit-internal gateway; its contract is not the public one.

## 2. API basics [V]

- Base URL `https://api.xendit.co`. Auth: HTTP Basic, secret API key as username, empty password (colon at the end), HTTPS only. Test and live use the same host; the key decides the mode **[I]** (docs: requests in test mode "never hit the banking networks").
- Rate limits: 60 requests/min per endpoint per account in test mode, 600 in live; 429 `RATE_LIMIT_EXCEEDED`; headers `Rate-Limit-*`.
- Error body: `{"error_code": "...", "message": "...", "errors": [...]}`. Create-session codes: `API_VALIDATION_ERROR`, `INVALID_AMOUNT`, `INVALID_CURRENCY`, `INVALID_COUNTRY`, `INVALID_URL`, `INVALID_EXPIRY_DATE`, `MODE_NOT_SUPPORTED`... (400); `INVALID_API_KEY` (401); `DATA_NOT_FOUND` (404); `DUPLICATE_ERROR` (409); `SERVER_ERROR` (5xx).

## 3. Create session `POST /sessions` [V]

- Required: `reference_id` (1–64 chars), `session_type` (`PAY` for a one-time payment), `mode` (`PAYMENT_LINK` = Xendit hosted checkout redirect), `amount` (a JSON **number**), `currency`, `country`.
- Currencies **IDR, PHP, VND, THB, SGD, MYR, USD**; countries **ID, PH, VN, TH, SG, MY**. Per-currency decimals and min/max amounts: **[not found]**.
- Useful optional fields: `capture_method` (`AUTOMATIC`/`MANUAL`), `description` (1–1000), `success_return_url` and `cancel_return_url` (must be **HTTPS**), `expires_at` (ISO 8601, default 30 minutes after creation, must be at least 10 minutes ahead; maximum not stated), `locale`, `metadata`, `items`, `customer` (required only for SAVE sessions or when saving payment methods).
- Response 201: `payment_session_id` (`ps-…`, 27 chars; the id field is **not** called `id`), `status: ACTIVE`, `payment_link_url` (the hosted checkout URL to redirect the buyer to), `expires_at`, `reference_id`, `amount`, `currency`, `payment_id`/`payment_request_id` (null until a payment exists).
- **Idempotency header for POST /sessions: [not found]**. Whether `reference_id` must be unique for sessions: **[not found]** (only customer `reference_id` is documented as unique, 409 `DUPLICATE_ERROR`).

## 4. Reading and ending a session [V]

- `GET /sessions/{payment_session_id}` returns the same object. `POST /sessions/{id}/cancel` cancels an ACTIVE session (422 `INVALID_SESSION_STATUS` otherwise).
- Session statuses: `ACTIVE`, `COMPLETED`, `EXPIRED`, `CANCELED` (an expired or canceled session cannot be reactivated).
- A session has **no paid-amount field**. Xendit says the underlying **Payment** is "the transaction source of truth (did the money move)". Read it with `GET /v3/payments/{payment_id}` (optional header `api-version: 2024-11-11`): `status` (`AUTHORIZED`, `CANCELED`, `SUCCEEDED`, `FAILED`, `EXPIRED`, `PENDING`), `request_amount`, `captures[].capture_amount`. `captures[].capture_amount` is what was actually captured and can be lower than requested (partial capture exists). `payment_id` comes from the session object once it is completed.

## 5. Webhooks [V]

- Retries: up to six, with exponential backoff over about 24 hours, until a 2xx; the endpoint must answer quickly. Duplicates and out-of-order delivery are both possible. A webhook can also arrive after expiry (delayed webhooks).
- Session events: **`payment_session.completed`** and **`payment_session.expired`** only. There is **no** webhook for `CANCELED`. Payload: `{ "event", "business_id", "created", "data": { "payment_session_id", "reference_id", "status", "amount", "currency", "payment_id", "payment_request_id", ... } }`. `data.created`/`created` is the delivery-attempt time and changes on retry.
- Payment events (separate subscription): `payment.capture`, `payment.failure`, `payment.authorization`, `refund.succeeded`, `refund.failed`. The payment payload has `data.payment_id`, `reference_id`, `request_amount`, `captures[]`, `status`, **but no `payment_session_id`**. (One migration page says `payment.succeeded`; the webhook reference says `payment.capture`. Check the dashboard before relying on either.)
- **Authenticity:** payment webhooks carry a static `x-callback-token` header that you compare with the token in the dashboard's Webhook settings (per business/sub-account, and test/live appear separate). No HMAC or signature option exists. For **session** webhooks the OpenAPI spec lists "basic" auth and no `x-callback-token` header: **ambiguous**, to confirm in the dashboard. No token-rotation procedure is documented.
- **No unique event/delivery id** is documented, so de-duplication must use fields of the data (`payment_session_id` plus final status).
- Webhook URLs are set in the dashboard (per product); a per-request webhook URL for sessions: **[not found]**. Xendit's source IPs: **[not found]**.

## 6. Consequences for the adapter

1. **Do not trust the webhook body for the outcome. Confirm with an authenticated API call.** A webhook (valid token, when one is configured) only *triggers* the adapter to fetch `GET /sessions/{id}` and, for a completed session, `GET /v3/payments/{payment_id}`. `PAID` requires session `COMPLETED`, payment `SUCCEEDED`, and the sum of `captures[].capture_amount` equal to the stored amount in the stored currency. This works whichever way Xendit authenticates session webhooks, supplies the missing paid-amount field, and makes the missing event id harmless.
2. If a session is `COMPLETED` but the payment is not yet `SUCCEEDED`, the adapter returns a retryable error so Xendit redelivers.
3. `provider_ref` = `payment_session_id`; `payment_url` = `payment_link_url`; `reference_id` = the gateway transaction id. Subscribe only to the two session events; other deliveries are acknowledged and ignored.
4. **No idempotency key for create** means a retry after a timeout can create a second session. The first one's URL was never given to anyone, so it cannot be paid; it expires. A 409 `DUPLICATE_ERROR` is treated as an unknown outcome, not a definite failure.
5. Amounts are JSON numbers: the adapter writes the canonical decimal text as a JSON number literal (never a float). `country` comes from the currency (IDR→ID, PHP→PH, VND→VN, THB→TH, SGD→SG, MYR→MY); USD needs `settings.country`.
6. Return URLs come from the config (`return_url`, `cancel_url`), HTTPS only.

## 7. Still to verify against a real Xendit account

Session webhook authentication; whether Xendit's `reference_id` must be unique; decimals and minimum amounts per currency; what `payment_link_url` looks like in test mode (`checkout-staging.xendit.co` / `dev.xen.to` per the docs); behaviour of `expires_at` bounds; the `payment.capture` vs `payment.succeeded` name; whether `payment_id` is present in the webhook for completed sessions.
