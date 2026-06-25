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
       MAX(CASE WHEN b.attempt=1 AND r.status='sent'   THEN 1 ELSE 0 END) AS a1s,
       MAX(CASE WHEN b.attempt=1 AND r.status='failed' THEN 1 ELSE 0 END) AS a1f,
       MAX(CASE WHEN b.attempt=2 AND r.status='sent'   THEN 1 ELSE 0 END) AS a2s,
       MAX(CASE WHEN b.attempt=3 AND r.status='sent'   THEN 1 ELSE 0 END) AS a3s,
       COALESCE(MAX(CASE WHEN b.attempt=1 AND r.status='failed' THEN r.error END), '') AS a1err,
       t.status,
       COALESCE(t.reject_reason, '')
FROM blast_recipients r
JOIN blast_logs b ON r.blast_log_id = b.id
JOIN chat_threads t ON t.phone = r.phone
WHERE t.status IN ('after_blast', 'in_progress', 'rejected', 'force_close')
  AND COALESCE(r.nomer_invoice, '') != ''
  AND NOT EXISTS (SELECT 1 FROM excluded_invoices x WHERE x.suite='majoo' AND x.phone=r.phone AND x.nomer_invoice=r.nomer_invoice)
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
	att1 := "-"
	if a1f == 1 {
		att1 = "Rejected"
	} else if a1s == 1 {
		att1 = "No Response"
	}
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
	if a1f == 1 {
		rejected = "reject"
		note = a1err
		if note == "" {
			note = "Gagal kirim Attempt 1"
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
