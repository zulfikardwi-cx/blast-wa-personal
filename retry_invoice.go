package main

// Helper retry PER NOMOR INVOICE — dipakai BERSAMA majoo (chat_threads/blast_*) dan Zopoz
// (zopoz_threads/zopoz_blast_*). Hanya operasi BACA + 1 update thread; bagian kirim/record
// pesan tetap di masing-masing suite (client & tabel beda). Nama tabel di-pass sebagai
// argumen (konstanta internal, bukan input user — aman dari injection).
//
// Model: attempt tiap invoice direkonstruksi dari log blast (MAX attempt yang 'sent').
// Sebuah invoice eligible retry kalau:
//   - nomornya BELUM terminal: status NOT IN (done, invalid, on_going, scheduled,
//     force_close, konfirmasi_web, open) → after_blast / in_progress / rejected tetap jalan.
//     open = customer SUDAH membalas via WA (respons masuk) → keluar antrian, ditangani manual
//     oleh tim; JANGAN kirim blast "belum konfirmasi" lagi. konfirmasi_web = konfirmasi via web
//     (respons positif) → juga keluar antrian. (in_progress = agent sudah balas, sengaja TETAP
//     dikejar sampai Done/Attempt 3.)
//   - Attempt 1 invoice itu BERHASIL terkirim (max_att >= 1) dan belum mentok (max_att < 3).
//   - Belum dikirimi attempt hari ini (maks 1 attempt/hari per invoice).

import "time"

type invoiceRetry struct {
	phone        string
	namaOutlet   string
	nomerInvoice string
	nextAttempt  int
}

func collectInvoiceRetries(suite, threadsTbl, recvTbl, logsTbl string, targetAttempt int, startOfToday time.Time) []invoiceRetry {
	q := `
SELECT r.phone, COALESCE(r.nomer_invoice,''), COALESCE(MAX(r.nama_outlet),''),
       MAX(CASE WHEN r.status='sent' THEN COALESCE(r.attempt,b.attempt) ELSE 0 END) AS max_att,
       COALESCE(MAX(CASE WHEN r.status='sent' THEN COALESCE(r.sent_at, r.created_at) END), '') AS last_sent
FROM ` + recvTbl + ` r
JOIN ` + logsTbl + ` b ON r.blast_log_id = b.id
JOIN ` + threadsTbl + ` t ON t.phone = r.phone
WHERE t.status NOT IN ('done','invalid','on_going','scheduled','force_close','konfirmasi_web','open')
  AND COALESCE(r.nomer_invoice,'') != ''
  -- Hanya hitung baris di CYCLE (putaran) TERKINI invoice. Data lama semua cycle=1 → no-op;
  -- setelah reset re-blast, attempt dihitung ulang dari cycle baru (Attempt 1-2-3 lagi).
  AND r.cycle = (SELECT MAX(cycle) FROM ` + recvTbl + ` r2 WHERE r2.phone=r.phone AND COALESCE(r2.nomer_invoice,'')=COALESCE(r.nomer_invoice,''))
  -- Revisi nomor: hanya nomor TERKINI (Attempt 1 paling baru) yg dikejar; nomor lama supersede.
  AND r.phone = (SELECT r3.phone FROM ` + recvTbl + ` r3 JOIN ` + logsTbl + ` b3 ON r3.blast_log_id=b3.id
                 WHERE COALESCE(r3.nomer_invoice,'')=COALESCE(r.nomer_invoice,'') AND COALESCE(r3.attempt,b3.attempt)=1
                 ORDER BY COALESCE(r3.sent_at, r3.created_at) DESC, r3.id DESC LIMIT 1)
  AND NOT EXISTS (SELECT 1 FROM excluded_invoices x WHERE x.suite=? AND x.phone=r.phone AND x.nomer_invoice=r.nomer_invoice)
  AND NOT EXISTS (SELECT 1 FROM resolved_invoices rv WHERE rv.suite=? AND rv.phone=r.phone AND rv.nomer_invoice=r.nomer_invoice)
GROUP BY r.phone, r.nomer_invoice
HAVING max_att >= 1 AND max_att < 3
ORDER BY max_att DESC, last_sent ASC`
	rows, err := auditDB.Query(q, suite, suite)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []invoiceRetry
	for rows.Next() {
		var phone, inv, outlet, lastSent string
		var maxAtt int
		if err := rows.Scan(&phone, &inv, &outlet, &maxAtt, &lastSent); err != nil {
			continue
		}
		next := maxAtt + 1
		if (targetAttempt == 2 || targetAttempt == 3) && next != targetAttempt {
			continue
		}
		if attemptedToday(lastSent, startOfToday) {
			continue
		}
		out = append(out, invoiceRetry{phone, outlet, inv, next})
	}
	return out
}

