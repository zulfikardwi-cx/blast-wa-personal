package main

// resolved_invoices — SET invoice yang sudah Done/Resolved, per (suite, phone, nomer_invoice).
// Tujuan: sekali sebuah invoice/percakapan di-Done, invoice itu TIDAK boleh ikut antrian
// retry attempt 2/3 lagi — walaupun nomornya di-blast ulang untuk invoice LAIN (yang
// me-reset status thread ke after_blast). Sebelumnya "sudah Done" hanya tersimpan sebagai
// status thread PER NOMOR, jadi hilang saat re-blast → invoice lama ikut eligible lagi.
//
// Juga jadi sumber report "report resolved" (permanen, tahan re-blast).
//
// Model: satu Done menutup SATU percakapan = SEMUA invoice yang sudah ter-blast (attempt-1
// terkirim) ke nomor itu s/d saat Done. Jadi saat Done kita snapshot semua invoice tsb.

import (
	"fmt"
	"strings"
	"time"
)

func initResolvedInvoices() error {
	if _, err := auditDB.Exec(`
CREATE TABLE IF NOT EXISTS resolved_invoices (
	suite TEXT NOT NULL DEFAULT 'majoo',
	phone TEXT NOT NULL,
	nomer_invoice TEXT NOT NULL,
	nama_outlet TEXT,
	resolver_email TEXT,
	resolver_name TEXT,
	resolved_at TEXT NOT NULL DEFAULT (datetime('now')),
	PRIMARY KEY (suite, phone, nomer_invoice)
);`); err != nil {
		return err
	}
	// resolve_logs (dipakai singkat di iterasi sebelumnya) digantikan resolved_invoices.
	_, _ = auditDB.Exec(`DROP TABLE IF EXISTS resolve_logs`)
	backfillResolvedInvoices()
	return nil
}

