# Blast WA Personal

Web tool blast WhatsApp ke nomor pribadi untuk tim majoo, dengan login Google `@majoo.id` dan audit log per blast.

**Arsitektur split:**
- **Frontend** → GitHub Pages (`docs/`) — tim akses lewat URL GitHub
- **Backend** → Go + whatsmeow di Mac, expose via Cloudflare Tunnel — backend wajib jalan karena whatsmeow butuh proses persisten ke WhatsApp

```
┌─ Frontend (GitHub Pages) ─────────┐    ┌─ Backend (Mac + Tunnel) ──────┐
│ https://<user>.github.io/blast-…  │ ── │ https://blast-wa-api.majoo.id │
│ • docs/index.html, login.html     │    │ • Go + whatsmeow              │
│ • docs/config.js → API_BASE       │    │ • OAuth callback              │
│ • docs/assets/logo                │    │ • SQLite audit log            │
└───────────────────────────────────┘    └───────────────────────────────┘
```

## Stack

- **Backend:** Go + [whatsmeow](https://github.com/tulir/whatsmeow), `net/http` stdlib, SQLite
- **Auth:** Google OAuth 2.0, domain check `@majoo.id`, HMAC-signed session cookie (SameSite=None+Secure di produksi untuk cross-site)
- **Frontend:** vanilla JS, brand colors majoo (teal `#2DBDB6` → lime `#9ACF87`)
- **CORS:** allowlist per env via `ALLOWED_ORIGINS`

## Struktur repo

```
.
├── docs/                      # ← GitHub Pages root (Settings → Pages → /docs)
│   ├── index.html             # Main app, fetch ke API_BASE
│   ├── login.html             # Halaman login (link ke backend OAuth)
│   ├── config.js              # window.APP_CONFIG.API_BASE — edit URL backend di sini
│   └── assets/majoo-logo.svg
├── static/                    # Fallback (served oleh backend kalau Pages down)
├── main.go, auth.go, audit.go, blast.go
├── go.mod, go.sum
├── .env.example               # Template config
├── .env                       # (gitignored — secrets)
├── session/                   # (gitignored — WA session + audit DB)
└── sample.csv
```

## Setup lengkap — lihat [DEPLOY.md](DEPLOY.md)

Untuk first-time deployment dari nol (GCP OAuth, Cloudflare Tunnel, GitHub Pages enable), ikuti DEPLOY.md step-by-step.

## Quick reference

### Run backend lokal (untuk dev)

```bash
cp .env.example .env       # isi credentials
go build -o blast-wa-personal ./...
./blast-wa-personal
```

### Run produksi (Mac always-on)

Tab 1:
```bash
./blast-wa-personal
```

Tab 2:
```bash
cloudflared tunnel run blast-wa
```

### Format CSV

```csv
phone,nama_outlet,nomer_invoice
081234567890,Toko Sambal Pak Budi,INV-2026-0001
628111234567,Warung Mie Bu Sari,INV-2026-0002
```

- `phone` auto-normalize `0…` → `62…`
- Nomor non-WA ditandai `failed: nomor tidak terdaftar di WhatsApp`

## Endpoints (backend)

| Method | Path             | Auth | CORS | Keterangan                                |
|--------|------------------|------|------|-------------------------------------------|
| GET    | `/auth/login`    | —    | —    | Redirect ke Google OAuth                  |
| GET    | `/auth/callback` | —    | —    | Callback, set cookie, redirect ke FRONTEND_URL |
| POST   | `/auth/logout`   | —    | —    | Clear cookie                              |
| GET    | `/api/me`        | —    | ✅   | Current user (null kalau belum login)     |
| GET    | `/api/status`    | ✅   | ✅   | `{loggedIn, connected, hasQR}`            |
| GET    | `/api/qr`        | ✅   | ✅   | QR string                                 |
| POST   | `/api/blast`     | ✅   | ✅   | multipart: template, csv, min/max_delay   |
| GET    | `/api/progress`  | ✅   | ✅   | Snapshot job terkini                      |
| GET    | `/api/history`   | ✅   | ✅   | 100 audit logs terakhir                   |

## Keamanan

- **Session cookie**: HttpOnly, HMAC-signed dengan `SESSION_SECRET`. SameSite=None+Secure saat HTTPS (untuk cross-site dari Pages), Lax saat dev lokal HTTP. TTL 7 hari.
- **CORS**: hanya origin di `ALLOWED_ORIGINS` yang di-allow + credentials.
- **Domain check**: `@majoo.id` divalidasi di server (suffix check + `hd` param Google).
- **Audit log immutable dari UI**: hanya endpoint read; insert/update internal saat blast jalan.

## Anti-banned

- Default delay 6–14 detik random. Jangan turun di bawah 4.
- Batasi 100–200 nomor/hari per session WA.
- Variasikan template antar campaign.
- Sertakan opt-out (`Balas STOP`) + honor manual.

## Catatan

- Standalone — belum integrate ke Inbox Chat Dashboard Enterprise. Reply masuk perlu dipantau manual di HP sender.
- Audit per-recipient belum disimpan (cuma summary).
