# Unified API Design (Proposal)

**Status: proposal for review. Nothing is implemented.** Phase 2 of `CLAUDE.md`. Items marked **[Decision]** need an answer before implementation. Provider API details are **[U]** until checked against official PayPal and Xendit docs.

## 1. Goals and constraints

- Go + Gin + PostgreSQL, base path `/api/v1.0`.
- One normalized request and response regardless of provider. Provider logic stays in adapters.
- Provider configuration lives in PostgreSQL, separate from transactions (CLAUDE.md §4.4).
- A payment is only `paid` on verified evidence. Idempotent. No secrets in responses or logs (§4.5–4.8).

## 2. Endpoints

### 2.1 `POST /api/v1.0/payments`

Request (as specified):

```json
{ "id": 5, "uniquecode": "ORDER-10001", "amount": "100000.00", "currency": "IDR" }
```

| Field | Type | Rules (proposed) |
|---|---|---|
| `id` | integer | `provider_configs.id`. Must exist, be active, and be permitted for the caller. |
| `uniquecode` | string | Caller's order reference. 1–64 chars, `[A-Za-z0-9._:-]`. Unique per caller (see §5). |
| `amount` | string | Decimal string, `> 0`, scale ≤ currency exponent. Never a JSON float. |
| `currency` | string | ISO-4217 uppercase; must be allowed by the config and the provider. |

Success `201 Created` (new) or `200 OK` (idempotent replay of an identical request):

```json
{
  "uniquecode": "ORDER-10001",
  "status": "pending",
  "amount": "100000.00",
  "currency": "IDR",
  "provider": "xendit",
  "checkout_url": "https://…",
  "expires_at": "2026-10-24T10:00:00Z",
  "created_at": "2026-10-23T10:00:00Z"
}
```

`provider` is derived from the config and is not an input. The provider's own reference is stored but shown only if needed. Never returned: credentials, webhook secrets, raw provider payloads.

Errors use one envelope `{ "error": { "code": "…", "message": "…" } }` with normalized codes:

| HTTP | `code` | When |
|---|---|---|
| 400 | `invalid_request` | Validation failure |
| 401 | `unauthorized` | Missing or bad gateway credential (see §8.1) |
| 403 | `config_not_permitted` | Config not available to this caller |
| 404 | `config_not_found` | No such, or inactive, `id` |
| 409 | `uniquecode_conflict` | Same `uniquecode`, different amount, currency or config |
| 422 | `currency_not_supported` | Provider or config cannot take this currency |
| 502 | `provider_error` | Provider rejected or failed; detail logged, not returned |
| 504 | `provider_timeout` | Timeout; transaction state `unknown`, see §5 |

### 2.2 `GET /api/v1.0/status/{uniquecode}`

Returns the normalized transaction: `uniquecode`, `status`, `amount`, `currency`, `provider`, `checkout_url` (while pending), `paid_at`, `expires_at`, `updated_at`, and for failures a normalized `failure_code`. Reads from the DB. Same auth as above and scoped to the caller. `404` if unknown. **[Decision]** whether a `pending` read may do a live provider lookup (reconciliation) or whether this is a separate internal job. Recommendation: DB-only for v1, plus a background reconciler later.

### 2.3 Webhooks

- `POST /api/v1.0/webhooks/paypal`
- `POST /api/v1.0/webhooks/xendit`

Unauthenticated by API credential; authenticated only by provider verification (§6). Always read the **raw body** for verification. Reply 2xx for verified and processed or ignored events, 4xx (401/400) for failed verification or malformed events, and 5xx only for retryable internal failures.

### 2.4 Not in the supplied contract, to decide later
Refund (`POST /payments/{uniquecode}/refunds`), cancel, list. Out of scope for the first provider milestone. Included in the schema design so we do not repaint later.

## 3. Normalized models

### 3.1 Status model

| Status | Meaning | Terminal |
|---|---|---|
| `pending` | Created at provider, awaiting payer | no |
| `paid` | Verified settled | no (may be refunded) |
| `failed` | Provider reported failure or denial | yes |
| `expired` | Provider or gateway expiry | yes\* |
| `cancelled` | Cancelled by payer or merchant before payment | yes\* |
| `partially_refunded` / `refunded` | After refund phase | `refunded` yes |

\* A verified `paid` event arriving after `expired`/`cancelled` is accepted (Ref3 documents this race), recorded in history and flagged for review. Every other transition from a terminal state is rejected and logged. `unknown` (provider call timed out) is an internal state to reconcile, not exposed as a final status. Shown to callers as `pending` or `processing`. **[Decision]**

Transition table is enforced in one function in the service layer, never in adapters.

### 3.2 Adapter interface (sketch, not code)

```
Provider
  Type() ProviderType
  CreatePayment(ctx, cfg, NormalizedRequest) -> {ProviderRef, CheckoutURL, ExpiresAt, RawStatus}
  GetStatus(ctx, cfg, ProviderRef)            -> {Status, Amount, Currency, ...}   // optional in v1
  VerifyWebhook(ctx, headers, rawBody, cfg)   -> VerifiedEvent | ErrNotVerified
  ParseEvent(VerifiedEvent)                   -> {EventID, ProviderRef, OurRef, Status, Amount, Currency, ReceiverID}
  Refund(...)                                 -> later phase
```

Adapters know HTTP, field names and signature schemes. They **never** write to the DB or decide transitions. They return normalized values, and the service layer applies rules.

## 4. Request flow (create)

1. Authenticate caller. Validate JSON.
2. Load config by `id`, check it is active and permitted, and that the currency is allowed.
3. In one DB transaction, insert the transaction row with status `pending_creation` (internal) using the unique key `(caller, uniquecode)`:
   - conflict and identical parameters: return the existing result (replay);
   - conflict and different parameters: `409`.
