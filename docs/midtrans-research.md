# Midtrans Documentation Review (Phase 2b, Midtrans)

Read on 2026-10-23 from `docs.midtrans.com` by helper agents. Several long pages were only partly returned verbatim; points that rest on a model summary are marked **[S]** (to confirm on the live page). **[V]** = stated verbatim on an official page. **[I]** = my inference or prior knowledge, to confirm in the sandbox. **[not found]** = absent from what could be read. Nothing was tried against a Midtrans account.

## 1. Product: Snap (hosted payment page) [V]

`POST /snap/v1/transactions` returns a `token` and a `redirect_url`; the customer is sent to `redirect_url`. Hosts differ per environment:

| | Snap (create) | Core API (status, expire, cancel) |
|---|---|---|
| Sandbox | `https://app.sandbox.midtrans.com/snap/v1/transactions` | `https://api.sandbox.midtrans.com/v2/...` |
| Production | `https://app.midtrans.com/snap/v1/transactions` | `https://api.midtrans.com/v2/...` |

Auth is HTTP Basic: **username = Server Key, empty password**. Headers `Accept: application/json`, `Content-Type: application/json`. Sandbox and production have **separate keys and separate hosts**, so the config's `environment` matters here (unlike Xendit and Stripe).

## 2. Create a Snap transaction [V]

- Required: `transaction_details.order_id` and `transaction_details.gross_amount`.
- `order_id`: string, **max 50**, allowed characters alphanumeric, `-`, `_`, `~`, `.`; "A single ID could be used only once by a Merchant." An expired order id can be reused.
- `gross_amount`: **integer**. Decimals are not documented for requests (do not send them). Amounts are in **IDR**; no other currency is documented for Snap.
- Optional and useful: `callbacks.finish` (redirect after success, max 255) and `callbacks.error` (redirect after failure); `expiry` `{"unit":"minutes","duration":N}` (minimum 20 seconds, **maximum 7 days**, units day/hour/minute); `customer_details`; `item_details` (if present, the sum of price×quantity must equal `gross_amount` exactly); `enabled_payments`.
- Default lifetime if `expiry` is omitted: the dashboard Snap preference **[S: 24 hours]**.
- Response: **201** with `{"token": "...", "redirect_url": "https://app.sandbox.midtrans.com/snap/v2/vtweb/<token>"}` (one page shows a v3 path; the form of `redirect_url` varies, so the adapter takes it from the response).
- Errors: `{"error_messages": ["..."]}` with 400 (bad parameter), 401 (wrong key), 500 (retry). **Duplicate `order_id`** gives an error (the summary quotes the Indonesian text "transaction_details.order_id sudah digunakan" **[S]**); the HTTP code (the docs do not mention 406) is **[not found]**. An unpaid Snap order id can reportedly be re-initiated before the customer picks a payment method **[S]**.
- **No idempotency header.** `order_id` uniqueness plays that role.

## 3. Reading a transaction [V unless marked]

`GET {core}/v2/{order_id}/status`. Fields: `status_code` (string), `status_message`, `transaction_id`, `order_id`, `payment_type`, `transaction_time`, `transaction_status`, `fraud_status`, `gross_amount` (a **string** such as `"30000.00"`, in IDR), `signature_key`, `currency` (in samples). A Snap order with no payment attempt yet has no status: "you might encounter 404 or Payment not found" **[V]**. The Core API reports errors in the body as `{"status_code":"404"|"401"|"412",...}`; whether the HTTP code matches is **[not found]** (the adapter reads both).

`transaction_status` values [V]: `pending`, `capture`, `settlement`, `deny`, `cancel`, `expire`, `failure`, `refund`, `partial_refund`, `chargeback`, `partial_chargeback`, `authorize`. Meaning of "paid":

