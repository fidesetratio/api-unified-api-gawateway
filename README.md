# Unified Payment Gateway

One API for several payment providers. Your shop talks to this gateway; the gateway talks to PayPal, Xendit, Stripe or Midtrans and keeps a single, normalized view of every payment.

Written in Go with Gin and PostgreSQL. Indonesian version of the usage guide: [README.id.md](README.id.md).

## Status

| Provider | State |
|---|---|
| PayPal (Orders v2) | Run end to end against the PayPal **sandbox**: create, approve, return, capture, `PAID` |
| Xendit (Payment Sessions), Stripe (Checkout), Midtrans (Snap) | Implemented and tested against in-process stand-ins; **not yet run against the real sandboxes** |
| Fake provider | Development only; disabled when `GATEWAY_ENV=production` |

## How it works

```
shop ──POST /payments──▶ gateway ──create──▶ provider
shop ◀── payment_url ────┘
buyer ──pays on provider's page──▶ provider ──redirect──▶ gateway /return ──asks provider──▶ PAID
                                      └──signed webhook──▶ gateway /webhooks/{provider}   (safety net)
shop ──GET /status/{uniquecode}──▶ gateway
```

Rules the gateway enforces:

- A payment becomes `PAID` only on provider evidence: a **verified webhook**, or a status the gateway fetched itself from the provider's API using its own credentials. The data in a redirect URL is never trusted.
- Amount and currency must equal what was stored, or the payment is not marked paid and a review item is opened.
- Creating the same payment twice (same `uniquecode`) never creates a second provider payment.
- Provider credentials are stored encrypted (AES-GCM) and never appear in API responses or logs.

## Requirements

- Go (version in [go.mod](go.mod))
- PostgreSQL (a recent version; the CI uses 16)
- Windows scripts (`*.bat`) are provided for convenience; every step also has a plain `go` command.

## Quick start

1. **Configure the database.** Copy `.env.example` to `.env` and fill in the five values:

   ```
   DB_HOST=127.0.0.1
   DB_PORT=5432
   DB_USER=postgres
   DB_PASSWORD=your-password
   DB_NAME=gateway_dev
   ```

   Special characters in the password need no escaping. The `.env` file is looked up in the working directory, next to the executable, then in its parent directory.

2. **Build** (runs vet and tests first; `build.bat build` skips the tests):

   ```
   build.bat
   ```

   This creates `bin\gateway.exe`, `bin\gatewayctl.exe`, `bin\migrate.exe` and refreshes `schema.sql`.
   Without the script: `go build -o bin/ ./cmd/...`

3. **Create the database and tables.** The database is created if it does not exist; running it again is safe.

   ```
   bin\migrate.exe          (or: migrate.bat / go run ./cmd/migrate)
   ```

   Prefer plain SQL? Create an empty database, then run [schema.sql](schema.sql) with `psql`:
   `psql -v ON_ERROR_STOP=1 -1 -d gateway_dev -f schema.sql`

4. **Run the gateway.**

   ```
   bin\gateway.exe          (or: run.bat / go run ./cmd/gateway)
   ```

   It listens on `:8080`. On first start in dev it generates `master.key` (the key that encrypts provider credentials). Keep that file: without it stored credentials cannot be decrypted. Check it: `curl http://localhost:8080/healthz` and `/readyz`.

## Using the API

All routes are under `/api/v1.0`. Keys are sent as `Authorization: Bearer <key>`. There are two kinds of client:

| Role | Can do |
|---|---|
| `admin` | Manage provider configs and grants (`/admin/...`) |
| `caller` | Create payments and read their status (`/payments`, `/status`) |

An admin key is rejected on `/payments` and a caller key on `/admin` (403).

### 1. Create the two API keys (once)

```
bin\gatewayctl.exe client create --name admin1 --role admin  --operator yourname
bin\gatewayctl.exe client create --name shop1  --role caller --operator yourname
bin\gatewayctl.exe client list
```

Each key is printed **once**; store it. `client list` shows the numeric id of `shop1`. Lost a key? `client rotate-key --name shop1 --operator yourname`.

### 2. Register a provider (admin key)

```
POST /api/v1.0/admin/configs
{
  "provider": "paypal",
  "environment": "sandbox",
  "name": "paypal sandbox",
  "allowed_currencies": ["USD"],
  "credentials": { "client_id": "<sandbox client id>", "client_secret": "<sandbox secret>" },
  "settings": {
    "return_url": "http://localhost:8080/api/v1.0/return?ref={reference}",
    "cancel_url": "http://localhost:8080/cancel"
  }
}
```

The response contains the config `id` and never the credentials. `{reference}` in `return_url` / `cancel_url` is replaced by the payment's id when a payment is created, so the buyer comes back to the gateway's own return page.

### 3. Let the shop use it (admin key)

```
PUT /api/v1.0/admin/configs/{configId}/grants/{shopClientId}      → 204
```

### 4. Create a payment (caller key)

```
POST /api/v1.0/payments
{ "id": <configId>, "uniquecode": "ORDER-1001", "amount": "10.00", "currency": "USD" }
```

`amount` is a string. The answer is `201 {"uniquecode":"ORDER-1001","status":"PENDING","payment_url":"https://..."}` (`200` with the same body if you repeat the identical request, `409` if the same `uniquecode` comes with a different amount).

### 5. The buyer pays

