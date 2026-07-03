package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func initChat() error {
	loadClosingTemplate()
	loadAttemptTemplates()
	_, err := auditDB.Exec(`
CREATE TABLE IF NOT EXISTS chat_threads (
	phone TEXT PRIMARY KEY,
	nama_outlet TEXT,
	last_blast_id INTEGER,
	last_message_at TEXT,
	last_message_preview TEXT,
	last_message_direction TEXT,
	unread_count INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT 'open',
	assigned_email TEXT,
	assigned_name TEXT,
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_threads_updated ON chat_threads(updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_threads_status ON chat_threads(status);

-- Kolom retry tracking — di-tambahkan via ALTER IF EXISTS check di Go
-- (SQLite tidak support ADD COLUMN IF NOT EXISTS sebelum versi 3.35)

CREATE TABLE IF NOT EXISTS chat_messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	phone TEXT NOT NULL,
	direction TEXT NOT NULL,
	body TEXT,
	is_media INTEGER NOT NULL DEFAULT 0,
	media_type TEXT,
	wa_message_id TEXT,
	timestamp TEXT NOT NULL,
	blast_log_id INTEGER,
	sender_email TEXT,
	sender_name TEXT,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_messages_phone ON chat_messages(phone, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_wa_id ON chat_messages(wa_message_id) WHERE wa_message_id IS NOT NULL;
`)
	if err != nil {
		return err
	}

	// Migration: tambah kolom retry tracking ke chat_threads kalau belum ada
	addColumns := []struct {
		col, def string
	}{
		{"nomer_invoice", "TEXT"},
		{"current_attempt", "INTEGER NOT NULL DEFAULT 1"},
		{"last_attempt_at", "TEXT"},
		{"team", "TEXT"},
		{"area", "TEXT"},
		// attempt1_failed: blast Attempt 1 gagal kirim (mis. nomor tidak terdaftar WA) →
		// sel Attempt 1 ditandai "Rejected". rejected_at: timestamp saat thread jadi reject
		// (Attempt 1 gagal, atau Attempt 3 tanpa respons s/d jam reject) → kolom Rejected.
		{"attempt1_failed", "INTEGER NOT NULL DEFAULT 0"},
		{"rejected_at", "TEXT"},
		// followup_at: tanggal follow-up untuk thread berstatus 'scheduled' (validasi hari lain).
		{"followup_at", "TEXT"},
		// reject_reason: alasan reject untuk ditampilkan di kolom Info/Alasan Log Status
		// Update — mis. "nomor tidak terdaftar di WhatsApp" (Attempt 1 gagal) atau
		// "Tidak ada respons s/d 16:00 WIB" (Attempt 3).
		{"reject_reason", "TEXT"},
		// wa_jid: JID pengirim asli DI INTI kalau pelanggan chat dari nomor BEDA dari nomor
		// yang di-blast (thread tetap di-key pakai nomor blast/canonical dari token). Reply
		// dikirim ke wa_jid ini bila terisi. Kosong = balas ke `phone` (kasus umum).
		{"wa_jid", "TEXT"},
		// blast_phone: untuk thread 'inbound_non_blast' yang SUDAH dicocokkan agent via Kode
		// Referensi — nomor ASLI yang di-blast (canonical) yang memiliki invoice tsb. Saat Done,
		// resolve pakai nomor ini (bukan nomor chat manual) supaya invoice blast-nya benar-benar
		// keluar dari antrian retry & Belum Respons. Kosong = thread biasa (resolve pakai `phone`).
		{"blast_phone", "TEXT"},
	}
	for _, c := range addColumns {
		_, e := auditDB.Exec(fmt.Sprintf("ALTER TABLE chat_threads ADD COLUMN %s %s", c.col, c.def))
		// abaikan error "duplicate column name" — kolom sudah ada
		if e != nil && !strings.Contains(e.Error(), "duplicate column") {
			return e
		}
	}

	// Migrasi: kolom media_path di chat_messages (path file media yang sudah diunduh).
	if _, e := auditDB.Exec(`ALTER TABLE chat_messages ADD COLUMN media_path TEXT`); e != nil && !strings.Contains(e.Error(), "duplicate column") {
		return e
	}

	if err := backfillFailedThreads(); err != nil {
		// non-fatal: backfill best-effort, jangan halangi startup
		fmt.Println("warn: backfillFailedThreads:", err)
	}
	fixupBackfillRetryLogs()
	migrateRejectedAttempt3ToForceClose()
	return nil
}

