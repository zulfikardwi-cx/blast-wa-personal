# blast-wa-personal — Arsitektur & Full Flow (Handoff Doc)

> Dokumen ini untuk agent AI / developer yang baru masuk. Berisi peta arsitektur, model data,
> state machine status, semua flow inti (blast, retry, inbox, resolve, report), API, deploy,
> dan gotcha operasional. **Status per 2026-07-02** (HEAD `6b3e395`).

---

## 1. Tujuan aplikasi

Tool internal majoo untuk **broadcast (blast) WhatsApp validasi invoice** ke outlet pelanggan,
lalu **menangani balasan lewat Inbox**, dengan **auto-retry Attempt 2/3**, **pelacakan PIC**,
dan **laporan** yang di-export ke Google Sheet. Pesan yang dikirim adalah permintaan validasi
data untuk sebuah Invoice (Nama Outlet + Nomor Invoice).

Alur bisnis singkat:
1. Upload CSV (`phone, nama_outlet, nomer_invoice`) → blast **Attempt 1**.
2. Kalau tak dibalas → **Attempt 2**, lalu **Attempt 3** (manual/cron).
3. Pelanggan membalas → masuk **Inbox** → agent balas → tandai **On Going** (sedang validasi) → **Done/Resolved** (kirim closing message + kunci).
4. Yang tidak merespons sampai cutoff → **Force Close**; nomor tidak terdaftar → **Rejected** (untuk tim WO).
5. Report **Belum Respons** & **Report Resolved** → export ke Google Sheet.

---

## 2. Tech stack

- **Backend**: Go (satu binary `blast-wa-personal`), HTTP server via `net/http` (`mux`).
- **WhatsApp**: library **whatsmeow** (multi-device / linked-device, TIDAK resmi). Session di SQLite.
- **DB**: SQLite tunggal `session/audit.db` (driver `mattn/go-sqlite3`). Migrasi via `ALTER TABLE ADD COLUMN` yang di-guard cek "duplicate column".
- **Frontend**: HTML statis (vanilla JS, no framework) di folder `docs/`, dilayani same-origin oleh backend (`http.FileServer(http.Dir("docs"))`). Juga bisa via GitHub Pages.
- **Google Sheets**: `google.golang.org/api/sheets/v4` (service account) untuk export report.
- **Auth**: email+password per-user (bcrypt), session cookie HMAC. OAuth Google opsional (sudah dilepas dari UI).

---

## 3. Arsitektur tingkat tinggi

```
                    ┌──────────────────────────────────────────────┐
   Browser (docs/)  │  Go backend (:8090)                          │
   ├ index.html ────┤  ┌ HTTP handlers (main.go route table)       │
   ├ inbox.html     │  ├ whatsmeow client (state.client) ── WA ────┼─▶ WhatsApp (nomor majoo)
   ├ zopoz*.html    │  ├ zopoz whatsmeow (zopozState.client) ─ WA ─┼─▶ WhatsApp (nomor Zopoz)
   ├ profil.html    │  ├ retry scheduler (cron harian)             │
   └ login.html     │  └ SQLite auditDB (session/audit.db)         │
                    └──────────────────────────────────────────────┘
                              │ Cloudflare named tunnel
                              ▼
              https://blastvalidasi.cxmajoo.my.id  (public, same-origin cookie)
```

**Suite majoo sekarang MODEL 2-NOMOR (blast dipisah dari inbox):**
- **INTI** (`state.client`, `session/store.db`) — nomor majoo existing = **inbound-only**. Pegang
  Inbox + balas + Done. TIDAK pernah blast. Balasan dikirim dari sini.
- **BLASTER** (`blasterState.client`, `session/store-blaster.db`, file `blaster.go`) — nomor
  **disposable** = **outbound-only**. Semua blast (Attempt 1/2/3) dikirim dari sini (`sendOne`,
  `sendRetryOne`). Kena banned → logout (tombol "Ganti Nomor Blaster" / `/api/blaster/wa-logout`)
  → scan QR nomor pengganti; INTI tidak terganggu.
