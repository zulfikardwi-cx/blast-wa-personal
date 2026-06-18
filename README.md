# Blast WA Personal — STAGING (Node.js)

> Branch `staging`. Migrasi infrastruktur dari **Go + whatsmeow** → **Node.js + whatsapp-web.js**.
> Branch `main` (produksi Go di Mac Mini) TIDAK disentuh dan tetap jalan seperti biasa.

Web tool blast WhatsApp + Inbox follow-up untuk tim majoo, login Google `@majoo.id`, audit log, retry terjadwal, dan export Google Sheets.

## Apa yang berubah dari versi Go

| Aspek | Lama (main) | Sekarang (staging) |
|---|---|---|
| Bahasa | Go | Node.js (CommonJS) |
| Library WA | whatsmeow (protokol langsung) | whatsapp-web.js (Puppeteer + Chromium) |
| Sesi WA | `session/store.db` (SQLite whatsmeow) | folder LocalAuth `.wwebjs_auth/` (sesi browser) |
| LID handling | manual (`resolveToLID`, `GetLIDForPN`) | otomatis oleh whatsapp-web.js (`getNumberId`) |
| HTTP | `net/http` stdlib | Express |
| DB bisnis | `session/audit.db` | sama (`session/audit.db`, skema identik via better-sqlite3) |
| Frontend `docs/` | sama | sama (tidak berubah, kecuali `config.js`) |

> **Sesi tidak bisa dimigrasi**: LocalAuth ≠ store.db. Staging WAJIB scan QR baru (pakai nomor sender staging terpisah).

## Stack

- **Backend:** Node.js ≥18, Express, [whatsapp-web.js](https://github.com/pedroslopez/whatsapp-web.js), better-sqlite3
- **Auth:** Google OAuth 2.0 (`google-auth-library`), domain `@majoo.id`, cookie HMAC-SHA256 (skema identik versi Go), TTL 7 hari
- **Sheets:** `googleapis` + service account
- **Scheduler retry:** window jam-9 WIB (`luxon`, zona `Asia/Jakarta`)

## Struktur (branch staging)

```
.
├── server.js        # bootstrap Express + routes (port main.go)
├── wa.js            # layer whatsapp-web.js (QR/connect/send/incoming, auto re-init)
├── db.js            # better-sqlite3 + skema identik
├── auth.js          # OAuth + cookie HMAC + CORS
├── blast.js         # CSV + job blast attempt-1 + progress
├── chat.js          # Inbox 7-bucket state machine + handlers
├── templates.js     # 3 attempt template + closing
├── retry.js         # scheduler window jam-9 + manual run/preview
├── report.js        # report "Belum Respons"
├── sheets.js        # export Blast Log + sync team
├── util.js          # normalizePhone, render, dll
├── audit.js         # blast_logs / blast_recipients + history
├── docs/            # frontend (sama; config.js → API_BASE)
├── package.json
├── .env.staging.example   # → copy ke .env
└── session/         # (gitignored) audit.db
```

## Jalankan lokal (di mesin staging)

```bash
cp .env.staging.example .env     # isi kredensial staging
npm install                      # build better-sqlite3 + unduh Chromium (puppeteer)
npm start                        # node server.js
```

Buka `http://localhost:8080`, login `@majoo.id`, lalu scan QR (muncul di terminal **dan** di UI via `/api/qr`) pakai **nomor sender staging**.

Deploy ke mesin terpisah + Cloudflare Tunnel + pm2: lihat [DEPLOY.md](DEPLOY.md).

## Inbox state machine (7 bucket — identik versi Go)

`after_blast` (blast attempt-1) → `open` (user balas) → `in_progress` (agent balas) ; `on_going` (manual, sticky) ; `force_close` (auto setelah attempt-3 no-response) ; `done` (locked + closing) ; `invalid` (locked). `done`/`invalid`/`on_going` mengunci status dari incoming/agent-reply.

## Retry

Tiap hari saat jam `RETRY_WINDOW_HOUR` (WIB), proses antrian "Belum Respons" (after_blast + in_progress, attempt<3), max 1×/hari per nomor. **Default staging `RETRY_ENABLED=false`** (auto-cron mati) — pakai tombol manual "Run Blast Attempt" / `POST /api/retry/run-now` untuk uji terkontrol.

## Format CSV

```csv
phone,nama_outlet,nomer_invoice
081234567890,Toko Sambal Pak Budi,INV-2026-0001
628111234567,Warung Mie Bu Sari,INV-2026-0002
```

`phone` auto-normalize `0…`/`8…` → `62…`. Nomor non-WA → `failed: nomor tidak terdaftar di WhatsApp`.

## Endpoints (paritas dengan versi Go)

Auth: `GET /auth/login`, `GET /auth/callback`, `/auth/logout`.
API (semua butuh login, kecuali `/api/me`): `/api/me`, `/api/status`, `/api/qr`, `POST /api/logout`, `POST /api/blast`, `/api/progress`, `/api/history`, `/api/sheet-status`, `POST /api/export-sheet`, `/api/templates`, `/api/retry/preview`, `POST /api/retry/run-now`, `/api/report/unresponsive(.csv)`, `POST /api/report/export-sheet`, `/api/inbox/{threads,messages,read,status,reply,resolve,sync-teams}`.

## ⚠️ Risiko ban — tetap berlaku

whatsapp-web.js **juga unofficial**. Migrasi ini TIDAK menghilangkan risiko ban (insiden `error 463` di versi Go). Pertahankan jitter konservatif, volume rendah, dan pertimbangkan strategi inbound-first / WA Cloud API resmi untuk produksi luas.