4. Call the adapter with a **gateway-generated reference** (not the caller's `uniquecode` as the sole key; Ref1's `time()` id shows the risk) and, where supported, a provider idempotency key derived from the transaction id.
5. Persist `provider_ref`, `checkout_url`, `expires_at`, status `pending`. Do not hold a DB lock during the HTTP call.
6. On timeout or unknown outcome, keep the row in the internal `unknown` state. A retry with the same `uniquecode` re-queries or retries with the same provider idempotency key and **never creates a second provider payment**.

## 5. Idempotency and duplicate prevention

- Uniqueness: `UNIQUE (client_id, uniquecode)` **[Decision: scope.]** If there is no client concept yet, a global `uniquecode` is the simplest but couples different merchants, so see §8.1.
- Same `uniquecode` + same fingerprint (config id, amount, currency) → replay. Different → `409`.
- Webhook dedup: `UNIQUE (provider, event_id)` in `webhook_events`; a duplicate delivery returns 2xx without reprocessing.
- Settlement serialised with `SELECT … FOR UPDATE` on the transaction row.
- Optional `Idempotency-Key` header is not required, because `uniquecode` already is the key. **[Decision]**

## 6. Webhook verification (all adapters)

Order is mandatory: (1) read raw body, (2) identify the candidate transaction/config **only to choose verification material**, (3) verify authenticity with that config's secret or provider verification, (4) only then persist the event as verified and process, (5) cross-check provider ref, amount, currency and receiver against the stored transaction, (6) apply the transition table, (7) respond.

Pre-verification lookups must not write to or alter any transaction. Unverified events may be recorded in `webhook_events` with `verified=false` for forensics (payload size-limited, secrets scrubbed).

| Provider | Verification (to confirm in official docs, **[U]**) | Reference evidence |
|---|---|---|
| Xendit (Invoice API) | `x-callback-token` header vs the config's stored token, constant-time, reject empty | Ref1 shows the header; Ref1's implementation has weaknesses (see comparison §5) |
| Xendit (Payment Session) | ECDSA signature over a canonical string, per Ref3 | Ref3 only, and the contract is not public in the repo |
| PayPal | REST webhook signature verification (headers `PAYPAL-TRANSMISSION-*`, `webhook_id` stored in config) | **No reference implements it**; Ref2 uses legacy IPN post-back |

## 7. Security rules

- Credentials encrypted at rest (AES-256-GCM, key from env/KMS, key-version column). Never serialised in API responses; a `redact` layer for logs; request/response logging allow-lists, not deny-lists (Ref3 logged PII).
- Outbound provider calls: timeouts, TLS verification, response size limit. Fixed provider base URLs per environment, not caller supplied.
- Webhook body size limit. Strict JSON decoding. No `Content-Type` trust for verification.
- API rate limiting and structured audit log of status changes.
- Return/redirect URLs come from the config or the server, never from the request (open-redirect and phishing risk) **[Decision]**.

## 8. Design issues to resolve before implementation

1. **Authentication and authorization for the gateway API.** The contract has none. With `id` selecting a config, anyone who can call the API could charge any merchant's provider account or read statuses. Needed: API clients (key or JWT), and a mapping of clients to permitted configs. This also defines the scope of `uniquecode` uniqueness.
2. **Which Xendit product.** Public Invoice API (Ref1; `x-callback-token`) vs. Payment Session through Xendit's TPI gateway (Ref3; ECDSA, contract not public). Recommendation: Invoice API (or Xendit's documented Payments API) as the basis, subject to docs review. Ref3 is a Magento-specific path.
3. **Which PayPal API.** Orders v2 with REST webhooks is recommended. Legacy IPN/Standard is not suited to a JSON gateway. Redirect flow: create order → `approve` link as `checkout_url` → capture after approval (by webhook `CHECKOUT.ORDER.APPROVED` then capture, or `intent=CAPTURE` auto-complete). **[U]** details.
4. **Currency support.** The sample uses IDR; PayPal's support for IDR is **[U]** (Bagisto defaults to USD only). Config needs an allowed-currency list, and the API needs `422 currency_not_supported`. A conversion layer is out of scope.
5. **Webhook-to-config resolution.** Endpoints are per provider, but verification material is per config (several PayPal `webhook_id`s, several Xendit tokens). Options: (a) resolve via the provider reference in the payload → transaction → config (recommended; no extra URL); (b) per-config URLs `/webhooks/xendit/{config_id}` (simpler and safer, but deviates from the stated contract). Need a decision. With (a), a payload for an unknown reference gets 2xx-ignored or 404 without state change.
6. **Amount semantics.** Currency exponents (IDR 0 decimals vs the sample `"100000.00"`), rounding, minimum and maximum, and that `amount` is the gross total. Adapters must convert the string to each provider's format without float.
7. **Expiry.** Who sets it: config default or request? Xendit invoices have a duration (Ref1 uses 86400 s). PayPal orders expire on their own schedule **[U]**. Gateway-side expiry sweeper needed, or rely on provider events.
8. **Pending-creation recovery.** Reconciler for rows stuck in the internal `unknown` state.
9. **Raw payload retention** and PII: how long, and whether to store payloads at all.
10. **Refunds and partial payments** in v1 or later (schema ready, API later).
11. **Environment handling** (sandbox vs production): a field on `provider_configs`; fixed base URLs per environment.
12. **Document set:** CLAUDE.md §7 names `database-design.md`, `provider-integration.md`, `testing-guide.md`, while Task 001 asks for `database-design-proposal.md` and `implementation-plan.md`. Confirm the final set (see `implementation-plan.md`).
