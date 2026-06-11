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
	rows, err := auditDB.Query(`SELECT id, user_email, COALESCE(user_name,''), started_at, COALESCE(ended_at,''), template, total, sent, failed, skipped, COALESCE(min_delay,0), COALESCE(max_delay,0) FROM blast_logs ORDER BY id DESC LIMIT 100`)
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
