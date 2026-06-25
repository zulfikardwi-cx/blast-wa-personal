package main

// Exclude per NOMOR INVOICE — tagging manual supaya satu invoice tidak ikut antrian
// retry attempt 2/3 DAN keluar dari report "Belum Respons". Satu tabel dipakai bersama
// majoo & Zopoz, dibedakan kolom `suite`. Filter penerapannya:
//   - retry: collectInvoiceRetries / invoiceStillNeedsRetry (retry_invoice.go) — NOT EXISTS.
//   - report: queryUnresponsive (report.go) & zopozQueryUnresponsive (zopoz_report.go).

import (
	"net/http"
	"strings"
)

func initExclusions() error {
	_, err := auditDB.Exec(`
CREATE TABLE IF NOT EXISTS excluded_invoices (
	suite TEXT NOT NULL,
	phone TEXT NOT NULL,
	nomer_invoice TEXT NOT NULL,
	excluded_by TEXT,
	excluded_at TEXT NOT NULL DEFAULT (datetime('now')),
	PRIMARY KEY (suite, phone, nomer_invoice)
);`)
	return err
}

func isInvoiceExcluded(suite, phone, invoice string) bool {
	var c int
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM excluded_invoices WHERE suite=? AND phone=? AND nomer_invoice=?`, suite, phone, invoice).Scan(&c)
	return c > 0
}

func excludeInvoice(w http.ResponseWriter, r *http.Request, suite string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if err := r.ParseForm(); err != nil {
		httpErr(w, 400, "form: %v", err)
		return
	}
	phone := strings.TrimSpace(r.FormValue("phone"))
	inv := strings.TrimSpace(r.FormValue("nomer_invoice"))
	if phone == "" || inv == "" {
		httpErr(w, 400, "phone & nomer_invoice wajib")
		return
	}
	user, _ := userFromCtx(r.Context())
	if _, err := auditDB.Exec(`INSERT OR IGNORE INTO excluded_invoices (suite, phone, nomer_invoice, excluded_by) VALUES (?,?,?,?)`,
		suite, phone, inv, user.Email); err != nil {
		httpErr(w, 500, "exclude: %v", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func includeInvoice(w http.ResponseWriter, r *http.Request, suite string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if err := r.ParseForm(); err != nil {
		httpErr(w, 400, "form: %v", err)
		return
	}
	phone := strings.TrimSpace(r.FormValue("phone"))
	inv := strings.TrimSpace(r.FormValue("nomer_invoice"))
	if phone == "" || inv == "" {
		httpErr(w, 400, "phone & nomer_invoice wajib")
		return
	}
	if _, err := auditDB.Exec(`DELETE FROM excluded_invoices WHERE suite=? AND phone=? AND nomer_invoice=?`, suite, phone, inv); err != nil {
		httpErr(w, 500, "include: %v", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func listExcluded(w http.ResponseWriter, r *http.Request, suite string) {
	rows, err := auditDB.Query(`SELECT phone, nomer_invoice, COALESCE(excluded_by,''), COALESCE(excluded_at,'') FROM excluded_invoices WHERE suite=? ORDER BY excluded_at DESC LIMIT 1000`, suite)
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	defer rows.Close()
	type exRow struct {
		Phone        string `json:"phone"`
		NomerInvoice string `json:"nomer_invoice"`
		ExcludedBy   string `json:"excluded_by"`
		ExcludedAt   string `json:"excluded_at"`
	}
	out := []exRow{}
	for rows.Next() {
		var x exRow
		if rows.Scan(&x.Phone, &x.NomerInvoice, &x.ExcludedBy, &x.ExcludedAt) == nil {
			out = append(out, x)
		}
	}
	writeJSON(w, map[string]any{"rows": out, "count": len(out)})
}

// ---- majoo ----
func handleRetryExclude(w http.ResponseWriter, r *http.Request)  { excludeInvoice(w, r, "majoo") }
func handleRetryInclude(w http.ResponseWriter, r *http.Request)  { includeInvoice(w, r, "majoo") }
func handleRetryExcluded(w http.ResponseWriter, r *http.Request) { listExcluded(w, r, "majoo") }

// ---- Zopoz ----
func zopozHandleRetryExclude(w http.ResponseWriter, r *http.Request)  { excludeInvoice(w, r, "zopoz") }
func zopozHandleRetryInclude(w http.ResponseWriter, r *http.Request)  { includeInvoice(w, r, "zopoz") }
func zopozHandleRetryExcluded(w http.ResponseWriter, r *http.Request) { listExcluded(w, r, "zopoz") }
