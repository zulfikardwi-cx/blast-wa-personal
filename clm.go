package main

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

// ============================================================================
// Inbox CLM — layer assignment MANUAL yang BERDIRI SENDIRI di atas Inbox.
//
// Model PER-INVOICE: 1 assignment = 1 (phone, nomer_invoice). Satu nomor bisa
// punya banyak invoice → banyak assignment terpisah, masing-masing dengan nama
// outlet & timeline percakapan SENDIRI (tabel clm_messages).
//
// Saat "Assign to CLM", agent memilih invoice mana yang di-assign (picker kalau
// nomor punya >1 invoice) — jadi tidak semua invoice ikut ter-assign.
//
// Tampilan chat CLM sengaja BLANK di awal (seolah tools chat baru): timeline CLM
// hanya berisi pesan yang mengalir SEJAK di-assign, TIDAK menarik riwayat WA
// Inbox. State machine CLM independen:
//   - new      : baru di-assign (New Assignment) — chat blank
//   - open     : customer (user) membalas via WA (incoming di-append ke timeline)
//   - progress : team internal membalas chat DI CLM (kirim WA + catat di timeline)
//   - done     : sudah selesai (manual)
//
// Inbox biasa TIDAK terpengaruh: satu-satunya kaitan adalah hook incoming WA
// (inheren per-nomor) yang no-op kalau nomor itu tak punya assignment CLM.
// ============================================================================

const (
	clmNew      = "new"
	clmOpen     = "open"
	clmProgress = "progress"
	clmDone     = "done"
)

func clmValidStatus(s string) bool {
	return s == clmNew || s == clmOpen || s == clmProgress || s == clmDone
}

func initCLM() error {
	// Migrasi dari skema lama (per-nomor: phone PRIMARY KEY, tanpa kolom id) ke skema baru
	// per-invoice (id PK + UNIQUE(phone,nomer_invoice)). Idempoten: hanya jalan sekali.
	var tblExists int
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='clm_assignments'`).Scan(&tblExists)
	if tblExists > 0 {
		var hasID int
		_ = auditDB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('clm_assignments') WHERE name='id'`).Scan(&hasID)
		if hasID == 0 {
			if err := migrateCLMToPerInvoice(); err != nil {
				return err
			}
		}
	}

	_, err := auditDB.Exec(`
CREATE TABLE IF NOT EXISTS clm_assignments (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	phone TEXT NOT NULL,
	nomer_invoice TEXT NOT NULL DEFAULT '',
	nama_outlet TEXT,
	status TEXT NOT NULL DEFAULT 'new',
	assigned_email TEXT,
	assigned_name TEXT,
	last_event_at TEXT,
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at TEXT NOT NULL DEFAULT (datetime('now')),
	UNIQUE(phone, nomer_invoice)
);
CREATE INDEX IF NOT EXISTS idx_clm_status ON clm_assignments(status);
CREATE INDEX IF NOT EXISTS idx_clm_phone ON clm_assignments(phone);

CREATE TABLE IF NOT EXISTS clm_messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	assignment_id INTEGER NOT NULL,
	direction TEXT NOT NULL,          -- in | out | note
	body TEXT,
	sender_email TEXT,
	sender_name TEXT,
	wa_message_id TEXT,
	timestamp TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_clm_msg_assignment ON clm_messages(assignment_id, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_clm_msg_wa ON clm_messages(assignment_id, wa_message_id) WHERE wa_message_id IS NOT NULL AND wa_message_id != '';
`)
	return err
}

// migrateCLMToPerInvoice — pindahkan baris skema lama (phone PK) ke tabel baru per-invoice.
// Tiap baris lama membawa nomer_invoice-nya sendiri → jadi 1 assignment per (phone,invoice).
func migrateCLMToPerInvoice() error {
	stmts := []string{
		`ALTER TABLE clm_assignments RENAME TO clm_assignments_old`,
		`CREATE TABLE clm_assignments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			phone TEXT NOT NULL,
			nomer_invoice TEXT NOT NULL DEFAULT '',
			nama_outlet TEXT,
			status TEXT NOT NULL DEFAULT 'new',
			assigned_email TEXT,
			assigned_name TEXT,
			last_event_at TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE(phone, nomer_invoice)
		)`,
		`INSERT INTO clm_assignments (phone, nomer_invoice, nama_outlet, status, assigned_email, assigned_name, last_event_at, created_at, updated_at)
		 SELECT phone, COALESCE(nomer_invoice,''), nama_outlet, status, assigned_email, assigned_name, last_event_at, created_at, updated_at FROM clm_assignments_old`,
		`DROP TABLE clm_assignments_old`,
	}
	for _, s := range stmts {
		if _, err := auditDB.Exec(s); err != nil {
			return fmt.Errorf("migrateCLM: %w", err)
		}
	}
	fmt.Println("migrasi: clm_assignments per-nomor → per-invoice selesai")
	return nil
}

