package main

// ============================================================================
// GENERATE KODE & LINK — enrich CSV tanpa mengirim WA.
//
// User upload CSV (punya kolom phone, nama_outlet, nomer_invoice; kolom lain
// dibiarkan apa adanya). Untuk tiap baris backend generate:
//   - kode : token validasi (getOrCreateToken, idempoten per phone+invoice)
//   - link : wa.me ke Nomor Inti dengan teks prefilled berisi kode (buildTriggerLink)
// Lalu kembalikan CSV yang sama + 2 kolom baru (kode, link) sebagai download.
//
// Tidak ada blast/kirim di sini — hanya generate. Token tetap ditulis ke
// validation_tokens supaya balasan pelanggan (yang membawa kode) tetap bisa
// di-resolve ke invoice yang benar di Inbox Inti.
// ============================================================================

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// handleGenerateLinks — POST /api/generate-links. Multipart form field "csv".
// Return: text/csv attachment (kolom asli + kode + link).
func handleGenerateLinks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	// Link menunjuk ke Nomor Inti. Kalau Inti belum diketahui (belum login &
	// INTI_WA_NUMBER kosong), kolom link akan kosong → tolak lebih dulu supaya
	// user tidak dapat CSV tanpa link.
	if intiNumber() == "" {
		httpErr(w, 400, "Nomor Inti belum diketahui — login WhatsApp Inti dulu atau set INTI_WA_NUMBER di .env. Link tidak bisa dibuat.")
		return
	}
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		httpErr(w, 400, "form: %v", err)
		return
	}
	file, _, err := r.FormFile("csv")
	if err != nil {
		httpErr(w, 400, "csv: %v", err)
		return
	}
	defer file.Close()

	rd := csv.NewReader(file)
	rd.TrimLeadingSpace = true
	rd.FieldsPerRecord = -1 // toleran kolom tak seragam

	header, err := rd.Read()
	if err != nil {
		httpErr(w, 400, "header: %v", err)
		return
	}
	// Cari kolom wajib (case-insensitive, buang BOM di sel pertama).
	col := map[string]int{}
	for i, h := range header {
		h = strings.TrimPrefix(strings.TrimSpace(h), "\ufeff")
		col[strings.ToLower(h)] = i
	}
	pi, ok1 := col["phone"]
	ni, ok2 := col["nama_outlet"]
	ii, ok3 := col["nomer_invoice"]
	if !ok1 || !ok2 || !ok3 {
		httpErr(w, 400, "header wajib memuat: phone, nama_outlet, nomer_invoice")
		return
	}

	var buf bytes.Buffer
	buf.WriteString("\ufeff") // BOM supaya Excel baca UTF-8 (nama outlet non-ASCII aman)
	cw := csv.NewWriter(&buf)

	// Header keluaran FIXED (format Tools Blast Resmi Majoo). Data kita dipetakan posisional:
	// phone→phone, nama_outlet→full_name, nomer_invoice→nick_name, kode→gender, package kosong.
	if err := cw.Write([]string{"phone", "full_name", "nick_name", "gender", "package"}); err != nil {
		httpErr(w, 500, "write header: %v", err)
		return
	}

	// Generate = masuk flow blast (dikirim tools eksternal) → catat sebagai Riwayat
	// Blast Attempt 1. recordedCount = yang BENAR-BENAR baru dicatat (anti-dobel utk
	// invoice yang sudah pernah attempt 1, mis. data metode lama / re-upload).
	user, _ := userFromCtx(r.Context())
	now := time.Now()
	logID, err := recordRetryBatchStart(user.Email, user.Name, GetAttemptTemplate(1), 1, 0, now)
	if err != nil {
		httpErr(w, 500, "batch start: %v", err)
		return
	}

	rows, generated, recorded := 0, 0, 0
	for {
		rec, err := rd.Read()
		if err != nil {
			break // EOF atau baris rusak — hentikan
		}
		rows++
		phone := ""
		if pi < len(rec) {
			phone = normalizePhone(rec[pi])
		}
		outlet := ""
		if ni < len(rec) {
			outlet = strings.TrimSpace(rec[ni])
		}
		invoice := ""
		if ii < len(rec) {
			invoice = strings.TrimSpace(rec[ii])
		}

		kode := ""
		if phone != "" {
			kode = getOrCreateToken(phone, invoice, outlet)
			generated++
			// Catat Attempt 1 ke Riwayat Blast + buat thread after_blast (kalau baru).
			body := applyLink(renderTemplateWithVars(GetAttemptTemplate(1), outlet, invoice), phone, invoice, outlet)
			if recordAttempt1Sent(logID, phone, outlet, invoice, body, now) {
				recorded++
				// Tampilkan pesan Attempt 1 di Inbox (penanda sudah di-blast), walau kirim via tools eksternal.
				_ = recordChatMessage(phone, "out", body, "", "", now, logID, user.Email, user.Name)
			}
		}
		// Baris FIXED: phone, full_name(outlet), nick_name(invoice), gender(kode), package(kosong).
		if err := cw.Write([]string{phone, outlet, invoice, kode, ""}); err != nil {
			httpErr(w, 500, "write row: %v", err)
			return
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		httpErr(w, 500, "csv flush: %v", err)
		return
	}

	// Tutup entri Riwayat Blast Attempt 1: kalau ada yang baru dicatat → update
	// total/sent; kalau semua sudah pernah attempt 1 (0 baru) → hapus entri kosong.
	if recorded > 0 {
		_ = recordRetryBatchEnd(logID, recorded, 0, now)
	} else {
		deleteEmptyBlastLog(logID)
	}

	fname := fmt.Sprintf("kode-link-%s.csv", time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fname+`"`)
	w.Header().Set("X-Rows-Total", fmt.Sprintf("%d", rows))
	w.Header().Set("X-Rows-Generated", fmt.Sprintf("%d", generated))
	_, _ = w.Write(buf.Bytes())
}

