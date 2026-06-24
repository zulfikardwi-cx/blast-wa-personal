package main

// Zopoz inbox data-layer + HTTP handlers. Mirror chat.go tetapi pada tabel zopoz_threads /
// zopoz_messages (id-space & status machine terpisah dari inbox utama). Struct ThreadRow &
// MessageRow di-reuse dari chat.go (bentuk JSON sama).

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// initZopozChat — buat tabel zopoz_* di auditDB (DB yang sama, tabel terpisah → tidak
// menyentuh chat_threads/chat_messages). Skema lengkap di awal (tak perlu migrasi runtime).
func initZopozChat() error {
	_, err := auditDB.Exec(`
CREATE TABLE IF NOT EXISTS zopoz_threads (
	phone TEXT PRIMARY KEY,
	nama_outlet TEXT,
	nomer_invoice TEXT,
	last_blast_id INTEGER,
	last_message_at TEXT,
	last_message_preview TEXT,
	last_message_direction TEXT,
	unread_count INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT 'open',
	assigned_email TEXT,
	assigned_name TEXT,
	current_attempt INTEGER NOT NULL DEFAULT 1,
	last_attempt_at TEXT,
	team TEXT,
	area TEXT,
	attempt1_failed INTEGER NOT NULL DEFAULT 0,
	rejected_at TEXT,
	followup_at TEXT,
	reject_reason TEXT,
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_zopoz_threads_updated ON zopoz_threads(updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_zopoz_threads_status ON zopoz_threads(status);

CREATE TABLE IF NOT EXISTS zopoz_messages (
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
	media_path TEXT,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_zopoz_messages_phone ON zopoz_messages(phone, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_zopoz_messages_wa_id ON zopoz_messages(wa_message_id) WHERE wa_message_id IS NOT NULL;
`)
	return err
}

