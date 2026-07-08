package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"os"
	"time"

	"google.golang.org/api/sheets/v4"
)

// reportSheetName — nama tab terpisah untuk report belum-respons (jangan clobber
// sheet "Blast Log"). Override via GSHEET_REPORT_SHEET_NAME.
func reportSheetName() string {
	if n := os.Getenv("GSHEET_REPORT_SHEET_NAME"); n != "" {
		return n
	}
	return "Belum Respons"
}

type UnresponsiveRow struct {
	Phone        string `json:"phone"`
	NamaOutlet   string `json:"nama_outlet"`
	NomerInvoice string `json:"nomer_invoice"`
	Attempt1     string `json:"attempt1"`
	Attempt2     string `json:"attempt2"`
	Attempt3     string `json:"attempt3"`
	Rejected     string `json:"rejected"`
	Note         string `json:"note"`
}

// attStatus — status tiap attempt untuk nomor yang belum merespons:
// "No Response" kalau attempt ke-n sudah dikirim (current_attempt >= n), "-" kalau
// belum dikirim cron. Karena thread ini masih after_blast/in_progress (belum dibalas),
// setiap attempt yang sudah terkirim pasti belum direspons.
func attStatus(n, current int) string {
	if current >= n {
		return "No Response"
	}
	return "-"
}