- **Korelasi balasan → invoice** lewat **token** (`validation_tokens`, file `tokens.go`): pesan
  blast membawa link `wa.me/<INTI>?text=...Kode Referensi : <token>`. Inbound di INTI di-resolve
  via token (`resolveInboundThread`), fallback nomor HP. Token di-mark **used saat Done**.
  Tabel majoo (`chat_threads`, `blast_logs`, `blast_recipients`) DIPAKAI BERSAMA kedua client
  (blaster nulis blast + pre-create thread; INTI nulis inbox). Kalau pelanggan chat dari nomor
  beda, JID pengirim disimpan di `chat_threads.wa_jid` (reply routing).
- **Zopoz** — nomor WA ketiga, fully isolated (TIDAK ikut model 2-nomor). Tabel `zopoz_*`. Kode
  `zopoz_*.go`. Logika mirror majoo lama (blast+inbox 1 nomor).
- (Pernah ada channel **watzap** — sudah **di-revert total** 2026-07-02, lihat §12.)

---

## 4. Peta file Go (`*.go`, semua `package main`)

| File | Peran |
|---|---|
| `main.go` | Bootstrap: init semua subsistem (urutan penting), daftar **route table**, start HTTP server + retry schedulers, graceful shutdown. |
| `auth.go` | Login email+password (bcrypt), session cookie HMAC, `requireAuth`/`corsMiddleware`, ganti password, (OAuth opsional). |
| `users.go` | Tabel `app_users`; seed roster dari `APP_LOGIN_EMAILS` (default password `admin123`). `userFromCtx`. |
| `blast.go` | **Blast majoo Attempt 1**: `BlastJob`/`RecipientStatus`, `handleBlast`, `runBlast`, `sendOne` (kirim via **blasterState.client**), `parseCSV`, `normalizePhone`, `renderTemplate`, `resolveToLID(ctx, client, jid)`. Guard: abort setelah 3× error 463 (`is463Err`); reject HANYA kalau `isInvalidNumberErr` (nomor tidak terdaftar). |
| `blaster.go` | **Client BLASTER** (nomor disposable pengirim blast). Mirror scaffolding `zopoz.go`: `blasterState`, `initBlasterClient` (store terpisah, event handler TANPA `events.Message`, auto re-pair saat logout), `blasterHandleStatus/QR/Logout`. |
| `tokens.go` | **Layer token validasi**: tabel `validation_tokens`, `getOrCreateToken`, `buildTriggerLink`/`intiNumber` (link wa.me ke INTI), `applyLink` (substitusi `{{link}}`), `lookupToken`/`resolveInboundThread` (korelasi inbound), `parseTokenFromBody`, `markTokenUsed`. |
| `chat.go` | **Inbox majoo**: tabel `chat_threads`/`chat_messages`, **state machine status**, upsert thread (blast/incoming/agent reply/failed), handler threads/messages/read/status/reply/resolve, template Attempt 1/2/3 + closing, `backfillFailedThreads`, `initChat`. |
| `retry.go` | **Scheduler retry Attempt 2/3 majoo** (cron harian jam `RETRY_HOUR`), force-close sweep, handler `retry/preview` `run-now`. |
| `retry_invoice.go` | Helper retry **PER (phone, invoice)** dipakai bersama majoo & Zopoz: `collectInvoiceRetries`, `invoiceStillNeedsRetry`, `phoneHasPendingInvoice`, `bumpThreadAfterRetry`. |
| `resolved.go` | Tabel `resolved_invoices` (SET invoice yang sudah Done, permanen). `markPhoneResolved`, `isInvoiceResolved`, `backfillResolvedInvoices` (dari closing message + thread done). |
| `exclude.go` | Tabel `excluded_invoices` (exclude manual per invoice dari retry & report). Dipakai majoo+Zopoz (kolom `suite`). |
| `report.go` | Report majoo: `queryUnresponsive` (Belum Respons), `queryResolved` (Report Resolved), export CSV + Google Sheet. |
| `sheets.go` | Init Google Sheets client, `ensureSheetExists`, export "Blast Log" (audit lengkap). |
| `audit.go` | Tabel `blast_logs`/`blast_recipients`, record start/recipient/end, `handleHistory`, `closeStaleRunningBlasts` (auto-tutup blast basi saat startup, majoo+zopoz). |
| `media.go` | Download & serve media WA (gambar/dokumen) untuk inbox. |
| `teams.go` | Sync data "team/area" per thread dari sheet helper (fitur team filter di inbox). |
| `zopoz*.go` | Cermin majoo untuk suite Zopoz: `zopoz.go` (client/koneksi/QR), `zopoz_chat.go` (inbox), `zopoz_blast.go` (blast+audit+retry), `zopoz_report.go`, `zopoz_sheets.go`, `zopoz_templates.go`. |

