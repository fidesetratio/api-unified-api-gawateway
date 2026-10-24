# Provider Integration

## The adapter contract (`internal/provider`)

```
Name() string                                  // value of provider_configs.provider
CreatePayment(ctx, CreateRequest) (CreateResult, error)
GetStatus(ctx, StatusRequest) (StatusResult, error)
VerifyWebhook(ctx, WebhookRequest) (Event, error)   // must authenticate before returning an event
```

- `CreateRequest`: `Reference` (the gateway transaction id), `IdempotencyKey` (stable across retries), `Amount` (decimal string plus currency, never a float), `Credentials` (redacting `secrets.Secret` values), `Settings`.
- `CreateResult`: `ProviderRef`, `PaymentURL` (what becomes `payment_url`), optional `ExpiresAt`, `RawStatus`.
- Errors: `provider.ErrTimeout` (outcome unknown, the payment may exist), `*provider.Error` (definite rejection), `provider.ErrNotVerified` (webhook authenticity not established), `provider.ErrNotImplemented`.
- Adapters know HTTP and signatures. They never touch the database, never decide transitions and never log credentials.

`service.NewRegistry(allowFake)` registers the Fake Provider (only outside production), the **PayPal adapter** (`internal/provider/paypal`, see `paypal-research.md`) the **Xendit adapter** (`internal/provider/xendit`, Payment Sessions, see `xendit-research.md`) the **Stripe adapter** (`internal/provider/stripe`, Checkout Sessions, see `stripe-research.md`) and the **Midtrans adapter** (`internal/provider/midtrans`, Snap, IDR only, see `midtrans-research.md`); all are described in `phase4-decisions.md`. `provider.Stub` remains for any provider without an adapter and fails every call with `ErrNotImplemented` (`502 provider_error`).

Optional adapter capabilities (`provider.Capturer`, `Locator`, `AmountValidator`) let the generic code handle two-step providers: after a verified approval the gateway captures; webhooks arrive at `POST /api/v1.0/webhooks/{provider}` and are attributed to a config by the provider reference in the payload before being verified with that config's secret.

**PayPal, Xendit, Stripe and Midtrans status:** adapters, flows and tests pass against in-process stand-ins (`paypaltest`, `xendittest`, `stripetest`, `midtranstest`) and a real database. Nothing has been tried against a real PayPal sandbox or Xendit, Stripe or Midtrans test account.

## Fake Provider (`internal/provider/fake`)

Deterministic and in memory. Behaviour comes from the config `settings`: `{"behavior":"success"}` (default), `"timeout"`, `"reject"`, `"slow"` with `delay_ms`. The same idempotency key always returns the same payment, like a real provider honouring the key. Webhooks use header `X-Fake-Token` compared in constant time with the config's webhook secret `token`; an empty secret never authenticates. Payment URL form: `https://fake.invalid/pay/FAKE-<hash>`.

To try it: create a config with `"provider":"fake"` through the admin API.

> PayPal's official documentation has now been reviewed: see `paypal-research.md` (verified facts, gaps, and what they mean for the adapter). The Xendit review is in `xendit-research.md`. The "facts to verify" list below is superseded for both.

## Phase 4 plan (unchanged: PayPal first, then Xendit)

Each adapter must be built from the provider's **official documentation**, which has not yet been consulted. Facts to verify before coding, none of which any reference repository proves:

- **PayPal**: Orders v2 create/capture flow and the exact approval link relation for `payment_url` (the example `https://www.sandbox.paypal.com/checkoutnow?token=<order id>` is the user's sample, not verified); REST webhook signature verification and the `webhook_id` it needs; `PayPal-Request-Id` as the idempotency key; event types and statuses to map; **which currencies are supported (the sample request uses IDR)**; refund endpoints.
- **Xendit**: which product (Invoice API with `x-callback-token`, or Payments); status vocabulary; expiry; idempotency header support.
- Both: how a webhook finds its config, behaviour on retries and out-of-order events.

Acceptance for each adapter: the checklist in `testing-guide.md` (unverified, wrong amount/currency, duplicate, unknown reference, out-of-order, concurrent) against recorded fixtures or a stub server, then a sandbox smoke test with the user's own credentials, never committed.
