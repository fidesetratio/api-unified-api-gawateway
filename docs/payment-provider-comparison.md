# Payment Provider Comparison

Compares the three references on each integration concern. Evidence labels as in `reference-analysis.md`: **[V]** verified, **[I]** inferred, **[U]** unverified. "Ref1/2/3" = `opensource1/2/3`.

| Ref | Subject | Shape |
|---|---|---|
| Ref1 | Xendit Invoice API (public SDK) | Demo app, about 120 lines |
| Ref2 | PayPal Standard/IPN and Smart Button (plus Stripe etc.) | Plug-ins in a large platform, well tested |
| Ref3 | Xendit Payment Session via Xendit's TPI gateway | Official plug-in, signed webhooks |

## 1. Feature matrix

| Concern | Ref1 (Xendit Invoice) | Ref2 (PayPal) | Ref3 (Xendit Session) |
|---|---|---|---|
| Provider config | Env vars, single global key [V] | Platform config store per method and channel; sandbox flag; `password` fields [V] | Admin config, encrypted backend, test/live key pairs [V] |
| Auth to provider | SDK, secret key [I] | OAuth client credentials through SDK (Smart Button); none for Standard [V] | HTTP Basic with secret key [V] |
| Create payment | Create invoice [V] | Create Orders v2 order, or form POST (Standard) [V] | `POST /magento/checkout` creating a payment session [V] |
| Checkout URL | `invoice_url` redirect [V] | Client-side JS approval or form redirect [V] | `payment_link_url` redirect [V] |
| Status inquiry | No | `getOrder` (Smart Button) [V] | None found [U] |
| Webhook transport | `POST /payment/callback` | IPN `POST paypal/standard/ipn`; none for Smart Button [V] | `POST xendit/checkout/integrationNotification` [V] |
| Webhook authenticity | Static shared token header, plain `!==` [V] | IPN post-back to PayPal must return `VERIFIED` [V] | ECDSA P-384 signature against embedded public keys, key rotation [V] |
| Extra webhook checks | None | Receiver account, amount, currency, `Completed` only [V] | Required-field and type checks. **No amount check** [V] |
| Status model | Raw provider strings stored [V] | Provider enum, only `Completed` acted on [V] | COMPLETED / CANCELED / EXPIRED → order state [V] |
| Expired / failed | Not handled [V] | Ignored (Standard) | Expired and cancelled handled, including out-of-order revert [V] |
| Idempotency | None; stock decremented per callback [V] | Cache lock + "find existing order" + `canInvoice()` [V] | `idempotency_key` sent to provider; `canInvoice()` on webhook; no lock or event table [V] |
| Duplicate-payment prevention | None | Yes (per cart) | Partly (per order) |
| Refund | None | Smart Button refund via capture id [V] | Stub, no provider call [V] |
| Cancellation | None | Voided/Expired recognised, not acted on | Order cancel on provider CANCELED/EXPIRED [V] |
| Persistence | `orders` table with raw status columns, `invoice_id` unique [V] | Generic `order_transactions` with `data jsonb`, no unique `transaction_id` [V] | One column on `sales_order` [V] |
| Secrets handling | env only; callback logging may include provider error bodies [I] | Config store; at-rest encryption not verified [U] | Encrypted at rest; but request bodies with PII are logged at info [V] |
| Tests | None meaningful | Extensive Pest feature tests for webhook and amount paths [V] | Helper unit tests only [V] |
| License | Unclear (no LICENSE) | MIT | GPL-3.0 |

## 2. Similarities

- All three redirect the buyer to a hosted checkout page (or JS widget) and **rely on an asynchronous provider notification** to finalise payment.
- All map provider-specific payloads onto a host-system order state. None has a provider-independent transaction model, because each is a plug-in bound to one commerce system.
- Each separates "payment started" from "payment confirmed", using order state `pending`/`pending_payment`.

## 3. Differences that matter for a unified gateway

| Topic | Spread |
|---|---|
| Who owns the identifier we correlate on | Ref1: our `external_id`. Ref2 PayPal: `invoice` = cart id (IPN), PayPal order id (Smart Button). Ref3: our order id as `idempotency_key`, provider `payment_session_id`. The gateway needs both its own reference and the provider reference stored. |
| Authenticity mechanism | Shared secret header (Ref1), provider callback/post-back (Ref2), asymmetric signature (Ref3). The adapter interface must therefore be `Verify(request, config) -> verified event`, not "compare a token". |
| Source of truth for "paid" | Webhook-only (Ref1, Ref3), browser-return plus webhook (Ref2), browser capture only (Smart Button). Policy for our gateway: only a verified webhook or a verified server-to-server fetch may mark `paid` (CLAUDE.md §4.7). |
| Where credentials live | env (Ref1), platform config (Ref2), encrypted config (Ref3). Ours: PostgreSQL, encrypted. |
| Money | Ref1 `decimal(10,2)`, Ref2 `decimal(12,4)`, Ref3 string. Ours: NUMERIC plus an explicit currency exponent. |

## 4. Reusable design patterns (concepts only, no code copied)

