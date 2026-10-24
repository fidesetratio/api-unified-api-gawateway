# Unified Payment Gateway (Panduan Pemakaian)

Satu API untuk beberapa penyedia pembayaran. Toko Anda hanya bicara ke gateway ini; gateway yang bicara ke PayPal, Xendit, Stripe, atau Midtrans. Dokumentasi lengkap (bahasa Inggris) ada di [README.md](README.md).

Dibuat dengan Go, Gin, dan PostgreSQL.

## Status

- **PayPal:** sudah dijalankan dari awal sampai `PAID` di **sandbox** PayPal.
- **Xendit, Stripe, Midtrans:** sudah dibuat dan dites dengan stand-in, **belum** dicoba di sandbox aslinya.

## Aturan penting

- Pembayaran menjadi `PAID` hanya dengan bukti dari provider: webhook yang terverifikasi, atau status yang diambil gateway sendiri dari API provider. Parameter di URL redirect tidak pernah dipercaya.
- Jumlah dan mata uang harus sama dengan yang tersimpan. Kalau berbeda, pembayaran tidak ditandai lunas.
- Kredensial provider disimpan terenkripsi dan tidak pernah muncul di respons API atau log.

## Menjalankan dari nol

1. **Isi `.env`.** Salin `.env.example` menjadi `.env`, lalu isi koneksi PostgreSQL:

   ```
   DB_HOST=127.0.0.1
   DB_PORT=5432
   DB_USER=postgres
   DB_PASSWORD=password-anda
   DB_NAME=gateway_dev
   ```

2. **Build:** `build.bat` (menjalankan vet dan tes dulu; `build.bat build` untuk melewati tes). Hasilnya `bin\gateway.exe`, `bin\gatewayctl.exe`, `bin\migrate.exe`, dan `schema.sql`.
   Tanpa script: `go build -o bin/ ./cmd/...`

3. **Buat database dan tabel:** `bin\migrate.exe`. Database dibuat bila belum ada. Aman dijalankan ulang.
   Lebih suka SQL biasa? Buat database kosong, lalu `psql -v ON_ERROR_STOP=1 -1 -d gateway_dev -f schema.sql`.

4. **Jalankan gateway:** `bin\gateway.exe`. Berjalan di `:8080`. Saat pertama kali, file `master.key` dibuat otomatis. **Simpan file itu**; tanpanya kredensial yang tersimpan tidak bisa dibuka lagi.
   Cek: `http://localhost:8080/healthz`.

## Memakai API

Semua rute di bawah `/api/v1.0`, dengan header `Authorization: Bearer <key>`. Ada dua jenis client: **admin** (mengatur config) dan **caller** (membuat pembayaran dan membaca status). Key admin ditolak di `/payments`, key caller ditolak di `/admin`.

1. **Buat dua key** (sekali saja). Key hanya tampil sekali.
   ```
   bin\gatewayctl.exe client create --name admin1 --role admin  --operator namaanda
   bin\gatewayctl.exe client create --name shop1  --role caller --operator namaanda
   bin\gatewayctl.exe client list
   ```
2. **Daftarkan provider** (key admin) lewat `POST /admin/configs`. Untuk PayPal sandbox:
   ```json
   {
     "provider": "paypal", "environment": "sandbox", "name": "paypal sandbox",
     "allowed_currencies": ["USD"],
     "credentials": { "client_id": "<client id sandbox>", "client_secret": "<secret sandbox>" },
     "settings": {
       "return_url": "http://localhost:8080/api/v1.0/return?ref={reference}",
       "cancel_url": "http://localhost:8080/cancel"
     }
   }
   ```
   `{reference}` otomatis diganti dengan id pembayaran, sehingga pembeli kembali ke halaman return gateway.
3. **Beri akses ke toko** (key admin): `PUT /admin/configs/{configId}/grants/{idClientToko}` → 204.
4. **Buat pembayaran** (key caller): `POST /payments` dengan
   `{ "id": <configId>, "uniquecode": "ORDER-1001", "amount": "10.00", "currency": "USD" }`.
   Jawabannya berisi `payment_url`.
5. **Pembeli membayar** di `payment_url`. Setelah selesai, browser kembali ke halaman return gateway, yang memeriksa status ke provider dan menampilkan hasilnya. Client yang mengirim `Accept: application/json` mendapat JSON.
6. **Cek status** (key caller): `GET /status/ORDER-1001`. Status: `PENDING`, `PAID`, `FAILED`, `EXPIRED`, `CANCELLED`.

Catatan: Xendit mewajibkan return URL `https://` (pakai host publik atau tunnel HTTPS). Webhook bersifat tambahan, untuk pembeli yang menutup browser sebelum kembali; butuh URL yang bisa dijangkau provider.

## Postman

Impor dua file di folder [postman/](postman/), pilih environment **Unified Gateway - Local Dev**, lalu isi `adminKey`, `callerKey`, dan `clientId`. Folder **8** menjalankan skenario PayPal sandbox langkah demi langkah.

## Produksi

- Set `GATEWAY_ENV=production`, serta `GATEWAY_MASTER_KEYS` dan `GATEWAY_ACTIVE_KEY_ID` secara eksplisit (file key tidak dibuat otomatis, Fake provider mati).
- Jalankan migrasi sebagai langkah terpisah: `gatewayctl migrate up`.
- API admin hanya menerima HTTPS; pasang gateway di belakang proxy TLS.

## Tes

`go vet ./...` dan `go test ./...`. Tes database membutuhkan `GATEWAY_TEST_DATABASE_URL` (database khusus, mis. `gateway_test`); tanpa itu tes database dilewati.

## Keamanan dan lisensi

Jangan pernah meng-commit `.env`, `master.key`, atau kredensial provider. Lihat [SECURITY.md](SECURITY.md). Belum ada lisensi; seluruh hak dipegang penulis.
