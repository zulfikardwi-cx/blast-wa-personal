package main

// Suite "watzap" — channel broadcast via gateway watzap.id (HTTP API), TERPISAH & ADITIF
// dari majoo (whatsmeow) & Zopoz. TIDAK ada koneksi whatsmeow di sini: kirim = HTTP POST ke
// watzap /send_message. Fase 1: config + kirim + broadcast + riwayat. Inbox (webhook) &
// report menyusul di fase berikutnya. Tabel: watzap_blast_logs / watzap_blast_recipients.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ---- config ----
var (
	watzapAPIKey    string
	watzapNumberKey string
	watzapBaseURL   string
)

func initWatzap() {
	watzapAPIKey = os.Getenv("WATZAP_API_KEY")
	watzapNumberKey = os.Getenv("WATZAP_NUMBER_KEY")
	watzapBaseURL = strings.TrimRight(os.Getenv("WATZAP_BASE_URL"), "/")
	if watzapBaseURL == "" {
		watzapBaseURL = "https://api.watzap.id/v1"
	}
	if watzapConfigured() {
		fmt.Println("Watzap enabled — base:", watzapBaseURL, "/ number_key:", maskKey(watzapNumberKey))
	} else {
		fmt.Println("Watzap disabled (set WATZAP_API_KEY + WATZAP_NUMBER_KEY di .env untuk aktifkan)")
	}
}

func watzapConfigured() bool { return watzapAPIKey != "" && watzapNumberKey != "" }

func maskKey(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return s[:2] + "****" + s[len(s)-2:]
}

// ---- kirim satu pesan via watzap ----
// Return (rawResponse, error). error != nil kalau HTTP gagal atau watzap menandai gagal.
func watzapSend(phone, message string) (string, error) {
	if !watzapConfigured() {
		return "", fmt.Errorf("watzap belum dikonfigurasi (WATZAP_API_KEY / WATZAP_NUMBER_KEY)")
	}
	payload, _ := json.Marshal(map[string]string{
		"api_key":    watzapAPIKey,
		"number_key": watzapNumberKey,
		"phone_no":   phone,
		"message":    message,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, watzapBaseURL+"/send_message", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("koneksi watzap: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	rawStr := strings.TrimSpace(string(raw))

	// Deteksi sukses/gagal secara toleran (dok resmi minim contoh response). Watzap sukses
	// biasanya status "success"; gagal → HTTP >=400 atau status berisi error/invalid.
	// Log payload mentah saat gagal supaya bisa dikalibrasi setelah tes pertama.
	lower := strings.ToLower(rawStr)
	bad := resp.StatusCode >= 400 ||
		strings.Contains(lower, "\"error\"") ||
		strings.Contains(lower, "failed") ||
		strings.Contains(lower, "gagal") ||
		strings.Contains(lower, "invalid") ||
		strings.Contains(lower, "not registered") ||
		strings.Contains(lower, "tidak terdaftar") ||
		strings.Contains(lower, "not found")
	if bad {
		msg := watzapExtractMessage(raw)
		if msg == "" {
			msg = rawStr
		}
		return rawStr, fmt.Errorf("%s", truncate(msg, 200))
	}
	return rawStr, nil
}

func watzapExtractMessage(raw []byte) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"message", "reason", "error", "status"} {
		if v, ok := m[k]; ok {
			if s := strings.TrimSpace(fmt.Sprintf("%v", v)); s != "" && s != "<nil>" {
				return s
			}
		}
	}
	return ""
}

// ---- audit (watzap_blast_logs / watzap_blast_recipients) ----
func initWatzapBlastAudit() error {
	_, err := auditDB.Exec(`
CREATE TABLE IF NOT EXISTS watzap_blast_logs (
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
	attempt INTEGER NOT NULL DEFAULT 1,
	min_delay INTEGER,
	max_delay INTEGER
);
CREATE INDEX IF NOT EXISTS idx_watzap_blast_logs_started ON watzap_blast_logs(started_at DESC);

CREATE TABLE IF NOT EXISTS watzap_blast_recipients (
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
CREATE INDEX IF NOT EXISTS idx_watzap_recipients_blast ON watzap_blast_recipients(blast_log_id);
CREATE INDEX IF NOT EXISTS idx_watzap_recipients_phone ON watzap_blast_recipients(phone);
`)
	return err
}

func recordWatzapBlastStart(j *BlastJob) (int64, error) {
	res, err := auditDB.Exec(
		`INSERT INTO watzap_blast_logs (user_email, user_name, started_at, template, total, min_delay, max_delay) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		j.UserEmail, j.UserName, j.StartedAt.Format(time.RFC3339), j.Template, j.Total, j.MinDelay, j.MaxDelay,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func recordWatzapRecipient(blastLogID int64, rec *RecipientStatus) error {
	if blastLogID == 0 {
		return nil
	}
	_, err := auditDB.Exec(
		`INSERT INTO watzap_blast_recipients (blast_log_id, phone, nama_outlet, nomer_invoice, status, error, message, sent_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		blastLogID, rec.Phone, rec.NamaOutlet, rec.NomerInv, rec.Status, rec.Error, rec.Message, rec.SentAt,
	)
	return err
}

func recordWatzapBlastEnd(id int64, j *BlastJob) error {
	if id == 0 {
		return nil
	}
	end := ""
	if j.EndedAt != nil {
		end = j.EndedAt.Format(time.RFC3339)
	}
	_, err := auditDB.Exec(
		`UPDATE watzap_blast_logs SET ended_at = ?, sent = ?, failed = ?, skipped = ? WHERE id = ?`,
		end, j.Sent, j.Failed, j.Skipped, id,
	)
	return err
}

// ---- job state (terpisah dari majoo state.job & zopoz) ----
var watzapState struct {
	mu  sync.Mutex
	job *BlastJob
}

// ---- handlers ----

// watzapHandleStatus — status konfigurasi (untuk enable tombol di UI). Tidak ada koneksi
// WA seperti majoo/zopoz; "siap" = kredensial terisi.
func watzapHandleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"enabled":    watzapConfigured(),
		"base_url":   watzapBaseURL,
		"number_key": maskKey(watzapNumberKey),
	})
}

func watzapHandleTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"attempt_1": GetAttemptTemplate(1)})
}

func watzapHandleBlast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if !watzapConfigured() {
		httpErr(w, 400, "Watzap belum dikonfigurasi. Set WATZAP_API_KEY + WATZAP_NUMBER_KEY di .env, lalu restart backend.")
		return
	}
	watzapState.mu.Lock()
	if watzapState.job != nil && watzapState.job.Running {
		watzapState.mu.Unlock()
		httpErr(w, 409, "Ada blast watzap yang sedang berjalan.")
		return
	}
	watzapState.mu.Unlock()

	if err := r.ParseMultipartForm(10 << 20); err != nil {
		httpErr(w, 400, "form: %v", err)
		return
	}
	template := GetAttemptTemplate(1)
	minDelay := atoiOr(r.FormValue("min_delay"), 20)
	maxDelay := atoiOr(r.FormValue("max_delay"), 40)
	if minDelay < 2 {
		minDelay = 2
	}
	if maxDelay < minDelay {
		maxDelay = minDelay + 4
	}

	file, _, err := r.FormFile("csv")
	if err != nil {
		httpErr(w, 400, "csv: %v", err)
		return
	}
	defer file.Close()

	rows, err := parseCSV(file)
	if err != nil {
		httpErr(w, 400, "csv parse: %v", err)
		return
	}
	if len(rows) == 0 {
		httpErr(w, 400, "csv kosong")
		return
	}

	user, _ := userFromCtx(r.Context())
	ctx, cancel := context.WithCancel(context.Background())
	job := &BlastJob{
		ID:        fmt.Sprintf("watzap-%d", time.Now().Unix()),
		UserEmail: user.Email,
		UserName:  user.Name,
		Template:  template,
		StartedAt: time.Now(),
		Running:   true,
		MinDelay:  minDelay,
		MaxDelay:  maxDelay,
		Total:     len(rows),
		Items:     rows,
		cancel:    cancel,
	}
	watzapState.mu.Lock()
	watzapState.job = job
	watzapState.mu.Unlock()

	id, err := recordWatzapBlastStart(job)
	if err != nil {
		fmt.Println("watzap audit start failed:", err)
	}
	job.auditID = id

	go runWatzapBlast(ctx, job)

	writeJSON(w, map[string]any{"ok": true, "job_id": job.ID, "total": len(rows)})
}