1. **Provider interface with a code and a config accessor** (Ref2 `Payment` base class). In Go: an interface plus a registry keyed by provider type.
2. **Verify first, then act** with a typed outcome per webhook (Ref2 `IpnStatus`: `not_verified`, `ignored`, `wrong_receiver`, `amount_mismatch`, `order_settled`, `already_settled`, `error`). Adopt as the webhook processing result stored on an event row.
3. **Cross-check amount, currency and receiver against our own record**, never trusting the notification body (Ref2 `paidToThisStore`, `paymentCovers`; Ref2 Smart Button `approvalCoversCart`).
4. **Serialise settlement per transaction**. Ref2 uses a cache lock. In PostgreSQL use `SELECT … FOR UPDATE` on the transaction row, or a unique constraint on the webhook event id.
5. **Return 2xx for processed/ignored events and 5xx only for retryable failures** (Ref2 IPN and Stripe).
6. **Out-of-order tolerance** (Ref3: COMPLETED after CANCELED). Our state machine needs explicit allowed transitions, with `paid` able to override `expired`/`cancelled` only on verified evidence.
7. **Key rotation support** in signature verification (Ref3: multiple public keys). Mirror this by allowing several webhook secrets or keys per config.
8. **Test checklist** from Ref2's IPN tests: unverified, wrong receiver, wrong amount, wrong currency, non-final status, duplicate delivery, unknown reference.
9. **Pass an idempotency key to the provider** where supported (Ref3 does it; Ref2 does not). [Provider support for each of our targets must be confirmed in official docs. U]

## 5. Security weaknesses and gaps to investigate

| # | Ref | Issue | Level | Action |
|---|---|---|---|---|
| 1 | Ref1 | Callback token comparison with `!==`; passes if both secret and header are null | [V code, I impact] | Use constant-time compare; reject when the secret is empty. Test explicitly. |
| 2 | Ref1 | Callback body trusted: no amount or currency check, no event dedup, stock decremented per `PAID` | [V] | Never replicate. Verify amount and make state transitions idempotent. |
| 3 | Ref1 | `POST /payment/callback` likely blocked by CSRF middleware (no exemption in `bootstrap/app.php`) | [I] | Not applicable to Gin, but shows the need for an end-to-end webhook test. |
| 4 | Ref1 | `external_id = 'inv-'.time()` collides within one second | [V] | Gateway generates its own unique reference per transaction. |
| 5 | Ref2 | Smart Button has no webhook; confirmation depends on a browser call | [V] | A gateway must have a server-side webhook or poll. |
| 6 | Ref2 | Provider error messages returned to clients in 400 body | [V] | Return normalised error codes only, log the detail without secrets. |
| 7 | Ref2 | Refund uses first capture only; no `transaction_id` uniqueness | [V] | Add uniqueness and a refunds table. |
| 8 | Ref2 | IPN is a legacy mechanism and is not signature-based | [V] about the code, [U] about PayPal's current guidance | Use PayPal REST webhooks; verify the scheme from official docs. |
| 9 | Ref3 | Webhook trusts status without amount check | [V] | Compare against the stored transaction. |
| 10 | Ref3 | Request bodies with PII logged at info | [V] | Redact PII and credentials in our logs. |
| 11 | Ref3 | No webhook-event dedup or lock | [V] | Unique event table plus row lock. |
| 12 | Ref3 | Public keys hard-coded, so rotation requires a release | [V] | Keep verification material in DB config. |
| 13 | all | Refund flow is absent or weak in the Xendit references | [V] | Define refund semantics in a later phase. |

## 6. Which reference to use for which concern

| Concern | Best reference | Why |
|---|---|---|
| PayPal order create / capture / refund sequence | Ref2 `SmartButton.php`, `SmartButtonController.php`, `Refund.php` | Only PayPal REST usage; shows the amount-vs-order checks |
| PayPal webhook verification | **None.** Use PayPal docs. Ref2 `Helpers/Ipn.php` only for the check list (receiver, amount, currency, status) | No reference has REST webhook signature verification |
| Xendit create payment, basic flow | Ref1 `OrderController::store` | Simplest public-API example (Invoice) |
| Xendit webhook auth | Ref1 (callback token) for the public API; Ref3 for signature-style verification and key rotation | Depends on the Xendit product chosen |
| Webhook handler structure / outcomes | Ref2 `Ipn.php`, `IpnStatus`, and Stripe controller | Typed outcomes, retry semantics, locks |
| Status model & out-of-order handling | Ref3 `IntegrationNotification.php`; Ref2 `PaypalPaymentStatus` | Both define terminal and non-terminal states |
| Credential storage | Ref3 (`Encrypted` backend model) | Only reference that encrypts secrets at rest, verified |
| Test design | Ref2 `Paypal/tests/Feature/*` | Real negative-path coverage |
| Persistence | None adequate | Design ours from scratch (`database-design-proposal.md`) |

## 7. Missing information (not claimed)

- Official Xendit and PayPal API behavior (idempotency headers, webhook payloads, status vocabularies, currency support).
- Xendit TPI Gateway's public contract.
- Runtime behavior of any reference; nothing was executed.
