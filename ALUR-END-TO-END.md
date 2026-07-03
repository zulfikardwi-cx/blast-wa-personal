# Alur End-to-End — WA Validasi Invoice (Model 2-Nomor + Token)

> Cara kerja tools **setelah** pemisahan blast/inbox (model 2-nomor). Menjelaskan dari upload
> CSV → blast → Attempt 2/3 → balasan pelanggan lewat Kode Referensi → Inbox → Done →
> export Google Sheet. Untuk peta arsitektur & file, lihat `ARCHITECTURE.md`. **Status: 2026-07-02.**

---

## 0. Konsep 2 nomor (wajib dipahami dulu)

| Nomor | Peran | Client | Session store |
|---|---|---|---|
| **BLASTER** | **Kirim** pesan blast (Attempt 1/2/3). Disposable — kalau banned tinggal ganti. | `blasterState.client` | `session/store-blaster.db` |
| **INTI** | **Terima** balasan pelanggan + Inbox + balas + Done. Tidak pernah blast. Dijaga stabil. | `state.client` (nomor majoo lama) | `session/store.db` |

**Kenapa dipisah:** blast massal ke stranger memicu ban WhatsApp. Dengan memisah, yang berisiko
banned hanya nomor **blaster** (bisa diganti); histori percakapan pelanggan aman di **INTI**.
Penghubung antara "pesan dikirim dari blaster" dan "balasan masuk ke INTI" adalah **token**
(Kode Referensi) di dalam link `wa.me`.

> ⚠️ Ini **memindah** risiko ban ke nomor disposable, bukan menghilangkan. Solusi bebas-ban tuntas
> tetap WhatsApp Business API resmi (lihat `ARCHITECTURE.md` §12).

---

## 1. Input: CSV

Upload di halaman **Blast** (`/api/blast`). Header **wajib persis**:

```
phone,nama_outlet,nomer_invoice
```

- `phone` — nomor tujuan (dari dashboard). Otomatis dinormalkan ke `62...` (`08..`/`8..`/`62..` semua diterima).
- `nama_outlet`, `nomer_invoice` — konteks invoice.
- **Tidak ada kolom token.** Token dibuat otomatis oleh sistem.

Syarat kirim: nomor **BLASTER connected** DAN nomor **INTI diketahui** (login atau `INTI_WA_NUMBER`
di `.env`) — kalau INTI tak diketahui, blast ditolak supaya tidak mengirim pesan tanpa link.

---

## 2. Blast Attempt 1 (dikirim dari BLASTER)

Untuk tiap baris CSV (`blast.go` → `runBlast`):

1. Render template **Attempt 1** (`{{nama_outlet}}`, `{{nomer_invoice}}` diisi).
2. `applyLink` menyisipkan `{{link}}`:
   - `getOrCreateToken(phone, invoice, outlet)` → buat token unik 8-char, simpan ke
     **`validation_tokens`** (status `pending`). Token di-**reuse** per (phone,invoice).
   - `buildTriggerLink` → `https://wa.me/<NOMOR_INTI>?text=<teks prefilled + token>`.
3. `sendOne` kirim via **client BLASTER** (cek `IsOnWhatsApp` dulu).
4. Hasil:
   - **Sukses** → `blast_recipients.status=sent`; buat/utak thread INTI (`chat_threads` →
     status `after_blast`, `current_attempt=1`); catat pesan blast sebagai histori `out`.
   - **Gagal karena nomor tidak terdaftar WA** → thread `rejected` (untuk tim WO).
   - **Gagal lain** (koneksi/463/rate-limit) → TIDAK di-reject (biar bisa di-blast ulang).
   - **3× error 463 beruntun** → blast di-abort (sesi blaster bermasalah).

Isi pesan blast (template Attempt 1, bisa override via `TEMPLATE_ATTEMPT_1`):

```
Halo, Majoopreneurs!
...
Nama Outlet: <outlet>
No. Invoice: <invoice>

Apabila Kakak bersedia ... silakan klik link di bawah ini untuk terhubung dengan Tim Validator kami ...:
https://wa.me/<INTI>?text=...        ← {{link}}

Terima kasih! 🙏
```