// invoiceStillNeedsRetry — re-check satu invoice tepat sebelum kirim (race guard). Return
// (nextAttempt, true) kalau masih perlu dikirim; (0,false) kalau tidak.
func invoiceStillNeedsRetry(suite, threadsTbl, recvTbl, logsTbl, phone, invoice string, startOfToday time.Time) (int, bool) {
	if isInvoiceExcluded(suite, phone, invoice) {
		return 0, false
	}
	// Sudah pernah Done/Resolved → jangan retry lagi walau nomornya di-blast ulang.
	if isInvoiceResolved(suite, phone, invoice) {
		return 0, false
	}
	var status string
	if err := auditDB.QueryRow(`SELECT status FROM `+threadsTbl+` WHERE phone=?`, phone).Scan(&status); err != nil {
		return 0, false
	}
	switch status {
	case "done", "invalid", "on_going", "scheduled", "force_close", "konfirmasi_web", "open":
		return 0, false
	}
	// Revisi nomor: kalau invoice ini sudah di-blast ke nomor LEBIH BARU, nomor ini di-supersede
	// (bukan nomor terkini) → jangan dikejar lagi.
	var curPhone string
	_ = auditDB.QueryRow(`SELECT r3.phone FROM `+recvTbl+` r3 JOIN `+logsTbl+` b3 ON r3.blast_log_id=b3.id
		WHERE COALESCE(r3.nomer_invoice,'')=? AND COALESCE(r3.attempt,b3.attempt)=1
		ORDER BY COALESCE(r3.sent_at, r3.created_at) DESC, r3.id DESC LIMIT 1`, invoice).Scan(&curPhone)
	if curPhone != "" && curPhone != phone {
		return 0, false
	}
	var maxAtt int
	var lastSent string
	err := auditDB.QueryRow(`
SELECT COALESCE(MAX(CASE WHEN r.status='sent' THEN COALESCE(r.attempt,b.attempt) ELSE 0 END),0),
       COALESCE(MAX(CASE WHEN r.status='sent' THEN COALESCE(r.sent_at, r.created_at) END),'')
FROM `+recvTbl+` r JOIN `+logsTbl+` b ON r.blast_log_id=b.id
WHERE r.phone=? AND r.nomer_invoice=?
  AND r.cycle = (SELECT MAX(cycle) FROM `+recvTbl+` r2 WHERE r2.phone=r.phone AND COALESCE(r2.nomer_invoice,'')=COALESCE(r.nomer_invoice,''))`, phone, invoice).Scan(&maxAtt, &lastSent)
	if err != nil || maxAtt < 1 || maxAtt >= 3 {
		return 0, false
	}
	if attemptedToday(lastSent, startOfToday) {
		return 0, false
	}
	return maxAtt + 1, true
}

// bumpThreadAfterRetry — update thread nomor setelah satu invoice-nya dikirimi attempt.
// current_attempt = MAX (banyak invoice bisa update thread yg sama), status TIDAK diubah.
func bumpThreadAfterRetry(threadsTbl, phone, preview string, attempt int, ts time.Time) error {
	tsStr := ts.Format(time.RFC3339)
	_, err := auditDB.Exec(`UPDATE `+threadsTbl+` SET
		last_message_at = ?,
		last_message_preview = ?,
		last_message_direction = 'out',
		current_attempt = MAX(current_attempt, ?),
		last_attempt_at = ?,
		updated_at = ?
	WHERE phone = ?`, tsStr, truncate(preview, 80), attempt, tsStr, tsStr, phone)
	return err
}

// phoneHasPendingInvoice — true kalau nomor masih punya invoice yang attempt-nya 1..2 (belum
// sampai 3). Dipakai force-close sweep: jangan tutup nomor selagi ada invoice yang masih
// perlu attempt berikutnya.
func phoneHasPendingInvoice(recvTbl, logsTbl, phone string) bool {
	var c int
	_ = auditDB.QueryRow(`
SELECT COUNT(*) FROM (
  SELECT r.nomer_invoice, MAX(CASE WHEN r.status='sent' THEN COALESCE(r.attempt,b.attempt) ELSE 0 END) m
  FROM `+recvTbl+` r JOIN `+logsTbl+` b ON r.blast_log_id=b.id
  WHERE r.phone=? AND COALESCE(r.nomer_invoice,'')!=''
    AND r.cycle = (SELECT MAX(cycle) FROM `+recvTbl+` r2 WHERE r2.phone=r.phone AND COALESCE(r2.nomer_invoice,'')=COALESCE(r.nomer_invoice,''))
  GROUP BY r.nomer_invoice HAVING m BETWEEN 1 AND 2
)`, phone).Scan(&c)
	return c > 0
}