// migrateRejectedAttempt3ToForceClose — pindahkan thread lama yang di-'reject' karena
// Attempt 3 tanpa respons (attempt1_failed=0) ke bucket 'force_close', sesuai perubahan
// aturan (auto-close Attempt 3 → Force Close, bukan rejected). Thread 'rejected' karena
// GAGAL kirim Attempt 1 (attempt1_failed=1) TIDAK disentuh — tetap rejected utk tim WO.
// Idempoten: setelah jalan, tak ada lagi rejected+attempt1_failed=0 → run berikutnya no-op.
func migrateRejectedAttempt3ToForceClose() {
	res, err := auditDB.Exec(`UPDATE chat_threads SET status='force_close', updated_at=datetime('now') WHERE status='rejected' AND COALESCE(attempt1_failed,0)=0`)
	if err != nil {
		fmt.Println("warn: migrateRejectedAttempt3ToForceClose:", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		fmt.Printf("migrasi: %d thread rejected (Attempt 3) → force_close\n", n)
	}
}

// fixupBackfillRetryLogs — betulkan entri Riwayat hasil backfill retry lama: isi kolom
// attempt (2/3) dan ganti template label ("Retry Attempt N (auto, backfill ...)") jadi
// template attempt ASLI (raw, dengan {{...}}) supaya tampil konsisten dgn attempt 1.
// Idempoten: setelah template di-update, tidak lagi cocok pola LIKE → run berikutnya no-op.
func fixupBackfillRetryLogs() {
	rows, err := auditDB.Query(`SELECT id, template FROM blast_logs WHERE template LIKE 'Retry Attempt % (auto, backfill %'`)
	if err != nil {
		return
	}
	type fr struct {
		id  int64
		tpl string
	}
	var list []fr
	for rows.Next() {
		var f fr
		if rows.Scan(&f.id, &f.tpl) == nil {
			list = append(list, f)
		}
	}
	rows.Close()

	fixed := 0
	for _, f := range list {
		var att int
		fmt.Sscanf(f.tpl, "Retry Attempt %d", &att)
		if att != 2 && att != 3 {
			continue
		}
		if _, e := auditDB.Exec(`UPDATE blast_logs SET attempt=?, template=? WHERE id=?`, att, GetAttemptTemplate(att), f.id); e != nil {
			fmt.Println("warn: fixupBackfillRetryLogs:", e)
			continue
		}
		fixed++
	}
	if fixed > 0 {
		fmt.Printf("fixup: %d entri Riwayat retry backfill dibetulkan (attempt + template asli)\n", fixed)
	}
}

// backfillFailedThreads — buat thread 'rejected' untuk recipient blast yang berstatus
// 'failed' tapi BELUM punya thread (kasus blast lama, sebelum logic Attempt-1-gagal ada).
// Idempoten: hanya insert kalau phone belum ada di chat_threads (NOT EXISTS), jadi aman
// dijalankan tiap startup & tidak menimpa thread yang sudah pernah direspons/sukses.
// Ambil baris failed TERBARU per phone (MAX(id)) untuk alasan & data outlet.
func backfillFailedThreads() error {
	res, err := auditDB.Exec(`
INSERT INTO chat_threads
	(phone, nama_outlet, nomer_invoice, last_blast_id, last_message_at, last_message_preview,
	 last_message_direction, status, unread_count, current_attempt, last_attempt_at,
	 attempt1_failed, rejected_at, reject_reason, created_at, updated_at)
SELECT r.phone, r.nama_outlet, r.nomer_invoice, r.blast_log_id,
	COALESCE(NULLIF(r.sent_at,''), r.created_at),
	'[GAGAL KIRIM] ' || COALESCE(r.error,''),
	'out', 'rejected', 0, 1,
	COALESCE(NULLIF(r.sent_at,''), r.created_at),
	1, datetime('now'),
	COALESCE(NULLIF(r.error,''), 'Gagal kirim'),
	r.created_at, datetime('now')
FROM blast_recipients r
JOIN (SELECT phone, MAX(id) AS maxid FROM blast_recipients
      WHERE status='failed'
        -- HANYA nomor yang benar-benar tidak terdaftar di WA yang jadi thread 'rejected'.
        -- Kegagalan lain (koneksi putus/logout, error 463 device baru, rate-limit, server)
        -- = nomor valid, gagal transport/anti-spam sementara → jangan jadi rejected, biar
        -- bisa di-blast ulang bersih.
        AND COALESCE(error,'') LIKE '%tidak terdaftar%'
      GROUP BY phone) m
	ON r.id = m.maxid
WHERE NOT EXISTS (SELECT 1 FROM chat_threads t WHERE t.phone = r.phone)`)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		fmt.Printf("backfill: %d nomor failed (blast lama) → thread 'rejected' dibuat\n", n)
	}
	return nil
}

// ---- 3 template attempts (sesuai spec user) ----
// Override via env: TEMPLATE_ATTEMPT_1 / TEMPLATE_ATTEMPT_2 / TEMPLATE_ATTEMPT_3
// Pakai \n untuk newline kalau di-set via env.

var attemptTemplates [3]string

// CATATAN ARSITEKTUR 2-NOMOR: pesan ini dikirim dari nomor BLASTER (disposable). Pelanggan
// TIDAK boleh diminta "balas pesan ini" (balasan ke blaster tak ditangani & nomornya bisa
// diganti). Sebaliknya, mereka diarahkan klik {{link}} → chat ke nomor INTI (inbound-only)
// yang membawa Kode Referensi (token). {{link}} di-render per (phone,invoice) via applyLink.
func loadAttemptTemplates() {
	attemptTemplates[0] = pickTemplate("TEMPLATE_ATTEMPT_1", `Halo, Majoopreneurs!

Terima kasih telah berlangganan aplikasi majoo.
Untuk menjaga keakuratan dan keamanan data, kami perlu melakukan validasi untuk Invoice berikut:

Nama Outlet: {{nama_outlet}}
No. Invoice: {{nomer_invoice}}

Apabila Kakak bersedia mengikuti proses validasi, silakan klik link di bawah ini atau chat ke nomer 085119016132 dengan memasukkan Kode Referensi {{kode_referensi}} untuk terhubung dengan Tim Validator kami (jam operasional 09.00–16.00 WIB):
{{link}}

NOTES : Mohon untuk tidak membalas pesan ini, klik link diatas untuk mendapatkan antrian validasi anda.

Terima kasih! 🙏`)

	attemptTemplates[1] = pickTemplate("TEMPLATE_ATTEMPT_2", `Halo, Majoopreneurs!

Mohon maaf, ingin melakukan konfirmasi kembali
Untuk menjaga keakuratan dan keamanan data, kami perlu melakukan validasi untuk Invoice berikut:

Nama Outlet: {{nama_outlet}}
No. Invoice: {{nomer_invoice}}

Apabila Kakak bersedia mengikuti proses validasi, silakan klik link di bawah ini atau chat ke nomer 085119016132 dengan memasukkan Kode Referensi {{kode_referensi}} untuk terhubung dengan Tim Validator kami (jam operasional 09.00–16.00 WIB):
{{link}}

NOTES : Mohon untuk tidak membalas pesan ini, klik link diatas untuk mendapatkan antrian validasi anda.

Terima kasih! 🙏`)

	attemptTemplates[2] = pickTemplate("TEMPLATE_ATTEMPT_3", `Halo, Majoopreneurs!

Izin follow up kembali
Untuk menjaga keakuratan dan keamanan data, kami perlu melakukan validasi untuk Invoice berikut:

Nama Outlet: {{nama_outlet}}
No. Invoice: {{nomer_invoice}}

Apabila Kakak bersedia mengikuti proses validasi, silakan klik link di bawah ini atau chat ke nomer 085119016132 dengan memasukkan Kode Referensi {{kode_referensi}} untuk terhubung dengan Tim Validator kami (jam operasional 09.00–16.00 WIB):
{{link}}

NOTES : Mohon untuk tidak membalas pesan ini, klik link diatas untuk mendapatkan antrian validasi anda.

Jika Kakak masih belum menghubungi kami, maka penjadwalan kami tutup. Jika terdapat permintaan dan informasi lainnya, silahkan menghubungi Hotline Majoo pada nomer 0811-500-460

Terima kasih! 🙏`)
}