func zopozIsPhoneBlasted(phone string) bool {
	var c int
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM zopoz_threads WHERE phone = ?`, phone).Scan(&c)
	return c > 0
}

func zopozUpsertThreadBlast(phone, namaOutlet, nomerInvoice string, blastID int64, preview string, ts time.Time) error {
	tsStr := ts.Format(time.RFC3339)
	prev := truncate(preview, 80)
	_, err := auditDB.Exec(`
INSERT INTO zopoz_threads (phone, nama_outlet, nomer_invoice, last_blast_id, last_message_at, last_message_preview, last_message_direction, status, assigned_email, assigned_name, unread_count, current_attempt, last_attempt_at, updated_at)
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

func zopozUpsertThreadBlastFailed(phone, namaOutlet, nomerInvoice string, blastID int64, errMsg string, ts time.Time) error {
	tsStr := ts.Format(time.RFC3339)
	prev := truncate("[GAGAL KIRIM] "+errMsg, 80)
	reason := errMsg
	if reason == "" {
		reason = "Gagal kirim"
	}
	_, err := auditDB.Exec(`
INSERT INTO zopoz_threads (phone, nama_outlet, nomer_invoice, last_blast_id, last_message_at, last_message_preview, last_message_direction, status, assigned_email, assigned_name, unread_count, current_attempt, last_attempt_at, attempt1_failed, rejected_at, reject_reason, updated_at)
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

func zopozUpsertThreadRetry(phone, preview string, attemptNum int, ts time.Time) error {
	tsStr := ts.Format(time.RFC3339)
	prev := truncate(preview, 80)
	_, err := auditDB.Exec(`
UPDATE zopoz_threads SET
	last_message_at = ?,
	last_message_preview = ?,
	last_message_direction = 'out',
	current_attempt = ?,
	last_attempt_at = ?,
	updated_at = ?
WHERE phone = ?`, tsStr, prev, attemptNum, tsStr, tsStr, phone)
	return err
}

func zopozUpsertThreadAgentReply(phone, preview, agentEmail, agentName string, ts time.Time) error {
	tsStr := ts.Format(time.RFC3339)
	prev := truncate(preview, 80)
	_, err := auditDB.Exec(`
UPDATE zopoz_threads SET
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

func zopozUpsertThreadIncoming(phone, preview string, ts time.Time) error {
	tsStr := ts.Format(time.RFC3339)
	prev := truncate(preview, 80)
	_, err := auditDB.Exec(`
UPDATE zopoz_threads SET
	last_message_at = ?,
	last_message_preview = ?,
	last_message_direction = 'in',
	unread_count = unread_count + 1,
	status = CASE WHEN status IN ('done','invalid','on_going','scheduled') THEN status ELSE 'open' END,
	updated_at = ?
WHERE phone = ?`, tsStr, prev, tsStr, phone)
	return err
}

func zopozRecordChatMessage(phone, direction, body, mediaType, waMsgID string, ts time.Time, blastID int64, senderEmail, senderName string) error {
	isMedia := 0
	if mediaType != "" {
		isMedia = 1
	}
	_, err := auditDB.Exec(`
INSERT OR IGNORE INTO zopoz_messages (phone, direction, body, is_media, media_type, wa_message_id, timestamp, blast_log_id, sender_email, sender_name)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		phone, direction, body, isMedia, nullableStr(mediaType), nullableStr(waMsgID),
		ts.Format(time.RFC3339), nullableID(blastID), nullableStr(senderEmail), nullableStr(senderName))
	return err
}

// ---- handlers ----

func zopozHandleThreads(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	var conds []string
	var qargs []any
	if status != "" && status != "all" {
		conds = append(conds, "status = ?")
		qargs = append(qargs, status)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	q := `SELECT phone, COALESCE(nama_outlet,''), COALESCE(nomer_invoice,''), COALESCE(last_blast_id,0), COALESCE(last_message_at,''), COALESCE(last_message_preview,''), COALESCE(last_message_direction,''), unread_count, status, COALESCE(assigned_email,''), COALESCE(assigned_name,''), COALESCE(team,''), COALESCE(area,''), COALESCE(followup_at,'') FROM zopoz_threads ` + where + ` ORDER BY last_message_at DESC, phone ASC LIMIT 200`

	rows, err := auditDB.Query(q, qargs...)
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	defer rows.Close()

	var out []ThreadRow
	for rows.Next() {
		var t ThreadRow
		if err := rows.Scan(&t.Phone, &t.NamaOutlet, &t.NomerInvoice, &t.LastBlastID, &t.LastMessageAt, &t.LastPreview, &t.LastDirection, &t.UnreadCount, &t.Status, &t.AssignedEmail, &t.AssignedName, &t.Team, &t.Area, &t.FollowupAt); err != nil {
			continue
		}
		out = append(out, t)
	}

	totals := map[string]int{}
	var cAfter, cOpen, cProg, cOnGoing, cForce, cDone, cInvalid, cScheduled, cUnread int
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM zopoz_threads WHERE status = 'after_blast'`).Scan(&cAfter)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM zopoz_threads WHERE status = 'open'`).Scan(&cOpen)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM zopoz_threads WHERE status = 'in_progress'`).Scan(&cProg)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM zopoz_threads WHERE status = 'on_going'`).Scan(&cOnGoing)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM zopoz_threads WHERE status = 'force_close'`).Scan(&cForce)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM zopoz_threads WHERE status = 'done'`).Scan(&cDone)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM zopoz_threads WHERE status = 'invalid'`).Scan(&cInvalid)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM zopoz_threads WHERE status = 'scheduled'`).Scan(&cScheduled)
	_ = auditDB.QueryRow(`SELECT COALESCE(SUM(unread_count), 0) FROM zopoz_threads`).Scan(&cUnread)
	totals["after_blast"] = cAfter
	totals["open"] = cOpen
	totals["in_progress"] = cProg
	totals["on_going"] = cOnGoing
	totals["force_close"] = cForce
	totals["done"] = cDone
	totals["invalid"] = cInvalid
	totals["scheduled"] = cScheduled
	totals["unread"] = cUnread

	writeJSON(w, map[string]any{"threads": out, "counts": totals, "teams": []string{}})
}

func zopozHandleMessages(w http.ResponseWriter, r *http.Request) {
	phone := r.URL.Query().Get("phone")
	if phone == "" {
		httpErr(w, 400, "phone required")
		return
	}
	rows, err := auditDB.Query(`SELECT id, direction, COALESCE(body,''), is_media, COALESCE(media_type,''), COALESCE(media_path,''), timestamp, COALESCE(sender_email,''), COALESCE(sender_name,'') FROM zopoz_messages WHERE phone = ? ORDER BY id ASC LIMIT 500`, phone)
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
		if mediaPath != "" {
			m.MediaURL = zopozMediaURLPath(m.ID)
		}
		out = append(out, m)
	}

	var nama, status, assignedEmail, assignedName, followupAt string
	_ = auditDB.QueryRow(`SELECT COALESCE(nama_outlet,''), status, COALESCE(assigned_email,''), COALESCE(assigned_name,''), COALESCE(followup_at,'') FROM zopoz_threads WHERE phone = ?`, phone).Scan(&nama, &status, &assignedEmail, &assignedName, &followupAt)

	writeJSON(w, map[string]any{
		"phone":            phone,
		"nama_outlet":      nama,
		"status":           status,
		"assigned_email":   assignedEmail,
		"assigned_name":    assignedName,
		"followup_at":      followupAt,
		"messages":         out,
		"closing_template": renderClosingTemplate(nama),
	})
}

func zopozHandleMarkRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	phone := r.URL.Query().Get("phone")
	if phone == "" {
		httpErr(w, 400, "phone required")
		return
	}
	if _, err := auditDB.Exec(`UPDATE zopoz_threads SET unread_count = 0, updated_at = datetime('now') WHERE phone = ?`, phone); err != nil {
		httpErr(w, 500, "update: %v", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func zopozHandleSetStatus(w http.ResponseWriter, r *http.Request) {
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
		if err := r.ParseForm(); err != nil {
			httpErr(w, 400, "form: %v", err)
			return
		}
	}
	status := r.FormValue("status")
	if status != "open" && status != "in_progress" && status != "done" && status != "invalid" && status != "on_going" && status != "force_close" && status != "scheduled" {
		httpErr(w, 400, "status invalid")
		return
	}
	user, _ := userFromCtx(r.Context())

	var assignedEmail, assignedName sql.NullString
	if status == "in_progress" || status == "on_going" || status == "scheduled" {
		assignedEmail = sql.NullString{String: user.Email, Valid: true}
		assignedName = sql.NullString{String: user.Name, Valid: true}
	}

	var followup sql.NullString
	if status == "scheduled" {
		if f := strings.TrimSpace(r.FormValue("followup_at")); f != "" {
			followup = sql.NullString{String: f, Valid: true}
		}
	}

	if _, err := auditDB.Exec(`UPDATE zopoz_threads SET status = ?, assigned_email = ?, assigned_name = ?, followup_at = ?, updated_at = datetime('now') WHERE phone = ?`,
		status, assignedEmail, assignedName, followup, phone); err != nil {
		httpErr(w, 500, "update: %v", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func zopozHandleReply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	_, loggedIn, connected := zopozState.snapshot()
	if !loggedIn || !connected {
		httpErr(w, 400, "WhatsApp Zopoz belum terhubung")
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

	jid := types.NewJID(phone, types.DefaultUserServer)
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	msg := &waProto.Message{Conversation: proto.String(body)}
	res, err := zopozState.client.SendMessage(ctx, jid, msg)
	if err != nil {
		httpErr(w, 500, "send: %v", err)
		return
	}

	now := time.Now()
	if e := zopozRecordChatMessage(phone, "out", body, "", res.ID, now, 0, user.Email, user.Name); e != nil {
		fmt.Println("zopoz warn: recordChatMessage:", e)
	}
	if e := zopozUpsertThreadAgentReply(phone, body, user.Email, user.Name, now); e != nil {
		fmt.Println("zopoz warn: upsertThreadAgentReply:", e)
	}

	writeJSON(w, map[string]any{"ok": true, "id": res.ID})
}