> Pesan **tidak** lagi meminta "balas pesan ini" — balasan ke blaster percuma. Pelanggan
> diarahkan **klik link** agar terhubung ke INTI.

---

## 3. Pelanggan klik link → chat ke INTI (Kode Referensi)

Klik link membuka WhatsApp pelanggan ke **Nomor INTI** dengan chat **sudah terisi** (prefill,
bisa override via `INTI_PREFILL_TEMPLATE`):

```
Halo,
Saya mau validasi atas invoice :
Nama Outlet: <outlet>
No. Invoice: <invoice>
Kode Referensi: <TOKEN>
```

Pelanggan tekan **Kirim**. Pesan sampai ke INTI → `handleIncomingWA` (`main.go`):

1. **`resolveInboundThread`** menentukan thread invoice:
   - **Utama:** parse baris `Kode Referensi: <TOKEN>` → `lookupToken` → dapat (phone, invoice, outlet) canonical.
   - **Fallback:** kalau token hilang/diedit → cocokkan **nomor pengirim** ke daftar yang pernah di-blast/di-token.
   - Kalau dua-duanya gagal → **stranger, di-skip** (tidak muncul di Inbox).
2. Catat pesan masuk + thread → status **`open`** (unread +1).
3. Kalau pelanggan chat dari **nomor beda** dari yang di-blast, JID pengirim asli disimpan di
   `chat_threads.wa_jid` supaya balasan agent nanti terkirim ke nomor itu.

> Catatan wa.me: token tampak sebagai teks biasa dan **bisa** dihapus pelanggan sebelum kirim →
> itulah gunanya fallback nomor HP.

---

## 4. Inbox (di INTI) — balas manual oleh agent

Halaman **Inbox** (`inbox.html`) menampilkan thread per bucket (Open / In Progress / dst) beserta
konteks invoice (outlet, no. invoice) di samping chat.

- **Balas** (`/api/inbox/reply`) → dikirim dari **INTI** ke `wa_jid` (kalau ada) atau nomor thread
  → status thread → **`in_progress`** (PIC = agent yang balas).
- **Tombol status**: `On Going` (sedang validasi/WA Call), `Scheduled` (validasi hari lain),
  `Invalid` (nomor salah), `Force Close`, `Done`.

**Tidak ada AI auto-reply** — validasi & jawaban murni oleh human (keputusan desain).

---

## 5. Attempt 2 & 3 (retry, dikirim dari BLASTER)

Retry bekerja **per (nomor, invoice)**, direkonstruksi dari `blast_recipients` (`retry.go` +
`retry_invoice.go`). Sebuah invoice **masih diretry** selama:

- thread **belum terminal** — status ∉ {`done`, `invalid`, `on_going`, `scheduled`, `force_close`};
- Attempt 1 invoice itu **sukses** dan attempt saat ini **< 3**;
- **belum** dikirimi attempt **hari ini** (maks 1 attempt/hari/invoice);
- **tidak** ada di `excluded_invoices` dan **tidak** ada di `resolved_invoices`.

Karakteristik:

- Attempt 2/3 kirim via **BLASTER** (`sendRetryOne`), **reuse token yang sama** → link tetap konsisten.
- Template Attempt 2/3 (variasi wording) juga membawa `{{link}}`.
- **Cron harian** jam `RETRY_HOUR` — tapi default `RETRY_ENABLED=false` (auto-send OFF untuk kurangi ban).
  Tombol **Run Attempt 2/3 manual** tetap jalan.
- **Force-close sweep** jam `RETRY_FORCECLOSE_HOUR`: thread `after_blast/in_progress` yang **semua**
  invoice-nya sudah Attempt 3 tanpa respons → status `force_close`.

### Kaitan Attempt 2/3 dengan token (poin utama)

