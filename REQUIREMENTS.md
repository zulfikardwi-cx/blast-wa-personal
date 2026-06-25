# Blast WA Personal — Requirement & Flow (majoo + Zopoz)

> Dokumen acuan flow & data. Update terakhir: 2026-06-25.
> Sumber kebenaran tetap kode; dokumen ini ringkasan terstruktur supaya mudah dibaca.

---

## 1. Arsitektur & Konsep Dasar

Aplikasi punya **dua "suite" yang identik secara fitur tapi terpisah total** (nomor WhatsApp berbeda):

| | **majoo** | **Zopoz** |
|---|---|---|
| Client WA | `state.client` (utama) | `zopozState.client` (kedua) |
| Sesi WA | `session/store.db` | `session/store-zopoz.db` |
| Folder media | `session/media/` | `session/media-zopoz/` |
| Tabel thread/pesan | `chat_threads`, `chat_messages` | `zopoz_threads`, `zopoz_messages` |
| Tabel blast | `blast_logs`, `blast_recipients` | `zopoz_blast_logs`, `zopoz_blast_recipients` |
| Prefix API | `/api/...`, `/api/inbox/...` | `/api/zopoz/...` |
| Halaman | `index.html` (Blast), `inbox.html` (Inbox), `upsell.html` | `zopoz-blast.html`, `zopoz.html`, `zopoz-upsell.html` |

**Dipakai bersama:** satu database `session/audit.db`, satu Google Spreadsheet (tab berbeda), login Google OAuth, dan tabel `excluded_invoices` (dibedakan kolom `suite`).

**Navigasi:** header dua brand `majoo | ZOPOZ` (switch suite). Tiap suite punya sub-menu `Blast | Inbox | Upsell`.

---

## 2. Model Data

### 2.1 `*_threads` — 1 baris per **NOMOR TELEPON**
Menyimpan hanya **invoice terakhir** untuk nomor itu. Kolom penting:

| Kolom | Arti |
|---|---|
| `phone` (PK) | nomor (format 62…) |
| `nama_outlet`, `nomer_invoice` | data invoice **terakhir** yang di-blast ke nomor ini |
| `status` | state machine (lihat §3) |
| `current_attempt` | attempt tertinggi lintas invoice nomor ini |
| `last_attempt_at` | waktu attempt terakhir |
| `unread_count` | pesan belum dibaca di inbox |
| `assigned_email` / `assigned_name` | PIC (agen) |
| `attempt1_failed` | 1 = Attempt 1 gagal kirim |
| `reject_reason` | alasan reject/force-close |
| `followup_at` | tanggal follow-up (status `scheduled`) |

### 2.2 `*_messages` — riwayat chat per nomor
`id`, `phone`, `direction` (`in`/`out`), `body`, `is_media`, `media_type`, `media_path`, `wa_message_id`, `timestamp`, `sender_email`, `sender_name`.

### 2.3 `*_blast_logs` — 1 baris per **batch blast**
`id`, `started_at`, `ended_at`, `template`, `total`, `sent`, `failed`, `skipped`, **`attempt`** (1 = blast awal, 2/3 = retry), `user_email`, `user_name`, `min_delay`, `max_delay`.

### 2.4 `*_blast_recipients` — 1 baris per **kirim ke 1 invoice** ⭐
**Sumber kebenaran per-invoice.** `id`, `blast_log_id`, `phone`, `nama_outlet`, **`nomer_invoice`**, `status` (`sent`/`failed`/`skipped`), `error`, `message`, `sent_at`.

### 2.5 `excluded_invoices` — shared (majoo + Zopoz)
`suite`, `phone`, `nomer_invoice`, `excluded_by`, `excluded_at`. PK = (`suite`, `phone`, `nomer_invoice`).

### 🔑 Konsep kunci: progres attempt PER INVOICE
`*_threads` di-key per **nomor** → kalau 1 nomor dipakai banyak invoice, hanya invoice terakhir yang tercatat di thread. Maka **progres attempt per invoice direkonstruksi dari `*_blast_recipients` + `*_blast_logs.attempt`**:

```
max_attempt(phone, invoice) = MAX(blast_logs.attempt)
  dari blast_recipients yang status='sent' untuk (phone, invoice) itu
```

