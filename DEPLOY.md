# Deploy Guide — STAGING (Node.js + whatsapp-web.js)

Panduan deploy environment **staging** ke **mesin terpisah** dari produksi (Mac Mini Go).
Claude tidak punya akses mesin staging — langkah ini dijalankan user.

## Prasyarat mesin staging

- **Node.js ≥ 18** (disarankan 20/22). Cek: `node -v`.
- **Chromium** untuk Puppeteer (whatsapp-web.js). Butuh ~300–500 MB RAM per sesi.
  - **macOS**: `npm install` otomatis mengunduh Chromium milik Puppeteer — tidak perlu apa-apa lagi. (Atau set `PUPPETEER_EXECUTABLE_PATH` ke Google Chrome yang sudah ada.)
  - **Linux (VPS)**: pasang dependensi system Chromium dulu, mis. (Debian/Ubuntu):
    ```bash
    sudo apt-get update && sudo apt-get install -y \
      ca-certificates fonts-liberation libasound2 libatk-bridge2.0-0 libatk1.0-0 \
      libcups2 libdbus-1-3 libdrm2 libgbm1 libgtk-3-0 libnspr4 libnss3 \
      libx11-xcb1 libxcomposite1 libxdamage1 libxrandr2 libxshmfence1 wget
    ```
    Kalau Puppeteer gagal unduh Chromium, pasang `chromium` system lalu set `PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium`.
- **git**, dan akun GitHub yang bisa akses repo `zulfikardwi-cx/blast-wa-personal`.

## Langkah

### 1. Clone + checkout branch staging
```bash
git clone https://github.com/zulfikardwi-cx/blast-wa-personal.git
cd blast-wa-personal
git checkout staging
npm install      # build better-sqlite3 + unduh Chromium (beberapa menit)
```

### 2. Konfigurasi `.env`
```bash
cp .env.staging.example .env
openssl rand -hex 32     # → tempel ke SESSION_SECRET (baru, beda dari produksi)
```
Isi minimal: `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `OAUTH_REDIRECT_URL`, `SESSION_SECRET`.
Untuk Sheets (opsional): taruh `service-account.json` di folder project, set `GSHEET_SPREADSHEET_ID` ke **spreadsheet staging terpisah** (jangan pakai sheet produksi), dan beri akses Editor ke email service account.

> Biarkan `RETRY_ENABLED=false` di staging (default). Auto-cron jam-9 mati → cegah burst yang memicu ban. Uji pakai tombol manual.

### 3. Cloudflare Tunnel (staging, terpisah)
Jalankan tunnel ke port app (default 8080), mis. quick tunnel:
```bash
cloudflared tunnel --url http://localhost:8080
```
Catat URL `https://<random>.trycloudflare.com` (atau named tunnel sendiri kalau sudah ada akses).
Lalu **sinkronkan 2 tempat**:
- `.env` → `OAUTH_REDIRECT_URL=https://<tunnel-staging>/auth/callback`
- `docs/config.js` → `API_BASE: "https://<tunnel-staging>"` (harus sama host dengan halaman → same-origin)

### 4. Google OAuth redirect URI
Di GCP → APIs & Services → Credentials → OAuth client (boleh client yang sama dengan produksi):
tambahkan ke **Authorized redirect URIs**:
- `https://<tunnel-staging>/auth/callback`
- (opsional dev lokal) `http://localhost:8080/auth/callback`

### 5. Jalankan dengan pm2 (auto-restart)
```bash
npm install -g pm2
pm2 start server.js --name blast-wa-staging
pm2 logs blast-wa-staging         # lihat QR untuk discan
pm2 save                          # (opsional) + pm2 startup untuk auto-boot
```
Scan QR (muncul di `pm2 logs` **dan** di UI saat status "Scan QR") pakai **nomor sender staging** (bukan nomor produksi).

### 6. Verifikasi
Buka URL tunnel staging → login `@majoo.id` → status "WA Connected" → uji blast 1–2 nomor test (lihat checklist di bawah).

## Update berikutnya
```bash
git pull && npm install && pm2 restart blast-wa-staging
```
Hapus folder `.wwebjs_auth/` = logout (harus scan QR lagi). Jangan commit folder ini.

## Checklist uji end-to-end
1. `/api/status` → `connected:true` setelah scan QR.
2. Upload CSV (1–2 nomor test) → pesan attempt-1 sampai; cek tab Riwayat & thread `after_blast`.
3. Balas dari HP test → masuk Inbox, thread → `open`.
4. Reply via web → `in_progress`; Done → closing terkirim + `done` (locked).
5. `Run Blast Attempt` / `POST /api/retry/run-now?limit=1` → attempt 2 terkirim.
6. Export Sheets → tab "Blast Log" & "Belum Respons" terisi; "Sync Team" dari tab "Query Blast".
7. Logout dari HP → server auto re-init → QR baru muncul tanpa restart.

## Troubleshooting
- **Chromium gagal launch / "Failed to launch the browser process"** (umum di Linux/VPS/root): pastikan dependensi system terpasang; arg `--no-sandbox` sudah diset di `wa.js`. Kalau perlu, set `PUPPETEER_EXECUTABLE_PATH`.
- **Login loop / cookie tidak nyimpan**: pastikan `API_BASE` = host yang sama dengan halaman (same-origin). Beda host → cookie pihak ketiga diblokir Safari/Firefox.
- **`error 463` / nomor diblok kirim**: itu dari WhatsApp (anti-spam), bukan bug. Turunkan volume, perbesar jitter, hangatkan nomor.