func pickTemplate(envKey, def string) string {
	t := os.Getenv(envKey)
	if t == "" {
		return def
	}
	return strings.ReplaceAll(t, `\n`, "\n")
}

// GetAttemptTemplate — return template untuk attempt N (1, 2, 3). Default ke attempt 1 kalau N out of range.
func GetAttemptTemplate(attempt int) string {
	if attempt < 1 || attempt > 3 {
		attempt = 1
	}
	return attemptTemplates[attempt-1]
}

// renderTemplateWithVars — substitusi {{nama_outlet}} dan {{nomer_invoice}}
func renderTemplateWithVars(tpl, namaOutlet, nomerInvoice string) string {
	out := tpl
	out = strings.ReplaceAll(out, "{{nama_outlet}}", namaOutlet)
	out = strings.ReplaceAll(out, "{{nomer_invoice}}", nomerInvoice)
	return out
}

// resolveSenderPhone — ambil phone number dari incoming MessageInfo.
// Handle LID (@lid) → phone (@s.whatsapp.net) mapping yang muncul di WA versi terbaru
// untuk privacy. Urutan resolve:
//  1. Kalau Sender server = s.whatsapp.net → langsung pakai User
//  2. Kalau SenderAlt server = s.whatsapp.net → pakai SenderAlt.User
//  3. Kalau Sender adalah @lid, lookup via whatsmeow LID store
//  4. Kalau gagal semua, return ""
func resolveSenderPhone(info types.MessageInfo) string {
	if info.Sender.Server == types.DefaultUserServer && info.Sender.User != "" {
		return info.Sender.User
	}
	if info.SenderAlt.Server == types.DefaultUserServer && info.SenderAlt.User != "" {
		return info.SenderAlt.User
	}
	// LID lookup — kalau whatsmeow punya mapping cached
	if info.Sender.Server == types.HiddenUserServer && state.client != nil && state.client.Store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if lids := state.client.Store.LIDs; lids != nil {
			if pnJID, err := lids.GetPNForLID(ctx, info.Sender); err == nil && pnJID.User != "" {
				return pnJID.User
			}
		}
	}
	return ""
}

