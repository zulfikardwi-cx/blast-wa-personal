package main

import (
	"bytes"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
)

// setupTokenDB — handleGenerateLinks sekarang mencatat ke Riwayat Blast (blast_logs/
// blast_recipients/chat_threads), jadi pakai skema penuh dari setupBlastHistoryDB.
func setupTokenDB(t *testing.T) { setupBlastHistoryDB(t) }

func multipartCSV(t *testing.T, csvBody string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("csv", "in.csv")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte(csvBody))
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestGenerateLinks_PreservesColumnsAndAppendsKodeLink(t *testing.T) {
	setupTokenDB(t)

	// Kolom sengaja diacak urutannya + ada kolom ekstra (tanggal, leads_id) yang harus dipertahankan.
	csvIn := "nomer_invoice,nama_outlet,phone,tanggal,leads_id\n" +
		"INV/NEW/202606/01818,Rm Dapur Mirasa,081383154078,01/07/2026,LEADS/1\n" +
		"INV/NEW/202607/00096,KEDAI KOPI,628138095050,01/07/2026,LEADS/2\n"

	body, ctype := multipartCSV(t, csvIn)
	req := httptest.NewRequest("POST", "/api/generate-links", body)
	req.Header.Set("Content-Type", ctype)
	rec := httptest.NewRecorder()

	handleGenerateLinks(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out := strings.ReplaceAll(rec.Body.String(), "\ufeff", "")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (header+2), got %d: %q", len(lines), out)
	}
	// Header FIXED (format Tools Blast Resmi Majoo), bukan kolom asli.
	if lines[0] != "phone,full_name,nick_name,gender,package" {
		t.Errorf("header = %q", lines[0])
	}
	// Baris 1 dipetakan: phone(normalisasi), full_name=outlet, nick_name=invoice,
	// gender=kode (8 char), package kosong.
	cols := strings.Split(lines[1], ",")
	if len(cols) != 5 {
		t.Fatalf("kolom = %d, want 5: %v", len(cols), cols)
	}
	if cols[0] != "6281383154078" {
		t.Errorf("phone = %q, want 6281383154078 (normalisasi 0→62)", cols[0])
	}
	if cols[1] != "Rm Dapur Mirasa" {
		t.Errorf("full_name = %q, want outlet", cols[1])
	}
	if cols[2] != "INV/NEW/202606/01818" {
		t.Errorf("nick_name = %q, want invoice", cols[2])
	}
	if len(cols[3]) != tokenLen {
		t.Errorf("gender(kode) len = %d (%q), want %d", len(cols[3]), cols[3], tokenLen)
	}
	if cols[4] != "" {
		t.Errorf("package harus kosong, got %q", cols[4])
	}
	if h := rec.Header().Get("X-Rows-Generated"); h != "2" {
		t.Errorf("X-Rows-Generated = %q, want 2", h)
	}
}

