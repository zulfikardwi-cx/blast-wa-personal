package main

// Report "Belum Response / Log Status Update" untuk Zopoz — logic IDENTIK report.go,
// tapi baca dari zopoz_threads dan export ke tab Sheet terpisah ("Belum Respons Zopoz").
// Reuse struct UnresponsiveRow, helper attStatus, dan reportHeader dari report.go (pure).
// Tidak menyentuh fungsi/tabel majoo.

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"os"
	"time"

	"google.golang.org/api/sheets/v4"
)

func zopozReportSheetName() string {
	if n := os.Getenv("ZOPOZ_GSHEET_REPORT_SHEET_NAME"); n != "" {
		return n
	}
	return "Belum Respons Zopoz"
}

// zopozQueryUnresponsive — versi PER NOMOR INVOICE (identik logika majoo, lihat
// queryUnresponsive di report.go). Sumber: zopoz_blast_recipients + zopoz_blast_logs,
// filter belum-respons dari zopoz_threads. Reuse buildInvoiceRow agar logika sama persis.
func zopozQueryUnresponsive() ([]UnresponsiveRow, error) {
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
FROM zopoz_blast_recipients r
JOIN zopoz_blast_logs b ON r.blast_log_id = b.id
JOIN zopoz_threads t ON t.phone = r.phone
WHERE t.status IN ('after_blast', 'in_progress', 'rejected', 'force_close')
  AND COALESCE(r.nomer_invoice, '') != ''
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

func zopozHandleReportUnresponsive(w http.ResponseWriter, r *http.Request) {
	list, err := zopozQueryUnresponsive()
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	writeJSON(w, map[string]any{"rows": list, "count": len(list)})
}

func zopozHandleReportUnresponsiveCSV(w http.ResponseWriter, r *http.Request) {
	list, err := zopozQueryUnresponsive()
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="belum-respons-zopoz.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write(reportHeader)
	for _, row := range list {
		_ = cw.Write([]string{row.Phone, row.NamaOutlet, row.NomerInvoice, row.Attempt1, row.Attempt2, row.Attempt3, row.Rejected, row.Note})
	}
	cw.Flush()
}

func zopozHandleReportExportSheet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if sheetsSvc == nil {
		httpErr(w, 400, "Sheets export belum dikonfigurasi. Set GOOGLE_SERVICE_ACCOUNT_JSON + GSHEET_SPREADSHEET_ID di .env, lalu restart backend.")
		return
	}
	list, err := zopozQueryUnresponsive()
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}

	sn := zopozReportSheetName()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := ensureSheetExists(ctx, sn); err != nil {
		httpErr(w, 500, "siapkan tab '%s': %v", sn, err)
		return
	}

	values := [][]any{{"Phone", "Nama Outlet", "Nomor Invoice", "Status Attempt 1", "Status Attempt 2", "Status Attempt 3", "Rejected", "Info / Alasan"}}
	for _, row := range list {
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
