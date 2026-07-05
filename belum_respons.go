package main

// ============================================================================
// BELUM RESPONS + EXPORT ATTEMPT (model "beda tools", disatukan ke Riwayat Blast)
//
// App TIDAK mengirim. Ia menghitung siapa yang belum respons dari HISTORI BLAST
// yang sama dengan report lama (collectInvoiceRetries: blast_recipients JOIN
// blast_logs JOIN chat_threads) — jadi data hasil metode LAMA (Blaster) ikut jadi
// kandidat Attempt 2/3 di masa transisi.
//
// Saat Export Attempt N dijalankan, tiap invoice DICATAT ke Riwayat Blast sebagai
// attempt N 'sent' (recordRetryBatchStart/recordRetryRecipient) + thread di-bump.
// Karena itu ia otomatis muncul di "Riwayat Blast (Audit Log)" DAN "Log Status
// Update" (keduanya baca tabel yang sama). Token di-reuse kalau sudah ada
// (getOrCreateToken idempoten); yang belum punya token (data metode lama) → generate.
// ============================================================================

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// startOfTodayWIB — 00:00 WIB hari ini (untuk guard "maks 1 attempt/hari per invoice"
// di collectInvoiceRetries). wibLoc di-set oleh startRetryScheduler saat startup.
func startOfTodayWIB() time.Time {
	loc := wibLoc
	if loc == nil {
		loc = time.FixedZone("WIB", 7*3600)
	}
	n := time.Now().In(loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
}

// handleBelumResponsStats — GET /api/belum-respons. Berapa invoice eligible untuk
// Attempt 2 & 3, dari histori blast (data lama + baru).
func handleBelumResponsStats(w http.ResponseWriter, r *http.Request) {
	batch := collectInvoiceRetries("majoo", "chat_threads", "blast_recipients", "blast_logs", 0, startOfTodayWIB())
	att2, att3 := 0, 0
	for _, x := range batch {
		switch x.nextAttempt {
		case 2:
			att2++
		case 3:
			att3++
		}
	}
	writeJSON(w, map[string]any{"attempt2": att2, "attempt3": att3, "total": att2 + att3})
}

// handleBelumResponsExport — GET /api/belum-respons/export?attempt=2|3.
// CSV (phone,nama_outlet,nomer_invoice,kode,link) untuk non-responder + CATAT ke
// Riwayat Blast sebagai attempt N 'sent'. Token di-reuse/di-generate per invoice.
func handleBelumResponsExport(w http.ResponseWriter, r *http.Request) {
	if intiNumber() == "" {
		httpErr(w, 400, "Nomor Inti belum diketahui — login WhatsApp Inti dulu atau set INTI_WA_NUMBER di .env.")
		return
	}
	targetAttempt, _ := strconv.Atoi(r.URL.Query().Get("attempt"))
	if targetAttempt != 2 && targetAttempt != 3 {
		httpErr(w, 400, "attempt harus 2 atau 3")
		return
	}
	batch := collectInvoiceRetries("majoo", "chat_threads", "blast_recipients", "blast_logs", targetAttempt, startOfTodayWIB())

	user, _ := userFromCtx(r.Context())
	now := time.Now()
	logID, err := recordRetryBatchStart(user.Email, user.Name, GetAttemptTemplate(targetAttempt), targetAttempt, len(batch), now)
	if err != nil {
		httpErr(w, 500, "batch start: %v", err)
		return
	}

	var buf bytes.Buffer
	buf.WriteString("\ufeff")
	cw := csv.NewWriter(&buf)
	// Header FIXED (format Tools Blast Resmi Majoo): phone, full_name(outlet),
	// nick_name(invoice), gender(kode), package(kosong).
	_ = cw.Write([]string{"phone", "full_name", "nick_name", "gender", "package"})

	n := 0
	for _, x := range batch {
		token := getOrCreateToken(x.phone, x.nomerInvoice, x.namaOutlet)
		if err := cw.Write([]string{x.phone, x.namaOutlet, x.nomerInvoice, token, ""}); err != nil {
			httpErr(w, 500, "write: %v", err)
			return
		}
		// Pesan yang "dikirim" (untuk record & Sheets) — template attempt N terisi + link/kode.
		body := applyLink(renderTemplateWithVars(GetAttemptTemplate(targetAttempt), x.namaOutlet, x.nomerInvoice), x.phone, x.nomerInvoice, x.namaOutlet)
		_ = recordRetryRecipient(logID, x.phone, x.namaOutlet, x.nomerInvoice, "sent", "", body, now)
		_ = bumpThreadAfterRetry("chat_threads", x.phone, body, targetAttempt, now)
		// Tampilkan pesan attempt ini di Inbox (penanda "sudah di-blast attempt N"),
		// walau pengiriman aktualnya via tools eksternal.
		_ = recordChatMessage(x.phone, "out", body, "", "", now, logID, user.Email, user.Name)
		n++
	}
	cw.Flush()

	if n > 0 {
		_ = recordRetryBatchEnd(logID, n, 0, now)
	} else {
		deleteEmptyBlastLog(logID)
	}

	fname := fmt.Sprintf("belum-respons-attempt%d-%s.csv", targetAttempt, now.Format("20060102-150405"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fname+`"`)
	w.Header().Set("X-Rows-Generated", strconv.Itoa(n))
	_, _ = w.Write(buf.Bytes())
}

// ---- Perekaman Attempt 1 (dipakai handleGenerateLinks) ----

// ensureThreadAfterBlast — buat thread 'after_blast' HANYA kalau belum ada. ON
// CONFLICT DO NOTHING supaya thread yang sudah maju (sudah balas / attempt lebih
// tinggi) TIDAK diklobrak balik ke after_blast/attempt 1.
func ensureThreadAfterBlast(phone, outlet, invoice string, logID int64, preview string, ts time.Time) {
	tsStr := ts.Format(time.RFC3339)
	_, _ = auditDB.Exec(`
INSERT INTO chat_threads (phone, nama_outlet, nomer_invoice, last_blast_id, last_message_at, last_message_preview, last_message_direction, status, unread_count, current_attempt, last_attempt_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, 'out', 'after_blast', 0, 1, ?, ?)
ON CONFLICT(phone) DO NOTHING`,
		phone, outlet, invoice, nullableID(logID), tsStr, truncate(preview, 80), tsStr, tsStr)
}

// recordAttempt1Sent — catat 1 invoice sebagai Attempt 1 'sent' ke Riwayat Blast +
// buat thread. Anti-dobel: skip kalau invoice ini sudah pernah tercatat attempt 1
// 'sent' (mis. data metode lama / re-upload). Return true kalau baru direkam.
func recordAttempt1Sent(logID int64, phone, outlet, invoice, body string, now time.Time) bool {
	var c int
	_ = auditDB.QueryRow(`
SELECT COUNT(*) FROM blast_recipients r JOIN blast_logs b ON r.blast_log_id=b.id
WHERE r.phone=? AND COALESCE(r.nomer_invoice,'')=COALESCE(?,'') AND r.status='sent' AND b.attempt=1`,
		phone, invoice).Scan(&c)
	if c > 0 {
		return false
	}
	_ = recordRetryRecipient(logID, phone, outlet, invoice, "sent", "", body, now)
	ensureThreadAfterBlast(phone, outlet, invoice, logID, body, now)
	return true
}
