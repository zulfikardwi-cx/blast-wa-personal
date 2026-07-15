package main

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func setupCLM(t *testing.T) {
	t.Helper()
	setupBlastHistoryDB(t)
	if err := initCLM(); err != nil {
		t.Fatalf("initCLM: %v", err)
	}
	// thread Inbox + 3 invoice ter-blast (attempt-1 sent) untuk nomor yang sama.
	if _, err := auditDB.Exec(`INSERT INTO chat_threads (phone,nama_outlet,nomer_invoice,status,last_message_at,last_message_preview,last_message_direction)
		VALUES ('628111','Toko Maju','INV-A','done','2026-07-01T10:00:00Z','halo','out')`); err != nil {
		t.Fatal(err)
	}
	for _, inv := range []string{"INV-A", "INV-B", "INV-C"} {
		res, _ := auditDB.Exec(`INSERT INTO blast_logs (started_at,template,attempt,total,sent) VALUES ('2026-06-01T00:00:00Z','tpl',1,1,1)`)
		id, _ := res.LastInsertId()
		if _, err := auditDB.Exec(`INSERT INTO blast_recipients (blast_log_id,phone,nama_outlet,nomer_invoice,status,sent_at,created_at,cycle)
			VALUES (?, '628111', 'Toko Maju', ?, 'sent','2026-06-01T00:00:00Z','2026-06-01T00:00:00Z',1)`, id, inv); err != nil {
			t.Fatal(err)
		}
	}
}

func postAssign(t *testing.T, phone string, invoices ...string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	for _, inv := range invoices {
		form.Add("invoices", inv)
	}
	req := httptest.NewRequest("POST", "/api/clm/assign?phone="+phone, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey{}, SessionUser{Email: "agent@majoo.id", Name: "Agen CLM"}))
	rec := httptest.NewRecorder()
	clmHandleAssign(rec, req)
	return rec
}

func assignmentID(t *testing.T, phone, invoice string) int64 {
	t.Helper()
	var id int64
	auditDB.QueryRow(`SELECT id FROM clm_assignments WHERE phone=? AND nomer_invoice=?`, phone, invoice).Scan(&id)
	return id
}

func clmStatusOfID(t *testing.T, id int64) string {
	t.Helper()
	var s string
	auditDB.QueryRow(`SELECT status FROM clm_assignments WHERE id=?`, id).Scan(&s)
	return s
}

func postClmStatusID(t *testing.T, id int64, status string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"status": {status}}
	req := httptest.NewRequest("POST", "/api/clm/status?id="+strconv.FormatInt(id, 10), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey{}, SessionUser{Email: "agent@majoo.id", Name: "Agen CLM"}))
	rec := httptest.NewRecorder()
	clmHandleSetStatus(rec, req)
	return rec
}

// Assign hanya invoice terpilih → 1 assignment per invoice; invoice lain tak ikut.
func TestCLM_AssignPerSelectedInvoice(t *testing.T) {
	setupCLM(t)
	rec := postAssign(t, "628111", "INV-A", "INV-C")
	if rec.Code != 200 {
		t.Fatalf("assign code=%d body=%s", rec.Code, rec.Body.String())
	}
	var n int
	auditDB.QueryRow(`SELECT COUNT(*) FROM clm_assignments WHERE phone='628111'`).Scan(&n)
	if n != 2 {
		t.Fatalf("assignment = %d, want 2 (hanya INV-A & INV-C)", n)
	}
	if assignmentID(t, "628111", "INV-A") == 0 || assignmentID(t, "628111", "INV-C") == 0 {
		t.Error("INV-A & INV-C harus ter-assign")
	}
	if assignmentID(t, "628111", "INV-B") != 0 {
		t.Error("INV-B TIDAK dipilih → tidak boleh ter-assign")
	}
	// outlet ke-snapshot
	var outlet string
	auditDB.QueryRow(`SELECT nama_outlet FROM clm_assignments WHERE phone='628111' AND nomer_invoice='INV-A'`).Scan(&outlet)
	if outlet != "Toko Maju" {
		t.Errorf("outlet=%q, want Toko Maju", outlet)
	}
}

// Menambah assignment invoice lain kemudian → assignment terpisah, tidak menimpa yang lama.
func TestCLM_AddAnotherInvoiceLater(t *testing.T) {
	setupCLM(t)
	postAssign(t, "628111", "INV-A")
	idA := assignmentID(t, "628111", "INV-A")
	clmOnAgentReplyOldProgress(t, idA) // set INV-A ke progress dulu

	postAssign(t, "628111", "INV-C") // tambah INV-C
	if assignmentID(t, "628111", "INV-C") == 0 {
		t.Error("INV-C harus ter-assign sebagai assignment baru")
	}
	if clmStatusOfID(t, idA) != "progress" {
		t.Error("assignment INV-A yang sudah progress tidak boleh berubah saat assign INV-C")
	}
}

// helper: set progress via reply record langsung (tanpa kirim WA)
func clmOnAgentReplyOldProgress(t *testing.T, id int64) {
	t.Helper()
	auditDB.Exec(`UPDATE clm_assignments SET status='progress' WHERE id=?`, id)
}