Pemicu keluar dari antrian Attempt 2/3 = **invoice sudah divalidasi (Done)**. Saat Done, invoice
masuk `resolved_invoices` → otomatis **di-exclude** dari `collectInvoiceRetries`, dan token-nya
ditandai `used`. Jadi:

- **Token belum used (belum Done)** → invoice **tetap** masuk antrian Attempt 2 → lalu 3.
- **Token used (sudah Done)** → invoice **tidak** masuk antrian blast berikutnya.

*(Sekadar dibalas pelanggan/`open`/`in_progress` TIDAK menghentikan retry — hanya Done, atau
status manual `on_going/scheduled/invalid/force_close`, atau Attempt 3 habis.)*

---

## 6. Done / Resolved (remarks)

Alur nyata: agent klik **Done** → kirim closing message → `POST /api/inbox/status status=done`
(`handleSetStatus`). Yang terjadi:

1. Thread → **`done`** (terkunci), PIC = **resolver** (yang klik Done).
2. `markPhoneResolved("majoo", …)` → snapshot **semua** invoice nomor itu ke **`resolved_invoices`**
   (permanen; tahan re-blast).
3. `markTokenUsed(phone, "")` → semua token nomor itu → status **`used`** (penanda eksplisit + audit).
4. Closing message dikirim dari **INTI** (ke `wa_jid`/nomor thread). Setelah Done, balasan pelanggan
   tidak mengubah status (locked).

Status terminal lain: `invalid` (nomor salah, terkunci), `force_close` (Attempt 3 lewat cutoff),
`rejected` (Attempt 1 gagal — nomor tidak terdaftar, untuk tim WO).

---

## 7. Export Google Sheet — **TIDAK ADA PERUBAHAN**

Export tetap membaca tabel lama (`blast_logs`, `blast_recipients`, `resolved_invoices`,
`chat_threads`). **`validation_tokens` TIDAK diekspor** (murni internal untuk korelasi). Semantik
kolom & nama tab tidak berubah:

| Tab (default) | Sumber | Isi |
|---|---|---|
| **Blast Log** | `blast_recipients` + `blast_logs` | Audit lengkap tiap kirim (Attempt 1/2/3, sent/failed). |
| **Belum Respons** | per (phone,invoice), thread `after_blast/in_progress/rejected/force_close` | Kolom Attempt 1/2/3 + Rejected + Alasan. Exclude `excluded_invoices` & `resolved_invoices`. |
| **report resolved** | `resolved_invoices` | Nomor Invoice \| Nama Outlet \| Nomor User \| Nama PIC (resolver). |

Yang berbeda hanya **operasional**: pesan yang tercatat kini dikirim dari nomor blaster, dan
Attempt 2/3 tetap tercatat ke `blast_recipients` seperti biasa → laporan konsisten. **Tidak perlu
mengubah tab/kolom di sistem WO eksternal.**

---

## 8. Ringkas satu layar

```
CSV (phone,nama_outlet,nomer_invoice)
      │  POST /api/blast
      ▼
[BLASTER] Attempt 1 ── kirim ──►  WhatsApp pelanggan
      │   • buat token → validation_tokens (pending)
      │   • pesan berisi link wa.me/<INTI>?text=...Kode Referensi:<token>
      │   • thread INTI dibuat: status after_blast, blast_recipients=sent
      │
      ▼  (pelanggan KLIK link)
[INTI] terima chat berisi "Kode Referensi: <token>"
      │   • resolveInboundThread: token → invoice (fallback: nomor HP)
      │   • thread → open  → muncul di INBOX
      ▼
[INBOX] agent balas (dari INTI) → in_progress
      │
      ├─ belum Done & belum 3x?  ─►  [BLASTER] Attempt 2, lalu Attempt 3 (reuse token)
      │
      ▼  agent klik DONE
      • thread → done (locked, PIC=resolver)
      • resolved_invoices  ← keluar dari antrian retry
      • token → used   • closing message dikirim dari INTI
      │
      ▼
EXPORT GOOGLE SHEET (tak berubah): Blast Log · Belum Respons · report resolved
```