Ini dasar dari **Report per-invoice** (§9) dan **Retry per-invoice** (§6).

---

## 3. Status / Bucket (state machine per nomor)

| Status | Arti | Masuk antrian retry? | Muncul di "Belum Respons"? |
|---|---|:---:|:---:|
| `after_blast` | Attempt 1 terkirim, belum dibalas | ✅ | ✅ |
| `open` | Customer sudah membalas, belum di-action agen | ✅ | ✅ |
| `in_progress` | Agen sudah membalas | ✅ | ✅ |
| `on_going` | Ditandai "sedang divalidasi" | ❌ | ❌ |
| `scheduled` | Dijadwalkan validasi hari lain (`followup_at`) | ❌ | ❌ |
| `done` | Selesai/resolved (closing terkirim) | ❌ | ❌ |
| `invalid` | Ditandai invalid | ❌ | ❌ |
| `force_close` | Attempt 3 habis tanpa respons s/d cutoff | ❌ | ✅ (tag `reject`) |
| `rejected` | Attempt 1 GAGAL kirim (nomor tak terdaftar WA) | ❌ | ✅ (tag `reject`) |

**Transisi utama:**
- Blast Attempt 1 sukses → `after_blast` · gagal kirim → `rejected`
- Customer balas → `open` (kalau belum terminal)
- Agen balas → `in_progress`
- Agen: Done → `done` · Invalid → `invalid` · On Going → `on_going` · Scheduled → `scheduled` · Force Close → `force_close` · Reopen → `open`
- Sweep 17:00: attempt 3 tuntas tanpa respons → `force_close`
- `force_close` → `open` lagi kalau customer akhirnya membalas

---

## 4. Flow: Login
1. Buka halaman → JS `fetch('/api/me')`.
2. Belum login → redirect Google OAuth → callback → cookie session (HMAC `SESSION_SECRET`).
3. Semua endpoint `/api/*` diproteksi `requireAuth`. Media (`/api/.../media`) diproteksi token HMAC di query (boleh tanpa cookie agar `<img>`/`<video>` jalan).

---

## 5. Flow: Blast Attempt 1
1. Upload CSV — header wajib `phone,nama_outlet,nomer_invoice` (BOM/UTF-8 ditoleransi).
2. Normalisasi nomor: `08…`→`62…`, `8…`→`62…`, `62…` tetap.
3. Per baris (jeda acak **min–max detik**, default 20–40):
   - Cek `IsOnWhatsApp`.
     - **Tidak terdaftar** → `blast_recipients.status='failed'`; thread → **`rejected`** (`attempt1_failed=1`, alasan "nomor tidak terdaftar di WhatsApp").
     - **Terdaftar** → resolve PN→LID → kirim **template Attempt 1** (`{{nama_outlet}}`, `{{nomer_invoice}}` di-replace) → catat `blast_recipients` (attempt 1, `sent`) + `chat_messages` (`out`) → thread → **`after_blast`** (`current_attempt=1`).
4. Template Attempt 1 selalu dari backend (template manual/CSV diabaikan agar konsisten dengan retry).
5. Progress real-time di tab **Progress**; semua tercatat di **Riwayat Blast**.

---

## 6. Flow: Retry Attempt 2/3 — PER INVOICE ⭐

**Pemicu:**
- **Auto-cron:** cek tiap `RETRY_CHECK_INTERVAL_MINUTES` (30 mnt), kirim hanya saat jam `RETRY_WINDOW_HOUR` (09:00 WIB). **Saat ini `RETRY_ENABLED=false` → auto-cron MATI.**
- **Manual:** tombol **Run Attempt 2** / **Run Attempt 3** di tab Log Status Update (melewati guard jam).

**Sebuah invoice eligible** jika SEMUA terpenuhi:
1. Nomornya **belum terminal** → status ∈ {`after_blast`, `open`, `in_progress`, `rejected`} (termasuk yang **sudah membalas**).
2. Attempt 1 invoice itu **berhasil terkirim** (`max_attempt ≥ 1`).
3. Attempt invoice itu masih **< 3**.
4. **Belum** dikirimi attempt **hari ini** (maks 1 attempt/hari/invoice).
5. **Tidak** ada di `excluded_invoices`.

