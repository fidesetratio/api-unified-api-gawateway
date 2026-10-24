# Reference Analysis

Phase 1 of `CLAUDE.md`. Analysis only; no reference repository was modified and nothing was installed or run.

**Evidence legend.** **[V]** verified in source or docs, with a file path. **[I]** inference from static reading, not run. **[U]** unverified or missing.

All paths are relative to the workspace root. Paths under `opensource3/` are read from git `HEAD` (see 1.3).

---

## 1. Repository inventory

| Dir | Project | Language / framework | Upstream (git remote) | Last commit seen | Providers implemented |
|---|---|---|---|---|---|
| `opensource1/` | `laravel-xendit`, a demo shop | PHP 8.2, Laravel 11 | github.com/iritmaulana/laravel-xendit | `87276f1` "Update Readme" | Xendit Invoice API only |
| `opensource2/` | Bagisto, a full e-commerce platform | PHP, Laravel, modular `packages/Webkul/*` | github.com/bagisto/bagisto | `c0599a7437` | PayPal (Standard/IPN and Smart Button), Stripe, Razorpay, PayU, PayGlocal, PhonePe, plus offline methods |
| `opensource3/` | `xendit-magento-payment-module` (Xendit's Magento 2 plugin) | PHP, Magento 2 | github.com/xendit/xendit-magento-payment-module | `e025a48` v14.x | Xendit "Payment Session" via a Xendit gateway service ("TPI") |

All three directories exist. None is empty in git, but see 1.3 for `opensource3/`. `result/` did not exist and was created for these docs.

### 1.1 Provider coverage vs. our targets

| Target | opensource1 | opensource2 | opensource3 |
|---|---|---|---|
| Xendit | Invoice API [V] | none | Payment Session [V] |
| PayPal | none: grep for "paypal" outside `vendor/` returned nothing | Standard (legacy IPN) and Smart Button (Orders v2 via SDK) [V] | none |

Neither Xendit reference is a Go project and none is a gateway. All three are checkout plug-ins bound to a shop's cart or order, so only the *payment-integration patterns* transfer. Cart, order and stock logic is irrelevant.

### 1.2 Licenses

| Repo | License evidence | Consequence for us |
|---|---|---|
| `opensource1/` | No `LICENSE` file [V]. `composer.json` says `"license": "MIT"` and `README.md` says "MIT" [V], but that is the Laravel skeleton's metadata. The author's own grant is not stated. | **Unclear.** Treat as all-rights-reserved for copying. Concepts only. |
| `opensource2/` | `opensource2/LICENSE`: MIT, "Copyright 2010-2025, Webkul Software" [V] | Permissive. Copying code would require keeping the copyright and permission notice. We plan no copying, only behavior patterns. |
| `opensource3/` | `LICENSE` is GNU GPL v3 [V]; `composer.json` says `GPL-3.0` [V] | **Copyleft.** Copying or translating its code into `result/` could force GPL on our gateway. Do not copy or transliterate. Use only as a description of Xendit's protocol and behavior. |

Open question: some `opensource3/` files carry a Magmodules.eu copyright header (e.g. `etc/db_schema.xml`), which suggests mixed provenance [V]. Another reason not to reuse code.

### 1.3 State of `opensource3/`

The working tree is empty, apart from `.git`. `git status` shows all 103 tracked files as **staged deletions**; `HEAD` still holds them. I read files with `git show HEAD:<path>` and `git ls-tree`, which are read-only. I did not restore or touch the index. If the empty tree is unintended, the user should restore it (outside this task).

---

## 2. opensource1: Laravel + Xendit Invoice API

Small app, about 120 lines of payment code. Everything is in `opensource1/app/Http/Controllers/OrderController.php`.

| Concern | Finding | Evidence |
|---|---|---|
| Config / credentials | `XENDIT_SECRET_KEY` and `XENDIT_WEBHOOK_SECRET` from env, read via `config('services.xendit.*')`. The `.env.example` also lists `XENDIT_PUBLIC_KEY`, but `config/services.php` does not read it. The README mentions `XENDIT_CALLBACK_TOKEN`, but code uses `XENDIT_WEBHOOK_SECRET`, so the README is inconsistent. One global key, no per-merchant config, no DB storage. | `config/services.php:37-40`, `.env.example`, `README.md` |
| Auth / API request | Uses the official `xendit/xendit-php ^6.0` SDK (`Configuration::setXenditKey`, `InvoiceApi`). The SDK presumably does HTTP Basic with the key as username (inferred; SDK is in `vendor/`, not inspected). | `OrderController.php:17-22`, `composer.json` |
| Payment creation | `store()` builds `CreateInvoiceRequest` with `external_id = 'inv-'.time()`, amount from the product, `currency: IDR`, `invoice_duration: 86400`, customer name/email, success/failure redirect URLs, and a fixed `payment_methods` list. | `OrderController.php:33-53` |
| Checkout URL | Redirects the browser to `$invoice['invoice_url']`. | `OrderController.php:66` |
| Persistence | Inserts an `orders` row *after* the API call. `invoice_id` stores the `external_id` (not Xendit's invoice id). `status='pending'`. | `OrderController.php:56-63`, migration `2025_01_19_135851_create_orders_table.php` |
| Webhook auth | Compares header `x-callback-token` with `services.xendit.webhook_secret` using `!==`, returning 403 on mismatch. This is a shared static token, not a payload signature. | `OrderController.php:77-81` |
| Status handling | Writes Xendit's raw `status`, `payment_method` and `payment_channel` into `payment_status`, `payment_method`, `payment_channel`. Only `PAID` triggers logic (stock decrement). No mapping for `EXPIRED` or `FAILED`. The `status` column is never updated after creation. | `OrderController.php:83-98` |
| Status inquiry | None. | n/a |
| Idempotency / duplicates | None. A repeated `PAID` callback decrements stock again. `external_id` uses `time()`, so two orders in the same second collide, and the unique index on `invoice_id` then makes the second fail after the invoice already exists at Xendit. | `OrderController.php:35,93-97`, migration (`unique`) |
| Refund / cancel | None. | n/a |
| Error handling | Catches `XenditSdkException`, logs message and full error (`getFullError()` may contain request data), returns a generic user message. Webhook catch-all returns 500. | `OrderController.php:67-71,101-104` |
| Tests | Only Laravel's two example tests. | `tests/` |

Gaps and risks:
- **No amount or currency check** on the callback, and the callback trusts the body. The shared-token check is the only gate.
- **Token compare is not constant-time** (`!==`). If the configured secret is empty/unset (`null`) and the header is absent (`null`), `null !== null` is false, so the check *passes*. [I] Edge case worth a test in our implementation.
- **No CSRF exemption** for `POST /payment/callback` in `bootstrap/app.php` (middleware block is empty) while the route is in `routes/web.php`. In Laravel 11 this likely returns HTTP 419 to Xendit. [I] Not run. Suggests the webhook may never work as shipped.
- `$payment['payment_method']` and `['payment_channel']` are read without `isset`, so `EXPIRED` callbacks may raise errors. [I]
- Money stored as `decimal(10,2)`; fine for IDR.

---

## 3. opensource2: Bagisto (PayPal focus)

Bagisto is a large platform. Payment methods are plug-in packages under `opensource2/packages/Webkul/{Paypal,Stripe,Razorpay,PayU,PayGlocal,PhonePe,Payment}`. I read PayPal in full and Stripe's controller as a comparison. **Razorpay, PayU, PayGlocal and PhonePe were only listed, not analyzed.** [U]

### 3.1 Shared abstractions
- `packages/Webkul/Payment/src/Payment/Payment.php`: abstract base with `getCode()`, `isAvailable()`, `getConfigData($field)` (reads `sales.payment_methods.<code>.<field>` from the platform config store), and abstract `getRedirectUrl()`. This is a simple provider-interface pattern, and the config is *per payment-method code and per channel*.
- `packages/Webkul/Paypal/src/Config/payment-methods.php`: defaults per method. It contains sandbox placeholders only (e.g. `client_id: 'sb'`, `business_account: 'test@webkul.com'`). No real credentials.
- Config fields typed `password` for `client_secret` in `packages/Webkul/Admin/src/Config/system.php` (~lines 1121, 1146). Whether the value is encrypted at rest was **not verified**; a grep of `Core/src` found no encryption of config values. [U]

### 3.2 PayPal Smart Button (Orders v2)
Files: `Paypal/src/Payment/SmartButton.php`, `Paypal/src/Http/Controllers/SmartButtonController.php`, `Paypal/src/Http/routes.php`.

| Concern | Finding |
|---|---|
| Credentials / auth | `client_id` / `client_secret` from config. Builds `PaypalServerSdkClient` with `ClientCredentialsAuthCredentialsBuilder` and `Environment::SANDBOX` or `PRODUCTION` from a `sandbox` flag. OAuth token handling is inside the SDK [I]. |
| Create | `createOrder()` sends `intent: CAPTURE`, `purchase_units[0].amount` (value, `currency_code`, `breakdown`), items, payer. Header `prefer: return=representation`. A `paypalPartnerAttributionId` is sent (a Bagisto-specific partner id, which we must **not** reuse). |
| Checkout | JS Smart Button on the page; server only exposes `GET paypal/smart-button/create-order` and `POST .../capture-order`. No redirect URL on the server side (`getRedirectUrl()` is empty). Approval UI is client-side. |
| Capture and verification | Before capture it fetches the PayPal order, requires `status === 'APPROVED'`, exactly one purchase unit, and amount and currency equal to the cart total. After capture it fetches again and checks the captured amount. Good defensive pattern. |
| Status inquiry | `getOrder($orderId)`. |
| Webhook | **None** for Smart Button [V: routes list only contains create-order and capture-order]. Success depends on the browser calling capture-order. |
| Idempotency | If an order already exists for the cart, it returns success instead of creating another (`saveOrder`, lookup by `cart_id`). No PayPal-side `PayPal-Request-Id` header is used. [V by reading `SmartButton.php`; none present] |
| Refund | `Admin/src/Listeners/Refund.php::refundOrder` calls `getCaptureId()` then `refundOrder($captureId, {amount, currency_code})`. Takes the *first* capture only. Partial-refund bookkeeping is not shown. |
| Persistence | Generic `order_transactions` table (`transaction_id`, `status`, `type`, `amount decimal(12,4)`, `payment_method`, `data jsonb`, `invoice_id`, `order_id`). No unique constraint on `transaction_id`. Migration: `Sales/src/Database/Migrations/2021_03_11_212124_create_order_transactions_table.php`. |
| Errors | Exceptions' message is `json_decode`d and returned as the 400 body, which may leak provider error detail to clients. |

### 3.3 PayPal Standard (legacy IPN)
Files: `Paypal/src/Payment/Standard.php`, `Paypal/src/Helpers/Ipn.php`, `Paypal/src/Http/Controllers/StandardController.php`, enums `IpnStatus.php`, `PaypalPaymentStatus.php`.

| Concern | Finding |
|---|---|
| Config | `business_account` (receiver email) and `sandbox`. No API credentials needed. |
| Creation / checkout | Browser form POST to `https://www[.sandbox].paypal.com/cgi-bin/webscr` with `notify_url`, `return`, `cancel_return`, `invoice = cart id`. |
| Webhook (IPN) authenticity | **Post-back verification.** `Ipn::postBack()` re-posts the full payload with `cmd=_notify-validate` to `ipnpb[.sandbox].paypal.com` and accepts only a body exactly equal to `VERIFIED` (`Ipn.php:113-127`). Anything else, or a network error, means "not verified" and nothing changes. |
| Further checks | Ignores empty or `recurring_payment` `txn_type`; only `payment_status = Completed`; **receiver must equal the configured business account** (`paidToThisStore`); `mc_gross` and `mc_currency` must equal the order total, rounded using currency decimals. |
| Idempotency | Cache lock `paypal.standard.order.<cartId>` (30 s lock, 10 s wait), then "find existing order else place". `canInvoice()` guards against a second invoice and returns `ALREADY_SETTLED`. 500 is returned only on exceptions, so PayPal retries. |
| Statuses | `PaypalPaymentStatus` enum: Completed, Pending, Processed, Failed, Denied, Refunded, Reversed, Canceled_Reversal, Voided, Expired. Only `Completed` is acted on; other statuses are ignored. |
| Tests | `Paypal/tests/Feature/PaypalStandardIpnTest.php` covers unverified IPN, wrong receiver, wrong amount, wrong currency, non-Completed status, duplicate IPN, recurring. Useful as a *test-case checklist*. |

**Key limitation:** the PayPal webhooks that a modern gateway needs (`PAYMENT.CAPTURE.COMPLETED`, `CHECKOUT.ORDER.APPROVED`, `PAYMENT.CAPTURE.REFUNDED`, verification through PayPal's `verify-webhook-signature` or local cert-based verification) are **not implemented in any reference**. Their design must come from PayPal's official docs, which this task did not consult. [U]

### 3.4 Stripe (comparison only)
`Stripe/src/Http/Controllers/StripeController.php`, `Stripe/src/Payment/Stripe.php`, `Stripe/src/Routes/web.php`.
- Proper signed webhook: `Webhook::constructEvent(rawBody, 'Stripe-Signature', secret)`; returns 400 if the secret or signature is missing or invalid (`Stripe.php:200-216`, controller `webhook()`).
- Handles `checkout.session.completed`, `…async_payment_succeeded`, `charge.refunded`, `charge.dispute.*`, and answers 500 only when retry is wanted.
- Same cache-lock "settle once" approach. Metadata `cart_id` and `base_grand_total` are used to re-check the amount.
- Good model for the *shape* of a verified-webhook handler, though Stripe is not a target.

---

## 4. opensource3: Xendit Magento 2 module

Official Xendit plug-in, v14.x. As of its CHANGELOG 14.1.0, the legacy Invoice API flow was **removed**, and Payment Session is the only flow [V: `CHANGELOG.md`].

| Concern | Finding | Evidence |
|---|---|---|
| Config / credentials | Admin fields `public_key`, `private_key`, `test_*` variants, `validation_key`; stored with `Magento\Config\Model\Config\Backend\Encrypted` (encrypted at rest) and shown as `obscure` fields. Test vs live selected by `xendit_env`. Default gateway base URL `https://tpi-gateway.xendit.co` (a Xendit service, not the public `api.xendit.co`). | `etc/adminhtml/system.xml`, `etc/config.xml`, `Model/Payment/Xendit.php` |
| Auth / API request | HTTP Basic: `base64("<apiKey>:")`, JSON body, 30 s timeout, plugin-identifying headers. Requests and bodies are logged at info level, including billing address and customer data (not the key). | `Helper/Crypto.php::generateBasicAuth`, `Helper/ApiRequest.php` |
| Payment creation | `POST {base}/magento/checkout` with `idempotency_key` (the Magento order entity id; for multi-shipping `"42-43-44"`), `amount` (string), `currency`, `items`, `notification_url`, `success_return_url`, `cancel_return_url`, billing address, customer. Response must contain `payment_link_url` and `payment_session_id`; an `error_code` field means failure. | `Controller/Checkout/AbstractAction.php:~529-640`, `Helper/Data.php:502-505`, `Controller/Checkout/Invoice.php` |
| Checkout redirect | Redirect to `payment_link_url`; order set to `pending_payment`, session id saved in payment additional info. | `AbstractAction::applyPaymentSessionToOrders`, `Invoice.php` |
| Webhook | `POST xendit/checkout/integrationNotification`. CSRF disabled for this route. Required fields: `_id`, `signature`, `magento_checkout_idempotency_key`, `magento_checkout_type`. | `Controller/Checkout/IntegrationNotification.php` |
| Webhook authenticity | **ECDSA P-384 / SHA-384 signature** over a canonical string `{_id}.{status}.{session_id}.{session_status}.{token_id}.{token_status}.{pr_id}.{pr_status}.{refund_id}.{refund_status}?magento_checkout_idempotency_key=…&magento_checkout_type=…`, verified against **embedded public keys** per environment (staging/production), with several keys supported for rotation. Raw (r‖s, 96 bytes) signatures are converted to DER. Failure returns 401. | `IntegrationNotification.php::verifyPayloadSignature`, `Helper/SignatureVerifier.php`, `Helper/IntegrationNotificationKeys.php` |
| Statuses | `COMPLETED` → invoice and processing; `CANCELED`/`EXPIRED` → cancel order (skipped if already processing); other → acknowledged. | `IntegrationNotification.php:217-228` |
| Idempotency | Request side: `idempotency_key` sent to Xendit. Webhook side: `canInvoice()` makes a repeated COMPLETED a no-op; an already-processing order is not cancelled; a cancelled order is reverted to pending if COMPLETED arrives afterwards (out-of-order handling). No webhook-event dedup table and no locking. | same file, lines ~295-305, 375-388 |
| Amount verification | **Not performed** on the webhook. The signed string contains no amount, and the handler trusts the status. [V by reading] | `IntegrationNotification.php` |
| Refund | `Gateway/Http/Client/RefundClient::placeRequest` returns `['IGNORED'=>['IGNORED']]` and `RefundHandler::handle` does nothing, so refunds are **not** sent to Xendit by this module. The signature string includes `refund_id/refund_status`, which suggests refunds are handled on Xendit's side or by a separate flow. [U] | `Gateway/Http/Client/RefundClient.php`, `Model/Handler/RefundHandler.php` |
| Persistence | Only adds `sales_order.xendit_transaction_id` (varchar 255, indexed). Everything else is Magento's own order/payment tables. | `etc/db_schema.xml` |
| Status inquiry | None found in the files read. [U] | n/a |
| Tests | Unit tests for helpers/config only (`Test/Unit/*`). Nothing for the webhook. | `Test/Unit/` |

Key observation: this module talks to a **Xendit-operated middleman ("TPI Gateway") whose contract is not publicly documented in the repo**. Its request/response shape and signature scheme would not apply if we integrate with Xendit's public API (Invoice API v2 or Payments API), and there the callback auth is the `x-callback-token` header (opensource1). Which one we target is an open design decision (see `unified-api-design.md` §8).

---

## 5. Unverified items and missing information

1. Official Xendit and PayPal API docs were not consulted. Endpoints, field names, status values and webhook schemas for the Go adapters are **not** verified by this analysis.
2. Whether PayPal supports IDR (the sample request uses IDR). Bagisto's default PayPal `accepted_currencies` is `USD` [V], which hints at the constraint, but it is not proof. Must be checked against PayPal's docs.
3. Whether Bagisto encrypts `password`-type config at rest.
4. Whether `opensource1`'s callback returns 419 in practice.
5. The Xendit SDK's internals (`opensource1/vendor/`), not inspected.
6. Razorpay, PayU, PayGlocal and PhonePe packages in Bagisto: not analyzed.
7. Bagisto's remaining refund flows and the unseen parts of `Stripe.php`.
8. Actual `opensource3/` working-tree state vs `HEAD` (staged deletions).