// isPhoneBlasted — return true kalau nomor pernah jadi target blast.
// Dipakai filter incoming message: kalau ada chat dari nomor random yang tidak pernah
// di-blast, skip (tidak muncul di inbox).
func isPhoneBlasted(phone string) bool {
	var c int
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads WHERE phone = ?`, phone).Scan(&c)
	return c > 0
}

// upsertThreadBlast — dipanggil saat BLAST outgoing (attempt 1, manual user action).
// State machine: blast SELALU set status=after_blast (termasuk reset dari done).
// Reset current_attempt=1, last_attempt_at=now untuk fresh retry tracking.
func upsertThreadBlast(phone, namaOutlet, nomerInvoice string, blastID int64, preview string, ts time.Time) error {
	tsStr := ts.Format(time.RFC3339)
	prev := truncate(preview, 80)
	_, err := auditDB.Exec(`
INSERT INTO chat_threads (phone, nama_outlet, nomer_invoice, last_blast_id, last_message_at, last_message_preview, last_message_direction, status, assigned_email, assigned_name, unread_count, current_attempt, last_attempt_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, 'out', 'after_blast', NULL, NULL, 0, 1, ?, ?)
ON CONFLICT(phone) DO UPDATE SET
	nama_outlet = COALESCE(NULLIF(excluded.nama_outlet, ''), nama_outlet),
	nomer_invoice = COALESCE(NULLIF(excluded.nomer_invoice, ''), nomer_invoice),
	last_blast_id = COALESCE(excluded.last_blast_id, last_blast_id),
	last_message_at = excluded.last_message_at,
	last_message_preview = excluded.last_message_preview,
	last_message_direction = 'out',
	status = 'after_blast',
	assigned_email = NULL,
	assigned_name = NULL,
	unread_count = 0,
	current_attempt = 1,
	last_attempt_at = excluded.last_attempt_at,
	updated_at = excluded.updated_at`,
		phone, namaOutlet, nomerInvoice, nullableID(blastID), tsStr, prev, tsStr, tsStr)
	return err
}

// upsertThreadBlastFailed — dipanggil saat BLAST Attempt 1 GAGAL kirim (mis. nomor tidak
// terdaftar di WhatsApp). Tandai thread langsung sebagai 'rejected' supaya muncul di tab
// Log Status Update dengan sel Attempt 1 = "Rejected" + kolom Rejected = "reject" (untuk
// tim WO melakukan reject). status 'rejected' membuatnya OTOMATIS keluar dari antrian
// auto-retry (query retry hanya after_blast/in_progress). Tidak menimpa thread yang sudah
// pernah direspons / sedang ditangani / selesai (open/in_progress/on_going/done).
func upsertThreadBlastFailed(phone, namaOutlet, nomerInvoice string, blastID int64, errMsg string, ts time.Time) error {
	tsStr := ts.Format(time.RFC3339)
	prev := truncate("[GAGAL KIRIM] "+errMsg, 80)
	reason := errMsg
	if reason == "" {
		reason = "Gagal kirim"
	}
	_, err := auditDB.Exec(`
INSERT INTO chat_threads (phone, nama_outlet, nomer_invoice, last_blast_id, last_message_at, last_message_preview, last_message_direction, status, assigned_email, assigned_name, unread_count, current_attempt, last_attempt_at, attempt1_failed, rejected_at, reject_reason, updated_at)
VALUES (?, ?, ?, ?, ?, ?, 'out', 'rejected', NULL, NULL, 0, 1, ?, 1, ?, ?, ?)
ON CONFLICT(phone) DO UPDATE SET
	nama_outlet = COALESCE(NULLIF(excluded.nama_outlet, ''), nama_outlet),
	nomer_invoice = COALESCE(NULLIF(excluded.nomer_invoice, ''), nomer_invoice),
	last_blast_id = COALESCE(excluded.last_blast_id, last_blast_id),
	last_message_at = excluded.last_message_at,
	last_message_preview = excluded.last_message_preview,
	last_message_direction = 'out',
	status = CASE WHEN status IN ('open','in_progress','on_going','done') THEN status ELSE 'rejected' END,
	attempt1_failed = CASE WHEN status IN ('open','in_progress','on_going','done') THEN attempt1_failed ELSE 1 END,
	current_attempt = CASE WHEN status IN ('open','in_progress','on_going','done') THEN current_attempt ELSE 1 END,
	rejected_at = CASE WHEN status IN ('open','in_progress','on_going','done') THEN rejected_at ELSE excluded.rejected_at END,
	reject_reason = CASE WHEN status IN ('open','in_progress','on_going','done') THEN reject_reason ELSE excluded.reject_reason END,
	updated_at = excluded.updated_at`,
		phone, namaOutlet, nomerInvoice, nullableID(blastID), tsStr, prev, tsStr, tsStr, reason, tsStr)
	return err
}

// upsertThreadRetry — dipanggil saat scheduler kirim attempt 2/3.
// HANYA update last_message + current_attempt + last_attempt_at. Tidak ubah status.
func upsertThreadRetry(phone, preview string, attemptNum int, ts time.Time) error {
	tsStr := ts.Format(time.RFC3339)
	prev := truncate(preview, 80)
	_, err := auditDB.Exec(`
UPDATE chat_threads SET
	last_message_at = ?,
	last_message_preview = ?,
	last_message_direction = 'out',
	current_attempt = ?,
	last_attempt_at = ?,
	updated_at = ?
WHERE phone = ?`, tsStr, prev, attemptNum, tsStr, tsStr, phone)
	return err
}

// upsertThreadAgentReply — dipanggil saat AGENT balas via inbox web.
// State: → in_progress (assigned ke user yang reply). Done LOCKED — tidak berubah.
func upsertThreadAgentReply(phone, preview, agentEmail, agentName string, ts time.Time) error {
	tsStr := ts.Format(time.RFC3339)
	prev := truncate(preview, 80)
	_, err := auditDB.Exec(`
UPDATE chat_threads SET
	last_message_at = ?,
	last_message_preview = ?,
	last_message_direction = 'out',
	-- Non-blast (inbound_non_blast/outside_blast) TIDAK dipromosikan ke in_progress supaya bar
	-- "Cocokkan Kode Referensi" tetap muncul sampai benar-benar dicocokkan/di-tag. Agent yang
	-- balas tetap dicatat sebagai PIC.
	status = CASE WHEN status IN ('done','invalid','on_going','scheduled','inbound_non_blast','outside_blast') THEN status ELSE 'in_progress' END,
	assigned_email = CASE WHEN status IN ('done','invalid','on_going','scheduled') THEN assigned_email ELSE ? END,
	assigned_name = CASE WHEN status IN ('done','invalid','on_going','scheduled') THEN assigned_name ELSE ? END,
	updated_at = ?
WHERE phone = ?`, tsStr, prev, agentEmail, agentName, tsStr, phone)
	return err
}

// upsertThreadIncoming — dipanggil saat incoming reply dari user.
// State machine:
//   - done → STAY done (locked, tidak respons reply user)
//   - after_blast / in_progress / open → pindah ke open
func upsertThreadIncoming(phone, preview string, ts time.Time) error {
	tsStr := ts.Format(time.RFC3339)
	prev := truncate(preview, 80)
	_, err := auditDB.Exec(`
UPDATE chat_threads SET
	last_message_at = ?,
	last_message_preview = ?,
	last_message_direction = 'in',
	unread_count = unread_count + 1,
	status = CASE WHEN status IN ('done','invalid','on_going','scheduled') THEN status ELSE 'open' END,
	updated_at = ?
WHERE phone = ?`, tsStr, prev, tsStr, phone)
	return err
}

// setThreadReplyJID — simpan nomor pengirim asli (wa_jid) saat pelanggan chat ke INTI dari
// nomor BEDA dari yang di-blast. Balasan agent dikirim ke sini (lihat replyTargetPhone).
func setThreadReplyJID(phone, waJID string) error {
	_, err := auditDB.Exec(`UPDATE chat_threads SET wa_jid = ? WHERE phone = ?`, waJID, phone)
	return err
}

// threadByReplyJID — cari thread canonical yang wa_jid-nya = nomor pengirim (customer chat
// dari nomor beda dari yang di-blast). Dipakai supaya pesan lanjutan tanpa token ikut ke
// thread yang benar, bukan bikin thread baru. "" kalau tak ada.
func threadByReplyJID(waJID string) string {
	var phone string
	_ = auditDB.QueryRow(`SELECT phone FROM chat_threads WHERE wa_jid = ? ORDER BY updated_at DESC LIMIT 1`, waJID).Scan(&phone)
	return phone
}

// replyTargetPhone — nomor tujuan balas untuk sebuah thread: wa_jid kalau terisi (pelanggan
// chat dari nomor lain), else phone (kasus umum).
func replyTargetPhone(phone string) string {
	var waJID sql.NullString
	if err := auditDB.QueryRow(`SELECT wa_jid FROM chat_threads WHERE phone = ?`, phone).Scan(&waJID); err == nil && waJID.Valid && waJID.String != "" {
		return waJID.String
	}
	return phone
}

// threadStatus — status thread sebuah nomor, "" kalau belum ada thread.
func threadStatus(phone string) string {
	var s string
	_ = auditDB.QueryRow(`SELECT status FROM chat_threads WHERE phone = ?`, phone).Scan(&s)
	return s
}

// upsertThreadInboundNonBlast — chat MANUAL masuk dari nomor yang TIDAK bisa dikaitkan ke
// invoice (tak ada token valid & bukan nomor yang di-blast). Ditampung di bucket
// 'inbound_non_blast' (bukan di-skip). Agent lalu minta Kode Referensi ke customer &
// mencocokkannya (handleMatchToken). Thread di-key pakai nomor pengirim.
func upsertThreadInboundNonBlast(phone, preview string, ts time.Time) error {
	tsStr := ts.Format(time.RFC3339)
	prev := truncate(preview, 80)
	_, err := auditDB.Exec(`
INSERT INTO chat_threads (phone, last_message_at, last_message_preview, last_message_direction, unread_count, status, created_at, updated_at)
VALUES (?, ?, ?, 'in', 1, 'inbound_non_blast', ?, ?)
ON CONFLICT(phone) DO UPDATE SET
	last_message_at = excluded.last_message_at,
	last_message_preview = excluded.last_message_preview,
	last_message_direction = 'in',
	unread_count = unread_count + 1,
	status = CASE WHEN status IN ('done','invalid','outside_blast') THEN status ELSE 'inbound_non_blast' END,
	updated_at = excluded.updated_at
WHERE phone = ?`, phone, tsStr, prev, tsStr, tsStr, phone)
	return err
}

func recordChatMessage(phone, direction, body, mediaType, waMsgID string, ts time.Time, blastID int64, senderEmail, senderName string) error {
	isMedia := 0
	if mediaType != "" {
		isMedia = 1
	}
	_, err := auditDB.Exec(`
INSERT OR IGNORE INTO chat_messages (phone, direction, body, is_media, media_type, wa_message_id, timestamp, blast_log_id, sender_email, sender_name)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		phone, direction, body, isMedia, nullableStr(mediaType), nullableStr(waMsgID),
		ts.Format(time.RFC3339), nullableID(blastID), nullableStr(senderEmail), nullableStr(senderName))
	return err
}