---

## 5. Model data (SQLite `session/audit.db`)

**majoo:**
- `blast_logs(id, user_email, user_name, started_at, ended_at, template, total, sent, failed, skipped, attempt, min_delay, max_delay)` — 1 baris per batch blast (Attempt 1/2/3). `ended_at IS NULL` = sedang berjalan.
- `blast_recipients(id, blast_log_id, phone, nama_outlet, nomer_invoice, status, error, message, sent_at, created_at)` — 1 baris per kirim per invoice. `status` ∈ sent/failed. **Ini sumber kebenaran per-(phone,invoice)** untuk report & retry (thread hanya simpan invoice TERAKHIR).
- `chat_threads(phone PK, nama_outlet, nomer_invoice, last_blast_id, last_message_at, last_message_preview, last_message_direction, unread_count, status, assigned_email, assigned_name, created_at, updated_at, + current_attempt, last_attempt_at, team, area, attempt1_failed, rejected_at, followup_at, reject_reason)` — 1 baris per NOMOR (bukan per invoice).
- `chat_messages(id, phone, direction[in/out], body, is_media, media_type, media_path, wa_message_id, timestamp, blast_log_id, sender_email, sender_name, created_at)` — histori chat inbox.
- `resolved_invoices(suite, phone, nomer_invoice, nama_outlet, resolver_email, resolver_name, resolved_at, PK(suite,phone,nomer_invoice))` — SET invoice yang sudah Done (permanen, tahan re-blast). Sumber Report Resolved + exclude retry.
- `excluded_invoices(suite, phone, nomer_invoice, excluded_by, excluded_at, PK(suite,phone,nomer_invoice))` — exclude manual.
- `validation_tokens(token PK, phone, nomer_invoice, nama_outlet, status['pending'|'used'], created_at, used_at, UNIQUE(phone,nomer_invoice))` — token korelasi balasan↔invoice (model 2-nomor). 1 token per (phone,invoice), di-reuse lintas attempt. `status='used'` di-set saat Done. Kolom baru `chat_threads.wa_jid` = JID pengirim asli kalau pelanggan chat dari nomor beda dari yang di-blast.
- `app_users(email PK, pass_hash, must_change, ...)` — akun login.

**Zopoz:** `zopoz_threads`, `zopoz_messages`, `zopoz_blast_logs`, `zopoz_blast_recipients` (skema sama). `excluded_invoices`/`resolved_invoices` dipakai bersama via kolom `suite` (namun resolved_invoices saat ini HANYA diisi majoo).

---

## 6. State machine STATUS thread (inti alur)

`chat_threads.status` (dan `zopoz_threads.status`):

