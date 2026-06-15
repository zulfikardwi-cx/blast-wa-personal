package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// querySheetName — tab sumber mapping team. Override via GSHEET_QUERY_SHEET_NAME.
func querySheetName() string {
	if n := os.Getenv("GSHEET_QUERY_SHEET_NAME"); n != "" {
		return n
	}
	return "Query Blast"
}

// syncTeamsFromSheet — baca tab "Query Blast", ambil mapping Phone -> Team (+Area),
// lalu update kolom team/area di chat_threads. Return jumlah thread yang ke-update.
// Cocok by phone (sudah dinormalkan ke 62...) supaya nyambung ke thread inbox.
func syncTeamsFromSheet(ctx context.Context) (int, error) {
	if sheetsSvc == nil {
		return 0, fmt.Errorf("Sheets belum dikonfigurasi (GOOGLE_SERVICE_ACCOUNT_JSON + GSHEET_SPREADSHEET_ID)")
	}
	sn := querySheetName()
	resp, err := sheetsSvc.Spreadsheets.Values.Get(spreadsheetID, fmt.Sprintf("%s!A:Z", sn)).Context(ctx).Do()
	if err != nil {
		return 0, fmt.Errorf("baca tab '%s': %w", sn, err)
	}
	if len(resp.Values) < 2 {
		return 0, fmt.Errorf("tab '%s' kosong / tidak ada baris data", sn)
	}

	// index kolom dari header (case-insensitive) — robust kalau urutan kolom berubah
	idx := map[string]int{}
	for i, c := range resp.Values[0] {
		idx[strings.ToLower(strings.TrimSpace(fmt.Sprint(c)))] = i
	}
	pi, ok1 := idx["phone"]
	ti, ok2 := idx["team"]
	if !ok1 || !ok2 {
		return 0, fmt.Errorf("kolom 'Phone' & 'Team' wajib ada di tab '%s'", sn)
	}
	ai, hasArea := idx["area"]

	cell := func(row []interface{}, i int) string {
		if i < 0 || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(row[i]))
	}

	// mapping phone -> {team, area}; baris terakhir menang kalau ada duplikat phone
	type ta struct{ team, area string }
	m := map[string]ta{}
	for _, row := range resp.Values[1:] {
		phone := normalizePhone(cell(row, pi))
		team := cell(row, ti)
		if phone == "" || team == "" {
			continue
		}
		area := ""
		if hasArea {
			area = cell(row, ai)
		}
		m[phone] = ta{team: team, area: area}
	}

	updated := 0
	for phone, v := range m {
		res, e := auditDB.Exec(`UPDATE chat_threads SET team = ?, area = ? WHERE phone = ?`, v.team, v.area, phone)
		if e != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			updated++
		}
	}
	return updated, nil
}

// handleSyncTeams — POST /api/inbox/sync-teams. Tarik mapping team dari sheet.
func handleSyncTeams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	n, err := syncTeamsFromSheet(ctx)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "updated": n, "sheet": querySheetName()})
}