Send the buyer to `payment_url`. After paying, the provider redirects the browser to your `return_url`. With the gateway's return endpoint the gateway asks the provider for the real status, captures the order where the provider needs it (PayPal) and shows a result page. A client that sends `Accept: application/json` gets `{"uniquecode":"...","status":"paid"}` instead of HTML.

### 6. Read the status (caller key)

```
GET /api/v1.0/status/ORDER-1001   →  {"uniquecode":"ORDER-1001","status":"PAID","payment_url":"https://..."}
```

Statuses: `PENDING`, `PAID`, `FAILED`, `EXPIRED`, `CANCELLED`.

### Endpoints

| Endpoint | Auth | Purpose |
|---|---|---|
| `POST /payments` | caller | Create a payment |
| `GET /status/{uniquecode}` | caller | Status of the caller's own payment |
| `GET /return?ref={id}` | none | Buyer landing page; verifies with the provider |
| `POST /webhooks/{provider}` | provider signature | Provider notifications (verified before anything changes) |
| `/admin/configs...` | admin, HTTPS (HTTP allowed in dev) | Create / list / show / patch configs, rotate credentials, grant / revoke |
| `GET /healthz`, `GET /readyz` | none | Liveness / database readiness |

### Provider notes

| Provider | Currencies | Return URL |
|---|---|---|
| PayPal | USD and other PayPal currencies (not IDR) | http or https |
| Xendit | IDR; other currencies need a `country` setting | **https only** (use a public host or HTTPS tunnel) |
| Stripe | USD EUR GBP CAD AUD NZD SGD CHF HKD SEK NOK DKK MYR JPY KRW | used as `success_url` |
| Midtrans | IDR | used as the Snap `finish` callback |

Webhooks are an optional safety net for buyers who close the browser before returning. They need a URL the provider can reach (a tunnel such as ngrok in development) and the provider's webhook id or secret stored with the config. Register `https://<public host>/api/v1.0/webhooks/<provider>` in the provider's dashboard.

## Configuration

Only the five `DB_*` values are needed to run locally. Everything else has a default and can be overridden with environment variables, which win over `.env`.

| Variable | Default | Meaning |
|---|---|---|
| `DB_HOST`, `DB_PORT`, `DB_USER`, `DB_PASSWORD`, `DB_NAME` | none | PostgreSQL connection (from `.env`) |
| `GATEWAY_ENV` | `dev` | `dev`, `test` or `production` |
| `GATEWAY_ADDR` | `:8080` | HTTP listen address |
| `GATEWAY_DATABASE_URL` | built from `DB_*` | Full connection URL, if you prefer one |
| `GATEWAY_MASTER_KEYS`, `GATEWAY_ACTIVE_KEY_ID` | `master.key` (non-production) | Encryption keys, format `id:base64(32 bytes)` |
| `GATEWAY_AUTO_MIGRATE` | on outside production | Set to `0` to stop the gateway from migrating at start |
| `GATEWAYCTL_OPERATOR` | none | Operator name recorded in the audit log by `gatewayctl` |

### Production

- Set `GATEWAY_ENV=production`, and provide `GATEWAY_MASTER_KEYS` and `GATEWAY_ACTIVE_KEY_ID` explicitly. No key file is generated and the Fake provider is disabled.
- Migrate with `gatewayctl migrate up` (or `migrate`) as a separate step; the server does not migrate itself in production.
- The admin API only accepts HTTPS. Put the gateway behind a TLS-terminating proxy.
- Use sandbox credentials until you have tested each provider yourself.

## Operator CLI

`bin\gatewayctl.exe` (changes need `--operator NAME` or `GATEWAYCTL_OPERATOR`):

```
migrate up | status
client create --name N --role caller|admin
client list | disable | enable | rotate-key --name N
key reencrypt            re-encrypt credentials under the active master key
review list | resolve --id N --resolution "why"
reconcile run            expire due payments, finish stuck ones
```

## Postman

Import both files from [postman/](postman/) and pick the environment **Unified Gateway - Local Dev**; fill `adminKey`, `callerKey` and `clientId`. Folder **8** walks through the PayPal sandbox scenario step by step. See [postman/README.md](postman/README.md).

## Testing

```
go vet ./...
go test ./...
```

Database tests need `GATEWAY_TEST_DATABASE_URL` (a dedicated database such as `gateway_test`; every test works in a throwaway schema and drops it). Without it they are skipped. Details: [docs/testing-guide.md](docs/testing-guide.md).

## Project layout

```
cmd/gateway      HTTP server (+ background expiry and reconciliation worker)
cmd/gatewayctl   operator CLI
cmd/migrate      create database + apply migrations (+ -sql to print one script)
internal/        api, service, store, provider (paypal, xendit, stripe, midtrans, fake), domain, secrets, auth, config, worker, cli
migrations/      numbered SQL files (embedded in the binaries)
schema.sql       all migrations as one script, generated by `migrate -sql`
docs/            design, provider research and test documentation
postman/         collection and environment
```

## Documentation

[unified-api-design](docs/unified-api-design.md) · [database-design](docs/database-design.md) · [provider-integration](docs/provider-integration.md) · [testing-guide](docs/testing-guide.md) · [reference-analysis](docs/reference-analysis.md) · provider research: [PayPal](docs/paypal-research.md), [Xendit](docs/xendit-research.md), [Stripe](docs/stripe-research.md), [Midtrans](docs/midtrans-research.md)

## Security

See [SECURITY.md](SECURITY.md). Never commit `.env`, `master.key` or provider credentials.

## License

No license has been chosen yet, so all rights are reserved by the author.
