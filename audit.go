package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
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
	return nil
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
	_, err := auditDB.Exec(
		`INSERT INTO blast_recipients (blast_log_id, phone, nama_outlet, nomer_invoice, status, error, message, sent_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		blastLogID, rec.Phone, rec.NamaOutlet, rec.NomerInv, rec.Status, rec.Error, rec.Message, rec.SentAt,
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

func recordRetryBatchStart(email, name, template string, total int, startedAt time.Time) (int64, error) {
	res, err := auditDB.Exec(
		`INSERT INTO blast_logs (user_email, user_name, started_at, template, total) VALUES (?, ?, ?, ?, ?)`,
		email, name, startedAt.Format(time.RFC3339), template, total,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func recordRetryRecipient(logID int64, phone, outlet, invoice, status, errMsg, message string, sentAt time.Time) error {
	if logID == 0 {
		return nil
	}
	_, err := auditDB.Exec(
		`INSERT INTO blast_recipients (blast_log_id, phone, nama_outlet, nomer_invoice, status, error, message, sent_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		logID, phone, outlet, invoice, status, nullableStr(errMsg), nullableStr(message), sentAt.Format(time.RFC3339),
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
}

func handleHistory(w http.ResponseWriter, r *http.Request) {
	// Urut by started_at (waktu kejadian), bukan id — supaya entri retry hasil backfill
	// (di-insert belakangan tapi tanggalnya lama) tetap tampil kronologis. id DESC tiebreak.
	rows, err := auditDB.Query(`SELECT id, user_email, COALESCE(user_name,''), started_at, COALESCE(ended_at,''), template, total, sent, failed, skipped, COALESCE(min_delay,0), COALESCE(max_delay,0) FROM blast_logs ORDER BY started_at DESC, id DESC LIMIT 100`)
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	defer rows.Close()
	var out []BlastLogRow
	for rows.Next() {
		var b BlastLogRow
		if err := rows.Scan(&b.ID, &b.UserEmail, &b.UserName, &b.StartedAt, &b.EndedAt, &b.Template, &b.Total, &b.Sent, &b.Failed, &b.Skipped, &b.MinDelay, &b.MaxDelay); err != nil {
			httpErr(w, 500, "scan: %v", err)
			return
		}
		out = append(out, b)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"logs": out})
}

// suppress unused
var _ = fmt.Sprintf