func TestGenerateLinks_IdempotentTokenPerPhoneInvoice(t *testing.T) {
	setupTokenDB(t)
	csvIn := "phone,nama_outlet,nomer_invoice\n081383154078,Rm Dapur Mirasa,INV/NEW/202606/01818\n"

	run := func() string {
		body, ctype := multipartCSV(t, csvIn)
		req := httptest.NewRequest("POST", "/api/generate-links", body)
		req.Header.Set("Content-Type", ctype)
		rec := httptest.NewRecorder()
		handleGenerateLinks(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		out := strings.ReplaceAll(rec.Body.String(), "\ufeff", "")
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		return strings.Split(lines[1], ",")[3] // kolom kode
	}
	k1, k2 := run(), run()
	if k1 != k2 {
		t.Errorf("token tidak idempoten: %q vs %q", k1, k2)
	}
}

func TestGenerateLinks_AutoAdvanceAttempt(t *testing.T) {
	setupTokenDB(t)
	phone, inv := "6281383154078", "INV/NEW/202606/01818"
	// Seed: invoice sudah Attempt 1 terkirim di hari lampau + thread after_blast.
	res, _ := auditDB.Exec(`INSERT INTO blast_logs (started_at,template,attempt,total,sent) VALUES ('2026-07-01T00:00:00+07:00','tpl',1,1,1)`)
	logID, _ := res.LastInsertId()
	auditDB.Exec(`INSERT INTO blast_recipients (blast_log_id,phone,nama_outlet,nomer_invoice,status,sent_at) VALUES (?,?,?,?, 'sent','2026-07-01T09:00:00+07:00')`, logID, phone, "Rm Dapur Mirasa", inv)
	auditDB.Exec(`INSERT INTO chat_threads (phone,nama_outlet,nomer_invoice,status,current_attempt) VALUES (?,?,?, 'after_blast',1)`, phone, "Rm Dapur Mirasa", inv)

	gen := func() {
		body, ctype := multipartCSV(t, "phone,nama_outlet,nomer_invoice\n081383154078,Rm Dapur Mirasa,INV/NEW/202606/01818\n")
		req := httptest.NewRequest("POST", "/api/generate-links", body)
		req.Header.Set("Content-Type", ctype)
		rec := httptest.NewRecorder()
		handleGenerateLinks(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}

	// Generate → harus mencatat Attempt 2 (bukan lagi selalu Attempt 1).
	gen()
	var maxAtt int
	auditDB.QueryRow(`SELECT COALESCE(MAX(b.attempt),0) FROM blast_recipients r JOIN blast_logs b ON r.blast_log_id=b.id
		WHERE r.phone=? AND r.nomer_invoice=? AND r.status='sent'`, phone, inv).Scan(&maxAtt)
	if maxAtt != 2 {
		t.Fatalf("setelah generate, max attempt = %d, want 2", maxAtt)
	}

	// Re-generate HARI YANG SAMA → guard 1 attempt/hari: tidak dobel.
	gen()
	var att2Count int
	auditDB.QueryRow(`SELECT COUNT(*) FROM blast_recipients r JOIN blast_logs b ON r.blast_log_id=b.id
		WHERE r.phone=? AND r.nomer_invoice=? AND b.attempt=2`, phone, inv).Scan(&att2Count)
	if att2Count != 1 {
		t.Errorf("attempt 2 tercatat %d kali, want 1 (guard 1 attempt/hari)", att2Count)
	}
}

// genCSV — POST /api/generate-links utk 1 invoice, opsional mode reset.
func genCSV(t *testing.T, phone, outlet, inv string, reset bool) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("csv", "in.csv")
	fw.Write([]byte("phone,nama_outlet,nomer_invoice\n" + phone + "," + outlet + "," + inv + "\n"))
	if reset {
		_ = mw.WriteField("reset", "1")
	}
	mw.Close()
	req := httptest.NewRequest("POST", "/api/generate-links", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	handleGenerateLinks(rec, req)
	if rec.Code != 200 {
		t.Fatalf("generate status %d: %s", rec.Code, rec.Body.String())
	}
}

// Invoice yang sudah selesai Attempt 1-2-3 (cycle 1): mode normal SKIP (mentok 3), mode reset
// membuka cycle 2 & mulai Attempt 1 lagi + reset thread + report menampilkan progres cycle baru.
func TestGenerateLinks_ResetCycleRestartsAttempt1(t *testing.T) {
	setupTokenDB(t)
	phone, inv, outlet := "6281383154078", "INV/NEW/202606/01818", "Rm Dapur Mirasa"
	// Seed cycle 1 penuh: Attempt 1/2/3 sent di hari lampau, thread force_close.
	for _, s := range []struct {
		att int
		ts  string
	}{{1, "2026-07-01T09:00:00+07:00"}, {2, "2026-07-03T09:00:00+07:00"}, {3, "2026-07-05T09:00:00+07:00"}} {
		res, _ := auditDB.Exec(`INSERT INTO blast_logs (started_at,template,attempt,total,sent) VALUES (?,'tpl',?,1,1)`, s.ts, s.att)
		id, _ := res.LastInsertId()
		auditDB.Exec(`INSERT INTO blast_recipients (blast_log_id,phone,nama_outlet,nomer_invoice,status,sent_at,attempt,cycle) VALUES (?,?,?,?, 'sent',?,?,1)`, id, phone, outlet, inv, s.ts, s.att)
	}
	auditDB.Exec(`INSERT INTO chat_threads (phone,nama_outlet,nomer_invoice,status,current_attempt) VALUES (?,?,?, 'force_close',3)`, phone, outlet, inv)

	// Mode normal → sudah Attempt 3 → tidak menambah record.
	genCSV(t, "081383154078", outlet, inv, false)
	var n int
	auditDB.QueryRow(`SELECT COUNT(*) FROM blast_recipients WHERE phone=? AND nomer_invoice=?`, phone, inv).Scan(&n)
	if n != 3 {
		t.Fatalf("mode normal menambah record utk invoice Attempt-3 (total=%d, want 3)", n)
	}

	// Mode reset → cycle 2, Attempt 1 baru.
	genCSV(t, "081383154078", outlet, inv, true)
	var cycle, cycleAtt int
	auditDB.QueryRow(`SELECT COALESCE(MAX(cycle),0) FROM blast_recipients WHERE phone=? AND nomer_invoice=?`, phone, inv).Scan(&cycle)
	if cycle != 2 {
		t.Fatalf("setelah reset cycle terkini = %d, want 2", cycle)
	}
	auditDB.QueryRow(`SELECT COALESCE(MAX(COALESCE(r.attempt,b.attempt)),0) FROM blast_recipients r JOIN blast_logs b ON r.blast_log_id=b.id WHERE r.phone=? AND r.nomer_invoice=? AND r.cycle=2`, phone, inv).Scan(&cycleAtt)
	if cycleAtt != 1 {
		t.Errorf("attempt di cycle 2 = %d, want 1", cycleAtt)
	}
	// Thread reset ke after_blast attempt 1.
	var st string
	var ca int
	auditDB.QueryRow(`SELECT status, current_attempt FROM chat_threads WHERE phone=?`, phone).Scan(&st, &ca)
	if st != "after_blast" || ca != 1 {
		t.Errorf("thread setelah reset = (%s, att %d), want (after_blast, 1)", st, ca)
	}
	// Report Belum Respons: kolom hanya dari cycle terkini → Attempt 1 saja, 2/3 kosong.
	rowsRep, err := queryUnresponsive()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, rr := range rowsRep {
		if rr.NomerInvoice == inv {
			found = true
			if rr.Attempt1 != "No Response" || rr.Attempt2 != "-" || rr.Attempt3 != "-" {
				t.Errorf("report cycle baru = (a1=%q a2=%q a3=%q), want (No Response, -, -)", rr.Attempt1, rr.Attempt2, rr.Attempt3)
			}
		}
	}
	if !found {
		t.Errorf("invoice tidak muncul di report Belum Respons setelah reset")
	}
}

// invoiceStillNeedsRetry harus cycle-aware: setelah reset (cycle 2 baru Attempt 1), invoice
// eligible Attempt 2 lagi — bukan dianggap mentok 3 dari cycle lama.
func TestRetry_CycleAwareAfterReset(t *testing.T) {
	setupTokenDB(t)
	phone, inv := "628111", "INV/CYCLE"
	// cycle 1 penuh (1-2-3) + cycle 2 baru Attempt 1, semua di hari lampau (bukan hari ini).
	seed := []struct {
		att, cyc int
		ts       string
	}{{1, 1, "2026-07-01T09:00:00+07:00"}, {2, 1, "2026-07-02T09:00:00+07:00"}, {3, 1, "2026-07-03T09:00:00+07:00"}, {1, 2, "2026-07-05T09:00:00+07:00"}}
	for _, s := range seed {
		res, _ := auditDB.Exec(`INSERT INTO blast_logs (started_at,template,attempt,total,sent) VALUES (?,'tpl',?,1,1)`, s.ts, s.att)
		id, _ := res.LastInsertId()
		auditDB.Exec(`INSERT INTO blast_recipients (blast_log_id,phone,nomer_invoice,status,sent_at,attempt,cycle) VALUES (?,?,?, 'sent',?,?,?)`, id, phone, inv, s.ts, s.att, s.cyc)
	}
	auditDB.Exec(`INSERT INTO chat_threads (phone,nomer_invoice,status,current_attempt) VALUES (?,?, 'after_blast',1)`, phone, inv)

	next, ok := invoiceStillNeedsRetry("majoo", "chat_threads", "blast_recipients", "blast_logs", phone, inv, startOfTodayWIB())
	if !ok || next != 2 {
		t.Errorf("cycle-aware retry = (%d,%v), want (2,true) — harus baca cycle 2 (maxAtt 1), bukan cycle 1 (maxAtt 3)", next, ok)
	}
}

func TestGenerateLinks_MissingRequiredHeader(t *testing.T) {
	setupTokenDB(t)
	body, ctype := multipartCSV(t, "phone,outlet_salah,invoice_salah\n0813,X,INV1\n")
	req := httptest.NewRequest("POST", "/api/generate-links", body)
	req.Header.Set("Content-Type", ctype)
	rec := httptest.NewRecorder()
	handleGenerateLinks(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400 (header wajib kurang)", rec.Code)
	}
}