**Eksekusi:** tiap invoice eligible → **pesan terpisah** (jeda 20–40s) berisi template Attempt 2/3 dengan outlet+invoice-nya → catat `blast_recipients` (attempt N).

> ⚠️ Konsekuensi: satu nomor dengan N invoice bisa menerima sampai N pesan/hari retry. Risiko ban WA → saat ini ditahan oleh `RETRY_ENABLED=false` (manual only).

**Stop total:** begitu nomor di-action agen jadi `done`/`on_going`/`invalid`/`scheduled` → semua invoice nomor itu berhenti.

---

## 7. Flow: Force Close (auto, ~17:00 WIB)
1. Sweep tiap 30 mnt — **selalu jalan** (termasuk saat `RETRY_ENABLED=false`) karena hanya update DB, tidak kirim pesan.
2. Nomor `after_blast`/`in_progress`, attempt tertinggi ≥ 3, **semua invoice-nya sudah tuntas attempt 3** (tidak ada yang pending), dan sudah lewat jam `RETRY_FORCECLOSE_HOUR` (17:00) pada hari Attempt-3 → status → **`force_close`**.
3. Kalau customer membalas sebelum cutoff → thread balik `open` (lepas dari sweep).

---

## 8. Flow: Inbox
**Pesan masuk:** event WA → `resolveSenderPhone` (handle LID) → **skip kalau nomor tak pernah di-blast** → catat `chat_messages` (`in`), `unread++`, thread → `open` (kecuali sudah terminal). Media diunduh async ke folder media.

**Aksi agen di chatbox:**
| Aksi | Efek |
|---|---|
| Kirim balasan | status → `in_progress`, assign ke agen |
| ✓ Done / Resolved | muat closing → kirim → `done` (terkunci) |
| ⚠ Invalid | `invalid` (tanpa kirim) |
| ⏳ On Going | `on_going` |
| 📅 Scheduled | pilih tanggal → `scheduled` + `followup_at` |
| ⛔ Force Close | `force_close` (manual) |
| ↺ Reopen | balik `open` |

**Bucket** = filter per status + jumlah masing-masing + badge unread. Ada quick-reply macro template.

---

## 9. Flow: Log Status Update / "Belum Respons" — PER INVOICE ⭐
1. Baris = per **(nomor, invoice)** dari `blast_recipients`, untuk nomor yang **belum respons** (status ∈ {`after_blast`, `in_progress`, `rejected`, `force_close`}), **kecuali** yang di-exclude.
2. Kolom **Attempt 1/2/3** (per invoice):
   - terkirim → **No Response** · attempt-1 gagal → **Rejected** · belum → **-**
3. Kolom **Rejected** = `reject` bila: (a) Attempt 1 gagal kirim, ATAU (b) invoice sampai Attempt 3 di nomor `force_close`. Alasan di kolom **Info/Alasan**.
4. **Download CSV** + **Export Google Sheet** (tab `Belum Respons` / `Belum Respons Zopoz`).
5. **Blast manual:** chip `Att 2 / Att 3: N eligible` (klikable → detail) + tombol **Run Attempt 2/3**.

> Catatan: filter "belum respons" tetap **per nomor** (balasan WA tak terikat invoice). Begitu nomor membalas/Done/Invalid → SEMUA invoice nomor itu keluar.

---

## 10. Flow: Exclude per invoice ⭐
1. Klik chip `Att N eligible` → modal **Detail Eligible** (daftar invoice yang akan diblast).
2. Tombol **Exclude** per invoice → insert `excluded_invoices`.
3. Efek: invoice **keluar dari antrian retry** (auto & manual) **dan keluar dari "Belum Respons"**.
4. Tombol **Include kembali** untuk membatalkan (reversible).

---

## 11. Flow: Export Google Sheet
- **Riwayat blast** → tab `Blast Log` (majoo) / `Log Blast Zopoz` (env `GSHEET_ZOPOZ_SHEET_NAME`).
- **Belum Respons** → tab `Belum Respons` (majoo) / `Belum Respons Zopoz` (env `ZOPOZ_GSHEET_REPORT_SHEET_NAME`).
- Manual (tombol), **full-snapshot** (clear → tulis ulang), tab auto-dibuat kalau belum ada. Spreadsheet sama untuk kedua suite, tab berbeda.

