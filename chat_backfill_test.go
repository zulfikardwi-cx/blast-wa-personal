package main

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// setupBackfillDB — skema minimal untuk menguji backfillMissingThreads.
func setupBackfillDB(t *testing.T) {
	t.Helper()
	if auditDB != nil {
		auditDB.Close()
		auditDB = nil
	}
	db, err := sql.Open("sqlite3", "file:bmt_"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE blast_logs (id INTEGER PRIMARY KEY AUTOINCREMENT, attempt INTEGER DEFAULT 1)`,
		`CREATE TABLE blast_recipients (id INTEGER PRIMARY KEY AUTOINCREMENT, blast_log_id INTEGER, phone TEXT,
			nama_outlet TEXT, nomer_invoice TEXT, status TEXT, error TEXT, message TEXT, sent_at TEXT,
			created_at TEXT DEFAULT (datetime('now')))`,
		`CREATE TABLE chat_threads (phone TEXT PRIMARY KEY, nama_outlet TEXT, nomer_invoice TEXT, last_blast_id INTEGER,
			last_message_at TEXT, last_message_preview TEXT, last_message_direction TEXT, status TEXT,
			unread_count INTEGER DEFAULT 0, current_attempt INTEGER DEFAULT 1, last_attempt_at TEXT,
			created_at TEXT, updated_at TEXT)`,
		`CREATE TABLE resolved_invoices (suite TEXT, phone TEXT, nomer_invoice TEXT)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	auditDB = db
}

// rcpt — 1 baris recipient (blast attempt 1) untuk phone/invoice.
func rcpt(t *testing.T, phone, inv, status string) {
	t.Helper()
	res, _ := auditDB.Exec(`INSERT INTO blast_logs (attempt) VALUES (1)`)
	id, _ := res.LastInsertId()
	if _, err := auditDB.Exec(`INSERT INTO blast_recipients (blast_log_id,phone,nama_outlet,nomer_invoice,status,message,sent_at)
		VALUES (?,?,?,?,?,?, '2026-07-02 04:10:43')`, id, phone, "Outlet "+phone, inv, status, "pesan blast"); err != nil {
		t.Fatal(err)
	}
}

func bmtThreadStatus(t *testing.T, phone string) string {
	t.Helper()
	var s string
	err := auditDB.QueryRow(`SELECT status FROM chat_threads WHERE phone=?`, phone).Scan(&s)
	if err == sql.ErrNoRows {
		return "<none>"
	}
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBackfillMissingThreads(t *testing.T) {
	setupBackfillDB(t)

	// A: 1 invoice sent, belum resolved → after_blast.
	rcpt(t, "628A", "INV/A1", "sent")
	// B: 2 invoice sent, KEDUANYA resolved → done.
	rcpt(t, "628B", "INV/B1", "sent")
	rcpt(t, "628B", "INV/B2", "sent")
	auditDB.Exec(`INSERT INTO resolved_invoices VALUES ('majoo','628B','INV/B1'),('majoo','628B','INV/B2')`)
	// C: 2 invoice sent, 1 resolved + 1 belum → after_blast (aturan per-(phone,invoice)).
	rcpt(t, "628C", "INV/C1", "sent")
	rcpt(t, "628C", "INV/C2", "sent")
	auditDB.Exec(`INSERT INTO resolved_invoices VALUES ('majoo','628C','INV/C1')`)
	// D: sudah punya thread (on_going) → JANGAN disentuh.
	rcpt(t, "628D", "INV/D1", "sent")
	auditDB.Exec(`INSERT INTO chat_threads (phone,status,created_at,updated_at) VALUES ('628D','on_going','x','x')`)
	// E: hanya failed → tidak dibuatkan thread (bukan urusan fungsi ini).
	rcpt(t, "628E", "INV/E1", "failed")

	if err := backfillMissingThreads(); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	cases := map[string]string{
		"628A": "after_blast",
		"628B": "done",
		"628C": "after_blast",
		"628D": "on_going", // tidak berubah
		"628E": "<none>",   // failed → tidak dibuat
	}
	for phone, want := range cases {
		if got := bmtThreadStatus(t, phone); got != want {
			t.Errorf("phone %s: status=%q, want %q", phone, got, want)
		}
	}

	// Idempoten: run kedua tidak menambah / mengubah apa pun.
	var before int
	auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads`).Scan(&before)
	if err := backfillMissingThreads(); err != nil {
		t.Fatalf("backfill 2: %v", err)
	}
	var after int
	auditDB.QueryRow(`SELECT COUNT(*) FROM chat_threads`).Scan(&after)
	if before != after {
		t.Errorf("tidak idempoten: thread %d → %d", before, after)
	}
}
