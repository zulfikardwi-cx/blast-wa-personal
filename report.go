package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"os"
	"strconv"
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
	Attempts     int    `json:"attempts"`
	LastAttempt  string `json:"last_attempt_at"`
}

// queryUnresponsive — thread yang sudah di-blast minAttempt+ kali tapi belum direspons.
// status='after_blast' = belum ada balasan user (balasan user memindahkan ke 'open',
// agent reply ke 'in_progress', resolve ke 'done'). current_attempt = jumlah blast
// (1=blast awal, 2/3 = auto-retry). Jadi after_blast + attempt>=2 = sudah ditagih
// berulang tapi nomor itu tidak pernah merespons.
func queryUnresponsive(minAttempt int) ([]UnresponsiveRow, error) {
	rows, err := auditDB.Query(`
SELECT phone, COALESCE(nama_outlet, ''), COALESCE(nomer_invoice, ''), current_attempt, COALESCE(last_attempt_at, '')
FROM chat_threads
WHERE status = 'after_blast' AND current_attempt >= ?
ORDER BY current_attempt DESC, last_attempt_at ASC`, minAttempt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UnresponsiveRow{}
	for rows.Next() {
		var r UnresponsiveRow
		if err := rows.Scan(&r.Phone, &r.NamaOutlet, &r.NomerInvoice, &r.Attempts, &r.LastAttempt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// minAttemptParam — ?min_attempt=2|3, default 2. Clamp 1..3.
func minAttemptParam(r *http.Request) int {
	m := 2
	if v := r.URL.Query().Get("min_attempt"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 3 {
			m = n
		}
	}
	return m
}

// handleReportUnresponsive — JSON list untuk render tabel di UI.
func handleReportUnresponsive(w http.ResponseWriter, r *http.Request) {
	list, err := queryUnresponsive(minAttemptParam(r))
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	writeJSON(w, map[string]any{"rows": list, "count": len(list), "min_attempt": minAttemptParam(r)})
}

// handleReportUnresponsiveCSV — download CSV (Excel-friendly).
func handleReportUnresponsiveCSV(w http.ResponseWriter, r *http.Request) {
	list, err := queryUnresponsive(minAttemptParam(r))
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="belum-respons.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"Phone", "Nama Outlet", "Nomor Invoice", "Jumlah Blast", "Blast Terakhir"})
	for _, row := range list {
		_ = cw.Write([]string{row.Phone, row.NamaOutlet, row.NomerInvoice, strconv.Itoa(row.Attempts), row.LastAttempt})
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
	list, err := queryUnresponsive(minAttemptParam(r))
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

	now := time.Now().Format("2006-01-02 15:04")
	values := [][]any{{"Phone", "Nama Outlet", "Nomor Invoice", "Jumlah Blast", "Blast Terakhir", "Diperbarui"}}
	for _, row := range list {
		// prefix ' supaya Sheets treat phone sebagai text (bukan scientific notation)
		values = append(values, []any{"'" + row.Phone, row.NamaOutlet, row.NomerInvoice, row.Attempts, row.LastAttempt, now})
	}

	clearRange := fmt.Sprintf("%s!A:F", sn)
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
