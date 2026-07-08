package main

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func setupAttemptDB(t *testing.T) {
	t.Helper()
	if auditDB != nil {
		auditDB.Close()
		auditDB = nil
	}
	db, err := sql.Open("sqlite3", "file:att_"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE blast_logs (id INTEGER PRIMARY KEY AUTOINCREMENT, attempt INTEGER DEFAULT 1)`,
		`CREATE TABLE blast_recipients (id INTEGER PRIMARY KEY AUTOINCREMENT, blast_log_id INTEGER, phone TEXT,
			nomer_invoice TEXT, status TEXT, sent_at TEXT, created_at TEXT DEFAULT (datetime('now')), attempt INTEGER, cycle INTEGER NOT NULL DEFAULT 1)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	auditDB = db
}

// seedSend — 1 baris kirim dengan attempt-log & waktu tertentu.
func seedSend(t *testing.T, phone, inv string, loggedAtt int, ts, status string) {
	t.Helper()
	res, _ := auditDB.Exec(`INSERT INTO blast_logs (attempt) VALUES (?)`, loggedAtt)
	id, _ := res.LastInsertId()
	if _, err := auditDB.Exec(`INSERT INTO blast_recipients (blast_log_id,phone,nomer_invoice,status,sent_at) VALUES (?,?,?,?,?)`,
		id, phone, inv, status, ts); err != nil {
		t.Fatal(err)
	}
}

func attemptsOf(t *testing.T, phone, inv string) []int {
	t.Helper()
	rows, _ := auditDB.Query(`SELECT r.attempt FROM blast_recipients r WHERE r.phone=? AND r.nomer_invoice=? ORDER BY COALESCE(NULLIF(r.sent_at,''),r.created_at), r.id`, phone, inv)
	defer rows.Close()
	var out []int
	for rows.Next() {
		var a sql.NullInt64
		rows.Scan(&a)
		out = append(out, int(a.Int64))
	}
	return out
}

func TestBackfillRecipientAttempts(t *testing.T) {
	setupAttemptDB(t)

	// A: mislabel — ter-blast 3x tapi log-nya 1,2,1 → harusnya kronologis 1,2,3.
	seedSend(t, "628A", "INV/A", 1, "2026-06-16T09:00:00+07:00", "sent")
	seedSend(t, "628A", "INV/A", 2, "2026-06-17T09:00:00+07:00", "sent")
	seedSend(t, "628A", "INV/A", 1, "2026-06-19T09:00:00+07:00", "sent") // mislabel att1

	// B: clean — 1,2,3 sudah benar → tetap 1,2,3.
	seedSend(t, "628B", "INV/B", 1, "2026-07-01T09:00:00+07:00", "sent")
	seedSend(t, "628B", "INV/B", 2, "2026-07-05T09:00:00+07:00", "sent")
	seedSend(t, "628B", "INV/B", 3, "2026-07-06T09:00:00+07:00", "sent")

	// C: attempt-1 GAGAL lalu 2x sent → kronologis 1(failed),2,3.
	seedSend(t, "628C", "INV/C", 1, "2026-06-20T09:00:00+07:00", "failed")
	seedSend(t, "628C", "INV/C", 1, "2026-06-21T09:00:00+07:00", "sent")
	seedSend(t, "628C", "INV/C", 1, "2026-06-22T09:00:00+07:00", "sent")

	// D: ter-blast 4x → cap di 3 (1,2,3,3).
	for i, ts := range []string{"2026-06-01", "2026-06-02", "2026-06-03", "2026-06-04"} {
		_ = i
		seedSend(t, "628D", "INV/D", 1, ts+"T09:00:00+07:00", "sent")
	}

	backfillRecipientAttempts()

	check := func(phone, inv string, want ...int) {
		got := attemptsOf(t, phone, inv)
		if len(got) != len(want) {
			t.Fatalf("%s: len %v want %v", inv, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: attempt[%d]=%d want %d (all=%v)", inv, i, got[i], want[i], got)
			}
		}
	}
	check("628A", "INV/A", 1, 2, 3) // mislabel diperbaiki
	check("628B", "INV/B", 1, 2, 3) // clean tetap
	check("628C", "INV/C", 1, 2, 3) // failed dihitung sebagai attempt 1
	check("628D", "INV/D", 1, 2, 3, 3)

	// Idempoten: run kedua tidak mengubah.
	backfillRecipientAttempts()
	check("628A", "INV/A", 1, 2, 3)
}
