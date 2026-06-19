package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

var (
	sheetsSvc     *sheets.Service
	spreadsheetID string
	sheetName     string
	sheetSALabel  string
)

// initSheets — opsional. Kalau env var tidak diset, fitur export ke Sheets dinonaktifkan
// (endpoint /api/export-sheet akan return error message yang menjelaskan).
func initSheets() error {
	saPath := os.Getenv("GOOGLE_SERVICE_ACCOUNT_JSON")
	spreadsheetID = os.Getenv("GSHEET_SPREADSHEET_ID")
	sheetName = os.Getenv("GSHEET_SHEET_NAME")
	if sheetName == "" {
		sheetName = "Blast Log"
	}

	if saPath == "" || spreadsheetID == "" {
		fmt.Println("Sheets export disabled (set GOOGLE_SERVICE_ACCOUNT_JSON + GSHEET_SPREADSHEET_ID di .env untuk aktifkan)")
		return nil
	}

	if _, err := os.Stat(saPath); err != nil {
		return fmt.Errorf("service account JSON tidak ditemukan di %s: %w", saPath, err)
	}

	ctx := context.Background()
	svc, err := sheets.NewService(ctx, option.WithCredentialsFile(saPath))
	if err != nil {
		return fmt.Errorf("init sheets client: %w", err)
	}
	sheetsSvc = svc
	sheetSALabel = saPath
	fmt.Println("Sheets export enabled — spreadsheet:", spreadsheetID, "/ sheet:", sheetName)
	return nil
}

func handleExportSheet(w http.ResponseWriter, r *http.Request) {
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
FROM blast_recipients r
JOIN blast_logs b ON r.blast_log_id = b.id
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
		// Excel/Sheets butuh phone sebagai string supaya tidak di-convert ke scientific notation
		// → prefix ' agar Sheets treat as text.
		values = append(values, []any{
			startedAt, userName, userEmail, attempt, "'" + phone, namaOutlet, nomerInv, status, errMsg, sentAt, message, blastID,
		})
		count++
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Clear sheet (range A:L cover semua kolom — termasuk Attempt)
	clearRange := fmt.Sprintf("%s!A:L", sheetName)
	if _, err := sheetsSvc.Spreadsheets.Values.Clear(spreadsheetID, clearRange, &sheets.ClearValuesRequest{}).Context(ctx).Do(); err != nil {
		httpErr(w, 500, "clear sheet: %v. Pastikan service account punya akses Editor ke spreadsheet.", err)
		return
	}

	// Write all values
	writeRange := fmt.Sprintf("%s!A1", sheetName)
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
		"sheet_name": sheetName,
	})
}

// ensureSheetExists — bikin tab (sheet) baru kalau belum ada. Dipakai report export
// supaya tab "Belum Respons" otomatis dibuat tanpa user harus bikin manual.
func ensureSheetExists(ctx context.Context, name string) error {
	ss, err := sheetsSvc.Spreadsheets.Get(spreadsheetID).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("get spreadsheet: %w", err)
	}
	for _, s := range ss.Sheets {
		if s.Properties != nil && s.Properties.Title == name {
			return nil
		}
	}
	_, err = sheetsSvc.Spreadsheets.BatchUpdate(spreadsheetID, &sheets.BatchUpdateSpreadsheetRequest{
		Requests: []*sheets.Request{{
			AddSheet: &sheets.AddSheetRequest{Properties: &sheets.SheetProperties{Title: name}},
		}},
	}).Context(ctx).Do()
	return err
}

// handleSheetStatus — quick check apakah Sheets export aktif (untuk UI button enable/disable)
func handleSheetStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"enabled":     sheetsSvc != nil,
		"spreadsheet": spreadsheetID,
		"sheet_name":  sheetName,
		"sheet_url":   fmt.Sprintf("https://docs.google.com/spreadsheets/d/%s/edit", spreadsheetID),
	})
}