// ---- assignment helpers ----

type clmAssignment struct {
	ID           int64
	Phone        string
	NomerInvoice string
	NamaOutlet   string
	Status       string
}

func clmGetAssignment(id int64) (clmAssignment, bool) {
	var a clmAssignment
	err := auditDB.QueryRow(`SELECT id, phone, COALESCE(nomer_invoice,''), COALESCE(nama_outlet,''), status FROM clm_assignments WHERE id=?`, id).
		Scan(&a.ID, &a.Phone, &a.NomerInvoice, &a.NamaOutlet, &a.Status)
	if err != nil {
		return a, false
	}
	return a, true
}

func clmRecordMessage(assignmentID int64, direction, body, senderEmail, senderName, waMsgID string, ts time.Time) {
	_, err := auditDB.Exec(`
INSERT OR IGNORE INTO clm_messages (assignment_id, direction, body, sender_email, sender_name, wa_message_id, timestamp)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		assignmentID, direction, body, nullableStr(senderEmail), nullableStr(senderName), nullableStr(waMsgID), ts.Format(time.RFC3339))
	if err != nil {
		fmt.Println("warn: clmRecordMessage:", err)
	}
}

// clmOnIncoming — customer membalas via WA (incoming). Untuk SETIAP assignment CLM nomor ini
// yang belum 'done': pindahkan ke 'open' + append pesan masuk ke timeline-nya. No-op kalau
// nomor tak punya assignment. Additif — dipanggil dari handleIncomingWA setelah logika Inbox.
func clmOnIncoming(phone, body string, ts time.Time, waMsgID string) {
	if phone == "" {
		return
	}
	rows, err := auditDB.Query(`SELECT id FROM clm_assignments WHERE phone=? AND status!='done'`, phone)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	now := ts.Format(time.RFC3339)
	for _, id := range ids {
		clmRecordMessage(id, "in", body, "", "", waMsgID, ts)
		_, _ = auditDB.Exec(`UPDATE clm_assignments SET status='open', last_event_at=?, updated_at=? WHERE id=? AND status!='done'`, now, now, id)
	}
}

// ---- handlers ----

// clmHandleAssign — POST /api/clm/assign?phone= . Field "invoices" (boleh berulang / dipisah
// koma) = daftar invoice yang mau di-assign. Tiap invoice → 1 assignment (phone,invoice,outlet).
// Row baru status 'new'. Yang sudah 'done' → reset ke 'new' (re-follow up). Yang masih aktif
// (new/open/progress) tidak diturunkan.
func clmHandleAssign(w http.ResponseWriter, r *http.Request) {
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
	// Pastikan thread ada di Inbox.
	var threadInvoice, threadOutlet string
	err := auditDB.QueryRow(`SELECT COALESCE(nomer_invoice,''), COALESCE(nama_outlet,'') FROM chat_threads WHERE phone=?`, phone).Scan(&threadInvoice, &threadOutlet)
	if err == sql.ErrNoRows {
		httpErr(w, 404, "thread tidak ditemukan di Inbox")
		return
	} else if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}

	requested := parseInvoicesField(r)
	// Outlet per invoice dari data blast (attempt-1 sent). Juga jadi whitelist invoice sah.
	outletByInv := map[string]string{}
	for _, s := range phoneInvoiceStatuses("majoo", "blast_recipients", "blast_logs", phone) {
		outletByInv[s.Invoice] = s.Outlet
	}

	// Tentukan daftar final invoice yang di-assign.
	var toAssign []string
	if len(requested) == 0 {
		// Tanpa pilihan eksplisit: assign invoice thread (fallback single).
		if threadInvoice != "" {
			toAssign = []string{threadInvoice}
		} else {
			toAssign = []string{""} // thread tanpa invoice (mis. inbound non-blast)
		}
	} else {
		for _, inv := range requested {
			toAssign = append(toAssign, inv)
		}
	}

	user, _ := userFromCtx(r.Context())
	now := time.Now().Format(time.RFC3339)
	assigned := 0
	for _, inv := range toAssign {
		outlet := outletByInv[inv]
		if outlet == "" {
			if inv == threadInvoice || inv == "" {
				outlet = threadOutlet
			}
		}
		_, e := auditDB.Exec(`
INSERT INTO clm_assignments (phone, nomer_invoice, nama_outlet, status, assigned_email, assigned_name, last_event_at, created_at, updated_at)
VALUES (?, ?, ?, 'new', ?, ?, ?, ?, ?)
ON CONFLICT(phone, nomer_invoice) DO UPDATE SET
	nama_outlet = COALESCE(NULLIF(excluded.nama_outlet,''), nama_outlet),
	status = CASE WHEN clm_assignments.status = 'done' THEN 'new' ELSE clm_assignments.status END,
	assigned_email = excluded.assigned_email,
	assigned_name = excluded.assigned_name,
	updated_at = excluded.updated_at`,
			phone, inv, outlet, user.Email, user.Name, now, now, now)
		if e != nil {
			httpErr(w, 500, "assign: %v", e)
			return
		}
		assigned++
	}
	writeJSON(w, map[string]any{"ok": true, "assigned": assigned})
}

// clmHandleAssigned — GET /api/clm/assigned?phone= . Daftar invoice nomor ini yang SUDAH
// ter-assign ke CLM & masih AKTIF (status != done). Dipakai picker Assign untuk men-disable
// invoice yang sudah masuk CLM (tak bisa di-assign dobel). Yang sudah Done di CLM TIDAK ikut
// (boleh di-assign ulang untuk re-follow up).
func clmHandleAssigned(w http.ResponseWriter, r *http.Request) {
	phone := r.URL.Query().Get("phone")
	if phone == "" {
		httpErr(w, 400, "phone required")
		return
	}
	rows, err := auditDB.Query(`SELECT COALESCE(nomer_invoice,'') FROM clm_assignments WHERE phone=? AND status!='done'`, phone)
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	defer rows.Close()
	invoices := []string{}
	for rows.Next() {
		var inv string
		if rows.Scan(&inv) == nil {
			invoices = append(invoices, inv)
		}
	}
	writeJSON(w, map[string]any{"invoices": invoices})
}

// clmHandleSetStatus — POST /api/clm/status?id= (field status). Perpindahan MANUAL
// (Done / Reopen ke new). Assign PIC ke user yang klik.
func clmHandleSetStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	id := atoi64(r.URL.Query().Get("id"))
	if id == 0 {
		httpErr(w, 400, "id required")
		return
	}
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		if err := r.ParseForm(); err != nil {
			httpErr(w, 400, "form: %v", err)
			return
		}
	}
	status := r.FormValue("status")
	if !clmValidStatus(status) {
		httpErr(w, 400, "status invalid (new|open|progress|done)")
		return
	}
	if _, ok := clmGetAssignment(id); !ok {
		httpErr(w, 404, "assignment tidak ditemukan")
		return
	}
	user, _ := userFromCtx(r.Context())
	now := time.Now().Format(time.RFC3339)
	if _, err := auditDB.Exec(`UPDATE clm_assignments SET status=?, assigned_email=?, assigned_name=?, updated_at=? WHERE id=?`,
		status, user.Email, user.Name, now, id); err != nil {
		httpErr(w, 500, "update: %v", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "status": status})
}

type CLMThreadRow struct {
	ID            int64  `json:"id"`
	Phone         string `json:"phone"`
	NomerInvoice  string `json:"nomer_invoice"`
	NamaOutlet    string `json:"nama_outlet"`
	Status        string `json:"status"`
	AssignedName  string `json:"assigned_name"`
	LastMessageAt string `json:"last_message_at"`
	LastPreview   string `json:"last_preview"`
	LastDirection string `json:"last_direction"`
}

// clmHandleThreads — GET /api/clm/threads?status= . Daftar assignment (per invoice) + counts.
// Preview diambil dari clm_messages (timeline CLM sendiri), bukan Inbox.
func clmHandleThreads(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	var where string
	var qargs []any
	if status != "" && status != "all" {
		where = "WHERE c.status = ?"
		qargs = append(qargs, status)
	}
	q := `
SELECT c.id, c.phone, COALESCE(c.nomer_invoice,''), COALESCE(c.nama_outlet,''), c.status, COALESCE(c.assigned_name,''),
       COALESCE(m.timestamp,''), COALESCE(m.body,''), COALESCE(m.direction,'')
FROM clm_assignments c
LEFT JOIN clm_messages m ON m.id = (SELECT id FROM clm_messages m2 WHERE m2.assignment_id=c.id ORDER BY m2.id DESC LIMIT 1)
` + where + `
ORDER BY COALESCE(NULLIF(m.timestamp,''), c.updated_at) DESC, c.id DESC
LIMIT 300`
	rows, err := auditDB.Query(q, qargs...)
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	defer rows.Close()
	var out []CLMThreadRow
	for rows.Next() {
		var t CLMThreadRow
		if err := rows.Scan(&t.ID, &t.Phone, &t.NomerInvoice, &t.NamaOutlet, &t.Status, &t.AssignedName,
			&t.LastMessageAt, &t.LastPreview, &t.LastDirection); err != nil {
			continue
		}
		out = append(out, t)
	}

	counts := map[string]int{clmNew: 0, clmOpen: 0, clmProgress: 0, clmDone: 0}
	crows, err := auditDB.Query(`SELECT status, COUNT(*) FROM clm_assignments GROUP BY status`)
	if err == nil {
		for crows.Next() {
			var s string
			var n int
			if crows.Scan(&s, &n) == nil {
				counts[s] = n
			}
		}
		crows.Close()
	}
	writeJSON(w, map[string]any{"threads": out, "counts": counts})
}

type CLMMessageRow struct {
	ID          int64  `json:"id"`
	Direction   string `json:"direction"`
	Body        string `json:"body"`
	Timestamp   string `json:"timestamp"`
	SenderName  string `json:"sender_name"`
	SenderEmail string `json:"sender_email"`
}

// clmHandleMessages — GET /api/clm/messages?id= . Timeline CLM sebuah assignment (BLANK di
// awal — hanya pesan sejak di-assign, bukan riwayat WA Inbox) + meta assignment.
func clmHandleMessages(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.URL.Query().Get("id"))
	if id == 0 {
		httpErr(w, 400, "id required")
		return
	}
	a, ok := clmGetAssignment(id)
	if !ok {
		httpErr(w, 404, "assignment tidak ditemukan")
		return
	}
	rows, err := auditDB.Query(`SELECT id, direction, COALESCE(body,''), timestamp, COALESCE(sender_name,''), COALESCE(sender_email,'')
		FROM clm_messages WHERE assignment_id=? ORDER BY id ASC LIMIT 500`, id)
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	defer rows.Close()
	var out []CLMMessageRow
	for rows.Next() {
		var m CLMMessageRow
		if rows.Scan(&m.ID, &m.Direction, &m.Body, &m.Timestamp, &m.SenderName, &m.SenderEmail) == nil {
			out = append(out, m)
		}
	}
	writeJSON(w, map[string]any{
		"id":            a.ID,
		"phone":         a.Phone,
		"nomer_invoice": a.NomerInvoice,
		"nama_outlet":   a.NamaOutlet,
		"status":        a.Status,
		"messages":      out,
	})
}

// clmHandleReply — POST /api/clm/reply?id= (field body). Team internal balas chat DI CLM:
// KIRIM WA ke customer (nomor assignment) via INTI + catat di timeline CLM + status → progress.
func clmHandleReply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	id := atoi64(r.URL.Query().Get("id"))
	if id == 0 {
		httpErr(w, 400, "id required")
		return
	}
	a, ok := clmGetAssignment(id)
	if !ok {
		httpErr(w, 404, "assignment tidak ditemukan")
		return
	}
	_, loggedIn, connected := state.snapshot()
	if !loggedIn || !connected {
		httpErr(w, 400, "WhatsApp belum terhubung")
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

	jid := types.NewJID(replyTargetPhone(a.Phone), types.DefaultUserServer)
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	jid = resolveToLID(ctx, state.client, jid)
	if _, e := state.client.GetUserDevicesContext(ctx, []types.JID{jid}); e != nil {
		fmt.Println("warn: clm reply prefetch devices for", a.Phone, ":", e)
	}
	msg := &waProto.Message{Conversation: proto.String(body)}
	res, err := state.client.SendMessage(ctx, jid, msg)
	if err != nil {
		if is463Err(err) {
			httpErr(w, 400, "WhatsApp menolak kirim (error 463 / anti-spam). Nomor Inti tidak bisa mengirim ke customer yang belum pernah chat ke Inti.")
			return
		}
		httpErr(w, 500, "send: %v", err)
		return
	}
	now := time.Now()
	clmRecordMessage(id, "out", body, user.Email, user.Name, res.ID, now)
	nowStr := now.Format(time.RFC3339)
	// Team balas → progress (kecuali sudah done: tetap done/locked).
	_, _ = auditDB.Exec(`UPDATE clm_assignments SET status=CASE WHEN status='done' THEN status ELSE 'progress' END,
		assigned_email=?, assigned_name=?, last_event_at=?, updated_at=? WHERE id=?`,
		user.Email, user.Name, nowStr, nowStr, id)
	writeJSON(w, map[string]any{"ok": true, "id": res.ID})
}

// clmHandleNote — POST /api/clm/note?id= (field body). Catatan internal di timeline CLM
// (tidak dikirim ke customer, tidak mengubah status).
func clmHandleNote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	id := atoi64(r.URL.Query().Get("id"))
	if id == 0 {
		httpErr(w, 400, "id required")
		return
	}
	if _, ok := clmGetAssignment(id); !ok {
		httpErr(w, 404, "assignment tidak ditemukan")
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
	now := time.Now()
	clmRecordMessage(id, "note", body, user.Email, user.Name, "", now)
	_, _ = auditDB.Exec(`UPDATE clm_assignments SET updated_at=? WHERE id=?`, now.Format(time.RFC3339), id)
	writeJSON(w, map[string]any{"ok": true})
}

func atoi64(s string) int64 {
	var n int64
	_, _ = fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n
}