// ---- handlers ----

type ThreadRow struct {
	Phone         string `json:"phone"`
	NamaOutlet    string `json:"nama_outlet"`
	NomerInvoice  string `json:"nomer_invoice"`
	LastBlastID   int64  `json:"last_blast_id"`
	LastMessageAt string `json:"last_message_at"`
	LastPreview   string `json:"last_preview"`
	LastDirection string `json:"last_direction"`
	UnreadCount   int    `json:"unread_count"`
	Status        string `json:"status"`
	AssignedEmail string `json:"assigned_email"`
	AssignedName  string `json:"assigned_name"`
	Team          string `json:"team"`
	Area          string `json:"area"`
	FollowupAt    string `json:"followup_at"`
}

func handleThreads(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	team := r.URL.Query().Get("team")

	// build WHERE dinamis dari status + team (filter helper per team)
	var conds []string
	var qargs []any
	if status != "" && status != "all" {
		conds = append(conds, "status = ?")
		qargs = append(qargs, status)
	}
	if team != "" && team != "all" {
		conds = append(conds, "team = ?")
		qargs = append(qargs, team)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	// Urut by last_message_at (waktu pesan asli), BUKAN updated_at — supaya buka/klik
	// thread (yang hanya bump updated_at via mark-read) tidak mengubah urutan. Pesan
	// terbaru tetap di atas; tiebreak phone biar deterministik (stabil saat re-fetch).
	q := `SELECT phone, COALESCE(nama_outlet,''), COALESCE(nomer_invoice,''), COALESCE(last_blast_id,0), COALESCE(last_message_at,''), COALESCE(last_message_preview,''), COALESCE(last_message_direction,''), unread_count, status, COALESCE(assigned_email,''), COALESCE(assigned_name,''), COALESCE(team,''), COALESCE(area,''), COALESCE(followup_at,'') FROM chat_threads ` + where + ` ORDER BY last_message_at DESC, phone ASC LIMIT 200`

	rows, err := auditDB.Query(q, qargs...)
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	defer rows.Close()

	var out []ThreadRow
	totals := map[string]int{"open": 0, "in_progress": 0, "done": 0, "unread": 0}
	for rows.Next() {
		var t ThreadRow
		if err := rows.Scan(&t.Phone, &t.NamaOutlet, &t.NomerInvoice, &t.LastBlastID, &t.LastMessageAt, &t.LastPreview, &t.LastDirection, &t.UnreadCount, &t.Status, &t.AssignedEmail, &t.AssignedName, &t.Team, &t.Area, &t.FollowupAt); err != nil {
			continue
		}
		out = append(out, t)
	}

	// summary counts
	var cAfter, cOpen, cProg, cOnGoing, cForce, cDone, cInvalid, cScheduled, cUnread, cNonBlast, cOutside int
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads WHERE status = 'inbound_non_blast'`).Scan(&cNonBlast)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads WHERE status = 'outside_blast'`).Scan(&cOutside)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads WHERE status = 'after_blast'`).Scan(&cAfter)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads WHERE status = 'open'`).Scan(&cOpen)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads WHERE status = 'in_progress'`).Scan(&cProg)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads WHERE status = 'on_going'`).Scan(&cOnGoing)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads WHERE status = 'force_close'`).Scan(&cForce)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads WHERE status = 'done'`).Scan(&cDone)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads WHERE status = 'invalid'`).Scan(&cInvalid)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads WHERE status = 'scheduled'`).Scan(&cScheduled)
	_ = auditDB.QueryRow(`SELECT COALESCE(SUM(unread_count), 0) FROM chat_threads`).Scan(&cUnread)
	totals["after_blast"] = cAfter
	totals["open"] = cOpen
	totals["in_progress"] = cProg
	totals["on_going"] = cOnGoing
	totals["force_close"] = cForce
	totals["done"] = cDone
	totals["invalid"] = cInvalid
	totals["scheduled"] = cScheduled
	totals["inbound_non_blast"] = cNonBlast
	totals["outside_blast"] = cOutside
	totals["unread"] = cUnread

	// daftar team (distinct) untuk dropdown filter — lintas semua bucket biar stabil
	teams := []string{}
	if trows, e := auditDB.Query(`SELECT DISTINCT team FROM chat_threads WHERE team IS NOT NULL AND team != '' ORDER BY team`); e == nil {
		for trows.Next() {
			var tm string
			if trows.Scan(&tm) == nil {
				teams = append(teams, tm)
			}
		}
		trows.Close()
	}

	writeJSON(w, map[string]any{"threads": out, "counts": totals, "teams": teams})
}