// queryUnresponsive — Log Status Update, PER NOMOR INVOICE (bukan per-nomor telepon).
// Satu nomor telepon bisa di-blast untuk banyak invoice; chat_threads (key = phone) hanya
// menyimpan invoice TERAKHIR, jadi report lama kehilangan invoice-invoice sebelumnya.
// Di sini sumbernya blast_recipients + blast_logs (punya kolom invoice & attempt per kirim),
// jadi tiap invoice tampil sebagai baris sendiri dengan progres attempt-nya masing-masing.
//
// Filter "Belum Respons" TETAP per-nomor (chat_threads.status ∈ after_blast/in_progress/
// rejected/force_close) — begitu nomor membalas/Done/Invalid, SEMUA invoice nomor itu keluar
// (balasan WA tidak terikat ke invoice tertentu). Logika kolom tidak berubah maknanya:
//   - Attempt N: "No Response" kalau attempt N invoice itu terkirim, "-" kalau belum,
//     "Rejected" untuk Attempt 1 yang GAGAL kirim.
//   - Rejected: "reject" bila Attempt 1 invoice gagal kirim, ATAU invoice sudah sampai
//     Attempt 3 dan nomornya ber-status force_close (tidak respons s/d jam cutoff).
func queryUnresponsive() ([]UnresponsiveRow, error) {
	rows, err := auditDB.Query(`
SELECT r.phone,
       COALESCE(MAX(r.nama_outlet), ''),
       r.nomer_invoice,
       MAX(CASE WHEN COALESCE(r.attempt,b.attempt)=1 AND r.status='sent'   THEN 1 ELSE 0 END) AS a1s,
       MAX(CASE WHEN COALESCE(r.attempt,b.attempt)=1 AND r.status='failed' THEN 1 ELSE 0 END) AS a1f,
       MAX(CASE WHEN COALESCE(r.attempt,b.attempt)=2 AND r.status='sent'   THEN 1 ELSE 0 END) AS a2s,
       MAX(CASE WHEN COALESCE(r.attempt,b.attempt)=3 AND r.status='sent'   THEN 1 ELSE 0 END) AS a3s,
       COALESCE(MAX(CASE WHEN COALESCE(r.attempt,b.attempt)=1 AND r.status='failed' THEN r.error END), '') AS a1err,
       t.status,
       COALESCE(t.reject_reason, '')
FROM blast_recipients r
JOIN blast_logs b ON r.blast_log_id = b.id
JOIN chat_threads t ON t.phone = r.phone
WHERE t.status IN ('after_blast', 'in_progress', 'rejected', 'force_close')
  AND COALESCE(r.nomer_invoice, '') != ''
  -- Kolom Attempt 1/2/3 hanya dari CYCLE (putaran) TERKINI. Data lama semua cycle=1 → no-op;
  -- setelah reset re-blast, report menampilkan progres putaran baru (Attempt 1 lagi), bukan lama.
  AND r.cycle = (SELECT MAX(cycle) FROM blast_recipients r2 WHERE r2.phone=r.phone AND COALESCE(r2.nomer_invoice,'')=COALESCE(r.nomer_invoice,''))
  AND NOT EXISTS (SELECT 1 FROM excluded_invoices x WHERE x.suite='majoo' AND x.phone=r.phone AND x.nomer_invoice=r.nomer_invoice)
  AND NOT EXISTS (SELECT 1 FROM resolved_invoices rv WHERE rv.suite='majoo' AND rv.phone=r.phone AND rv.nomer_invoice=r.nomer_invoice)
GROUP BY r.phone, r.nomer_invoice
ORDER BY r.phone ASC, r.nomer_invoice ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UnresponsiveRow{}
	for rows.Next() {
		var phone, outlet, inv, a1err, threadStatus, reason string
		var a1s, a1f, a2s, a3s int
		if err := rows.Scan(&phone, &outlet, &inv, &a1s, &a1f, &a2s, &a3s, &a1err, &threadStatus, &reason); err != nil {
			return nil, err
		}
		out = append(out, buildInvoiceRow(phone, outlet, inv, a1s, a1f, a2s, a3s, a1err, threadStatus, reason))
	}
	return out, rows.Err()
}

// buildInvoiceRow — susun UnresponsiveRow per-invoice dari agregat blast log (dipakai
// majoo & Zopoz supaya logikanya identik). Lihat queryUnresponsive untuk aturan kolom.
func buildInvoiceRow(phone, outlet, inv string, a1s, a1f, a2s, a3s int, a1err, threadStatus, reason string) UnresponsiveRow {
	// PRIORITAS SENT: untuk satu (nomor, invoice) bisa ada beberapa baris blast log — mis. satu
	// gagal (error sistem/ban: usync timeout / 463 / websocket) lalu di batch lain berhasil.
	// Kalau ada yang SENT, ambil yang sent → "No Response". "Rejected" HANYA untuk penolakan
	// NYATA (nomor tidak terdaftar di WhatsApp), yang ditandai lewat thread status='rejected'.
	// Error sistem/ban BUKAN reject dan tidak boleh menutupi baris sent.
	realReject := (threadStatus == "rejected")

	att1 := "-"
	if a1s == 1 {
		att1 = "No Response" // pernah terkirim → ambil yang sent, abaikan baris gagal error sistem
	} else if realReject {
		att1 = "Rejected"
	}
	// a1f tanpa sent & bukan realReject = gagal error sistem/ban → biarkan "-" (bukan Rejected).

	att2 := "-"
	if a2s == 1 {
		att2 = "No Response"
	}
	att3 := "-"
	if a3s == 1 {
		att3 = "No Response"
	}
	rejected := "-"
	note := ""
	if att1 == "Rejected" {
		rejected = "reject"
		note = reason
		if note == "" {
			note = a1err
		}
		if note == "" {
			note = "Nomor tidak terdaftar di WhatsApp"
		}
	} else if a3s == 1 && threadStatus == "force_close" {
		rejected = "reject"
		note = reason
	}
	return UnresponsiveRow{
		Phone:        phone,
		NamaOutlet:   outlet,
		NomerInvoice: inv,
		Attempt1:     att1,
		Attempt2:     att2,
		Attempt3:     att3,
		Rejected:     rejected,
		Note:         note,
	}
}

// handleReportUnresponsive — JSON list untuk render tabel di UI.
func handleReportUnresponsive(w http.ResponseWriter, r *http.Request) {
	list, err := queryUnresponsive()
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	writeJSON(w, map[string]any{"rows": list, "count": len(list)})
}

var reportHeader = []string{"Phone", "Nama Outlet", "Nomor Invoice", "Status Attempt 1", "Status Attempt 2", "Status Attempt 3", "Rejected", "Info / Alasan"}

// handleReportUnresponsiveCSV — download CSV (Excel-friendly).
func handleReportUnresponsiveCSV(w http.ResponseWriter, r *http.Request) {
	list, err := queryUnresponsive()
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="belum-respons.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write(reportHeader)
	for _, row := range list {
		_ = cw.Write([]string{row.Phone, row.NamaOutlet, row.NomerInvoice, row.Attempt1, row.Attempt2, row.Attempt3, row.Rejected, row.Note})
	}
	cw.Flush()
}

// handleReportExportSheet — push report ke tab "Belum Respons" di Google Sheet.
func handleReportExportSheet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if sheetsSvc == nil {
		httpErr(w, 400, "Sheets export belum dikonfigurasi. Set GOOGLE_SERVICE_ACCOUNT_JSON + GSHEET_SPREADSHEET_ID di .env, lalu restart backend.")
		return
	}
	list, err := queryUnresponsive()
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}

	sn := reportSheetName()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := ensureSheetExists(ctx, sn); err != nil {
		httpErr(w, 500, "siapkan tab '%s': %v", sn, err)
		return
	}

	values := [][]any{{"Phone", "Nama Outlet", "Nomor Invoice", "Status Attempt 1", "Status Attempt 2", "Status Attempt 3", "Rejected", "Info / Alasan"}}
	for _, row := range list {
		// prefix ' supaya Sheets treat phone sebagai text (bukan scientific notation)
		values = append(values, []any{"'" + row.Phone, row.NamaOutlet, row.NomerInvoice, row.Attempt1, row.Attempt2, row.Attempt3, row.Rejected, row.Note})
	}

	clearRange := fmt.Sprintf("%s!A:H", sn)
	if _, err := sheetsSvc.Spreadsheets.Values.Clear(spreadsheetID, clearRange, &sheets.ClearValuesRequest{}).Context(ctx).Do(); err != nil {
		httpErr(w, 500, "clear sheet: %v. Pastikan service account punya akses Editor.", err)
		return
	}
	writeRange := fmt.Sprintf("%s!A1", sn)
	if _, err := sheetsSvc.Spreadsheets.Values.Update(spreadsheetID, writeRange, &sheets.ValueRange{Values: values}).
		ValueInputOption("USER_ENTERED").Context(ctx).Do(); err != nil {
		httpErr(w, 500, "write sheet: %v", err)
		return
	}

	writeJSON(w, map[string]any{
		"ok":         true,
		"rows":       len(list),
		"sheet_url":  fmt.Sprintf("https://docs.google.com/spreadsheets/d/%s/edit", spreadsheetID),
		"sheet_name": sn,
	})
}

// ---- Report Resolved (siapa PIC yang menyelesaikan tiap invoice) ----

// resolvedSheetName — tab untuk report resolved. Override via GSHEET_RESOLVED_SHEET_NAME.
func resolvedSheetName() string {
	if n := os.Getenv("GSHEET_RESOLVED_SHEET_NAME"); n != "" {
		return n
	}
	return "report resolved"
}

type ResolvedRow struct {
	NomerInvoice string `json:"nomer_invoice"`
	NamaOutlet   string `json:"nama_outlet"`
	Phone        string `json:"phone"`
	PICName      string `json:"pic_name"`
	PICEmail     string `json:"pic_email"`
	ResolvedAt   string `json:"resolved_at"` // waktu Done/Resolved (WIB, "2006-01-02 15:04")
}

// formatResolvedAt — normalkan resolved_at ke tampilan WIB "YYYY-MM-DD HH:MM".
// Dua kemungkinan sumber: RFC3339 +07:00 (dari markPhoneResolved) atau "YYYY-MM-DD HH:MM:SS"
// UTC (default datetime('now') pada baris backfill lama).
func formatResolvedAt(s string) string {
	if s == "" {
		return ""
	}
	wib := time.FixedZone("WIB", 7*3600)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(wib).Format("2006-01-02 15:04")
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t.Add(7 * time.Hour).Format("2006-01-02 15:04") // datetime('now') = UTC → +7
	}
	return s
}

// queryResolved — PER NOMOR INVOICE yang sudah Done/Resolved, dari tabel resolved_invoices
// (permanen: sekali di-Done tetap tercatat walau nomornya di-blast ulang untuk invoice lain).
// Satu nomor bisa punya banyak invoice → tiap invoice jadi baris sendiri, di-tag PIC resolver.
func queryResolved() ([]ResolvedRow, error) {
	rows, err := auditDB.Query(`
SELECT nomer_invoice,
       COALESCE(nama_outlet, ''),
       phone,
       COALESCE(resolver_name, ''),
       COALESCE(resolver_email, ''),
       COALESCE(resolved_at, '')
FROM resolved_invoices
WHERE suite = 'majoo'
ORDER BY resolved_at DESC, phone ASC, nomer_invoice ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResolvedRow{}
	for rows.Next() {
		var row ResolvedRow
		var resolvedAt string
		if err := rows.Scan(&row.NomerInvoice, &row.NamaOutlet, &row.Phone, &row.PICName, &row.PICEmail, &resolvedAt); err != nil {
			return nil, err
		}
		row.ResolvedAt = formatResolvedAt(resolvedAt)
		out = append(out, row)
	}
	return out, rows.Err()
}

// handleReportResolved — JSON list untuk render tabel di UI.
func handleReportResolved(w http.ResponseWriter, r *http.Request) {
	list, err := queryResolved()
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	writeJSON(w, map[string]any{"rows": list, "count": len(list)})
}

var resolvedHeader = []string{"Nomor Invoice", "Nama Outlet", "Nomor User", "Nama PIC (Resolved)", "Waktu Resolved (WIB)"}

// handleReportResolvedCSV — download CSV.
func handleReportResolvedCSV(w http.ResponseWriter, r *http.Request) {
	list, err := queryResolved()
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="report-resolved.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write(resolvedHeader)
	for _, row := range list {
		_ = cw.Write([]string{row.NomerInvoice, row.NamaOutlet, row.Phone, row.PICName, row.ResolvedAt})
	}
	cw.Flush()
}

// handleReportResolvedExportSheet — push report ke tab "report resolved" di Google Sheet.
func handleReportResolvedExportSheet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if sheetsSvc == nil {
		httpErr(w, 400, "Sheets export belum dikonfigurasi. Set GOOGLE_SERVICE_ACCOUNT_JSON + GSHEET_SPREADSHEET_ID di .env, lalu restart backend.")
		return
	}
	list, err := queryResolved()
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}

	sn := resolvedSheetName()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := ensureSheetExists(ctx, sn); err != nil {
		httpErr(w, 500, "siapkan tab '%s': %v", sn, err)
		return
	}

	values := [][]any{{"Nomor Invoice", "Nama Outlet", "Nomor User", "Nama PIC (Resolved)", "Waktu Resolved (WIB)"}}
	for _, row := range list {
		// prefix ' supaya Sheets treat nomor sebagai text (bukan scientific notation)
		values = append(values, []any{row.NomerInvoice, row.NamaOutlet, "'" + row.Phone, row.PICName, row.ResolvedAt})
	}

	clearRange := fmt.Sprintf("%s!A:E", sn)
	if _, err := sheetsSvc.Spreadsheets.Values.Clear(spreadsheetID, clearRange, &sheets.ClearValuesRequest{}).Context(ctx).Do(); err != nil {
		httpErr(w, 500, "clear sheet: %v. Pastikan service account punya akses Editor.", err)
		return
	}
	writeRange := fmt.Sprintf("%s!A1", sn)
	if _, err := sheetsSvc.Spreadsheets.Values.Update(spreadsheetID, writeRange, &sheets.ValueRange{Values: values}).
		ValueInputOption("USER_ENTERED").Context(ctx).Do(); err != nil {
		httpErr(w, 500, "write sheet: %v", err)
		return
	}

	writeJSON(w, map[string]any{
		"ok":         true,
		"rows":       len(list),
		"sheet_url":  fmt.Sprintf("https://docs.google.com/spreadsheets/d/%s/edit", spreadsheetID),
		"sheet_name": sn,
	})
}
