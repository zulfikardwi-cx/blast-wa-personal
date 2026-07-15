package main

import (
	"testing"
)

// reblastAttempt1 — simulasikan jalur blast LIVE (handleBlast/runBlast) untuk 1 recipient
// Attempt 1 sukses: buat blast_log attempt 1 + recordRecipient (yang kini cycle-aware).
// Reset thread ke after_blast disimulasikan dengan UPDATE langsung (di produksi dilakukan
// upsertThreadBlast — perilaku existing, sudah teruji terpisah; skema test minimal tak punya
// kolom assigned_email sehingga fungsi itu tak dipakai di unit ini).
func reblastAttempt1(t *testing.T, phone, outlet, invoice, sentAt string) {
	t.Helper()
	res, err := auditDB.Exec(`INSERT INTO blast_logs (started_at,template,attempt,total,sent) VALUES (?,?,1,1,1)`, sentAt, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	logID, _ := res.LastInsertId()
	rec := &RecipientStatus{Phone: phone, NamaOutlet: outlet, NomerInv: invoice, Status: "sent", Message: "halo", SentAt: sentAt}
	if err := recordRecipient(logID, rec); err != nil {
		t.Fatalf("recordRecipient: %v", err)
	}
	// upsertThreadBlast di produksi meng-set status='after_blast', current_attempt=1.
	if _, err := auditDB.Exec(`UPDATE chat_threads SET status='after_blast', current_attempt=1 WHERE phone=?`, phone); err != nil {
		t.Fatal(err)
	}
}

// Re-blast invoice yang sudah DONE (resolved, sudah 1 putaran Attempt 1-2-3) harus:
//   - invoice lepas dari resolved_invoices (dilakukan recordRecipient)
//   - baris Attempt 1 baru masuk PUTARAN (cycle) baru
//   - invoice kembali eligible antrian Attempt 2 (proses mengulang dari awal)
func TestReblast_DoneInvoiceRestartsAttemptCycle(t *testing.T) {
	setupBlastHistoryDB(t)
	seedBlasted(t, "628138", "INV-1", 3, "done") // putaran 1 penuh, thread done
	// tandai resolved (skema test minimal: kolom inti saja)
	if _, err := auditDB.Exec(`INSERT INTO resolved_invoices (suite,phone,nomer_invoice) VALUES ('majoo','628138','INV-1')`); err != nil {
		t.Fatal(err)
	}
	if !isInvoiceResolved("majoo", "628138", "INV-1") {
		t.Fatal("prasyarat: invoice harus resolved dulu")
	}

	reblastAttempt1(t, "628138", "Outlet 628138", "INV-1", "2026-06-01T00:00:00Z")

	if isInvoiceResolved("majoo", "628138", "INV-1") {
		t.Error("invoice masih resolved setelah re-blast; harusnya dilepas")
	}
	if cyc := currentInvoiceCycle("628138", "INV-1"); cyc != 2 {
		t.Errorf("cycle terkini = %d, want 2 (putaran baru)", cyc)
	}
	var st string
	auditDB.QueryRow(`SELECT status FROM chat_threads WHERE phone='628138'`).Scan(&st)
	if st != "after_blast" {
		t.Errorf("thread status = %q, want after_blast", st)
	}

	retries := collectInvoiceRetries("majoo", "chat_threads", "blast_recipients", "blast_logs", 2, startOfTodayWIB())
	found := false
	for _, r := range retries {
		if r.phone == "628138" && r.nomerInvoice == "INV-1" {
			found = true
			if r.nextAttempt != 2 {
				t.Errorf("nextAttempt = %d, want 2 (mulai putaran dari awal)", r.nextAttempt)
			}
		}
	}
	if !found {
		t.Error("invoice re-blast tidak muncul di antrian Attempt 2")
	}
}

// Blast PERTAMA sebuah invoice baru tetap cycle 1 (tidak ada perubahan perilaku / tidak
// keliru dianggap re-blast).
func TestReblast_FirstBlastStaysCycle1(t *testing.T) {
	setupBlastHistoryDB(t)
	// thread awal (belum ada) — reblastAttempt1 UPDATE tak berpengaruh; sisipkan supaya query aman.
	if _, err := auditDB.Exec(`INSERT INTO chat_threads (phone,nomer_invoice,status,current_attempt) VALUES ('628999','INV-NEW','after_blast',1)`); err != nil {
		t.Fatal(err)
	}
	reblastAttempt1(t, "628999", "Outlet Baru", "INV-NEW", "2026-06-01T00:00:00Z")
	if cyc := currentInvoiceCycle("628999", "INV-NEW"); cyc != 1 {
		t.Errorf("cycle blast pertama = %d, want 1", cyc)
	}
	var n int
	auditDB.QueryRow(`SELECT COUNT(*) FROM blast_recipients WHERE phone='628999' AND cycle=1 AND status='sent'`).Scan(&n)
	if n != 1 {
		t.Errorf("baris sent cycle 1 = %d, want 1", n)
	}
}

// Re-blast invoice yang BELUM done (mis. baru Attempt 1, di after_blast) juga membuka putaran
// baru — konsisten: satu Attempt-1 blast baru = kampanye/putaran baru yang mengulang dari awal.
func TestReblast_ActiveInvoiceOpensNewCycle(t *testing.T) {
	setupBlastHistoryDB(t)
	seedBlasted(t, "628200", "INV-2", 1, "after_blast") // cycle 1, baru Attempt 1
	reblastAttempt1(t, "628200", "Outlet 628200", "INV-2", "2026-06-01T00:00:00Z")
	if cyc := currentInvoiceCycle("628200", "INV-2"); cyc != 2 {
		t.Errorf("cycle = %d, want 2", cyc)
	}
	retries := collectInvoiceRetries("majoo", "chat_threads", "blast_recipients", "blast_logs", 2, startOfTodayWIB())
	found := false
	for _, r := range retries {
		if r.phone == "628200" && r.nomerInvoice == "INV-2" && r.nextAttempt == 2 {
			found = true
		}
	}
	if !found {
		t.Error("re-blast invoice aktif harus eligible Attempt 2 di putaran baru")
	}
}

// seedDoneInvoice — 1 invoice untuk sebuah nomor yang sudah selesai 1 putaran (Attempt 1-2-3,
// cycle 1) & tercatat resolved. TIDAK menyentuh chat_threads (dikelola terpisah oleh caller,
// karena thread = per-nomor).
func seedDoneInvoice(t *testing.T, phone, inv string) {
	t.Helper()
	for a := 1; a <= 3; a++ {
		res, err := auditDB.Exec(`INSERT INTO blast_logs (started_at,template,attempt,total,sent) VALUES ('2026-06-01T00:00:00Z','tpl',?,1,1)`, a)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if _, err := auditDB.Exec(`INSERT INTO blast_recipients (blast_log_id,phone,nama_outlet,nomer_invoice,status,sent_at,created_at,cycle)
			VALUES (?,?, 'Outlet',?, 'sent','2026-06-01T00:00:00Z','2026-06-01T00:00:00Z',1)`, id, phone, inv); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := auditDB.Exec(`INSERT INTO resolved_invoices (suite,phone,nomer_invoice) VALUES ('majoo',?,?)`, phone, inv); err != nil {
		t.Fatal(err)
	}
}

// Use case user: nomor dengan beberapa invoice yang SUDAH done, lalu di-blast invoice BARU.
// Invoice baru → after_blast; ketiga invoice lama TETAP done (resolved tak tersentuh).
func TestReblast_NewInvoiceKeepsOtherDoneInvoices(t *testing.T) {
	setupBlastHistoryDB(t)
	phone := "6281380261784"
	done := []string{"INV/NEW/202606/00000245", "INV/NEW/202606/00000246", "INV/NEW/202606/00000247"}
	for _, inv := range done {
		seedDoneInvoice(t, phone, inv)
	}
	// thread nomor ini (per-nomor) awalnya done.
	if _, err := auditDB.Exec(`INSERT INTO chat_threads (phone,nomer_invoice,status,current_attempt) VALUES (?,?, 'done',3)`, phone, done[2]); err != nil {
		t.Fatal(err)
	}

	// Blast invoice BARU (belum pernah di-blast).
	reblastAttempt1(t, phone, "Outlet", "INV/NEW/202601/000001", "2026-06-10T00:00:00Z")

	// Ketiga invoice lama TETAP done.
	for _, inv := range done {
		if !isInvoiceResolved("majoo", phone, inv) {
			t.Errorf("invoice %s harus tetap done/resolved setelah blast invoice baru", inv)
		}
	}
	// Invoice baru: tidak resolved, cycle 1 (blast pertama-nya).
	if isInvoiceResolved("majoo", phone, "INV/NEW/202601/000001") {
		t.Error("invoice baru tidak boleh resolved")
	}
	if cyc := currentInvoiceCycle(phone, "INV/NEW/202601/000001"); cyc != 1 {
		t.Errorf("cycle invoice baru = %d, want 1", cyc)
	}
	// Thread nomor → after_blast.
	var st string
	auditDB.QueryRow(`SELECT status FROM chat_threads WHERE phone=?`, phone).Scan(&st)
	if st != "after_blast" {
		t.Errorf("thread = %q, want after_blast", st)
	}
	// Picker Done: 3 lama masih tercentang "sudah Done", invoice baru belum.
	statuses := phoneInvoiceStatuses("majoo", "blast_recipients", "blast_logs", phone)
	resolvedCount := 0
	for _, s := range statuses {
		if s.Resolved {
			resolvedCount++
		}
	}
	if resolvedCount != 3 {
		t.Errorf("invoice resolved di picker = %d, want 3 (245/246/247)", resolvedCount)
	}
}

// Kalau yang di-blast ulang justru salah satu invoice yang SUDAH done, hanya invoice ITU yang
// lepas dari done (buka putaran baru); invoice done lainnya tetap done.
func TestReblast_ReblastOneDoneInvoiceOnlyAffectsThatOne(t *testing.T) {
	setupBlastHistoryDB(t)
	phone := "6281380261784"
	for _, inv := range []string{"INV-245", "INV-246", "INV-247"} {
		seedDoneInvoice(t, phone, inv)
	}
	if _, err := auditDB.Exec(`INSERT INTO chat_threads (phone,nomer_invoice,status,current_attempt) VALUES (?, 'INV-247', 'done',3)`, phone); err != nil {
		t.Fatal(err)
	}

	reblastAttempt1(t, phone, "Outlet", "INV-245", "2026-06-10T00:00:00Z") // re-blast yang sudah done

	if isInvoiceResolved("majoo", phone, "INV-245") {
		t.Error("INV-245 di-blast ulang → harus lepas dari done")
	}
	if cyc := currentInvoiceCycle(phone, "INV-245"); cyc != 2 {
		t.Errorf("INV-245 cycle = %d, want 2 (putaran baru)", cyc)
	}
	for _, inv := range []string{"INV-246", "INV-247"} {
		if !isInvoiceResolved("majoo", phone, inv) {
			t.Errorf("%s tidak di-blast → harus tetap done", inv)
		}
		if cyc := currentInvoiceCycle(phone, inv); cyc != 1 {
			t.Errorf("%s cycle = %d, want 1 (tak tersentuh)", inv, cyc)
		}
	}
}
