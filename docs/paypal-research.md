# PayPal Documentation Review (Phase 2b)

Read on 2026-10-23 from PayPal's official developer pages. Page text was fetched by helper agents and summarized; pages longer than ~39K characters were truncated, so items marked **[not found]** are *not on the pages that could be read*, not proof that PayPal lacks them. Where PayPal's own pages disagree, that is stated. Nothing here was tested against a live sandbox yet.

Legend: **[V]** stated on an official page (source named). **[I]** my inference, to be confirmed in the sandbox. **[not found]** missing from what could be read.

## 1. Currency: IDR is not supported [V]

- `developer.paypal.com/docs/reports/reference/paypal-supported-currencies/` (updated 2026-07-10) "lists every fiat currency that PayPal supports for payments and account balances": AUD, BRL, CAD, CNY, CZK, DKK, EUR, HKD, HUF, ILS, JPY, MYR, MXN, TWD, NZD, NOK, PHP, PLN, GBP, SGD, SEK, CHF, THB, USD. **IDR is not in the list.**
- `developer.paypal.com/api/rest/reference/currency-codes/` (updated 2026-08-17, the REST/Checkout list) has the same codes plus RUB; no IDR.
- IDR does appear on PayPal pages about *sending money*, *payout conversion* and the *exchange-rate API*. Those are different products and do not make IDR a Checkout payment currency.
- **Consequence:** the sample request currency `IDR` cannot be used with PayPal. A PayPal config should list only supported currencies (e.g. `USD`), and an IDR payment must go to Xendit (Phase 4b).
- No-decimal currencies for PayPal: **HUF, JPY, TWD** ("If you pass a decimal amount, an error occurs"). Note our `domain` exponent table treats HUF as 2 decimals (the ISO value) and has no entry for TWD; the PayPal adapter must apply PayPal's rule itself (or the table must be changed).
- BRL, CNY, MYR are in-country-account only. Receiving a currency the merchant account does not hold needs "Payment Receiving Preferences", otherwise the payment stays pending until approved manually.

## 2. Authentication [V]

- Base URLs: sandbox `https://api-m.sandbox.paypal.com`, live `https://api-m.paypal.com` (`api/rest/requests/`).
- Token: `POST {base}/v1/oauth2/token`, HTTP Basic `client_id:client_secret`, form body `grant_type=client_credentials`; response has `access_token`, `token_type: Bearer`, `expires_in` (seconds), `scope`, `app_id`, `nonce`. Call again after expiry (`api/rest/authentication/`).
- The adapter should cache the token per config in memory and refresh before `expires_in`; never persist it.

## 3. Create order [V unless noted]

- `POST /v2/checkout/orders`. Required: `intent` (`CAPTURE` or `AUTHORIZE`), `purchase_units` (1–10) each with `amount {currency_code, value}` (`value` is a **string**).
- Useful fields: `reference_id` (1–256, defaults to `default` for a single unit), `custom_id` (1–255, "used to reconcile client transactions with PayPal transactions", not shown to the payer), `invoice_id` (1–127, shown to the payer, "required to be unique within each merchant account by default"), `description` (truncated beyond 127 chars).
- Headers: `PayPal-Request-Id` (idempotency, optional, 1–10000 chars), `Prefer` (`return=minimal` default: only id, status and links; `return=representation` for the full body).
- Redirect flow: `payment_source.paypal.experience_context` with `return_url`, `cancel_url`, and example values `user_action: PAY_NOW`, `landing_page: LOGIN`, `shipping_preference: GET_FROM_FILE`, `payment_method_preference: IMMEDIATE_PAYMENT_REQUIRED`. The full allowed-value lists are **[not found]**.
- Response (201; 200 for an idempotent repeat):
  - Request **without** `payment_source`: status `CREATED`, link `rel: "approve"` (e.g. `https://www.paypal.com/checkoutnow?token=<ORDER ID>`).
  - Request **with** `payment_source.paypal.experience_context`: status `PAYER_ACTION_REQUIRED`, link `rel: "payer-action"` with the same URL shape.
  - So the buyer redirect is the link whose rel is `payer-action` **or** `approve`; the `token` query value is the order id. Sandbox presumably uses `www.sandbox.paypal.com` **[I]** (the user's sample URL; PayPal's examples show the live host).
