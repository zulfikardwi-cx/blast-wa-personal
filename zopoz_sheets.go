package main

// Export riwayat blast Zopoz ke Google Sheet — spreadsheet SAMA dengan majoo
// (reuse sheetsSvc + spreadsheetID dari sheets.go, read-only), tapi tab TERPISAH
// "Log Blast Zopoz" supaya proses & data tidak campur dengan tab majoo ("Blast Log").
// Pola persis handleExportSheet: full-snapshot (clear A:L lalu tulis ulang), manual
// via tombol di halaman Zopoz Blast. Bedanya: baca dari zopoz_blast_recipients/logs
// dan auto-create tab-nya (ensureSheetExists) karena tab Zopoz belum tentu ada.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"google.golang.org/api/sheets/v4"
)

// nama tab Zopoz — override via GSHEET_ZOPOZ_SHEET_NAME, default "Log Blast Zopoz".
var zopozSheetName string

func initZopozSheetName() {
	zopozSheetName = os.Getenv("GSHEET_ZOPOZ_SHEET_NAME")
	if zopozSheetName == "" {
		zopozSheetName = "Log Blast Zopoz"
	}
}

func zopozHandleSheetStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"enabled":     sheetsSvc != nil,
		"spreadsheet": spreadsheetID,
		"sheet_name":  zopozSheetName,
		"sheet_url":   fmt.Sprintf("https://docs.google.com/spreadsheets/d/%s/edit", spreadsheetID),
	})
}

func zopozHandleExportSheet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if sheetsSvc == nil {
		httpErr(w, 400, "Sheets export belum dikonfigurasi. Set GOOGLE_SERVICE_ACCOUNT_JSON + GSHEET_SPREADSHEET_ID di .env, lalu restart backend.")
		return
	}

	rows, err := auditDB.Query(`
SELECT
	b.started_at,
	COALESCE(b.user_name, ''),
	b.user_email,
	r.phone,
	COALESCE(r.nama_outlet, ''),
	COALESCE(r.nomer_invoice, ''),
	r.status,
	COALESCE(r.error, ''),
	COALESCE(r.sent_at, ''),
	COALESCE(r.message, ''),
	r.blast_log_id,
	COALESCE(b.attempt, 1)
FROM zopoz_blast_recipients r
JOIN zopoz_blast_logs b ON r.blast_log_id = b.id
ORDER BY r.id ASC`)
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	defer rows.Close()

	values := [][]any{{
		"Waktu Blast",
		"User",
		"Email",
		"Attempt",
		"Phone",
		"Nama Outlet",
		"Nomor Invoice",
		"Status",
		"Error",
		"Sent At",
		"Pesan",
		"Blast ID",
	}}

	count := 0
	for rows.Next() {
		var startedAt, userName, userEmail, phone, namaOutlet, nomerInv, status, errMsg, sentAt, message string
		var blastID int64
		var attempt int
		if err := rows.Scan(&startedAt, &userName, &userEmail, &phone, &namaOutlet, &nomerInv, &status, &errMsg, &sentAt, &message, &blastID, &attempt); err != nil {
			continue
		}
		values = append(values, []any{
			startedAt, userName, userEmail, attempt, "'" + phone, namaOutlet, nomerInv, status, errMsg, sentAt, message, blastID,
		})
		count++
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Pastikan tab "Log Blast Zopoz" ada — auto-create kalau belum (tab majoo tak disentuh).
	if err := ensureSheetExists(ctx, zopozSheetName); err != nil {
		httpErr(w, 500, "buat tab: %v. Pastikan service account punya akses Editor ke spreadsheet.", err)
		return
	}

	clearRange := fmt.Sprintf("%s!A:L", zopozSheetName)
	if _, err := sheetsSvc.Spreadsheets.Values.Clear(spreadsheetID, clearRange, &sheets.ClearValuesRequest{}).Context(ctx).Do(); err != nil {
		httpErr(w, 500, "clear sheet: %v. Pastikan service account punya akses Editor ke spreadsheet.", err)
		return
	}

	writeRange := fmt.Sprintf("%s!A1", zopozSheetName)
	if _, err := sheetsSvc.Spreadsheets.Values.Update(spreadsheetID, writeRange, &sheets.ValueRange{Values: values}).
		ValueInputOption("USER_ENTERED").
		Context(ctx).Do(); err != nil {
		httpErr(w, 500, "write sheet: %v", err)
		return
	}

	sheetURL := fmt.Sprintf("https://docs.google.com/spreadsheets/d/%s/edit", spreadsheetID)
	writeJSON(w, map[string]any{
		"ok":         true,
		"rows":       count,
		"sheet_url":  sheetURL,
		"sheet_name": zopozSheetName,
	})
}