func watzapHandleProgress(w http.ResponseWriter, r *http.Request) {
	watzapState.mu.Lock()
	job := watzapState.job
	watzapState.mu.Unlock()
	if job == nil {
		writeJSON(w, map[string]any{"job": nil})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"job": job.snapshot()})
}

func watzapHandleHistory(w http.ResponseWriter, r *http.Request) {
	rows, err := auditDB.Query(`SELECT id, user_email, COALESCE(user_name,''), started_at, COALESCE(ended_at,''), template, total, sent, failed, skipped, COALESCE(min_delay,0), COALESCE(max_delay,0), COALESCE(attempt,1) FROM watzap_blast_logs ORDER BY started_at DESC, id DESC LIMIT 100`)
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	defer rows.Close()
	var out []BlastLogRow
	for rows.Next() {
		var b BlastLogRow
		if err := rows.Scan(&b.ID, &b.UserEmail, &b.UserName, &b.StartedAt, &b.EndedAt, &b.Template, &b.Total, &b.Sent, &b.Failed, &b.Skipped, &b.MinDelay, &b.MaxDelay, &b.Attempt); err != nil {
			continue
		}
		out = append(out, b)
	}
	writeJSON(w, map[string]any{"logs": out})
}

// maxConsecutiveWatzapFail — abort blast kalau gagal berturut-turut sebanyak ini (biasanya
// kredensial salah / nomor watzap disconnect) supaya tidak menghabiskan seluruh daftar.
const maxConsecutiveWatzapFail = 5

func runWatzapBlast(ctx context.Context, job *BlastJob) {
	defer func() {
		now := time.Now()
		job.mu.Lock()
		job.Running = false
		job.EndedAt = &now
		job.mu.Unlock()
		if err := recordWatzapBlastEnd(job.auditID, job); err != nil {
			fmt.Println("watzap audit end failed:", err)
		}
	}()

	consecutiveFail := 0
	for i, rec := range job.Items {
		select {
		case <-ctx.Done():
			job.mu.Lock()
			for _, it := range job.Items {
				if it.Status == "pending" {
					it.Status = "skipped"
					it.Error = "cancelled"
					job.Skipped++
				}
			}
			job.mu.Unlock()
			return
		default:
		}

		msg := renderTemplate(job.Template, rec)
		job.mu.Lock()
		rec.Message = msg
		job.mu.Unlock()

		raw, sendErr := watzapSend(rec.Phone, msg)
		if sendErr != nil {
			job.mu.Lock()
			rec.Status = "failed"
			rec.Error = sendErr.Error()
			job.Failed++
			job.mu.Unlock()
			fmt.Println("watzap send FAIL", rec.Phone, "-", sendErr.Error(), "| raw:", truncate(raw, 200))
			consecutiveFail++
		} else {
			job.mu.Lock()
			rec.Status = "sent"
			rec.SentAt = time.Now().Format(time.RFC3339)
			job.Sent++
			job.mu.Unlock()
			consecutiveFail = 0
		}

		if err := recordWatzapRecipient(job.auditID, rec); err != nil {
			fmt.Println("warn: recordWatzapRecipient:", err)
		}

		if consecutiveFail >= maxConsecutiveWatzapFail {
			job.mu.Lock()
			for _, it := range job.Items {
				if it.Status == "pending" {
					it.Status = "skipped"
					it.Error = fmt.Sprintf("aborted: %d gagal berturut-turut (cek kredensial/nomor watzap)", maxConsecutiveWatzapFail)
					job.Skipped++
				}
			}
			job.mu.Unlock()
			fmt.Printf("watzap blast di-abort: %d gagal berturut-turut\n", maxConsecutiveWatzapFail)
			return
		}

		if i < len(job.Items)-1 {
			d := job.MinDelay + rand.Intn(job.MaxDelay-job.MinDelay+1)
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(d) * time.Second):
			}
		}
	}
}