| Status | Arti | PIC (assigned) |
|---|---|---|
| `after_blast` | Attempt 1 terkirim, belum ada respons. Masuk antrian auto-retry. | — |
| `open` | Pelanggan membalas via **WA asli** (ada incoming). Perlu ditangani. Inti bisa balas. | dilepas (netral) |
| `konfirmasi_web` | Pelanggan konfirmasi lewat **halaman web only** (belum pernah chat WA ke Inti). Inti **tak bisa** kirim balasan (kontak dingin → error 463); tindak lanjut via WA Call / blast resmi. Kalau customer akhirnya chat WA asli → promote ke `open`. | dilepas (netral) |
| `in_progress` | Agent sudah membalas via inbox. | agent yang balas |
| `on_going` | Ditandai sedang divalidasi (WhatsApp Call dst). Keluar dari antrian retry. | agent yang klik |
| `scheduled` | Pelanggan minta validasi di hari lain (`followup_at`). Keluar dari retry. | agent yang klik |
| `done` | Selesai/Resolved. Closing message terkirim, thread terkunci. | **resolver** (yang klik Done) |
| `invalid` | Nomor salah / perlu revisi. Tidak kirim pesan. Terkunci. | dilepas |
| `force_close` | Attempt 3 lewat tanpa respons s/d cutoff (default 17:00 WIB). | — |
| `rejected` | Attempt 1 GAGAL kirim karena **nomor tidak terdaftar di WA** (`attempt1_failed=1`). Untuk tim WO. | — |

**Aturan penting:**
- **Terminal untuk retry** (keluar antrian Attempt 2/3): `done, invalid, on_going, scheduled, force_close`. Sisanya (`after_blast, open, in_progress, rejected`) TETAP diretry selama invoice-nya belum sampai Attempt 3 & Attempt 1-nya sukses.
- **Assign PIC** (di `handleSetStatus`): `in_progress/on_going/scheduled/done` → assign ke user yang klik. `open/invalid/force_close` → clear (netral). **Re-open melepas PIC**; PIC lain yang set `on_going` menggantikan.
- **Done** = jalur sebenarnya lewat `sendReply` → `POST /api/inbox/status` `status=done` (BUKAN `/api/inbox/resolve` yang kini legacy). Saat Done: assign resolver + `markPhoneResolved()` snapshot semua invoice nomor itu ke `resolved_invoices`.
- **Reject hanya untuk nomor tidak terdaftar.** Gagal kirim karena koneksi putus / error 463 / rate-limit / server → **tidak** ditandai rejected (nomor valid), biar bisa di-blast ulang. (`isInvalidNumberErr` di blast.go; `backfillFailedThreads` juga hanya backfill error "tidak terdaftar".)

---

## 7. Flow inti

### 7.1 Blast Attempt 1 (`blast.go`)
1. `POST /api/blast` (multipart: `csv`, `min_delay`, `max_delay`). Template = `GetAttemptTemplate(1)` (backend, bukan input user).
2. `parseCSV` → `normalizePhone` (→ format `62...`). `recordBlastStart` → `blast_logs`.
3. Goroutine `runBlast`: per recipient → `sendOne` (cek `IsOnWhatsApp` → kirim). Jeda `min–max` detik antar kirim.
4. Sukses → `blast_recipients.status=sent` + `upsertThreadBlast` (status→`after_blast`, current_attempt=1) + `recordChatMessage`.
5. Gagal → `status=failed`; kalau `isInvalidNumberErr` → `upsertThreadBlastFailed` (thread→`rejected`); selain itu tidak di-reject. Kalau **3× error 463 beruntun** → sisa antrian di-skip & blast abort (session WA bermasalah).
6. Progress live via `GET /api/progress` (job in-memory `state.job`). Job TIDAK punya resume → restart = job hilang (dan `closeStaleRunningBlasts` menutup baris `ended_at NULL` saat startup).

### 7.2 Retry Attempt 2/3 (`retry.go` + `retry_invoice.go`)
- **Per (phone, invoice)**, direkonstruksi dari `blast_recipients`+`blast_logs` (MAX attempt yang `sent`).
- Eligible bila: thread NOT terminal, Attempt 1 invoice sukses (`max_att≥1` & `<3`), belum dikirim hari ini, TIDAK ada di `excluded_invoices`, TIDAK ada di `resolved_invoices`.
- Cron harian jam `RETRY_HOUR` (default 09:00 WIB) — TAPI `RETRY_ENABLED=false` mematikan auto-send; tombol **Run Attempt 2/3** manual tetap jalan.
- Force-close sweep jam `RETRY_FORCECLOSE_HOUR` (default 17:00): thread `after_blast/in_progress` yang SEMUA invoice-nya sudah Attempt 3 tanpa respons → `force_close`.