type MessageRow struct {
	ID          int64  `json:"id"`
	Direction   string `json:"direction"`
	Body        string `json:"body"`
	IsMedia     bool   `json:"is_media"`
	MediaType   string `json:"media_type"`
	MediaURL    string `json:"media_url"` // path media (frontend prepend API_BASE); "" kalau belum/ tidak ada
	Timestamp   string `json:"timestamp"`
	SenderEmail string `json:"sender_email"`
	SenderName  string `json:"sender_name"`
}

func handleMessages(w http.ResponseWriter, r *http.Request) {
	phone := r.URL.Query().Get("phone")
	if phone == "" {
		httpErr(w, 400, "phone required")
		return
	}
	rows, err := auditDB.Query(`SELECT id, direction, COALESCE(body,''), is_media, COALESCE(media_type,''), COALESCE(media_path,''), timestamp, COALESCE(sender_email,''), COALESCE(sender_name,'') FROM chat_messages WHERE phone = ? ORDER BY id ASC LIMIT 500`, phone)
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	defer rows.Close()
	var out []MessageRow
	for rows.Next() {
		var m MessageRow
		var isMedia int
		var mediaPath string
		if err := rows.Scan(&m.ID, &m.Direction, &m.Body, &isMedia, &m.MediaType, &mediaPath, &m.Timestamp, &m.SenderEmail, &m.SenderName); err != nil {
			continue
		}
		m.IsMedia = isMedia == 1
		// Hanya kasih media_url kalau file-nya sudah benar-benar terunduh & tersimpan.
		if mediaPath != "" {
			m.MediaURL = mediaURLPath(m.ID)
		}
		out = append(out, m)
	}

	// fetch thread meta
	var nama, status, assignedEmail, assignedName, followupAt, nomerInvoice, blastPhone string
	_ = auditDB.QueryRow(`SELECT COALESCE(nama_outlet,''), status, COALESCE(assigned_email,''), COALESCE(assigned_name,''), COALESCE(followup_at,''), COALESCE(nomer_invoice,''), COALESCE(blast_phone,'') FROM chat_threads WHERE phone = ?`, phone).Scan(&nama, &status, &assignedEmail, &assignedName, &followupAt, &nomerInvoice, &blastPhone)

	writeJSON(w, map[string]any{
		"phone":            phone,
		"nama_outlet":      nama,
		"status":           status,
		"assigned_email":   assignedEmail,
		"assigned_name":    assignedName,
		"followup_at":      followupAt,
		"nomer_invoice":    nomerInvoice,
		"blast_phone":      blastPhone,
		"messages":         out,
		"closing_template": renderClosingTemplate(nama),
	})
}

func handleMarkRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	phone := r.URL.Query().Get("phone")
	if phone == "" {
		httpErr(w, 400, "phone required")
		return
	}
	if _, err := auditDB.Exec(`UPDATE chat_threads SET unread_count = 0, updated_at = datetime('now') WHERE phone = ?`, phone); err != nil {
		httpErr(w, 500, "update: %v", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func handleSetStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	phone := r.URL.Query().Get("phone")
	if phone == "" {
		httpErr(w, 400, "phone required")
		return
	}
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		// Bukan multipart? fallback ke URL-encoded
		if err := r.ParseForm(); err != nil {
			httpErr(w, 400, "form: %v", err)
			return
		}
	}
	status := r.FormValue("status")
	if status != "open" && status != "in_progress" && status != "done" && status != "invalid" && status != "on_going" && status != "force_close" && status != "scheduled" && status != "outside_blast" {
		httpErr(w, 400, "status invalid (open|in_progress|done|invalid|on_going|force_close|scheduled|outside_blast)")
		return
	}
	user, _ := userFromCtx(r.Context())

	// Auto-assign PIC ke user yang klik:
	//   - in_progress / on_going / scheduled → PIC penangan.
	//   - done → PIC = orang yang menyelesaikan (resolver). Alur Done di inbox lewat sini
	//     (sendReply → setStatus 'done'), jadi resolver DICATAT di sini (bukan di-clear).
	//   - open / invalid / force_close → clear (netral). Re-open otomatis melepas PIC.
	var assignedEmail, assignedName sql.NullString
	if status == "in_progress" || status == "on_going" || status == "scheduled" || status == "done" {
		assignedEmail = sql.NullString{String: user.Email, Valid: true}
		assignedName = sql.NullString{String: user.Name, Valid: true}
	} else {
		assignedEmail = sql.NullString{}
		assignedName = sql.NullString{}
	}

	// followup_at hanya untuk 'scheduled' (tanggal kapan perlu follow-up). Status lain → NULL.
	var followup sql.NullString
	if status == "scheduled" {
		if f := strings.TrimSpace(r.FormValue("followup_at")); f != "" {
			followup = sql.NullString{String: f, Valid: true}
		}
	}

	if _, err := auditDB.Exec(`UPDATE chat_threads SET status = ?, assigned_email = ?, assigned_name = ?, followup_at = ?, updated_at = datetime('now') WHERE phone = ?`,
		status, assignedEmail, assignedName, followup, phone); err != nil {
		httpErr(w, 500, "update: %v", err)
		return
	}

	// Saat thread di-Done: tandai SEMUA invoice nomor ini resolved (permanen) — sumber PIC
	// report "report resolved" + exclude dari antrian retry walau di-blast ulang nanti.
	if status == "done" {
		// Thread hasil match manual (inbound_non_blast) menyimpan blast_phone = nomor ASLI yang
		// di-blast. Resolve pakai itu supaya invoice blast-nya benar-benar keluar dari retry/Belum
		// Respons (nomor chat manual biasanya tak punya blast_recipients sendiri).
		resolvePhone := phone
		var bp string
		_ = auditDB.QueryRow(`SELECT COALESCE(blast_phone,'') FROM chat_threads WHERE phone=?`, phone).Scan(&bp)
		if bp != "" {
			resolvePhone = bp
		}
		markPhoneResolved("majoo", "blast_recipients", "blast_logs", resolvePhone, user.Email, user.Name, time.Now())
		// Token validasi → used (dok: token used saat Done). Keluar dari antrian blast.
		markTokenUsed(resolvePhone, "")
	}

	writeJSON(w, map[string]any{"ok": true})
}

