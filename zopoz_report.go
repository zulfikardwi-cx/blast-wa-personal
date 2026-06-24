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

func zopozQueryUnresponsive() ([]UnresponsiveRow, error) {
	rows, err := auditDB.Query(`
SELECT phone, COALESCE(nama_outlet, ''), COALESCE(nomer_invoice, ''), current_attempt, status, COALESCE(attempt1_failed, 0), COALESCE(reject_reason, '')
FROM zopoz_threads
WHERE status IN ('after_blast', 'in_progress', 'rejected', 'force_close')
ORDER BY current_attempt DESC, last_attempt_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UnresponsiveRow{}
	for rows.Next() {
		var phone, outlet, inv, status, reason string
		var cur, att1Failed int
		if err := rows.Scan(&phone, &outlet, &inv, &cur, &status, &att1Failed, &reason); err != nil {
			return nil, err
		}
		att1 := attStatus(1, cur)
		if att1Failed == 1 {
			att1 = "Rejected"
		}
		rejected := "-"
		note := ""
		if status == "rejected" || status == "force_close" {
			rejected = "reject"
			note = reason
		}
		out = append(out, UnresponsiveRow{
			Phone:        phone,
			NamaOutlet:   outlet,
			NomerInvoice: inv,
			Attempt1:     att1,
			Attempt2:     attStatus(2, cur),
			Attempt3:     attStatus(3, cur),
			Rejected:     rejected,
			Note:         note,
		})
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