- Order statuses seen: `CREATED`, `PAYER_ACTION_REQUIRED`, `APPROVED`, `COMPLETED`. Full enum **[not found]**.
- **Order expiry:** how long an unapproved order stays valid is **[not found]**. (Only authorizations: "best captured within three days, available up to 29 days".) Therefore `expires_at` for PayPal must be a gateway-side setting (config `settings`), not a PayPal value.

## 4. Idempotency [V with a conflict]

- `PayPal-Request-Id`: a retry with the same id returns the original result; omitted header means PayPal treats the call as new.
- Retention: the general requests page uses 45 days (refund example); a PayPal server-SDK page for Orders says keys are kept **6 hours**, longer via an account manager. Treat **6 hours as the safe assumption for Orders** **[I]**.
- Consequence for our design: the reconciler retries with the same key every few minutes, which is inside 6 hours; a retry *after* 6 hours could create a second PayPal order. Mitigations: (a) stop resuming `creating` payments after a few hours and open a review item instead; (b) set `custom_id` to the gateway transaction id so a duplicate is traceable; (c) `invoice_id` uniqueness per merchant account may reject a duplicate **[I]**, to be tested in the sandbox.
- Concurrent requests with the same id: "PayPal processes the first request and might fail the second." Our attempt lease already prevents concurrent calls.

## 5. Capture [V where quoted]

- `POST /v2/checkout/orders/{id}/capture`: "the buyer must first approve the order or a valid payment_source must be provided". The buyer approves by being redirected to the approve/payer-action URL. **Money moves at capture, not at approval.** So after the buyer returns, the gateway must capture.
- Capture response: top-level `status` (`COMPLETED` in the sample) and `purchase_units[0].payments.captures[0]` with `id`, `status`, `amount {currency_code, value}`, `final_capture`. "If an external payment processor declines a transaction, PayPal returns HTTP 201 CREATED and a status of DECLINED in the capture status", so the HTTP code alone is not evidence of payment: **the capture `status` must be `COMPLETED` and the amount/currency must match.** `PENDING` captures exist (reason values **[not found]**) and must not be treated as paid.
- Capturing twice: error code **[not found]**; the adapter must treat "already captured" as a state to re-read via Show order, not as failure **[I]**.
- Show order: `GET /v2/checkout/orders/{id}` returns `status` (e.g. `APPROVED`), `purchase_units[].amount`, links.
- Errors: JSON `{name, message, debug_id, details[], links}`; OAuth errors `{error, error_description}`. Codes: 400 INVALID_REQUEST, 404 RESOURCE_NOT_FOUND, 409 RESOURCE_CONFLICT, 422 UNPROCESSABLE_ENTITY, 429 RATE_LIMIT_REACHED, 500, 503. On 5xx/timeout PayPal recommends repeating the same call with the same `PayPal-Request-Id`. Log `debug_id`, never credentials or the full body.

## 6. Webhooks [V]

Sources: `api/rest/webhooks/rest` (updated 2026-06-18), `api/rest/webhooks/event-names/` (2026-09-10), `api/webhooks/v1/verify-webhook-signature-post`.