// handleMatchToken — agent mencocokkan chat MANUAL (bucket inbound_non_blast) ke invoice via
// Kode Referensi. Input: phone (thread) + code. Lookup token → tempel invoice/outlet + nomor
// blast asli (blast_phone) ke thread, pindahkan ke in_progress + assign agent.
func handleMatchToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	phone := r.URL.Query().Get("phone")
	if phone == "" {
		httpErr(w, 400, "phone required")
		return
	}
	// FormData dari browser = multipart → ParseMultipartForm (ParseForm saja tak baca body ini).
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		if err := r.ParseForm(); err != nil {
			httpErr(w, 400, "form: %v", err)
			return
		}
	}
	raw := strings.TrimSpace(r.FormValue("code"))
	if raw == "" {
		httpErr(w, 400, "Kode Referensi kosong")
		return
	}
	// Terima kode polos ("ABCD2345") atau baris lengkap ("Kode Referensi: ABCD2345").
	code := strings.ToUpper(raw)
	if tok := parseTokenFromBody(raw); tok != "" {
		code = tok
	}
	blastPhone, invoice, outlet, ok := lookupToken(code)
	if !ok {
		httpErr(w, 404, "Kode Referensi '%s' tidak ditemukan di data blast.", code)
		return
	}
	user, _ := userFromCtx(r.Context())
	// Setelah dicocokkan, thread jadi thread normal — bucket ikut arah pesan TERAKHIR:
	// pesan terakhir dari customer ('in') → open; dari agent ('out') → in_progress.
	if _, err := auditDB.Exec(`UPDATE chat_threads SET
		nomer_invoice = ?, nama_outlet = COALESCE(NULLIF(?, ''), nama_outlet),
		blast_phone = ?,
		status = CASE WHEN last_message_direction = 'in' THEN 'open' ELSE 'in_progress' END,
		assigned_email = ?, assigned_name = ?, updated_at = datetime('now')
		WHERE phone = ?`,
		invoice, outlet, blastPhone, user.Email, user.Name, phone); err != nil {
		httpErr(w, 500, "update: %v", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "nomer_invoice": invoice, "nama_outlet": outlet, "blast_phone": blastPhone})
}

// handleTemplates — return 3 attempt templates untuk preview di UI.
func handleTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"attempt_1":         attemptTemplates[0],
		"attempt_2":         attemptTemplates[1],
		"attempt_3":         attemptTemplates[2],
		"retry_window_hour": retryWindowHour,
		"closing":           closingTemplate,
	})
}

func handleReply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	_, loggedIn, connected := state.snapshot()
	if !loggedIn || !connected {
		httpErr(w, 400, "WhatsApp belum terhubung")
		return
	}
	phone := r.URL.Query().Get("phone")
	if phone == "" {
		httpErr(w, 400, "phone required")
		return
	}
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		if err := r.ParseForm(); err != nil {
			httpErr(w, 400, "form: %v", err)
			return
		}
	}
	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		httpErr(w, 400, "body kosong")
		return
	}
	user, _ := userFromCtx(r.Context())

	// Cek status thread — kalau done, reply masih boleh tapi status tidak berubah
	// (sesuai spec: Done lock). Balas ke wa_jid kalau pelanggan chat dari nomor beda.
	jid := types.NewJID(replyTargetPhone(phone), types.DefaultUserServer)
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	msg := &waProto.Message{Conversation: proto.String(body)}
	res, err := state.client.SendMessage(ctx, jid, msg)
	if err != nil {
		httpErr(w, 500, "send: %v", err)
		return
	}

	now := time.Now()
	if e := recordChatMessage(phone, "out", body, "", res.ID, now, 0, user.Email, user.Name); e != nil {
		fmt.Println("warn: recordChatMessage:", e)
	}
	// State transition: agent reply → in_progress (kecuali kalau sudah done)
	if e := upsertThreadAgentReply(phone, body, user.Email, user.Name, now); e != nil {
		fmt.Println("warn: upsertThreadAgentReply:", e)
	}

	writeJSON(w, map[string]any{"ok": true, "id": res.ID})
}

