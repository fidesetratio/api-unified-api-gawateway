# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem. Report it privately
through GitHub's "Report a vulnerability" button on the Security tab of this
repository (private vulnerability reporting), and include steps to reproduce.

## What never belongs in this repository

- Provider credentials (PayPal, Xendit, Stripe, Midtrans), webhook secrets, API keys
- `.env`, `master.key`, database dumps, logs

Both `.env` and `master.key` are git-ignored. If a secret was ever committed,
rotate it at the provider first; removing it from history is not enough.

## Design guarantees worth preserving

- A payment becomes `paid` only on provider evidence: a verified webhook, or a
  status that the gateway fetched itself from the provider's API. URL parameters
  and browser redirects are never evidence.
- Provider credentials are stored encrypted and are never returned by the API
  or written to logs.
