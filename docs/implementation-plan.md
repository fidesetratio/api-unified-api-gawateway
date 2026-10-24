# Implementation Plan (Proposal)

**Status: proposal. No implementation until the analysis and architecture are approved** (CLAUDE.md §6). Phases follow CLAUDE.md §5; one provider at a time.

## 0. Gate: decisions needed before Phase 3

From `unified-api-design.md` §8; the top blockers are:

1. Gateway API authentication and client-to-config authorization (and `uniquecode` scope).
2. Which Xendit product (Invoice API vs Payment Session) and where its official docs are the source of truth.
3. PayPal approach (Orders v2 + REST webhooks) and the IDR/currency question.
4. Webhook-to-config resolution strategy (payload lookup vs per-config URL).
5. Final documentation file set (CLAUDE.md §7 vs Task 001 names).
6. Migration tool, config loading, logging and test libraries. Each new dependency needs approval, and none are installed now.
7. Confirm the `opensource3/` empty working tree is acceptable (staged deletions, 1.3 of the analysis).

## 1. Recommended order

| Phase | Work | Output / exit criteria |
|---|---|---|
| 1 | Reference analysis (**done**, pending review) | `reference-analysis.md`, `payment-provider-comparison.md` |
| 2 | API contract + DB design (**drafted**, pending approval) | `unified-api-design.md`, `database-design-proposal.md`; decisions in §0 closed |
| 2b | Read official PayPal and Xendit docs; record verified facts in `provider-integration.md` | Each provider assumption in these docs becomes verified or removed |
| 3 | Go foundation in `result/`: module layout, config, Postgres connection and migrations, domain model + state machine, config repository with encryption, API auth, error envelope, logging redaction, health endpoint. **No providers yet**; a fake adapter for tests. | Unit tests for the state machine, money parsing and encryption; migrations apply on a clean DB |
| 4a | **PayPal adapter first** (OAuth, create order, approval link as `checkout_url`, capture, REST webhook verification). Endpoints: create, status, webhook. | Adapter tests against recorded fixtures or a stub server; negative-path webhook tests (below); sandbox smoke test with the user's own sandbox credentials, not committed |
| 4b | **Xendit adapter second** (create invoice, callback-token or signature webhook) | Same test set, run against the already-proven generic core |
| 5 | Integration tests (Postgres, concurrent webhooks, replay), security checks, docs (`testing-guide.md`, `provider-integration.md`, `database-design.md`); optional refunds and reconciler | Checklist in §3 passes |

Order: PayPal first, Xendit second (user decision). PayPal is the harder provider: OAuth, a multi-step approve/capture lifecycle, and webhook verification that **no reference implements**. Building it first forces the generic core (state machine, webhook event log, config-to-webhook resolution) to handle the harder case, so Xendit should then be a thinner adapter. Consequences: Phase 2b starts with PayPal's official docs (Orders v2, webhook verification, `PayPal-Request-Id` idempotency, currency support and the IDR question). The Xendit docs review can wait until Phase 4b. The first milestone's sample request needs a PayPal-supported currency until the IDR question is answered.

## 2. Proposed `result/` layout (for review, not created)

```
result/
  cmd/gateway/            main
  internal/
    api/                  Gin handlers, DTOs, middleware (auth, request id, recover)
    domain/               Transaction, Status, transitions, Money
    service/              CreatePayment, ProcessWebhook, GetStatus
    provider/             Provider interface + registry
      paypal/             adapter
      xendit/             adapter
    store/                Postgres repositories (configs, transactions, events)
    secrets/              encryption of provider credentials
    config/               process configuration (env)
  migrations/
  docs/
```

Rules: adapters import `domain` only; `service` depends on interfaces; handlers contain no business rules.

## 3. Mandatory test checklist

Taken from Ref2's PayPal IPN tests and extended for the gateway. Applies to every adapter.

**Create**
- valid request → `201` with `checkout_url`; the secret never appears in the response or logs
- same `uniquecode` + same params → replay `200`, one provider call
- same `uniquecode` + different amount → `409`
- unknown/inactive/unauthorized config → `404`/`403`
- unsupported currency → `422`
- provider timeout → no second provider payment on retry
- amount string edge cases: `"0"`, negative, too many decimals, JSON number instead of string

**Webhook**
- missing, invalid or tampered signature/token → `401`, status unchanged, event recorded `verified=false`
- empty configured secret never authenticates (Ref1 weakness)
- valid event → `paid` only for matching reference, amount, currency and receiver
- amount or currency mismatch → no transition, outcome `amount_mismatch`
- unknown reference → no state change
- duplicate delivery → 2xx, one transition
- concurrent duplicate deliveries → exactly one transition (race test)
- `expired` then verified `paid` → accepted and flagged; `paid` then `expired` → rejected
- non-final provider statuses (pending, authorised) → no `paid`
- internal error → 5xx so the provider retries

**Security**
- no secret in any response, error body or log line (golden-file check on logs)
- encrypted columns contain no plaintext
- request body size limits; webhook rejects non-JSON when JSON is required

## 4. Risks

| Risk | Mitigation |
|---|---|
| Provider facts taken from references that are plug-ins, not API docs | Phase 2b doc review before adapters |
| Xendit TPI contract is undocumented in the repo | Do not build on Ref3's endpoint; use public API |
| GPL-3.0 contamination (Ref3) / unclear license (Ref1) | No code copying; re-derive from official docs |
| Webhook fails silently in production (cf. Ref1's likely CSRF block) | End-to-end webhook test; startup config validation |
| Money mishandling | Decimal-string parsing, no floats, currency exponent table |
| Single point of key compromise | Envelope encryption + key versioning; key kept outside the DB |

## 5. Next step

Stop here. Waiting for review of the five documents and the decisions in §0. After approval, Phase 2b (official PayPal docs review first, Xendit later) comes before any code.