func isInvoiceResolved(suite, phone, invoice string) bool {
	var c int
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM resolved_invoices WHERE suite=? AND phone=? AND nomer_invoice=?`, suite, phone, invoice).Scan(&c)
	return c > 0
}

// markPhoneResolved — snapshot SEMUA invoice yang pernah ter-blast (attempt-1 terkirim) ke
// nomor ini sebagai resolved, di-tag resolver. Dipanggil saat thread di-Done. Idempoten;
// resolver/timestamp di-update ke Done TERAKHIR (ON CONFLICT DO UPDATE).
func markPhoneResolved(suite, recvTbl, logsTbl, phone, resolverEmail, resolverName string, at time.Time) {
	_, err := auditDB.Exec(`
INSERT INTO resolved_invoices (suite, phone, nomer_invoice, nama_outlet, resolver_email, resolver_name, resolved_at)
SELECT ?, r.phone, r.nomer_invoice, MAX(COALESCE(r.nama_outlet,'')), ?, ?, ?
FROM `+recvTbl+` r JOIN `+logsTbl+` b ON r.blast_log_id=b.id
WHERE r.phone=? AND b.attempt=1 AND r.status='sent' AND COALESCE(r.nomer_invoice,'')!=''
GROUP BY r.nomer_invoice
ON CONFLICT(suite, phone, nomer_invoice) DO UPDATE SET
	resolver_email=excluded.resolver_email,
	resolver_name=excluded.resolver_name,
	resolved_at=excluded.resolved_at,
	nama_outlet=COALESCE(NULLIF(excluded.nama_outlet,''), resolved_invoices.nama_outlet)`,
		suite, resolverEmail, resolverName, at.Format(time.RFC3339), phone)
	if err != nil {
		fmt.Println("warn: markPhoneResolved:", err)
	}
}

// unresolvePhone — lepas SEMUA invoice sebuah nomor dari resolved_invoices (undo Done).
// Dipakai saat Reopen: invoice kembali "belum selesai" → muncul lagi di picker Done +
// kembali eligible antrian Attempt 2/3.
func unresolvePhone(suite, phone string) {
	if _, err := auditDB.Exec(`DELETE FROM resolved_invoices WHERE suite=? AND phone=?`, suite, phone); err != nil {
		fmt.Println("warn: unresolvePhone:", err)
	}
}

// unresolveInvoice — lepas SATU invoice (per phone) dari resolved_invoices. Dipakai saat
// invoice yang sudah Done DI-BLAST ULANG (Attempt 1 baru): invoice keluar dari status
// resolved supaya thread balik ke after_blast & proses Attempt 1-2-3 mengulang dari awal.
func unresolveInvoice(suite, phone, invoice string) {
	if _, err := auditDB.Exec(`DELETE FROM resolved_invoices WHERE suite=? AND phone=? AND nomer_invoice=?`, suite, phone, invoice); err != nil {
		fmt.Println("warn: unresolveInvoice:", err)
	}
}

// invoiceStatus — 1 invoice yang pernah ter-blast (attempt-1 terkirim) ke sebuah nomor,
// beserta status resolved-nya. Dipakai picker "pilih invoice mana yang di-Done".
type invoiceStatus struct {
	Invoice  string `json:"nomer_invoice"`
	Outlet   string `json:"nama_outlet"`
	Resolved bool   `json:"resolved"`
}

// phoneInvoiceStatuses — daftar invoice attempt-1 'sent' untuk sebuah nomor + flag resolved.
func phoneInvoiceStatuses(suite, recvTbl, logsTbl, phone string) []invoiceStatus {
	rows, err := auditDB.Query(`
SELECT r.nomer_invoice, MAX(COALESCE(r.nama_outlet,'')) AS outlet,
       EXISTS(SELECT 1 FROM resolved_invoices rv WHERE rv.suite=? AND rv.phone=r.phone AND rv.nomer_invoice=r.nomer_invoice) AS resolved
FROM `+recvTbl+` r JOIN `+logsTbl+` b ON r.blast_log_id=b.id
WHERE r.phone=? AND b.attempt=1 AND r.status='sent' AND COALESCE(r.nomer_invoice,'')!=''
GROUP BY r.nomer_invoice
ORDER BY resolved ASC, r.nomer_invoice ASC`, suite, phone)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []invoiceStatus
	for rows.Next() {
		var s invoiceStatus
		var res int
		if rows.Scan(&s.Invoice, &s.Outlet, &res) == nil {
			s.Resolved = res == 1
			out = append(out, s)
		}
	}
	return out
}

// markInvoicesResolved — seperti markPhoneResolved tapi HANYA untuk daftar invoice tertentu
// (agent memilih invoice mana yang selesai divalidasi). invoices di-filter server-side ke
// invoice yang benar-benar pernah ter-blast ke nomor itu (aman dari input sembarangan).
// Kembalikan jumlah invoice yang tercatat resolved.
func markInvoicesResolved(suite, recvTbl, logsTbl, phone string, invoices []string, resolverEmail, resolverName string, at time.Time) int {
	if len(invoices) == 0 {
		return 0
	}
	ph := make([]string, len(invoices))
	args := []any{suite, resolverEmail, resolverName, at.Format(time.RFC3339), phone}
	for i, inv := range invoices {
		ph[i] = "?"
		args = append(args, inv)
	}
	res, err := auditDB.Exec(`
INSERT INTO resolved_invoices (suite, phone, nomer_invoice, nama_outlet, resolver_email, resolver_name, resolved_at)
SELECT ?, r.phone, r.nomer_invoice, MAX(COALESCE(r.nama_outlet,'')), ?, ?, ?
FROM `+recvTbl+` r JOIN `+logsTbl+` b ON r.blast_log_id=b.id
WHERE r.phone=? AND b.attempt=1 AND r.status='sent' AND COALESCE(r.nomer_invoice,'')!=''
  AND r.nomer_invoice IN (`+strings.Join(ph, ",")+`)
GROUP BY r.nomer_invoice
ON CONFLICT(suite, phone, nomer_invoice) DO UPDATE SET
	resolver_email=excluded.resolver_email,
	resolver_name=excluded.resolver_name,
	resolved_at=excluded.resolved_at,
	nama_outlet=COALESCE(NULLIF(excluded.nama_outlet,''), resolved_invoices.nama_outlet)`, args...)
	if err != nil {
		fmt.Println("warn: markInvoicesResolved:", err)
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// phoneHasUnresolvedInvoice — true kalau nomor masih punya ≥1 invoice attempt-1 'sent' yang
// BELUM resolved. Dipakai setelah Done sebagian: kalau masih ada sisa → thread jangan 'done'.
func phoneHasUnresolvedInvoice(suite, recvTbl, logsTbl, phone string) bool {
	var c int
	_ = auditDB.QueryRow(`
SELECT COUNT(*) FROM (
  SELECT r.nomer_invoice FROM `+recvTbl+` r JOIN `+logsTbl+` b ON r.blast_log_id=b.id
  WHERE r.phone=? AND b.attempt=1 AND r.status='sent' AND COALESCE(r.nomer_invoice,'')!=''
    AND NOT EXISTS(SELECT 1 FROM resolved_invoices rv WHERE rv.suite=? AND rv.phone=r.phone AND rv.nomer_invoice=r.nomer_invoice)
  GROUP BY r.nomer_invoice
)`, phone, suite).Scan(&c)
	return c > 0
}

// backfillResolvedInvoices — isi resolved_invoices dari data historis (idempoten, INSERT OR
// IGNORE). Menangani thread yang di-Done SEBELUM tabel ini ada, TERMASUK yang sudah di-blast
// ulang setelah Done (status thread-nya bukan 'done' lagi). majoo only.
func backfillResolvedInvoices() {
	// A) Dari closing message. Sebuah invoice dianggap resolved kalau ada closing message ke
	//    nomornya SETELAH attempt-1 invoice itu terkirim. Diproses urut waktu ASC + INSERT OR
	//    IGNORE → closing PALING AWAL yang menutup sebuah invoice yang tercatat sbg resolver.
	rows, err := auditDB.Query(`SELECT phone, timestamp, COALESCE(sender_email,''), COALESCE(sender_name,'')
FROM chat_messages
WHERE direction='out' AND body LIKE 'Baik, Terima kasih atas konfirmasinya%'
ORDER BY timestamp ASC`)
	if err == nil {
		type closing struct{ phone, ts, email, name string }
		var list []closing
		for rows.Next() {
			var c closing
			if rows.Scan(&c.phone, &c.ts, &c.email, &c.name) == nil {
				list = append(list, c)
			}
		}
		rows.Close()
		for _, c := range list {
			if _, e := auditDB.Exec(`
INSERT OR IGNORE INTO resolved_invoices (suite, phone, nomer_invoice, nama_outlet, resolver_email, resolver_name, resolved_at)
SELECT 'majoo', r.phone, r.nomer_invoice, MAX(COALESCE(r.nama_outlet,'')), ?, ?, ?
FROM blast_recipients r JOIN blast_logs b ON r.blast_log_id=b.id
WHERE r.phone=? AND b.attempt=1 AND r.status='sent' AND COALESCE(r.nomer_invoice,'')!=''
  AND COALESCE(r.sent_at, r.created_at) <= ?
GROUP BY r.nomer_invoice`, c.email, c.name, c.ts, c.phone, c.ts); e != nil {
				fmt.Println("warn: backfill resolved (closing):", e)
			}
		}
	}

	// B) Dari thread yang saat ini status='done' (jaga-jaga bila closing template di-edit
	//    agent sehingga LIKE di (A) tak match). Resolver = assigned_name/email thread.
	if _, e := auditDB.Exec(`
INSERT OR IGNORE INTO resolved_invoices (suite, phone, nomer_invoice, nama_outlet, resolver_email, resolver_name, resolved_at)
SELECT 'majoo', r.phone, r.nomer_invoice, MAX(COALESCE(r.nama_outlet,'')),
       COALESCE(t.assigned_email,''), COALESCE(t.assigned_name,''), COALESCE(NULLIF(t.updated_at,''), datetime('now'))
FROM blast_recipients r
JOIN blast_logs b ON r.blast_log_id=b.id
JOIN chat_threads t ON t.phone=r.phone
WHERE t.status='done' AND b.attempt=1 AND r.status='sent' AND COALESCE(r.nomer_invoice,'')!=''
GROUP BY r.phone, r.nomer_invoice`); e != nil {
		fmt.Println("warn: backfill resolved (done-threads):", e)
	}
}
