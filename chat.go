package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func initChat() error {
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
	return err
}

// isPhoneBlasted — return true kalau nomor pernah jadi target blast.
// Dipakai filter incoming message: kalau ada chat dari nomor random yang tidak pernah
// di-blast, skip (tidak muncul di inbox).
func isPhoneBlasted(phone string) bool {
	var c int
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads WHERE phone = ?`, phone).Scan(&c)
	return c > 0
}

// upsertThreadOutgoing — dipanggil setelah blast/reply outgoing.
// Buat thread baru kalau belum ada, atau update jadi nomor itu sudah di-blast.
func upsertThreadOutgoing(phone, namaOutlet string, blastID int64, preview string, ts time.Time) error {
	tsStr := ts.Format(time.RFC3339)
	prev := truncate(preview, 80)
	_, err := auditDB.Exec(`
INSERT INTO chat_threads (phone, nama_outlet, last_blast_id, last_message_at, last_message_preview, last_message_direction, updated_at)
VALUES (?, ?, ?, ?, ?, 'out', ?)
ON CONFLICT(phone) DO UPDATE SET
	nama_outlet = COALESCE(NULLIF(excluded.nama_outlet, ''), nama_outlet),
	last_blast_id = COALESCE(excluded.last_blast_id, last_blast_id),
	last_message_at = excluded.last_message_at,
	last_message_preview = excluded.last_message_preview,
	last_message_direction = 'out',
	updated_at = excluded.updated_at`,
		phone, namaOutlet, nullableID(blastID), tsStr, prev, tsStr)
	return err
}

// upsertThreadIncoming — dipanggil saat incoming reply dari nomor blasted.
func upsertThreadIncoming(phone, preview string, ts time.Time) error {
	tsStr := ts.Format(time.RFC3339)
	prev := truncate(preview, 80)
	_, err := auditDB.Exec(`
UPDATE chat_threads SET
	last_message_at = ?,
	last_message_preview = ?,
	last_message_direction = 'in',
	unread_count = unread_count + 1,
	status = CASE WHEN status = 'done' THEN 'open' ELSE status END,
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
	Phone              string `json:"phone"`
	NamaOutlet         string `json:"nama_outlet"`
	LastBlastID        int64  `json:"last_blast_id"`
	LastMessageAt      string `json:"last_message_at"`
	LastPreview        string `json:"last_preview"`
	LastDirection      string `json:"last_direction"`
	UnreadCount        int    `json:"unread_count"`
	Status             string `json:"status"`
	AssignedEmail      string `json:"assigned_email"`
	AssignedName       string `json:"assigned_name"`
}

func handleThreads(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	var rows *sql.Rows
	var err error
	if status == "" || status == "all" {
		rows, err = auditDB.Query(`SELECT phone, COALESCE(nama_outlet,''), COALESCE(last_blast_id,0), COALESCE(last_message_at,''), COALESCE(last_message_preview,''), COALESCE(last_message_direction,''), unread_count, status, COALESCE(assigned_email,''), COALESCE(assigned_name,'') FROM chat_threads ORDER BY updated_at DESC LIMIT 200`)
	} else {
		rows, err = auditDB.Query(`SELECT phone, COALESCE(nama_outlet,''), COALESCE(last_blast_id,0), COALESCE(last_message_at,''), COALESCE(last_message_preview,''), COALESCE(last_message_direction,''), unread_count, status, COALESCE(assigned_email,''), COALESCE(assigned_name,'') FROM chat_threads WHERE status = ? ORDER BY updated_at DESC LIMIT 200`, status)
	}
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	defer rows.Close()

	var out []ThreadRow
	totals := map[string]int{"open": 0, "in_progress": 0, "done": 0, "unread": 0}
	for rows.Next() {
		var t ThreadRow
		if err := rows.Scan(&t.Phone, &t.NamaOutlet, &t.LastBlastID, &t.LastMessageAt, &t.LastPreview, &t.LastDirection, &t.UnreadCount, &t.Status, &t.AssignedEmail, &t.AssignedName); err != nil {
			continue
		}
		out = append(out, t)
	}

	// summary counts
	var cOpen, cProg, cDone, cUnread int
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads WHERE status = 'open'`).Scan(&cOpen)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads WHERE status = 'in_progress'`).Scan(&cProg)
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads WHERE status = 'done'`).Scan(&cDone)
	_ = auditDB.QueryRow(`SELECT COALESCE(SUM(unread_count), 0) FROM chat_threads`).Scan(&cUnread)
	totals["open"] = cOpen
	totals["in_progress"] = cProg
	totals["done"] = cDone
	totals["unread"] = cUnread

	writeJSON(w, map[string]any{"threads": out, "counts": totals})
}

type MessageRow struct {
	ID          int64  `json:"id"`
	Direction   string `json:"direction"`
	Body        string `json:"body"`
	IsMedia     bool   `json:"is_media"`
	MediaType   string `json:"media_type"`
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
	rows, err := auditDB.Query(`SELECT id, direction, COALESCE(body,''), is_media, COALESCE(media_type,''), timestamp, COALESCE(sender_email,''), COALESCE(sender_name,'') FROM chat_messages WHERE phone = ? ORDER BY id ASC LIMIT 500`, phone)
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	defer rows.Close()
	var out []MessageRow
	for rows.Next() {
		var m MessageRow
		var isMedia int
		if err := rows.Scan(&m.ID, &m.Direction, &m.Body, &isMedia, &m.MediaType, &m.Timestamp, &m.SenderEmail, &m.SenderName); err != nil {
			continue
		}
		m.IsMedia = isMedia == 1
		out = append(out, m)
	}

	// fetch thread meta
	var nama, status, assignedEmail, assignedName string
	_ = auditDB.QueryRow(`SELECT COALESCE(nama_outlet,''), status, COALESCE(assigned_email,''), COALESCE(assigned_name,'') FROM chat_threads WHERE phone = ?`, phone).Scan(&nama, &status, &assignedEmail, &assignedName)

	writeJSON(w, map[string]any{
		"phone":          phone,
		"nama_outlet":    nama,
		"status":         status,
		"assigned_email": assignedEmail,
		"assigned_name":  assignedName,
		"messages":       out,
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
	if err := r.ParseForm(); err != nil {
		httpErr(w, 400, "form: %v", err)
		return
	}
	status := r.FormValue("status")
	if status != "open" && status != "in_progress" && status != "done" {
		httpErr(w, 400, "status invalid (open|in_progress|done)")
		return
	}
	user, _ := userFromCtx(r.Context())

	// Kalau status in_progress, auto-assign ke user yang klik
	var assignedEmail, assignedName sql.NullString
	if status == "in_progress" {
		assignedEmail = sql.NullString{String: user.Email, Valid: true}
		assignedName = sql.NullString{String: user.Name, Valid: true}
	} else if status == "open" || status == "done" {
		// clear assignment
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
	if err := r.ParseForm(); err != nil {
		httpErr(w, 400, "form: %v", err)
		return
	}
	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		httpErr(w, 400, "body kosong")
		return
	}
	user, _ := userFromCtx(r.Context())

	// kirim via whatsmeow
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
	// record di chat_messages + thread
	if e := recordChatMessage(phone, "out", body, "", res.ID, now, 0, user.Email, user.Name); e != nil {
		fmt.Println("warn: recordChatMessage:", e)
	}
	if e := upsertThreadOutgoing(phone, "", 0, body, now); e != nil {
		fmt.Println("warn: upsertThreadOutgoing:", e)
	}

	writeJSON(w, map[string]any{"ok": true, "id": res.ID})
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
