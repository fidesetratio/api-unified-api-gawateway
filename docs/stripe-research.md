# Stripe Documentation Review (Phase 2b, Stripe)

Read on 2026-10-23 from `docs.stripe.com` by helper agents. Long pages were partly summarized, so **[not found]** means "not on the pages that could be read". Nothing was tried against a Stripe account.

Legend: **[V]** stated on an official page. **[I]** my inference or prior knowledge, to confirm in test mode. **[not found]**.

## 1. Product: Checkout Sessions, hosted page, payment mode [V]

`POST /v1/checkout/sessions` with `mode=payment`; the customer is redirected to the session `url`. The Bagisto Stripe package in `opensource2/` uses the same product (reference only, MIT; nothing copied).

## 2. API basics

- Base `https://api.stripe.com/v1/...`; auth is the secret key as the username with an empty password (`-u sk_test_...:` in every example) **[V]**. Bearer wording and the exact `application/x-www-form-urlencoded` wording are **[not found]**; requests are form-encoded with bracket notation (`line_items[0][price_data][currency]=usd`) **[V by examples]**. `Stripe-Version` header: **[not found]**; the adapter leaves it unset (the account's default API version applies).
- Test vs live: separate keys (`sk_test_…`, `sk_live_…`) and a `livemode` flag on objects **[V]**.
- Errors: HTTP 400 bad parameters, 401 bad key, 402 request failed, 403, 404, 409 conflict ("perhaps due to using the same idempotent key"), 424, 429 (back off), 500/502/503/504 ("rare") **[V]**. Error object fields `type` (`api_error`, `card_error`, `idempotency_error`, `invalid_request_error`), `code`, `message`, `param` **[V]**; the top-level `{"error": {...}}` wrapper was not shown on the page but is how Stripe responds **[I]**.

## 3. Create session [V]

- `mode` (`payment`); `line_items` (required in payment mode, up to 100) each with `price_data[currency]` (lowercase ISO code), `price_data[unit_amount]` (non-negative **integer in the smallest currency unit**), `price_data[product_data][name]`, `quantity`; `success_url` (required); `cancel_url` (optional); `client_reference_id` (max 200 chars); `metadata`; `payment_intent_data[...]` (including `metadata`); `customer_email`.
- `expires_at`: "anywhere from 30 minutes to 24 hours after Checkout Session creation. By default, this value is 24 hours from creation."
- Metadata: up to 50 keys, key names up to 40 characters, values up to 500 (stated on the metadata page for the listed objects).

## 4. The session object [V]

- `id` (`cs_…` in examples), `url` (only while the session is open), `status` ∈ `open` / `complete` / `expired` ("`complete`: payment processing may still be in progress"), `payment_status` ∈ `paid` / `unpaid` / `no_payment_required`, `amount_total` (minor units), `currency` (lowercase), `payment_intent`, `expires_at`, `client_reference_id`.
- `paid` means "the payment funds are available in your account"; `unpaid` means "not yet available". So money has arrived only when **`status = complete` and `payment_status = paid`**. A complete but unpaid session is a delayed payment method still processing **[I from the definitions]**.
- `GET /v1/checkout/sessions/{id}`; `POST /v1/checkout/sessions/{id}/expire` for an open session.

## 5. Idempotency [V]

`Idempotency-Key` header on POST requests, up to 255 characters, keys removed after at least 24 hours; a retry with the same key returns the saved response (including 500s); the same key with different parameters is an `idempotency_error`; a request that conflicts with one still executing can fail with 409 and may be retried. This is stronger than PayPal (6 h) and Xendit (none).

## 6. Currencies and amounts

- Amounts are integers in the minor unit: "enter 1099 to charge 10.99 USD (or any other two-decimal currency)"; "enter 10 to charge 10 JPY (or any other zero-decimal currency)" **[V]**. "Currencies are two-decimal currencies unless otherwise specified." The full zero-decimal list and any three-decimal list **did not load [not found]**. ISK and UGX are called out as special cases (to charge 5 UGX send 500).
- Minimum charge depends on currency (for example 0.50 USD, 0.50 EUR, 0.30 GBP, 50 JPY) **[V]**.
- **Consequence:** the adapter accepts only a short allowlist whose decimal handling is certain (two-decimal: USD, EUR, GBP, CAD, AUD, NZD, SGD, CHF, HKD, SEK, NOK, DKK, MYR; zero-decimal: JPY, KRW). Everything else, notably **IDR** (Stripe has special handling the fetched pages do not confirm) and three-decimal currencies, is refused rather than risk charging 100x the amount. Extend the list only after the currency page is read in full or confirmed in test mode.

## 7. Webhooks [V]

- Header `Stripe-Signature: t=<unix time>,v1=<hex signature>[,v1=<second signature>][,v0=...]`. Ignore every scheme except `v1`. Signed payload is `t` + `.` + the **raw request body**; signature is **HMAC-SHA256 with the endpoint signing secret (`whsec_…`)**, compared in constant time; hex encoding is shown by 64-character examples but not stated **[I]**. Default tolerance **5 minutes** in Stripe's libraries; a tolerance of 0 disables the recency check. During secret rotation (up to 24 hours) the header carries one `v1` per active secret. Each retry has a new signature and timestamp.
- The secret is per endpoint, and different for test and live.
- Events: `checkout.session.completed` ("successfully completed"), `checkout.session.async_payment_succeeded` ("a delayed payment method finally succeeds"), `checkout.session.async_payment_failed`, `checkout.session.expired`. Stripe's fulfillment guide fulfills on `completed` and `async_payment_succeeded`, checks `payment_status`, and retrieves the session from the API.
- Duplicates happen; "guard against duplicated event receipts by logging the event IDs you've processed". Whether the event id stays the same across retries is implied but **not stated**. No ordering guarantee. Live mode retries for up to **three days** with exponential backoff; the endpoint must return 2xx quickly (redirects count as failures); sandbox retries three times over a few hours.
- Refund events exist (`charge.refunded`, `refund.created/updated/failed`) but refunds are out of scope for now.

## 8. Consequences for the adapter

1. Verify `Stripe-Signature` exactly as documented (HMAC-SHA256, constant time, any matching `v1`, 5-minute tolerance both ways, raw body). A missing or empty secret never verifies.
2. After the signature passes, still **read the session from the API** for `completed` and `async_payment_succeeded`, because Stripe's own guide says to, and use *its* `payment_status` and `amount_total`. `PAID` needs `status=complete`, `payment_status=paid`, and `amount_total` (converted from minor units) equal to the stored amount in the stored currency.
3. `completed` with `payment_status=unpaid` is not paid: it is acknowledged and the later `async_payment_succeeded` settles it.
4. `Idempotency-Key` = the gateway's attempt key on create (Stripe keeps it 24 hours, comfortably inside the 5-hour resume cap). A 409 is an unknown outcome (retry); an `idempotency_error` is a definite failure.
5. `provider_ref` = session id; `payment_url` = session `url`; `client_reference_id` and `metadata[gateway_transaction_id]` = the gateway transaction id; `expires_at` clamped to 30 minutes–24 hours (config `expiry_minutes`).
6. The database `provider` check constraint must allow `stripe` (migration 0012).

## 9. To verify in test mode

Exact zero-decimal list and IDR handling; whether the event id is stable across retries; the error JSON wrapper; behaviour when `expires_at` is exactly at a bound; whether `payment_status=paid` can arrive on `completed` for every card payment.
