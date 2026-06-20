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

// queryUnresponsive — Log Status Update: bucket After Blast + In Progress (belum
// direspons) PLUS rejected (Attempt 1 gagal kirim) PLUS force_close (Attempt 3 tanpa
// respons). after_blast/in_progress keluar saat user balas (→ open) / done / invalid.
// rejected & force_close SENGAJA tetap tampil & tertagging reject — data ini ditarik
// untuk diproses reject di sistem lain (jangan dihilangkan dari sini).
func queryUnresponsive() ([]UnresponsiveRow, error) {
	rows, err := auditDB.Query(`
SELECT phone, COALESCE(nama_outlet, ''), COALESCE(nomer_invoice, ''), current_attempt, status, COALESCE(attempt1_failed, 0), COALESCE(reject_reason, '')
FROM chat_threads
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
		// Attempt 1 gagal kirim → sel Attempt 1 = "Rejected" (bukan No Response).
		att1 := attStatus(1, cur)
		if att1Failed == 1 {
			att1 = "Rejected"
		}
		// rejected (gagal Attempt 1) & force_close (Attempt 3 no-response) → tertagging reject.
		rejected := "-"
		note := ""
		if status == "rejected" || status == "force_close" {
			rejected = "reject"
			note = reason // alasan: nomor tidak terdaftar / tidak respons s/d 17:00, dll
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