### 7.3 Inbox & balas (`chat.go`)
- Incoming WA → `upsertThreadIncoming` (status→`open` kecuali terkunci) + `recordChatMessage`.
- `GET /api/inbox/threads?status=&team=` (daftar + counts per bucket), `GET /api/inbox/messages?phone=`.
- Balas: `POST /api/inbox/reply` → `sendOne` + `upsertThreadAgentReply` (→`in_progress`).
- **Private Note**: `POST /api/inbox/note` → simpan `chat_messages` `direction='note'` (catatan internal antar-tim: "sudah dihubungi" dst). TIDAK kirim WA, TIDAK ubah status/bucket/unread. Frontend inbox punya toggle **Balas / Private Note** di kotak tulis; note boleh ditulis walau thread terkunci.
- **Konfirmasi web**: `POST /api/konfirmasi` (publik) → `upsertThreadKonfirmasiWeb` (→bucket `konfirmasi_web`, bukan `open`). Migrasi `migrateWebOnlyToKonfirmasiWeb` (startup) reklasifikasi thread `open` lama yang web-only.
- Set status: `POST /api/inbox/status` (open/in_progress/done/invalid/on_going/force_close/scheduled).

### 7.4 Done / Resolve
- UI "Done/Resolved" memuat closing template ke kotak balas → agent "Kirim & Tutup" → `sendReply` kirim closing + `POST /api/inbox/status status=done`.
- `handleSetStatus` status=done: assign resolver + `markPhoneResolved("majoo", ...)` → snapshot SEMUA invoice attempt-1-sent nomor itu ke `resolved_invoices`.

### 7.5 Report (`report.go`)
- **Belum Respons** (`queryUnresponsive`): per (phone,invoice) untuk thread `after_blast/in_progress/rejected/force_close`, kolom Attempt 1/2/3 + Rejected + Alasan. Exclude `excluded_invoices` & `resolved_invoices`. Tab Sheet "Belum Respons".
- **Report Resolved** (`queryResolved`): dari `resolved_invoices` (permanen). Kolom: Nomor Invoice | Nama Outlet | Nomor User | Nama PIC (Resolved). Satu nomor banyak invoice → banyak baris. Tab Sheet "report resolved".
- **Blast Log** (`sheets.go`): dump lengkap `blast_recipients`+`blast_logs`. Tab "Blast Log".
- Export via `POST /api/*/export-sheet` (clear + overwrite tab). CSV via `*.csv`.

### 7.6 Exclude per invoice (`exclude.go`)
- `POST /api/retry/{exclude,include}` (phone+invoice). Yang di-exclude keluar dari retry DAN Belum Respons. UI: chip "Att N eligible" → modal.

---

## 8. API surface (semua di `main.go`, mayoritas `requireAuth`)