- **Webhook id** comes from the Developer Portal when the listener URL is subscribed; it is *not* in headers or body. Store it as the config's webhook secret (`webhook_id`). One id per listener URL/app.
- **Postback verification:** `POST {base}/v1/notifications/verify-webhook-signature` with Bearer token and body `{auth_algo, cert_url, transmission_id, transmission_sig, transmission_time, webhook_id, webhook_event}` filled from headers `PAYPAL-AUTH-ALGO`, `PAYPAL-CERT-URL`, `PAYPAL-TRANSMISSION-ID`, `PAYPAL-TRANSMISSION-SIG`, `PAYPAL-TRANSMISSION-TIME`. Response `{"verification_status":"SUCCESS"}` (the other value is `FAILURE`). The page lists a token scope `https://uri.paypal.com/services/applications/verify-webhook-signature` **[whether a plain client-credentials token suffices: not found, test in sandbox]**.
- **Raw body rule:** the event "must be posted back exactly as it was received... Parsing to an array/object followed by re-serializing... can fail verification." So the adapter must embed the original bytes (e.g. `json.RawMessage`) into the verify request, never re-marshal a decoded struct.
- **Offline verification** (documented): signed string `transmissionId|timeStamp|webhookId|crc32` where `crc32` is the CRC-32 of the **raw body in decimal**; fetch the public key from the `paypal-cert-url` certificate (cache it) and verify the base64 `paypal-transmission-sig` (sample code uses SHA256 with RSA). No rule about which `cert_url` hosts are acceptable is on the pages **[not found]**; an offline implementation must pin PayPal's domains itself, so **postback is the safer first implementation**.
- Mock events from the simulator cannot be postback-verified (use webhook id `WEBHOOK_ID`).
- Events relevant to Orders v2: `CHECKOUT.ORDER.APPROVED` ("A buyer approved a checkout order"), `PAYMENT.CAPTURE.COMPLETED`, `PAYMENT.CAPTURE.PENDING`, `PAYMENT.CAPTURE.DECLINED` (Payments v2; the page also lists `DENIED` under v1), `PAYMENT.CAPTURE.REFUNDED`, `PAYMENT.CAPTURE.REVERSED`, `CHECKOUT.PAYMENT-APPROVAL.REVERSED` ("a problem occurred after the buyer approved the order but before you captured the payment").
- Body: `id`, `event_version`, `create_time`, `resource_type`, `resource_version`, `event_type`, `summary`, `resource`, `links`. A capture event's `resource` has `id`, `status`, `amount`, and `supplementary_data.related_ids.order_id` (sample), which links a capture back to the order, i.e. to our `provider_ref`.
- **Delivery:** the listener must answer 2xx; PayPal "will retry each delivery up to 25 times over 3 days" on non-2xx, no response, 404 or 500.
- **Is the event `id` unique and stable across retries?** **[not found].** Our dedup is `(provider, event_id)`, and the state machine already makes a repeated fact a no-op, so a changing id would be harmless but would not be deduplicated; confirm in the sandbox.

## 7. What this means for the adapter

1. **Currency:** PayPal configs accept only supported currencies; IDR goes to Xendit. Consider validating `allowed_currencies` per provider in the admin API.
2. **Create:** `intent=CAPTURE`, one purchase unit with `custom_id` = gateway transaction id, `PayPal-Request-Id` = the attempt idempotency key, `Prefer: return=representation`, `payment_source.paypal.experience_context` with `return_url`/`cancel_url` taken from the config (never from the request) and `user_action: PAY_NOW`. `payment_url` = the `payer-action` (or `approve`) link; `provider_ref` = order id; `expires_at` from config.
3. **The interface needs one more operation: capture.** `CreatePayment`/`GetStatus`/`VerifyWebhook` are not enough, because approval does not move money. The settlement flow becomes: verified `CHECKOUT.ORDER.APPROVED` (or the buyer's return) → gateway calls capture with a `PayPal-Request-Id` → capture response `COMPLETED` and a verified `PAYMENT.CAPTURE.COMPLETED` webhook with matching amount/currency → `PAID`. `PENDING`/`DECLINED` never mark paid.
4. **Webhook verification:** postback to `verify-webhook-signature`, passing the raw body through unchanged; the config's webhook secret holds `webhook_id`; a failed or erroring verification changes nothing.
5. **Resilience:** token cache with refresh; timeouts shorter than the lease; map 429/5xx to retry, 4xx to definite failure; log `debug_id`; stop resuming after about 5 hours because of the 6-hour idempotency window.
6. **Tests:** recorded sandbox fixtures for create, approve-pending, capture COMPLETED/DECLINED/PENDING, errors; negative webhook cases from `testing-guide.md`; one real sandbox smoke test with the user's own credentials, not committed.

## 8. Still to verify in the sandbox

Plain client-credentials token for the verify endpoint; exact approve/payer-action host for sandbox; `invoice_id` duplicate behaviour; recapture error; order expiry; whether event `id` repeats across retries; `PENDING` capture reasons; whether IDR orders are rejected as expected (they should be).

Sources: developer.paypal.com pages named above, read 2026-10-23.