- `settlement`: "Funds have been received."
- Cards: `capture` with `fraud_status = accept`: "it is safe to assume a successful payment". `capture` with `challenge`: questioned by the fraud system; "approve to accept it or it is automatically canceled".
- `pending`, `authorize`: not paid. `deny`, `failure`: failed. `cancel`, `expire`: ended without payment.
- **Caveat [V]:** for Permata bank transfer, Mandiri bill payment and Indomaret, a `settlement` can in rare cases later change to `deny`, usually within 1–5 minutes.

`POST {core}/v2/{order_id}/expire` and `/cancel` exist (412 "Merchant cannot modify the status of the transaction" once settled).

## 4. Notifications (webhooks)

- Configured in the dashboard (SETTINGS > CONFIGURATION > Payment Notification URL) **[V]**; it must be publicly reachable, no auth/VPN/odd ports. Per-request override headers (`X-Append-Notification`, `X-Override-Notification`) **[S]**.
- Body (JSON, POST, `User-Agent: Veritrans`): `transaction_time`, `transaction_status`, `transaction_id`, `status_message`, `status_code`, `signature_key`, `payment_type`, `order_id`, `merchant_id`, `gross_amount`, `fraud_status`, `currency`, `settlement_time`, plus per-method fields **[V]**.
- **Authenticity [S]:** `signature_key = SHA512(order_id + status_code + gross_amount + ServerKey)`, the four values concatenated as strings (`gross_amount` exactly as it appears in the notification, e.g. `"10000.00"`), hex digest. There is **no separate webhook secret**: the Server Key is the secret. Constant-time comparison is not mentioned **[not found]**, so the adapter does it. The page also says IP allow-listing is not recommended as the authenticity check (a published IP list exists).
- Retries [S]: up to 5 times, intervals about 2, 10, 30, 90 and 210 minutes; answer 2xx quickly (they wait up to 15 s). "Implement notification handling in an idempotent way."
- **No notification id** is documented; the only identifier is `transaction_id`/`order_id` **[not found]**. Ordering is not guaranteed.
- Sandbox: a web simulator for payments (`simulator.sandbox.midtrans.com`); no notification simulator documented. Test card `4811 1111 1111 1114`, CVV 123, 3DS OTP 112233.

## 5. Consequences for the adapter

1. **Currency IDR only**, integer amounts. Any other currency or any fractional IDR is refused before a call (the gateway's IDR amounts are already whole numbers).
2. `order_id` = the gateway transaction id (36 chars, valid characters); `provider_ref` = that same `order_id`. The Snap `token` is not needed afterwards.
3. **Webhook:** verify `signature_key` with the Server Key (constant time, lowercase/uppercase hex both accepted by decoding), then **read the transaction from the status API** and decide from that read, not from the body. `PAID` = `settlement`, or `capture` with `fraud_status = accept`; `gross_amount` from the API read is compared by the generic code against the stored amount. `challenge`, `pending`, `authorize` do not pay. `deny/failure` → failed, `cancel` → cancelled, `expire` → expired, refunds/chargebacks are ignored for now.
4. De-duplication key (no notification id exists): `order:<order_id>:<transaction_status>:<fraud_status>:<gross_amount>` built from the API read.
5. A duplicate-`order_id` error on a retried create is treated as an unknown outcome (retryable), not a failure, because the first attempt may have succeeded; whether a retry can recover is unconfirmed.
6. `environment` selects the sandbox or production hosts. `callbacks.finish`/`error` come from the config (`return_url`, `cancel_url`); `expiry` from `expiry_minutes` (1 to 10080).
7. The late `settlement → deny` reversal can arrive after the gateway marked a payment `PAID`; `paid → failed` is not an allowed transition, so it is recorded as `invalid_transition` on the event row and needs a human to notice. Revisit when refunds and reversals are designed.
8. Migration `0013_provider_midtrans.sql` extends the `provider_configs.provider` check.

## 6. To verify in the sandbox

The signature formula and `gross_amount` string format; HTTP vs body status codes of the Core API; the duplicate `order_id` error and whether an unpaid order id can be re-initiated; whether Snap `redirect_url` varies; the default expiry; `settlement_time` presence; whether notifications carry any usable id.
