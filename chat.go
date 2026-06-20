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
		// reject_reason: alasan reject untuk ditampilkan di kolom Info/Alasan Log Status
		// Update — mis. "nomor tidak terdaftar di WhatsApp" (Attempt 1 gagal) atau
		// "Tidak ada respons s/d 16:00 WIB" (Attempt 3).
		{"reject_reason", "TEXT"},
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
	return nil
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
JOIN (SELECT phone, MAX(id) AS maxid FROM blast_recipients WHERE status='failed' GROUP BY phone) m
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

func loadAttemptTemplates() {
	attemptTemplates[0] = pickTemplate("TEMPLATE_ATTEMPT_1", `Halo, Majoopreneurs!

Terima kasih telah berlangganan aplikasi majoo.
Untuk menjaga keakuratan dan keamanan data, kami perlu melakukan validasi untuk Invoice berikut:

Nama Outlet: {{nama_outlet}}
No. Invoice: {{nomer_invoice}}

Apabila Kakak bersedia mengikuti proses validasi, mohon balas pesan ini pada jam operasional kami (09.00–16.00 WIB) agar proses penjadwalan dapat segera dilakukan.
Tim Validator kami akan menghubungi Kakak untuk melakukan sesi Google Meet atau WhatsApp Call.

Terima kasih! 🙏`)

	attemptTemplates[1] = pickTemplate("TEMPLATE_ATTEMPT_2", `Halo, Majoopreneurs!

Mohon maaf, ingin melakukan konfirmasi kembali
Untuk menjaga keakuratan dan keamanan data, kami perlu melakukan validasi untuk Invoice berikut:

Nama Outlet: {{nama_outlet}}
No. Invoice: {{nomer_invoice}}

Apabila Kakak bersedia mengikuti proses validasi, mohon balas pesan ini pada jam operasional kami (09.00–16.00 WIB) agar proses penjadwalan dapat segera dilakukan.
Tim Validator kami akan menghubungi Kakak untuk melakukan sesi Google Meet atau WhatsApp Call.

Terima kasih! 🙏`)

	attemptTemplates[2] = pickTemplate("TEMPLATE_ATTEMPT_3", `Halo, Majoopreneurs!

Izin follow up kembali
Untuk menjaga keakuratan dan keamanan data, kami perlu melakukan validasi untuk Invoice berikut:

Nama Outlet: {{nama_outlet}}
No. Invoice: {{nomer_invoice}}

Apabila Kakak bersedia mengikuti proses validasi, mohon balas pesan ini pada jam operasional kami (09.00–16.00 WIB) agar proses penjadwalan dapat segera dilakukan.
Tim Validator kami akan menghubungi Kakak untuk melakukan sesi Google Meet atau WhatsApp Call.
Jika Kakak masih belum membalas pesan ini, maka penjadwalan kami tutup. Jika terdapat permintaan dan informasi lainnya, silahkan menghubungi Hotline Majoo pada nomer 0811-500-460

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
	status = CASE WHEN status IN ('done','invalid','on_going','scheduled') THEN status ELSE 'in_progress' END,
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
	q := `SELECT phone, COALESCE(nama_outlet,''), COALESCE(last_blast_id,0), COALESCE(last_message_at,''), COALESCE(last_message_preview,''), COALESCE(last_message_direction,''), unread_count, status, COALESCE(assigned_email,''), COALESCE(assigned_name,''), COALESCE(team,''), COALESCE(area,'') FROM chat_threads ` + where + ` ORDER BY last_message_at DESC, phone ASC LIMIT 200`

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
		if err := rows.Scan(&t.Phone, &t.NamaOutlet, &t.LastBlastID, &t.LastMessageAt, &t.LastPreview, &t.LastDirection, &t.UnreadCount, &t.Status, &t.AssignedEmail, &t.AssignedName, &t.Team, &t.Area); err != nil {
			continue
		}
		out = append(out, t)
	}

	// summary counts
	var cAfter, cOpen, cProg, cOnGoing, cForce, cDone, cInvalid, cScheduled, cUnread int
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
	var nama, status, assignedEmail, assignedName string
	_ = auditDB.QueryRow(`SELECT COALESCE(nama_outlet,''), status, COALESCE(assigned_email,''), COALESCE(assigned_name,'') FROM chat_threads WHERE phone = ?`, phone).Scan(&nama, &status, &assignedEmail, &assignedName)

	writeJSON(w, map[string]any{
		"phone":            phone,
		"nama_outlet":      nama,
		"status":           status,
		"assigned_email":   assignedEmail,
		"assigned_name":    assignedName,
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
	if status != "open" && status != "in_progress" && status != "done" && status != "invalid" && status != "on_going" && status != "force_close" && status != "scheduled" {
		httpErr(w, 400, "status invalid (open|in_progress|done|invalid|on_going|force_close|scheduled)")
		return
	}
	user, _ := userFromCtx(r.Context())

	// Kalau status in_progress / on_going / scheduled, auto-assign ke user yang klik (PIC)
	var assignedEmail, assignedName sql.NullString
	if status == "in_progress" || status == "on_going" || status == "scheduled" {
		assignedEmail = sql.NullString{String: user.Email, Valid: true}
		assignedName = sql.NullString{String: user.Name, Valid: true}
	} else {
		// open / done / invalid → clear assignment
		assignedEmail = sql.NullString{}
		assignedName = sql.NullString{}
	}

	if _, err := auditDB.Exec(`UPDATE chat_threads SET status = ?, assigned_email = ?, assigned_name = ?, updated_at = datetime('now') WHERE phone = ?`,
		status, assignedEmail, assignedName, phone); err != nil {
		httpErr(w, 500, "update: %v", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
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
	// (sesuai spec: Done lock)
	jid := types.NewJID(phone, types.DefaultUserServer)
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

	// Kirim closing via WA
	jid := types.NewJID(phone, types.DefaultUserServer)
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
