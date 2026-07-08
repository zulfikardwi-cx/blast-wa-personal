package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var auditDB *sql.DB

func initAudit() error {
	db, err := sql.Open("sqlite3", "file:session/audit.db?_foreign_keys=on")
	if err != nil {
		return err
	}
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS blast_logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_email TEXT NOT NULL,
	user_name TEXT,
	started_at TEXT NOT NULL,
	ended_at TEXT,
	template TEXT NOT NULL,
	total INTEGER NOT NULL DEFAULT 0,
	sent INTEGER NOT NULL DEFAULT 0,
	failed INTEGER NOT NULL DEFAULT 0,
	skipped INTEGER NOT NULL DEFAULT 0,
	min_delay INTEGER,
	max_delay INTEGER
);
CREATE INDEX IF NOT EXISTS idx_blast_logs_started ON blast_logs(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_blast_logs_user ON blast_logs(user_email);

CREATE TABLE IF NOT EXISTS blast_recipients (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	blast_log_id INTEGER NOT NULL,
	phone TEXT NOT NULL,
	nama_outlet TEXT,
	nomer_invoice TEXT,
	status TEXT NOT NULL,
	error TEXT,
	message TEXT,
	sent_at TEXT,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_recipients_blast ON blast_recipients(blast_log_id);
CREATE INDEX IF NOT EXISTS idx_recipients_phone ON blast_recipients(phone);
`)
	if err != nil {
		return err
	}
	auditDB = db

	// Migrasi: kolom attempt di blast_logs (1 = blast awal, 2/3 = retry). Row lama
	// otomatis dapat 1 (mayoritas memang attempt 1; entri retry backfill dibetulkan
	// jadi 2/3 oleh fixupBackfillRetryLogs di initChat).
	if _, e := db.Exec(`ALTER TABLE blast_logs ADD COLUMN attempt INTEGER NOT NULL DEFAULT 1`); e != nil && !strings.Contains(e.Error(), "duplicate column") {
		return e
	}
	// Migrasi: kolom attempt PER-RECIPIENT (override attempt dari blast_logs). Diisi oleh
	// backfillRecipientAttempts berdasarkan URUTAN KRONOLOGIS kirim per (phone,invoice) —
	// menangani data lama yang mislabel (mis. satu invoice ter-blast 3x tapi semuanya
	// tercatat Attempt 1). Query report/antrian pakai COALESCE(r.attempt, b.attempt).
	if _, e := db.Exec(`ALTER TABLE blast_recipients ADD COLUMN attempt INTEGER`); e != nil && !strings.Contains(e.Error(), "duplicate column") {
		return e
	}
	// Migrasi: kolom cycle (putaran blast). 1 = putaran normal (Attempt 1-2-3 pertama).
	// Re-blast "mulai ulang" untuk invoice yang sudah selesai 3x menaikkan cycle → attempt
	// dihitung ULANG per cycle (1-2-3 lagi). Semua baris lama default cycle=1, jadi semua
	// query yang memfilter "cycle terkini" identik dgn sebelumnya SAMPAI ada reset (no-op).
	if _, e := db.Exec(`ALTER TABLE blast_recipients ADD COLUMN cycle INTEGER NOT NULL DEFAULT 1`); e != nil && !strings.Contains(e.Error(), "duplicate column") {
		return e
	}
	return nil
}

// currentInvoiceCycle — cycle (putaran) terkini untuk (phone,invoice) di blast_recipients
// majoo, lintas SEMUA status (sent/failed). 0 kalau invoice belum pernah tercatat sama sekali.
// Baris baru (continuation) ikut cycle ini; reset re-blast memakai cycle+1.
func currentInvoiceCycle(phone, invoice string) int {
	var c int
	_ = auditDB.QueryRow(`SELECT COALESCE(MAX(cycle),0) FROM blast_recipients
		WHERE phone=? AND COALESCE(nomer_invoice,'')=COALESCE(?,'')`, phone, invoice).Scan(&c)
	return c
}

// backfillRecipientAttempts — set blast_recipients.attempt = urutan kronologis kirim ke-N per
// (phone, invoice), di-cap 3. Ini attempt SEBENARNYA tiap kontak: kirim pertama=1, kedua=2,
// dst — apa pun label attempt di blast_logs-nya. Menangani data lama yang mislabel (mis. re-blast
// via Generate lama yang selalu tercatat Attempt 1). Idempoten & self-healing: aman dijalankan
// tiap startup; baris baru (attempt NULL) tetap benar via COALESCE(r.attempt, b.attempt) sampai
// backfill berikutnya menstempelnya. majoo (blast_recipients) — Zopoz punya tabel sendiri.
func backfillRecipientAttempts() {
	// Ranking attempt di-PARTITION per cycle: tiap putaran blast punya Attempt 1-2-3 sendiri.
	// Data lama (semua cycle=1) tak berubah; hanya setelah reset re-blast cycle>1 muncul.
	_, err := auditDB.Exec(`
WITH ranked AS (
  SELECT r.id,
         MIN(3, ROW_NUMBER() OVER (
           PARTITION BY r.phone, COALESCE(r.nomer_invoice,''), r.cycle
           ORDER BY COALESCE(NULLIF(r.sent_at,''), r.created_at) ASC, r.id ASC)) AS att
  FROM blast_recipients r
  WHERE COALESCE(r.nomer_invoice,'') != ''
)
UPDATE blast_recipients
SET attempt = (SELECT att FROM ranked WHERE ranked.id = blast_recipients.id)
WHERE id IN (SELECT id FROM ranked)
  AND attempt IS NOT (SELECT att FROM ranked WHERE ranked.id = blast_recipients.id)`)
	if err != nil {
		fmt.Println("warn: backfillRecipientAttempts:", err)
	}
}

func recordBlastStart(j *BlastJob) (int64, error) {
	res, err := auditDB.Exec(
		`INSERT INTO blast_logs (user_email, user_name, started_at, template, total, min_delay, max_delay) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		j.UserEmail, j.UserName, j.StartedAt.Format(time.RFC3339), j.Template, j.Total, j.MinDelay, j.MaxDelay,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// recordRecipient — insert satu baris recipient detail. Dipanggil setelah tiap send selesai.
func recordRecipient(blastLogID int64, rec *RecipientStatus) error {
	if blastLogID == 0 {
		return nil
	}
	// Ikut cycle terkini invoice (min 1) — blast live biasanya cycle 1 (invoice baru).
	c := currentInvoiceCycle(rec.Phone, rec.NomerInv)
	if c < 1 {
		c = 1
	}
	_, err := auditDB.Exec(
		`INSERT INTO blast_recipients (blast_log_id, phone, nama_outlet, nomer_invoice, status, error, message, sent_at, cycle) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		blastLogID, rec.Phone, rec.NamaOutlet, rec.NomerInv, rec.Status, rec.Error, rec.Message, rec.SentAt, c,
	)
	return err
}

func recordBlastEnd(id int64, j *BlastJob) error {
	if id == 0 {
		return nil
	}
	end := ""
	if j.EndedAt != nil {
		end = j.EndedAt.Format(time.RFC3339)
	}
	_, err := auditDB.Exec(
		`UPDATE blast_logs SET ended_at = ?, sent = ?, failed = ?, skipped = ? WHERE id = ?`,
		end, j.Sent, j.Failed, j.Skipped, id,
	)
	return err
}

// ---- Retry batch audit (attempt 2/3) ----
// Catat batch retry sebagai entri Riwayat Blast supaya attempt 2/3 yang benar-benar
// terkirim ikut tercatat (report & monitoring), sejajar dengan blast attempt 1.

func recordRetryBatchStart(email, name, template string, attempt, total int, startedAt time.Time) (int64, error) {
	res, err := auditDB.Exec(
		`INSERT INTO blast_logs (user_email, user_name, started_at, template, attempt, total) VALUES (?, ?, ?, ?, ?, ?)`,
		email, name, startedAt.Format(time.RFC3339), template, attempt, total,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func recordRetryRecipient(logID int64, phone, outlet, invoice, status, errMsg, message string, sentAt time.Time) error {
	// Continuation: ikut cycle terkini invoice (min 1). Reset re-blast pakai
	// recordRetryRecipientInCycle dengan cycle eksplisit (cycle+1).
	c := currentInvoiceCycle(phone, invoice)
	if c < 1 {
		c = 1
	}
	return recordRetryRecipientInCycle(logID, phone, outlet, invoice, status, errMsg, message, sentAt, c)
}

// recordRetryRecipientInCycle — sama seperti recordRetryRecipient tapi cycle di-set eksplisit.
// Dipakai jalur reset re-blast (generate "mulai ulang Attempt 1") untuk membuka cycle baru.
func recordRetryRecipientInCycle(logID int64, phone, outlet, invoice, status, errMsg, message string, sentAt time.Time, cycle int) error {
	if logID == 0 {
		return nil
	}
	if cycle < 1 {
		cycle = 1
	}
	_, err := auditDB.Exec(
		`INSERT INTO blast_recipients (blast_log_id, phone, nama_outlet, nomer_invoice, status, error, message, sent_at, cycle) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		logID, phone, outlet, invoice, status, nullableStr(errMsg), nullableStr(message), sentAt.Format(time.RFC3339), cycle,
	)
	return err
}

func recordRetryBatchEnd(logID int64, sent, failed int, endedAt time.Time) error {
	if logID == 0 {
		return nil
	}
	_, err := auditDB.Exec(
		`UPDATE blast_logs SET sent = ?, failed = ?, total = ?, ended_at = ? WHERE id = ?`,
		sent, failed, sent+failed, endedAt.Format(time.RFC3339), logID,
	)
	return err
}

// deleteEmptyBlastLog — hapus entri batch yang ternyata 0 terkirim & 0 gagal (semua
// recipient ke-skip oleh race-guard) supaya Riwayat tidak terisi entri kosong.
func deleteEmptyBlastLog(logID int64) {
	if logID == 0 {
		return
	}
	_, _ = auditDB.Exec(`DELETE FROM blast_logs WHERE id = ?`, logID)
}

type BlastLogRow struct {
	ID         int64  `json:"id"`
	UserEmail  string `json:"user_email"`
	UserName   string `json:"user_name"`
	StartedAt  string `json:"started_at"`
	EndedAt    string `json:"ended_at"`
	Template   string `json:"template"`
	Total      int    `json:"total"`
	Sent       int    `json:"sent"`
	Failed     int    `json:"failed"`
	Skipped    int    `json:"skipped"`
	MinDelay   int    `json:"min_delay"`
	MaxDelay   int    `json:"max_delay"`
	Attempt    int    `json:"attempt"`
}

func handleHistory(w http.ResponseWriter, r *http.Request) {
	// Urut by started_at (waktu kejadian), bukan id — supaya entri retry hasil backfill
	// (di-insert belakangan tapi tanggalnya lama) tetap tampil kronologis. id DESC tiebreak.
	rows, err := auditDB.Query(`SELECT id, user_email, COALESCE(user_name,''), started_at, COALESCE(ended_at,''), template, total, sent, failed, skipped, COALESCE(min_delay,0), COALESCE(max_delay,0), COALESCE(attempt,1) FROM blast_logs ORDER BY started_at DESC, id DESC LIMIT 100`)
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	defer rows.Close()
	var out []BlastLogRow
	for rows.Next() {
		var b BlastLogRow
		if err := rows.Scan(&b.ID, &b.UserEmail, &b.UserName, &b.StartedAt, &b.EndedAt, &b.Template, &b.Total, &b.Sent, &b.Failed, &b.Skipped, &b.MinDelay, &b.MaxDelay, &b.Attempt); err != nil {
			httpErr(w, 500, "scan: %v", err)
			return
		}
		out = append(out, b)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"logs": out})
}

// closeStaleRunningBlasts — saat startup, tutup entri blast yang ended_at-nya masih NULL.
// Job blast murni in-memory (tidak ada resume), jadi setelah proses restart tak mungkin ada
// blast yang benar-benar berjalan. Tanpa ini, blast yang ke-kill saat restart (mis. WA
// logout di tengah blast) akan tampil "running" selamanya di Riwayat DAN menipu safety-check
// "ada blast berjalan?" (ended_at IS NULL). sent/failed di-rekap dari recipients. Idempoten.
func closeStaleRunningBlasts() {
	for _, t := range []struct{ logs, recv string }{
		{"blast_logs", "blast_recipients"},
		{"zopoz_blast_logs", "zopoz_blast_recipients"},
	} {
		res, err := auditDB.Exec(`UPDATE ` + t.logs + ` SET
			sent = (SELECT COUNT(*) FROM ` + t.recv + ` r WHERE r.blast_log_id = ` + t.logs + `.id AND r.status='sent'),
			failed = (SELECT COUNT(*) FROM ` + t.recv + ` r WHERE r.blast_log_id = ` + t.logs + `.id AND r.status LIKE 'failed%'),
			ended_at = COALESCE(
				NULLIF((SELECT MAX(COALESCE(NULLIF(r.sent_at,''), r.created_at)) FROM ` + t.recv + ` r WHERE r.blast_log_id = ` + t.logs + `.id), ''),
				NULLIF(started_at, ''),
				datetime('now'))
			WHERE ended_at IS NULL OR ended_at = ''`)
		if err != nil {
			// zopoz_blast_logs mungkin belum ada di DB lama — abaikan.
			if !strings.Contains(err.Error(), "no such table") {
				fmt.Println("warn: closeStaleRunningBlasts", t.logs, ":", err)
			}
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			fmt.Printf("startup: %d entri %s 'running' basi ditutup\n", n, t.logs)
		}
	}
}

// suppress unused
var _ = fmt.Sprintf