---

## 12. Template Pesan
- **majoo:** 3 template attempt + closing (brand majoo, hotline 0811-500-460). Env: `TEMPLATE_ATTEMPT_1/2/3`, `INBOX_CLOSING_TEMPLATE`.
- **Zopoz:** template sendiri (brand "Zopreneurs / zopoz"), terpisah penuh. Env: `ZOPOZ_TEMPLATE_ATTEMPT_1/2/3`, `ZOPOZ_INBOX_CLOSING_TEMPLATE`.
- Variabel auto-replace: `{{nama_outlet}}`, `{{nomer_invoice}}`.

---

## 13. Peta Endpoint (ringkas)

| Fungsi | majoo | Zopoz |
|---|---|---|
| Status WA / QR / Logout | `/api/status`, `/api/qr`, `/api/logout` | `/api/zopoz/wa-status`, `/api/zopoz/qr`, `/api/zopoz/wa-logout` |
| Blast + progress + riwayat | `/api/blast`, `/api/progress`, `/api/history` | `/api/zopoz/blast`, `/api/zopoz/progress`, `/api/zopoz/history` |
| Template | `/api/templates` | `/api/zopoz/templates` |
| Inbox thread/pesan/aksi | `/api/inbox/{threads,messages,read,status,reply,media}` | `/api/zopoz/{threads,messages,read,status,reply,media}` |
| Report Belum Respons | `/api/report/{unresponsive,unresponsive.csv,export-sheet}` | `/api/zopoz/report/{...}` |
| Retry preview/run | `/api/retry/{preview,run-now}` | `/api/zopoz/retry/{preview,run-now}` |
| Exclude | `/api/retry/{exclude,include,excluded}` | `/api/zopoz/retry/{exclude,include,excluded}` |
| Sheet status/export riwayat | `/api/sheet-status`, `/api/export-sheet` | `/api/zopoz/sheet-status`, `/api/zopoz/export-sheet` |

---

## 14. Konfigurasi (env penting)
| Env | Default | Fungsi |
|---|---|---|
| `RETRY_ENABLED` | **false** | saklar auto-cron kirim attempt 2/3 |
| `RETRY_WINDOW_HOUR` | 9 | jam kirim retry (WIB) |
| `RETRY_FORCECLOSE_HOUR` | 17 | jam cutoff force-close (WIB) |
| `RETRY_SEND_MIN_DELAY` / `MAX_DELAY` | 20 / 40 | jeda antar kirim (detik) |
| `RETRY_CHECK_INTERVAL_MINUTES` | 30 | interval cek scheduler |
| `ADDR` | :8090 | port backend |
| `GSHEET_*`, `GOOGLE_*` | — | konfigurasi Google Sheets / OAuth |

---

## 15. Infrastruktur & Deploy
- Backend Go di **`:8090`** (proses detached: `nohup ./blast-wa-personal > blast.log 2>&1 & disown`).
- Diekspos via **Cloudflare Tunnel**; URL tunnel di-hardcode di `docs/config.js` (`API_BASE`).
- Frontend `docs/` di-serve same-origin oleh backend **dan** via GitHub Pages (perlu `git push`).
- **Perubahan backend** butuh `go build -o blast-wa-personal ./...` + restart. **Perubahan frontend** langsung live via tunnel (Pages perlu push).
- **JANGAN restart saat ada blast berjalan** (cek: `sqlite3 session/audit.db "SELECT COUNT(*) FROM blast_logs WHERE ended_at IS NULL;"` harus 0).

---

## 16. Catatan Perubahan Penting (riwayat keputusan)
- **Report Belum Respons → per invoice** (sebelumnya per nomor; 1 nomor banyak invoice cuma tampil invoice terakhir).
- **Retry attempt 2/3 → per invoice**; nomor yang sudah membalas (`open`) **tetap** di-retry sampai di-action agen jadi terminal.
- **Force-close** menunggu **semua** invoice nomor tuntas attempt 3 sebelum menutup.
- **Exclude per invoice** menghentikan retry + mengeluarkan dari Belum Respons (reversible).
- Template Zopoz terpisah dari majoo; export Sheet ke tab terpisah.
