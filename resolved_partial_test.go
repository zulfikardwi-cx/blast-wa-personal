package main

import (
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// setupResolvedDB — skema minimal untuk menguji resolve per-invoice (partial Done).
func setupResolvedDB(t *testing.T) {
	t.Helper()
	if auditDB != nil {
		auditDB.Close()
		auditDB = nil
	}
	db, err := sql.Open("sqlite3", "file:rp_"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE blast_logs (id INTEGER PRIMARY KEY AUTOINCREMENT, attempt INTEGER DEFAULT 1)`,
		`CREATE TABLE blast_recipients (id INTEGER PRIMARY KEY AUTOINCREMENT, blast_log_id INTEGER, phone TEXT,
			nama_outlet TEXT, nomer_invoice TEXT, status TEXT, sent_at TEXT, created_at TEXT DEFAULT (datetime('now')))`,
		`CREATE TABLE resolved_invoices (suite TEXT, phone TEXT, nomer_invoice TEXT, nama_outlet TEXT,
			resolver_email TEXT, resolver_name TEXT, resolved_at TEXT, PRIMARY KEY(suite,phone,nomer_invoice))`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	auditDB = db
}

func seedSentInvoice(t *testing.T, phone, inv string) {
	t.Helper()
	res, _ := auditDB.Exec(`INSERT INTO blast_logs (attempt) VALUES (1)`)
	id, _ := res.LastInsertId()
	if _, err := auditDB.Exec(`INSERT INTO blast_recipients (blast_log_id,phone,nama_outlet,nomer_invoice,status,sent_at)
		VALUES (?,?,?,?, 'sent', '2026-07-06 04:00:00')`, id, phone, "Outlet "+phone, inv); err != nil {
		t.Fatal(err)
	}
}

func TestPartialResolve_PerInvoice(t *testing.T) {
	setupResolvedDB(t)
	phone := "628X"
	seedSentInvoice(t, phone, "INV/1")
	seedSentInvoice(t, phone, "INV/2")
	seedSentInvoice(t, phone, "INV/3")

	// Sebelum apa pun: ketiga invoice belum resolved.
	if got := len(phoneInvoiceStatuses("majoo", "blast_recipients", "blast_logs", phone)); got != 3 {
		t.Fatalf("invoice count = %d, want 3", got)
	}
	if !phoneHasUnresolvedInvoice("majoo", "blast_recipients", "blast_logs", phone) {
		t.Fatal("harusnya masih ada unresolved")
	}

	// Done SEBAGIAN: hanya INV/1.
	n := markInvoicesResolved("majoo", "blast_recipients", "blast_logs", phone, []string{"INV/1"}, "a@x", "A", time.Now())
	if n != 1 {
		t.Errorf("resolved count = %d, want 1", n)
	}
	// INV/2 & INV/3 masih unresolved → nomor masih punya sisa (thread TIDAK boleh 'done').
	if !phoneHasUnresolvedInvoice("majoo", "blast_recipients", "blast_logs", phone) {
		t.Error("setelah done sebagian, harusnya MASIH ada unresolved (INV/2, INV/3)")
	}
	// Cek flag resolved per invoice.
	res := map[string]bool{}
	for _, x := range phoneInvoiceStatuses("majoo", "blast_recipients", "blast_logs", phone) {
		res[x.Invoice] = x.Resolved
	}
	if !res["INV/1"] || res["INV/2"] || res["INV/3"] {
		t.Errorf("flag resolved salah: %+v", res)
	}

	// Guard injeksi: invoice yang tak pernah di-blast ke nomor ini TIDAK boleh tercatat.
	if got := markInvoicesResolved("majoo", "blast_recipients", "blast_logs", phone, []string{"INV/HACK"}, "a@x", "A", time.Now()); got != 0 {
		t.Errorf("invoice asing tercatat resolved (%d), harusnya 0", got)
	}

	// Done sisa (INV/2, INV/3) → tidak ada lagi unresolved (thread boleh 'done').
	markInvoicesResolved("majoo", "blast_recipients", "blast_logs", phone, []string{"INV/2", "INV/3"}, "a@x", "A", time.Now())
	if phoneHasUnresolvedInvoice("majoo", "blast_recipients", "blast_logs", phone) {
		t.Error("setelah semua invoice di-Done, harusnya tidak ada unresolved")
	}
}