// Incoming WA customer → SEMUA assignment non-done nomor itu jadi 'open' + pesan masuk ke
// timeline masing-masing.
func TestCLM_IncomingSetsOpenPerAssignment(t *testing.T) {
	setupCLM(t)
	postAssign(t, "628111", "INV-A", "INV-B")
	idA := assignmentID(t, "628111", "INV-A")
	idB := assignmentID(t, "628111", "INV-B")

	clmOnIncoming("628111", "halo saya mau tanya", time.Now(), "WAMSG1")

	if clmStatusOfID(t, idA) != "open" || clmStatusOfID(t, idB) != "open" {
		t.Errorf("kedua assignment harus open (A=%s B=%s)", clmStatusOfID(t, idA), clmStatusOfID(t, idB))
	}
	var nA, nB int
	auditDB.QueryRow(`SELECT COUNT(*) FROM clm_messages WHERE assignment_id=? AND direction='in'`, idA).Scan(&nA)
	auditDB.QueryRow(`SELECT COUNT(*) FROM clm_messages WHERE assignment_id=? AND direction='in'`, idB).Scan(&nB)
	if nA != 1 || nB != 1 {
		t.Errorf("pesan masuk harus di-append ke tiap timeline (A=%d B=%d)", nA, nB)
	}
}

// Done terkunci dari event incoming.
func TestCLM_DoneLockedFromIncoming(t *testing.T) {
	setupCLM(t)
	postAssign(t, "628111", "INV-A")
	idA := assignmentID(t, "628111", "INV-A")
	if rec := postClmStatusID(t, idA, "done"); rec.Code != 200 {
		t.Fatalf("done code=%d body=%s", rec.Code, rec.Body.String())
	}
	clmOnIncoming("628111", "halo", time.Now(), "WAMSG2")
	if clmStatusOfID(t, idA) != "done" {
		t.Errorf("assignment done harus tetap done, dapat %s", clmStatusOfID(t, idA))
	}
	// tidak menerima pesan masuk baru (locked)
	var n int
	auditDB.QueryRow(`SELECT COUNT(*) FROM clm_messages WHERE assignment_id=? AND direction='in'`, idA).Scan(&n)
	if n != 0 {
		t.Errorf("assignment done tak boleh dapat pesan baru, dapat %d", n)
	}
}

// Re-assign assignment yang sudah done → reset ke new.
func TestCLM_ReassignDoneResetsToNew(t *testing.T) {
	setupCLM(t)
	postAssign(t, "628111", "INV-A")
	idA := assignmentID(t, "628111", "INV-A")
	postClmStatusID(t, idA, "done")
	postAssign(t, "628111", "INV-A")
	if clmStatusOfID(t, idA) != "new" {
		t.Errorf("re-assign done → status=%s, want new", clmStatusOfID(t, idA))
	}
}

// Note tidak mengubah status; reply (progress) via record (tanpa WA di test) diuji via status endpoint.
func TestCLM_NoteDoesNotChangeStatus(t *testing.T) {
	setupCLM(t)
	postAssign(t, "628111", "INV-A")
	idA := assignmentID(t, "628111", "INV-A")
	form := url.Values{"body": {"catatan internal"}}
	req := httptest.NewRequest("POST", "/api/clm/note?id="+strconv.FormatInt(idA, 10), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey{}, SessionUser{Email: "agent@majoo.id", Name: "Agen"}))
	rec := httptest.NewRecorder()
	clmHandleNote(rec, req)
	if rec.Code != 200 {
		t.Fatalf("note code=%d body=%s", rec.Code, rec.Body.String())
	}
	if clmStatusOfID(t, idA) != "new" {
		t.Errorf("note tak boleh ubah status; dapat %s", clmStatusOfID(t, idA))
	}
	var n int
	auditDB.QueryRow(`SELECT COUNT(*) FROM clm_messages WHERE assignment_id=? AND direction='note'`, idA).Scan(&n)
	if n != 1 {
		t.Errorf("note harus tercatat 1, dapat %d", n)
	}
}

// Hooks no-op untuk nomor yang belum di-assign.
func TestCLM_IncomingNoopForUnassigned(t *testing.T) {
	setupCLM(t)
	clmOnIncoming("628111", "halo", time.Now(), "WAMSGX")
	var n int
	auditDB.QueryRow(`SELECT COUNT(*) FROM clm_assignments`).Scan(&n)
	if n != 0 {
		t.Errorf("assignment=%d, want 0", n)
	}
}

// /api/clm/assigned → invoice yang sudah di CLM & aktif (untuk disable di picker); yang done
// TIDAK ikut (boleh di-assign ulang).
func TestCLM_AssignedListForPicker(t *testing.T) {
	setupCLM(t)
	postAssign(t, "628111", "INV-A", "INV-B")
	postClmStatusID(t, assignmentID(t, "628111", "INV-B"), "done") // INV-B done → boleh assign lagi

	rec := httptest.NewRecorder()
	clmHandleAssigned(rec, httptest.NewRequest("GET", "/api/clm/assigned?phone=628111", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "INV-A") {
		t.Errorf("INV-A (aktif) harus ada di assigned: %s", body)
	}
	if strings.Contains(body, "INV-B") {
		t.Errorf("INV-B (done) tidak boleh ada di assigned (boleh re-assign): %s", body)
	}
}

// threads list per-invoice + counts.
func TestCLM_ThreadsListPerInvoice(t *testing.T) {
	setupCLM(t)
	postAssign(t, "628111", "INV-A", "INV-B")
	idB := assignmentID(t, "628111", "INV-B")
	postClmStatusID(t, idB, "done")

	rec := httptest.NewRecorder()
	clmHandleThreads(rec, httptest.NewRequest("GET", "/api/clm/threads?status=new", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "INV-A") || strings.Contains(body, "INV-B") {
		t.Errorf("filter status=new harus hanya INV-A: %s", body)
	}
	if !strings.Contains(body, `"new":1`) || !strings.Contains(body, `"done":1`) {
		t.Errorf("counts salah: %s", body)
	}
}