**Auth:** `/auth/{login,callback,logout,password-login,change-password}`
**Core majoo:** `/api/{me,status,qr,logout,blast,progress,history,templates}`
**Sheets:** `/api/{sheet-status,export-sheet}`
**Retry:** `/api/retry/{preview,run-now,exclude,include,excluded}`
**Report:** `/api/report/{unresponsive,unresponsive.csv,export-sheet,resolved,resolved.csv,resolved/export-sheet}`
**Inbox:** `/api/inbox/{threads,messages,read,status,reply,note,resolve,sync-teams,media}` (`note` = private note internal, tidak kirim WA)
**Blaster:** `/api/blaster/{wa-status,qr,wa-logout}` (koneksi nomor disposable pengirim blast; `wa-logout` = ganti nomor). Blast/retry (`/api/blast`, `/api/retry/*`) kini cek koneksi & kirim via BLASTER, bukan INTI.
**Zopoz:** `/api/zopoz/*` (mirror: wa-status,qr,wa-logout,templates,blast,progress,history,sheet-status,export-sheet,retry/*,report/*,threads,messages,read,status,reply,media)
**Static:** `/` → `docs/`

---

## 9. Frontend (`docs/`, vanilla JS)

| File | Halaman |
|---|---|
| `index.html` | **majoo Blast** — upload CSV, blast, tab Progress / Riwayat Blast / Log Status Update (Belum Respons + Run Attempt 2/3 + exclude) / **Report Resolved**. |
| `inbox.html` | **majoo Inbox** — daftar thread per bucket, chat, balas, tombol On Going/Scheduled/Invalid/Done/Reopen/Force Close. |
| `zopoz-blast.html`, `zopoz.html`, `zopoz-upsell.html` | Cermin untuk Zopoz. |
| `login.html`, `profil.html`, `upsell.html` | Login, ganti password, placeholder upsell. |
| `config.js` | `window.APP_CONFIG.API_BASE` = `https://blastvalidasi.cxmajoo.my.id`. |

Pola JS: `apiFetch(path)` (credentials:include, 401→login), poll `/progress` tiap ~2.5s saat blast, parse CSV client-side untuk preview (backend tetap validasi).

---

## 10. Konfigurasi (`.env`)

| Var | Fungsi |
|---|---|
| `ADDR` | Alamat listen (`:8090`). |
| `SESSION_SECRET` | Kunci HMAC cookie (wajib). |
| `APP_LOGIN_EMAILS` | Roster email @majoo.id (comma). Seed password default `admin123`. |
| `GOOGLE_SERVICE_ACCOUNT_JSON`, `GSHEET_SPREADSHEET_ID` | Export Sheets. |
| `GSHEET_SHEET_NAME` / `GSHEET_REPORT_SHEET_NAME` / `GSHEET_RESOLVED_SHEET_NAME` | Nama tab (default "Blast Log" / "Belum Respons" / "report resolved"). |
| `GSHEET_ZOPOZ_SHEET_NAME`, `ZOPOZ_GSHEET_REPORT_SHEET_NAME` | Tab Zopoz. |
| `RETRY_ENABLED` | **`false`** = auto-cron Attempt 2/3 OFF (cegah ban). Manual tetap jalan. |
| `RETRY_HOUR`, `RETRY_FORCECLOSE_HOUR` | Jam cron retry (9) & force-close (17). |
| `TEMPLATE_ATTEMPT_1/2/3`, `INBOX_CLOSING_TEMPLATE` | Override template (majoo). `ZOPOZ_*` untuk Zopoz. |
| `GOOGLE_CLIENT_ID/SECRET`, `OAUTH_REDIRECT_URL` | OAuth opsional (tidak dipakai UI). |

Spreadsheet aktif: `https://docs.google.com/spreadsheets/d/1qauDLd0VHGzpMYLFhDhHu7lXJ2fJLQMTvtnuvZkC2G0`.

---

## 11. Deploy & runtime

- Backend jalan di Mac user, listen `:8090`. **Dikelola launchd** (3 agent): `com.majoo.blast.backend`, `com.majoo.cloudflared.blastvalidasi` (named tunnel), `com.majoo.caffeinate`.
- **Named Cloudflare tunnel** `blastvalidasi` → `https://blastvalidasi.cxmajoo.my.id` (URL stabil). Config `~/.cloudflared/config.yml`.
- **Rebuild + restart backend:** `go build -o blast-wa-personal ./...` lalu `launchctl kickstart -k gui/$(id -u)/com.majoo.blast.backend` (JANGAN `nohup`/`pkill` — launchd yang punya proses).
- **Log launchd:** `~/Library/Logs/majoo-backend.log` & `majoo-cloudflared.log` (bukan blast.log project — TCC memblok tulis ke ~/Documents).
- **JANGAN restart saat blast berjalan.** Cek dulu: `sqlite3 session/audit.db "SELECT COUNT(*) FROM blast_logs WHERE ended_at IS NULL;"` (>0 = jalan). Job in-memory hilang saat restart.
- Frontend: GitHub Pages (`docs/` di branch `main` — perlu push) DAN tunnel same-origin (live dari disk).
- **Workflow git:** commit langsung ke `main` (tanpa branch/PR).

---

## 12. Gotcha & keputusan penting

- **Anti-spam WhatsApp (error 463 & "device removed") — MASALAH UTAMA yang belum tuntas.**
  Karena whatsmeow = perangkat tertaut (unofficial), WhatsApp mem-flag nomor yang blast massal ke stranger.
  - Error **463** = device baru/tidak dipercaya menolak kirim ke stranger.
  - **"device removed stream error"** = nomor di-logout paksa di tengah blast (harus re-scan QR).
  - Nomor primary & Zopoz sudah "terbakar" (2026-07-02). Nomor baru kena **permanent banned**.
- **watzap (gateway watzap.id) DICOBA & DI-REVERT (2026-07-02).** Mode "WA Unofficial" watzap = mekanisme sama → nomor tetap **permanent banned**. Semua kode watzap sudah dihapus (revert `6b3e395`). **JANGAN bangun ulang watzap-unofficial.**
- **Satu-satunya jalur anti-ban yang benar: WhatsApp Business API RESMI (WABA Cloud API / Meta)** — pakai template pesan yang di-approve untuk broadcast cold, ada biaya per-percakapan, perlu opt-in. watzap juga punya mode "WABA Official" (`/waba_send_message_template` + Access Token) yang BELUM dicoba — ini yang layak kalau mau lanjut.
- **Re-blast hygiene — belum diselesaikan.** Re-blast nomor yang sudah ada di inbox me-reset thread ke `after_blast`/Attempt 1 (bahkan dari Done) karena `upsertThreadBlast` selalu set `after_blast`. Mitigasi parsial: `resolved_invoices` mencegah invoice yang sudah Done ikut retry lagi walau nomornya di-blast ulang untuk invoice lain.
- **Report Belum Respons & Log Status Update = PER NOMOR INVOICE** (bukan per phone). Satu phone bisa muncul di banyak baris (banyak invoice). Sistem WO eksternal menarik tab "Belum Respons"/"reject" — jangan ubah semantik kolomnya.
- **`RETRY_ENABLED=false`** sengaja: auto-cron send OFF untuk kurangi ban. Hanya manual Run Attempt 2/3.

---

## 13. Riwayat perubahan terkini (git `main`)

```
6b3e395 Revert Watzap: hapus channel watzap (nomor kena permanent banned)
62c582d Blast majoo: abort otomatis setelah 3× error 463 beruntun
cb602a0 Watzap Fase 1 (SUDAH DI-REVERT oleh 6b3e395)
6a6fb7e Reject hanya untuk nomor tidak terdaftar; 463/koneksi/server tidak di-reject
d3bdbe4 Auto-tutup blast 'running' basi saat startup
3304535 Guard: gagal kirim karena koneksi WA putus tidak lagi ditandai 'rejected'
017c3eb Resolved invoices permanen: invoice Done tak ikut retry walau di-blast ulang
c41db1b PIC & Report Resolved: catat resolver saat Done + tab report ke Google Sheet
505f0ae Zopoz pairing: QR auto-regenerate sampai ter-scan
3a01d81 Auth: akun per-user (bcrypt) + ganti password via Profil
ae133c4 Cutover API_BASE → named Cloudflare tunnel
9b871e4 Auth: migrasi penuh ke login email+password
```

---

## 14. Untuk agent yang melanjutkan

- **Baca file per subsistem** sesuai §4. Titik masuk: `main.go` (route table = daftar isi fitur).
- **Perubahan backend butuh rebuild + restart** (§11). Cek blast berjalan dulu.
- **majoo & Zopoz kembar** — perubahan logika sering perlu diterapkan di keduanya (kecuali helper bersama di `retry_invoice.go`/`exclude.go`/`resolved.go`).
- **Isu #1 yang belum selesai = deliverability/ban.** Solusi teknis di dalam whatsmeow/unofficial sudah mentok; keputusan berikutnya bersifat operasional/produk (WABA resmi). Lihat §12.
- Dokumen tambahan: `README.md`, `DEPLOY.md`, `REQUIREMENTS.md`.
