# Postman collection

Files: `Unified-Payment-Gateway.postman_collection.json` and `Local-Dev.postman_environment.json`. In Postman: **Import** both files, then choose the environment "Unified Gateway - Local Dev" (top right).

## 1. Start the gateway (Windows, from `result/`)

```
set GATEWAY_DATABASE_URL=postgres://postgres:PASSWORD@127.0.0.1:5432/gateway_dev?sslmode=disable
set GATEWAY_ENV=dev
set GATEWAY_MASTER_KEYS=dev1:ZGV2LW9ubHktbWFzdGVyLWtleS0zMi1ieXRlcy0hISE=
set GATEWAY_ACTIVE_KEY_ID=dev1
set GATEWAYCTL_OPERATOR=yourname
build.bat build
bin\gatewayctl.exe migrate up
run.bat
```

(`@` inside the password is written `%40`. The master key above is for local development only.) `GATEWAY_ENV=dev` lets the admin routes work over plain HTTP; any other environment requires HTTPS for `/admin`.

## 2. Create two API keys (once)

```
bin\gatewayctl.exe client create --name admin1 --role admin
bin\gatewayctl.exe client create --name shop1  --role caller
bin\gatewayctl.exe client list
```

Each `create` prints its key **once**: copy it. `client list` shows the numeric id of `shop1`.

## 3. Fill the environment

| Variable | Value |
|---|---|
| `baseUrl` | `http://localhost:8080` (or the `GATEWAY_ADDR` you used) |
| `adminKey` | the key printed for `admin1` |
| `callerKey` | the key printed for `shop1` |
| `clientId` | the numeric id of `shop1` from `client list` |

## 4. Run the folders in order

1. **Health**: server and database are up.
2. **Admin - fake provider**: creates a config with the Fake Provider (saves `configId`), grants `shop1` access, lists, rotates and disables/enables it.
3. **Payments**: creates a payment (`201`, exactly `uniquecode`, `status`, `payment_url`), repeats it (`200`, same body), then a conflict (`409`) and a status lookup.
4. **Admin - PayPal sandbox config**: edit the body with your own PayPal sandbox `client_id`, `client_secret`, `webhook_id` and your return/cancel URLs. PayPal does not accept IDR, so use USD. Creating a payment here calls the real PayPal sandbox and returns a PayPal `payment_url` to open in a browser. For PayPal to reach your webhook, `https://<public host>/api/v1.0/webhooks/paypal` must be reachable from the internet (for example through a tunnel) and registered as the listener URL in your PayPal app.
5. **Error cases**: each request should be rejected; the tests assert the exact error codes.
6. **Xendit (Payment Sessions)**: edit the body with your Xendit TEST secret key and, optionally, the callback token from the dashboard. The sample IDR request (`100000.00`) works with Xendit. Register `https://<public host>/api/v1.0/webhooks/xendit` for the session events in the Xendit dashboard.
7. **Stripe (Checkout Sessions)**: edit the body with your Stripe TEST secret key (`sk_test_…`) and the endpoint signing secret (`whsec_…`). Register `https://<public host>/api/v1.0/webhooks/stripe` in the Stripe dashboard for the four `checkout.session.*` events listed in the folder description, and pay with the test card `4242 4242 4242 4242`. Currencies: USD EUR GBP CAD AUD NZD SGD CHF HKD SEK NOK DKK MYR JPY KRW.
8. **Midtrans (Snap)**: edit the body with your Midtrans SANDBOX Server Key. IDR only. Set the Payment Notification URL in the Midtrans dashboard to `https://<public host>/api/v1.0/webhooks/midtrans`; there is no separate webhook secret.

9. **PayPal sandbox - step by step**: the scenario that was run end to end. Order: `8.1` create config (put your sandbox `client_id` / `client_secret` in the body; `return_url` points at the gateway, `{reference}` is filled with the payment id), `8.2` grant (admin key), `8.3` submit payment (caller key), then open the `payment_url` from the response (also printed in the Postman Console) in a browser, log in with a PayPal sandbox buyer and approve. The browser returns to `GET /api/v1.0/return?ref=...`, which asks PayPal for the real status, captures the order and shows an HTML result page (send `Accept: application/json` to get `{"status":"paid"}` instead); `8.4` get status shows `PAID`. The folder stores its ids in `ppConfigId`, `ppCode` and `ppPaymentUrl`, so it does not clash with the other folders.

Every request has Postman tests; open the **Test Results** tab after sending. You can also use **Run collection** on one folder. Secrets you send (`client_secret`, `webhook_id`) are never returned by any endpoint: the tests check that.

## Notes

- The Fake Provider never contacts any external service; its `payment_url` (`https://fake.invalid/...`) is not a real page. To see a payment become `PAID` with the fake provider you would deliver a fake webhook, which has no HTTP route yet; use PayPal sandbox for a full flow.
- Status values: `PENDING`, `PAID`, `FAILED`, `EXPIRED`, `CANCELLED`.
- The PayPal, Xendit, Stripe and Midtrans parts have not yet been run against real test accounts (see the last section of each `*-research.md` in `result/docs/`).