// handleResolve — set status=done + kirim closing message.
// Sesuai spec: setelah done, reply user tidak ubah status (locked).
func handleResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	_, loggedIn, connected := state.snapshot()
	if !loggedIn || !connected {
		httpErr(w, 400, "WhatsApp belum terhubung")
		return
	}
	phone := r.URL.Query().Get("phone")
	if phone == "" {
		httpErr(w, 400, "phone required")
		return
	}
	user, _ := userFromCtx(r.Context())

	// Ambil nama_outlet untuk render template
	var namaOutlet string
	_ = auditDB.QueryRow(`SELECT COALESCE(nama_outlet, '') FROM chat_threads WHERE phone = ?`, phone).Scan(&namaOutlet)

	closing := renderClosingTemplate(namaOutlet)

	// Kirim closing via WA (ke wa_jid kalau pelanggan chat dari nomor beda)
	jid := types.NewJID(replyTargetPhone(phone), types.DefaultUserServer)
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	msg := &waProto.Message{Conversation: proto.String(closing)}
	res, err := state.client.SendMessage(ctx, jid, msg)
	if err != nil {
		httpErr(w, 500, "send closing: %v", err)
		return
	}

	now := time.Now()
	if e := recordChatMessage(phone, "out", closing, "", res.ID, now, 0, user.Email, user.Name); e != nil {
		fmt.Println("warn: recordChatMessage closing:", e)
	}

	// Set status = done + lock (assign tetap)
	_, err = auditDB.Exec(`UPDATE chat_threads SET
		last_message_at = ?,
		last_message_preview = ?,
		last_message_direction = 'out',
		status = 'done',
		assigned_email = ?, assigned_name = ?,
		updated_at = ?
		WHERE phone = ?`,
		now.Format(time.RFC3339), truncate(closing, 80), user.Email, user.Name, now.Format(time.RFC3339), phone)
	if err != nil {
		httpErr(w, 500, "update status: %v", err)
		return
	}

	// Tandai SEMUA invoice nomor ini sebagai resolved (sumber report "report resolved" +
	// exclude dari antrian retry, permanen walau di-blast ulang).
	resolvePhone := phone
	var bp string
	_ = auditDB.QueryRow(`SELECT COALESCE(blast_phone,'') FROM chat_threads WHERE phone=?`, phone).Scan(&bp)
	if bp != "" {
		resolvePhone = bp
	}
	markPhoneResolved("majoo", "blast_recipients", "blast_logs", resolvePhone, user.Email, user.Name, now)
	// Token validasi nomor ini → used (dok: token used saat Done).
	markTokenUsed(resolvePhone, "")

	writeJSON(w, map[string]any{"ok": true, "closing_sent": true})
}

// closingTemplate — di-load dari env var atau default.
var closingTemplate string

func loadClosingTemplate() {
	t := os.Getenv("INBOX_CLOSING_TEMPLATE")
	if t == "" {
		t = `Baik, Terima kasih atas konfirmasinya, Kak. 😊
Untuk percakapan ini akan saya tutup.

Jika nantinya ada pertanyaan atau kendala terkait layanan majoo, mohon tidak membalas atau menghubungi nomor ini karena nomor ini hanya digunakan untuk proses konfirmasi.

Untuk bantuan lebih lanjut, Kakak dapat menghubungi Hotline majoo di 0811-500-460.

Terima kasih`
	}
	// Replace escape \n dengan newline beneran (kalau di-set via env)
	t = strings.ReplaceAll(t, `\n`, "\n")
	closingTemplate = t
}

func renderClosingTemplate(namaOutlet string) string {
	out := closingTemplate
	out = strings.ReplaceAll(out, "{{nama_outlet}}", namaOutlet)
	return out
}

// unwrapMessage — buka pembungkus pesan (view-once, ephemeral/disappearing, device-sent,
// edited, document-with-caption) supaya media di dalamnya terdeteksi & bisa diunduh.
// Tanpa ini, foto view-once / pesan disappearing jadi "[Pesan tidak didukung]". Recursive
// dengan batas kedalaman. Getter protobuf nil-safe (aman walau wrapper-nya nil).
func unwrapMessage(m *waProto.Message) *waProto.Message {
	for i := 0; i < 6 && m != nil; i++ {
		switch {
		case m.GetEphemeralMessage().GetMessage() != nil:
			m = m.GetEphemeralMessage().GetMessage()
		case m.GetViewOnceMessage().GetMessage() != nil:
			m = m.GetViewOnceMessage().GetMessage()
		case m.GetViewOnceMessageV2().GetMessage() != nil:
			m = m.GetViewOnceMessageV2().GetMessage()
		case m.GetViewOnceMessageV2Extension().GetMessage() != nil:
			m = m.GetViewOnceMessageV2Extension().GetMessage()
		case m.GetDocumentWithCaptionMessage().GetMessage() != nil:
			m = m.GetDocumentWithCaptionMessage().GetMessage()
		case m.GetEditedMessage().GetMessage() != nil:
			m = m.GetEditedMessage().GetMessage()
		case m.GetDeviceSentMessage().GetMessage() != nil:
			m = m.GetDeviceSentMessage().GetMessage()
		default:
			return m
		}
	}
	return m
}

// extractTextFromMessage — ambil body text dari waProto.Message.
// Return (text, mediaType). Kalau bukan text murni, body diisi placeholder + mediaType di-set.
func extractTextFromMessage(m *waProto.Message) (string, string) {
	if m == nil {
		return "", ""
	}
	if t := m.GetConversation(); t != "" {
		return t, ""
	}
	if ext := m.GetExtendedTextMessage(); ext != nil {
		return ext.GetText(), ""
	}
	if m.GetImageMessage() != nil {
		cap := m.GetImageMessage().GetCaption()
		return placeholderText("[Gambar]", cap), "image"
	}
	if m.GetVideoMessage() != nil {
		cap := m.GetVideoMessage().GetCaption()
		return placeholderText("[Video]", cap), "video"
	}
	if m.GetAudioMessage() != nil {
		return "[Pesan suara]", "audio"
	}
	if m.GetPtvMessage() != nil { // video note (video bulat)
		return "[Video]", "video"
	}
	if m.GetDocumentMessage() != nil {
		return "[Dokumen] " + m.GetDocumentMessage().GetFileName(), "document"
	}
	if m.GetStickerMessage() != nil {
		return "[Stiker]", "sticker"
	}
	if m.GetLocationMessage() != nil {
		return "[Lokasi]", "location"
	}
	if m.GetContactMessage() != nil {
		return "[Kontak] " + m.GetContactMessage().GetDisplayName(), "contact"
	}
	return "[Pesan tidak didukung]", "unknown"
}

// ---- helpers ----

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func nullableID(id int64) sql.NullInt64 {
	if id == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: id, Valid: true}
}

func nullableStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func placeholderText(p, cap string) string {
	if cap != "" {
		return p + " — " + cap
	}
	return p
}

// suppress unused
var _ = json.Marshal
