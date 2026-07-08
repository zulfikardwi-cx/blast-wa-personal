package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func seedTokenRow(t *testing.T, token, phone, invoice, outlet string) {
	t.Helper()
	if _, err := auditDB.Exec(`INSERT INTO validation_tokens (token,phone,nomer_invoice,nama_outlet,status) VALUES (?,?,?,?, 'pending')`,
		token, phone, invoice, outlet); err != nil {
		t.Fatal(err)
	}
}

func postKonfirmasi(kode string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/konfirmasi", strings.NewReader(`{"kode":"`+kode+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.9:5000"
	rec := httptest.NewRecorder()
	handleKonfirmasi(rec, req)
	return rec
}

func TestKonfirmasi_ValidCodeMovesThreadToKonfirmasiWeb(t *testing.T) {
	setupBlastHistoryDB(t)
	seedTokenRow(t, "HWYTSJJU", "6285702526099", "INV/NEW/202606/01226", "Drip n Dine")
	// thread awal after_blast (sudah di-blast, belum respons)
	if _, err := auditDB.Exec(`INSERT INTO chat_threads (phone,nama_outlet,nomer_invoice,status,current_attempt) VALUES ('6285702526099','Drip n Dine','INV/NEW/202606/01226','after_blast',1)`); err != nil {
		t.Fatal(err)
	}

	rec := postKonfirmasi("hwytsjju") // lowercase → harus di-normalize
	body := rec.Body.String()
	if !strings.Contains(body, `"ok":true`) || !strings.Contains(body, "Drip n Dine") {
		t.Fatalf("konfirmasi gagal: %s", body)
	}
	// Konfirmasi via WEB → bucket 'konfirmasi_web' (BUKAN 'open'): customer belum chat WA.
	var status string
	auditDB.QueryRow(`SELECT status FROM chat_threads WHERE phone='6285702526099'`).Scan(&status)
	if status != "konfirmasi_web" {
		t.Errorf("status = %q, want konfirmasi_web", status)
	}
	var nMsg int
	auditDB.QueryRow(`SELECT COUNT(*) FROM chat_messages WHERE phone='6285702526099' AND direction='in' AND body LIKE '[Konfirmasi Validasi via Web]%'`).Scan(&nMsg)
	if nMsg != 1 {
		t.Errorf("penanda konfirmasi = %d, want 1", nMsg)
	}

	// Idempoten: konfirmasi lagi → tetap ok, tak dobel penanda.
	_ = postKonfirmasi("HWYTSJJU")
	auditDB.QueryRow(`SELECT COUNT(*) FROM chat_messages WHERE phone='6285702526099' AND direction='in' AND body LIKE '[Konfirmasi Validasi via Web]%'`).Scan(&nMsg)
	if nMsg != 1 {
		t.Errorf("setelah konfirmasi ulang penanda = %d, want tetap 1", nMsg)
	}
}

func TestKonfirmasi_ByInvoiceNumber(t *testing.T) {
	setupBlastHistoryDB(t)
	seedTokenRow(t, "HWYTSJJU", "6285702526099", "INV/NEW/202606/01226", "Drip n Dine")
	if _, err := auditDB.Exec(`INSERT INTO chat_threads (phone,status) VALUES ('6285702526099','after_blast')`); err != nil {
		t.Fatal(err)
	}
	// Pakai NOMOR INVOICE, bukan kode.
	rec := postKonfirmasi("INV/NEW/202606/01226")
	if !strings.Contains(rec.Body.String(), `"ok":true`) || !strings.Contains(rec.Body.String(), "Drip n Dine") {
		t.Fatalf("konfirmasi via invoice gagal: %s", rec.Body.String())
	}
	var status string
	auditDB.QueryRow(`SELECT status FROM chat_threads WHERE phone='6285702526099'`).Scan(&status)
	if status != "konfirmasi_web" {
		t.Errorf("status = %q, want konfirmasi_web", status)
	}
	// Penanda tetap pakai TOKEN (bukan nomor invoice mentah).
	var n int
	auditDB.QueryRow(`SELECT COUNT(*) FROM chat_messages WHERE phone='6285702526099' AND body LIKE '%Kode HWYTSJJU%'`).Scan(&n)
	if n != 1 {
		t.Errorf("penanda harus memuat token HWYTSJJU, n=%d", n)
	}
}

// Setelah konfirmasi web (bucket konfirmasi_web), kalau customer BENAR-BENAR chat WA ke
// Inti (upsertThreadIncoming), thread dipromosikan ke 'open' — sudah reachable.
func TestKonfirmasi_RealWAReplyPromotesToOpen(t *testing.T) {
	setupBlastHistoryDB(t)
	seedTokenRow(t, "HWYTSJJU", "6285702526099", "INV/NEW/202606/01226", "Drip n Dine")
	if _, err := auditDB.Exec(`INSERT INTO chat_threads (phone,status) VALUES ('6285702526099','after_blast')`); err != nil {
		t.Fatal(err)
	}
	_ = postKonfirmasi("HWYTSJJU")
	var status string
	auditDB.QueryRow(`SELECT status FROM chat_threads WHERE phone='6285702526099'`).Scan(&status)
	if status != "konfirmasi_web" {
		t.Fatalf("setelah konfirmasi web status=%q, want konfirmasi_web", status)
	}
	// Customer lalu chat WA asli → promote ke open.
	if err := upsertThreadIncoming("6285702526099", "Halo kak siap divalidasi", time.Now()); err != nil {
		t.Fatal(err)
	}
	auditDB.QueryRow(`SELECT status FROM chat_threads WHERE phone='6285702526099'`).Scan(&status)
	if status != "open" {
		t.Errorf("setelah chat WA asli status=%q, want open", status)
	}
}

func TestKonfirmasiCoba_RedirectsToCobaNumber(t *testing.T) {
	setupBlastHistoryDB(t) // set INTI_WA_NUMBER = 6285119012345
	seedTokenRow(t, "HWYTSJJU", "6285702526099", "INV/NEW/202606/01226", "Drip n Dine")
	if _, err := auditDB.Exec(`INSERT INTO chat_threads (phone,status) VALUES ('6285702526099','after_blast')`); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/api/konfirmasi-coba", strings.NewReader(`{"kode":"HWYTSJJU"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.10:5000"
	rec := httptest.NewRecorder()
	handleKonfirmasiCoba(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("konfirmasi-coba gagal: %s", body)
	}
	// wa_link HARUS ke nomor percobaan (62811500460), BUKAN nomor INTI.
	if !strings.Contains(body, "wa.me/62811500460?") {
		t.Errorf("wa_link tidak menuju 62811500460: %s", body)
	}
	if strings.Contains(body, "wa.me/6285119012345") {
		t.Errorf("wa_link tidak boleh ke nomor INTI: %s", body)
	}
}

func TestKonfirmasi_InvalidCode(t *testing.T) {
	setupBlastHistoryDB(t)
	rec := postKonfirmasi("ZZZZ9999")
	if !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Errorf("kode invalid harusnya ok:false, got %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tidak valid") {
		t.Errorf("pesan error harus 'tidak valid', got %s", rec.Body.String())
	}
}

func TestKonfirmasi_TerminalThreadNotReopened(t *testing.T) {
	setupBlastHistoryDB(t)
	seedTokenRow(t, "DONEDONE", "628999", "INV/DONE", "Sudah Selesai")
	if _, err := auditDB.Exec(`INSERT INTO chat_threads (phone,status) VALUES ('628999','done')`); err != nil {
		t.Fatal(err)
	}
	_ = postKonfirmasi("DONEDONE")
	var status string
	auditDB.QueryRow(`SELECT status FROM chat_threads WHERE phone='628999'`).Scan(&status)
	if status != "done" {
		t.Errorf("thread done tidak boleh dibuka lagi, status=%q", status)
	}
}
