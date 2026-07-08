package main

import (
	"database/sql"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// setupBlastHistoryDB — skema minimal Riwayat Blast + thread + token untuk menguji
// eligibility (collectInvoiceRetries) & export yang mencatat balik ke Riwayat Blast.
func setupBlastHistoryDB(t *testing.T) {
	t.Helper()
	if auditDB != nil {
		auditDB.Close()
		auditDB = nil
	}
	db, err := sql.Open("sqlite3", "file:brh_"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE blast_logs (id INTEGER PRIMARY KEY AUTOINCREMENT, user_email TEXT, user_name TEXT,
			started_at TEXT, ended_at TEXT, template TEXT, total INTEGER DEFAULT 0, sent INTEGER DEFAULT 0,
			failed INTEGER DEFAULT 0, skipped INTEGER DEFAULT 0, min_delay INTEGER, max_delay INTEGER, attempt INTEGER DEFAULT 1)`,
		`CREATE TABLE blast_recipients (id INTEGER PRIMARY KEY AUTOINCREMENT, blast_log_id INTEGER, phone TEXT,
			nama_outlet TEXT, nomer_invoice TEXT, status TEXT, error TEXT, message TEXT, sent_at TEXT,
			created_at TEXT DEFAULT (datetime('now')), attempt INTEGER, cycle INTEGER NOT NULL DEFAULT 1)`,
		`CREATE TABLE chat_threads (phone TEXT PRIMARY KEY, nama_outlet TEXT, nomer_invoice TEXT, last_blast_id INTEGER,
			last_message_at TEXT, last_message_preview TEXT, last_message_direction TEXT, status TEXT,
			unread_count INTEGER DEFAULT 0, current_attempt INTEGER DEFAULT 1, last_attempt_at TEXT, updated_at TEXT,
			reject_reason TEXT)`,
		`CREATE TABLE resolved_invoices (suite TEXT, phone TEXT, nomer_invoice TEXT)`,
		`CREATE TABLE excluded_invoices (suite TEXT, phone TEXT, nomer_invoice TEXT)`,
		`CREATE TABLE validation_tokens (token TEXT PRIMARY KEY, phone TEXT, nomer_invoice TEXT DEFAULT '',
			nama_outlet TEXT DEFAULT '', status TEXT DEFAULT 'pending', created_at TEXT, used_at TEXT, UNIQUE(phone,nomer_invoice))`,
		`CREATE TABLE chat_messages (id INTEGER PRIMARY KEY AUTOINCREMENT, phone TEXT, direction TEXT, body TEXT,
			is_media INTEGER, media_type TEXT, wa_message_id TEXT, timestamp TEXT, blast_log_id INTEGER,
			sender_email TEXT, sender_name TEXT, created_at TEXT DEFAULT (datetime('now')), media_path TEXT)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	auditDB = db
	loadPrefillTemplate()
	os.Setenv("INTI_WA_NUMBER", "6285119012345")
}

// seedBlasted — 1 invoice yang sudah di-blast s/d attempt maxAtt (metode lama), thread berstatus threadStatus.
func seedBlasted(t *testing.T, phone, inv string, maxAtt int, threadStatus string) {
	t.Helper()
	for a := 1; a <= maxAtt; a++ {
		res, err := auditDB.Exec(`INSERT INTO blast_logs (started_at,template,attempt,total,sent) VALUES ('2026-07-01T00:00:00Z','tpl',?,1,1)`, a)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		// sent_at lampau supaya tidak kena guard attemptedToday
		if _, err := auditDB.Exec(`INSERT INTO blast_recipients (blast_log_id,phone,nama_outlet,nomer_invoice,status,sent_at,created_at)
			VALUES (?,?,?,?, 'sent','2026-07-01T00:00:00Z','2026-07-01T00:00:00Z')`, id, phone, "Outlet "+phone, inv); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := auditDB.Exec(`INSERT INTO chat_threads (phone,nama_outlet,nomer_invoice,status,current_attempt) VALUES (?,?,?,?,?)`,
		phone, "Outlet "+phone, inv, threadStatus, maxAtt); err != nil {
		t.Fatal(err)
	}
}

func TestBelumRespons_StatsFromBlastHistory(t *testing.T) {
	setupBlastHistoryDB(t)
	seedBlasted(t, "6281", "INV-A", 1, "after_blast")  // eligible next=2
	seedBlasted(t, "6282", "INV-B", 2, "in_progress")  // agresif: masih dikejar, next=3
	seedBlasted(t, "6283", "INV-C", 1, "done")         // terminal → tidak eligible
	seedBlasted(t, "6284", "INV-D", 3, "after_blast")  // mentok attempt 3 → tidak eligible

	rec := httptest.NewRecorder()
	handleBelumResponsStats(rec, httptest.NewRequest("GET", "/api/belum-respons", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"attempt2":1`) || !strings.Contains(body, `"attempt3":1`) || !strings.Contains(body, `"total":2`) {
		t.Errorf("stats salah: %s", body)
	}
}

// konfirmasi_web = customer sudah konfirmasi via web (respons positif) → keluar antrian
// Attempt 2/3, tidak boleh ikut dihitung sebagai kandidat retry.
func TestBelumRespons_KonfirmasiWebExcludedFromRetry(t *testing.T) {
	setupBlastHistoryDB(t)
	seedBlasted(t, "6281", "INV-A", 1, "after_blast")    // eligible attempt 2
	seedBlasted(t, "6290", "INV-W", 1, "konfirmasi_web") // sudah konfirmasi web → TIDAK eligible

	rec := httptest.NewRecorder()
	handleBelumResponsStats(rec, httptest.NewRequest("GET", "/api/belum-respons", nil))
	body := rec.Body.String()
	// Hanya 6281 yang jadi kandidat; konfirmasi_web tidak ikut → total 1, attempt2 1.
	if !strings.Contains(body, `"attempt2":1`) || !strings.Contains(body, `"total":1`) {
		t.Errorf("konfirmasi_web harus dikecualikan dari retry: %s", body)
	}
}

func TestBelumRespons_ExportRecordsToRiwayat(t *testing.T) {
	setupBlastHistoryDB(t)
	seedBlasted(t, "6281", "INV-A", 1, "after_blast") // eligible attempt 2
	seedBlasted(t, "6282", "INV-B", 2, "in_progress") // eligible attempt 3 (bukan 2)

	rec := httptest.NewRecorder()
	handleBelumResponsExport(rec, httptest.NewRequest("GET", "/api/belum-respons/export?attempt=2", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	out := strings.ReplaceAll(rec.Body.String(), "\ufeff", "")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 { // header + hanya INV-A
		t.Fatalf("expected header+1, got %d: %q", len(lines), out)
	}
	if lines[0] != "phone,full_name,nick_name,gender,package" {
		t.Errorf("header salah: %q", lines[0])
	}
	cols := strings.SplitN(lines[1], ",", 5)
	// phone, full_name(outlet), nick_name(invoice=INV-A), gender(kode), package kosong
	if cols[0] != "6281" || cols[2] != "INV-A" || len(cols[3]) != tokenLen {
		t.Errorf("row salah: %v", cols)
	}
	if cols[4] != "" {
		t.Errorf("package harus kosong: %q", cols[4])
	}

	// Tercatat ke Riwayat Blast sebagai attempt 2 'sent'
	var nRec int
	auditDB.QueryRow(`SELECT COUNT(*) FROM blast_recipients r JOIN blast_logs b ON r.blast_log_id=b.id
		WHERE r.phone='6281' AND r.nomer_invoice='INV-A' AND r.status='sent' AND b.attempt=2`).Scan(&nRec)
	if nRec != 1 {
		t.Errorf("attempt-2 blast_recipients = %d, want 1", nRec)
	}
	// blast_logs attempt=2 batch ter-close (ended_at terisi, total>0)
	var nBatch int
	auditDB.QueryRow(`SELECT COUNT(*) FROM blast_logs WHERE attempt=2 AND ended_at IS NOT NULL AND total>0`).Scan(&nBatch)
	if nBatch != 1 {
		t.Errorf("blast_logs attempt=2 batch = %d, want 1", nBatch)
	}
	// thread INV-A current_attempt naik ke 2
	var ca int
	auditDB.QueryRow(`SELECT current_attempt FROM chat_threads WHERE phone='6281'`).Scan(&ca)
	if ca != 2 {
		t.Errorf("thread current_attempt = %d, want 2", ca)
	}
	// Pesan attempt 2 muncul di Inbox (chat_messages 'out')
	var nMsg int
	auditDB.QueryRow(`SELECT COUNT(*) FROM chat_messages WHERE phone='6281' AND direction='out'`).Scan(&nMsg)
	if nMsg != 1 {
		t.Errorf("chat_messages 'out' utk 6281 = %d, want 1 (penanda attempt 2 di inbox)", nMsg)
	}
}
