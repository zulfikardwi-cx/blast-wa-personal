package main

// Zopoz blast pipeline + auto-retry scheduler + blast audit. Mirror blast.go / retry.go /
// audit.go tetapi mengirim lewat client Zopoz (zopozState.client) dan menulis ke tabel
// zopoz_blast_logs / zopoz_blast_recipients / zopoz_threads / zopoz_messages.
//
// Knob retry (window jam, force-close, jitter, enabled, lokasi WIB) di-reuse dari retry.go
// (di-set sekali saat startRetryScheduler() dan read-only setelahnya) supaya perilaku Zopoz
// konsisten dgn inbox utama. Lock retry punya sendiri (zopozRetryMu) agar tidak saling tunggu.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// ---- audit (zopoz_blast_logs / zopoz_blast_recipients) ----

func initZopozBlastAudit() error {
	_, err := auditDB.Exec(`
CREATE TABLE IF NOT EXISTS zopoz_blast_logs (
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
	max_delay INTEGER,
	attempt INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_zopoz_blast_logs_started ON zopoz_blast_logs(started_at DESC);

CREATE TABLE IF NOT EXISTS zopoz_blast_recipients (
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
CREATE INDEX IF NOT EXISTS idx_zopoz_recipients_blast ON zopoz_blast_recipients(blast_log_id);
CREATE INDEX IF NOT EXISTS idx_zopoz_recipients_phone ON zopoz_blast_recipients(phone);
`)
	return err
}

func zopozRecordBlastStart(j *BlastJob) (int64, error) {
	res, err := auditDB.Exec(
		`INSERT INTO zopoz_blast_logs (user_email, user_name, started_at, template, total, min_delay, max_delay) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		j.UserEmail, j.UserName, j.StartedAt.Format(time.RFC3339), j.Template, j.Total, j.MinDelay, j.MaxDelay,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func zopozRecordRecipient(blastLogID int64, rec *RecipientStatus) error {
	if blastLogID == 0 {
		return nil
	}
	_, err := auditDB.Exec(
		`INSERT INTO zopoz_blast_recipients (blast_log_id, phone, nama_outlet, nomer_invoice, status, error, message, sent_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		blastLogID, rec.Phone, rec.NamaOutlet, rec.NomerInv, rec.Status, rec.Error, rec.Message, rec.SentAt,
	)
	return err
}

func zopozRecordBlastEnd(id int64, j *BlastJob) error {
	if id == 0 {
		return nil
	}
	end := ""
	if j.EndedAt != nil {
		end = j.EndedAt.Format(time.RFC3339)
	}
	_, err := auditDB.Exec(
		`UPDATE zopoz_blast_logs SET ended_at = ?, sent = ?, failed = ?, skipped = ? WHERE id = ?`,
		end, j.Sent, j.Failed, j.Skipped, id,
	)
	return err
}

func zopozRecordRetryBatchStart(email, name, template string, attempt, total int, startedAt time.Time) (int64, error) {
	res, err := auditDB.Exec(
		`INSERT INTO zopoz_blast_logs (user_email, user_name, started_at, template, attempt, total) VALUES (?, ?, ?, ?, ?, ?)`,
		email, name, startedAt.Format(time.RFC3339), template, attempt, total,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func zopozRecordRetryRecipient(logID int64, phone, outlet, invoice, status, errMsg, message string, sentAt time.Time) error {
	if logID == 0 {
		return nil
	}
	_, err := auditDB.Exec(
		`INSERT INTO zopoz_blast_recipients (blast_log_id, phone, nama_outlet, nomer_invoice, status, error, message, sent_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		logID, phone, outlet, invoice, status, nullableStr(errMsg), nullableStr(message), sentAt.Format(time.RFC3339),
	)
	return err
}

func zopozRecordRetryBatchEnd(logID int64, sent, failed int, endedAt time.Time) error {
	if logID == 0 {
		return nil
	}
	_, err := auditDB.Exec(
		`UPDATE zopoz_blast_logs SET sent = ?, failed = ?, total = ?, ended_at = ? WHERE id = ?`,
		sent, failed, sent+failed, endedAt.Format(time.RFC3339), logID,
	)
	return err
}

func zopozDeleteEmptyBlastLog(logID int64) {
	if logID == 0 {
		return
	}
	_, _ = auditDB.Exec(`DELETE FROM zopoz_blast_logs WHERE id = ?`, logID)
}

func zopozHandleHistory(w http.ResponseWriter, r *http.Request) {
	rows, err := auditDB.Query(`SELECT id, user_email, COALESCE(user_name,''), started_at, COALESCE(ended_at,''), template, total, sent, failed, skipped, COALESCE(min_delay,0), COALESCE(max_delay,0), COALESCE(attempt,1) FROM zopoz_blast_logs ORDER BY started_at DESC, id DESC LIMIT 100`)
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

// ---- blast (attempt 1) ----

func zopozHandleBlast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	_, loggedIn, connected := zopozState.snapshot()
	if !loggedIn || !connected {
		httpErr(w, 400, "WhatsApp Zopoz belum terhubung. Scan QR dulu.")
		return
	}
	if zopozState.job != nil && zopozState.job.Running {
		httpErr(w, 409, "Ada blast Zopoz yang sedang berjalan.")
		return
	}

	if err := r.ParseMultipartForm(10 << 20); err != nil {
		httpErr(w, 400, "form: %v", err)
		return
	}
	template := zopozGetAttemptTemplate(1)
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
		ID:        fmt.Sprintf("zopoz-job-%d", time.Now().Unix()),
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
	zopozState.job = job

	id, err := zopozRecordBlastStart(job)
	if err != nil {
		fmt.Println("zopoz audit start failed:", err)
	}
	job.auditID = id

	go zopozRunBlast(ctx, job)

	writeJSON(w, map[string]any{"ok": true, "job_id": job.ID, "total": len(rows)})
}

func zopozHandleProgress(w http.ResponseWriter, r *http.Request) {
	if zopozState.job == nil {
		writeJSON(w, map[string]any{"job": nil})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"job": zopozState.job.snapshot()})
}

func zopozRunBlast(ctx context.Context, job *BlastJob) {
	defer func() {
		now := time.Now()
		job.mu.Lock()
		job.Running = false
		job.EndedAt = &now
		job.mu.Unlock()
		if err := zopozRecordBlastEnd(job.auditID, job); err != nil {
			fmt.Println("zopoz audit end failed:", err)
		}
	}()

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

		if err := zopozSendOne(rec.Phone, msg); err != nil {
			job.mu.Lock()
			rec.Status = "failed"
			rec.Error = err.Error()
			job.Failed++
			job.mu.Unlock()
		} else {
			job.mu.Lock()
			rec.Status = "sent"
			rec.SentAt = time.Now().Format(time.RFC3339)
			job.Sent++
			job.mu.Unlock()
		}

		if err := zopozRecordRecipient(job.auditID, rec); err != nil {
			fmt.Println("zopoz warn: recordRecipient failed for", rec.Phone, ":", err)
		}

		if rec.Status == "sent" {
			now := time.Now()
			if err := zopozUpsertThreadBlast(rec.Phone, rec.NamaOutlet, rec.NomerInv, job.auditID, msg, now); err != nil {
				fmt.Println("zopoz warn: upsertThreadBlast:", err)
			}
			if err := zopozRecordChatMessage(rec.Phone, "out", msg, "", "", now, job.auditID, job.UserEmail, job.UserName); err != nil {
				fmt.Println("zopoz warn: recordChatMessage outgoing blast:", err)
			}
		} else if rec.Status == "failed" {
			if err := zopozUpsertThreadBlastFailed(rec.Phone, rec.NamaOutlet, rec.NomerInv, job.auditID, rec.Error, time.Now()); err != nil {
				fmt.Println("zopoz warn: upsertThreadBlastFailed:", err)
			}
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

func zopozSendOne(phone, body string) error {
	jid := types.NewJID(phone, types.DefaultUserServer)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	res, err := zopozState.client.IsOnWhatsApp(ctx, []string{"+" + phone})
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	if len(res) == 0 || !res[0].IsIn {
		return fmt.Errorf("nomor tidak terdaftar di WhatsApp")
	}

	jid = zopozResolveToLID(ctx, jid)

	if _, e := zopozState.client.GetUserDevicesContext(ctx, []types.JID{jid}); e != nil {
		fmt.Println("zopoz warn: prefetch devices failed for", phone, ":", e)
	}

	msg := &waProto.Message{Conversation: proto.String(body)}
	_, err = zopozState.client.SendMessage(ctx, jid, msg)
	return err
}

func zopozResolveToLID(ctx context.Context, jid types.JID) types.JID {
	if lid, e := zopozState.client.Store.LIDs.GetLIDForPN(ctx, jid); e == nil && !lid.IsEmpty() {
		return lid
	}
	if info, e := zopozState.client.GetUserInfo(ctx, []types.JID{jid}); e == nil && !info[jid].LID.IsEmpty() {
		return info[jid].LID
	}
	return jid
}

// ---- auto-retry scheduler (attempt 2/3) + force-close sweep ----

var (
	zopozRetryMu      sync.Mutex
	zopozRetryRunning bool
)

func startZopozRetryScheduler() {
	go func() {
		time.Sleep(2 * time.Minute) // beri waktu WA connect dulu
		ticker := time.NewTicker(time.Duration(retryIntervalMin) * time.Minute)
		defer ticker.Stop()
		for {
			if retryEnabled {
				zopozProcessRetries(false, 0, 2, "system@retry", "Zopoz Auto Retry (cron)")
				zopozProcessRetries(false, 0, 3, "system@retry", "Zopoz Auto Retry (cron)")
			}
			zopozProcessForceCloseSweep()
			<-ticker.C
		}
	}()
	log.Printf("zopoz retry scheduler: interval=%dm window=%02d:00 WIB force-close=%02d:00 WIB (mengikuti config retry utama)",
		retryIntervalMin, retryWindowHour, forceCloseHour)
}

func zopozProcessRetries(force bool, limit, targetAttempt int, actorEmail, actorName string) {
	zopozRetryMu.Lock()
	if zopozRetryRunning {
		zopozRetryMu.Unlock()
		log.Println("zopoz retry: previous batch still running, skip")
		return
	}
	zopozRetryRunning = true
	zopozRetryMu.Unlock()
	defer func() {
		zopozRetryMu.Lock()
		zopozRetryRunning = false
		zopozRetryMu.Unlock()
	}()

	now := time.Now().In(wibLoc)
	if !force && now.Hour() != retryWindowHour {
		return
	}

	_, loggedIn, connected := zopozState.snapshot()
	if !loggedIn || !connected {
		return
	}

	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, wibLoc)

	// Kandidat retry PER NOMOR INVOICE (sama seperti majoo). Lihat collectInvoiceRetries.
	batch := collectInvoiceRetries("zopoz_threads", "zopoz_blast_recipients", "zopoz_blast_logs", targetAttempt, startOfToday)

	if limit > 0 && len(batch) > limit {
		batch = batch[:limit]
	}
	if len(batch) == 0 {
		return
	}

	log.Printf("zopoz retry: antrikan %d invoice (per-invoice, target=%d force=%v)", len(batch), targetAttempt, force)

	batchStart := time.Now()
	auditEmail := actorEmail
	if auditEmail == "" {
		auditEmail = "system@retry"
	}
	auditTemplate := zopozGetAttemptTemplate(targetAttempt)
	auditAttempt := targetAttempt
	if targetAttempt != 2 && targetAttempt != 3 {
		auditTemplate = "Retry Attempt 2 & 3"
		auditAttempt = 0
	}
	retryLogID, err := zopozRecordRetryBatchStart(auditEmail, actorName, auditTemplate, auditAttempt, len(batch), batchStart)
	if err != nil {
		log.Printf("zopoz retry: recordRetryBatchStart error: %v", err)
	}

	sent, failed := 0, 0
	for i, rr := range batch {
		next, ok := invoiceStillNeedsRetry("zopoz_threads", "zopoz_blast_recipients", "zopoz_blast_logs", rr.phone, rr.nomerInvoice, startOfToday)
		if !ok {
			continue
		}
		template := zopozGetAttemptTemplate(next)
		body := renderTemplateWithVars(template, rr.namaOutlet, rr.nomerInvoice)

		if err := zopozSendRetryOne(rr.phone, body); err != nil {
			log.Printf("zopoz retry: phone=%s inv=%s attempt=%d FAILED: %v", rr.phone, rr.nomerInvoice, next, err)
			failed++
			_ = zopozRecordRetryRecipient(retryLogID, rr.phone, rr.namaOutlet, rr.nomerInvoice, "failed", err.Error(), body, time.Now())
			continue
		}

		now := time.Now()
		if err := bumpThreadAfterRetry("zopoz_threads", rr.phone, body, next, now); err != nil {
			log.Printf("zopoz retry: bumpThreadAfterRetry error: %v", err)
		}
		if err := zopozRecordChatMessage(rr.phone, "out", body, "", "", now, 0, "system@retry", fmt.Sprintf("Auto Attempt %d", next)); err != nil {
			log.Printf("zopoz retry: recordChatMessage error: %v", err)
		}
		_ = zopozRecordRetryRecipient(retryLogID, rr.phone, rr.namaOutlet, rr.nomerInvoice, "sent", "", body, now)
		log.Printf("zopoz retry: phone=%s inv=%s attempt %d sent", rr.phone, rr.nomerInvoice, next)
		sent++

		if i < len(batch)-1 {
			d := retryMinJitter
			if retryMaxJitter > retryMinJitter {
				d = retryMinJitter + rand.Intn(retryMaxJitter-retryMinJitter+1)
			}
			time.Sleep(time.Duration(d) * time.Second)
		}
	}

	if sent+failed > 0 {
		if err := zopozRecordRetryBatchEnd(retryLogID, sent, failed, time.Now()); err != nil {
			log.Printf("zopoz retry: recordRetryBatchEnd error: %v", err)
		}
	} else {
		zopozDeleteEmptyBlastLog(retryLogID)
	}
	log.Printf("zopoz retry: batch done — sent=%d failed=%d", sent, failed)
}

func zopozProcessForceCloseSweep() {
	now := time.Now().In(wibLoc)
	rows, err := auditDB.Query(`
SELECT phone, COALESCE(last_attempt_at, '')
FROM zopoz_threads
WHERE status IN ('after_blast', 'in_progress') AND current_attempt >= 3`)
	if err != nil {
		log.Printf("zopoz force-close-sweep: query error: %v", err)
		return
	}
	type rrow struct{ phone, lastAt string }
	var batch []rrow
	for rows.Next() {
		var rr rrow
		if err := rows.Scan(&rr.phone, &rr.lastAt); err != nil {
			continue
		}
		batch = append(batch, rr)
	}
	rows.Close()

	closed := 0
	for _, rr := range batch {
		if rr.lastAt == "" {
			continue
		}
		// Per-invoice: jangan tutup nomor selagi ada invoice yang belum tuntas attempt-nya.
		if phoneHasPendingInvoice("zopoz_blast_recipients", "zopoz_blast_logs", rr.phone) {
			continue
		}
		t3, err := time.Parse(time.RFC3339, rr.lastAt)
		if err != nil {
			continue
		}
		t3 = t3.In(wibLoc)
		cutoff := time.Date(t3.Year(), t3.Month(), t3.Day(), forceCloseHour, 0, 0, 0, wibLoc)
		if now.Before(cutoff) {
			continue
		}
		reason := fmt.Sprintf("Tidak ada respons s/d %02d:00 WIB (Attempt 3)", forceCloseHour)
		res, err := auditDB.Exec(`UPDATE zopoz_threads SET status='force_close', reject_reason=?, updated_at=datetime('now') WHERE phone=? AND status IN ('after_blast','in_progress') AND current_attempt >= 3`,
			reason, rr.phone)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			closed++
		}
	}
	if closed > 0 {
		log.Printf("zopoz force-close-sweep: %d nomor → force_close", closed)
	}
}

func zopozStillNeedsRetry(phone string, startOfToday time.Time) bool {
	var status, lastAt string
	var ca int
	err := auditDB.QueryRow(`SELECT status, current_attempt, COALESCE(last_attempt_at, '') FROM zopoz_threads WHERE phone = ?`, phone).Scan(&status, &ca, &lastAt)
	if err != nil {
		return false
	}
	if status != "after_blast" && status != "in_progress" {
		return false
	}
	if ca >= 3 {
		return false
	}
	return !attemptedToday(lastAt, startOfToday)
}

func zopozSendRetryOne(phone, body string) error {
	jid := types.NewJID(phone, types.DefaultUserServer)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	res, err := zopozState.client.IsOnWhatsApp(ctx, []string{"+" + phone})
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	if len(res) == 0 || !res[0].IsIn {
		return fmt.Errorf("nomor tidak terdaftar di WhatsApp")
	}
	jid = zopozResolveToLID(ctx, jid)
	_, _ = zopozState.client.GetUserDevicesContext(ctx, []types.JID{jid})

	msg := &waProto.Message{Conversation: proto.String(body)}
	_, err = zopozState.client.SendMessage(ctx, jid, msg)
	return err
}

// ---- manual retry endpoints (Blast manual Attempt 2/3 dari tab Log Status) ----

// zopozHandleRetryPreview — daftar nomor yang BENAR-BENAR akan diblast kalau Run ditekan
// (after_blast/in_progress, attempt<3, belum dikirim hari ini). Tidak mengirim apa pun.
func zopozHandleRetryPreview(w http.ResponseWriter, r *http.Request) {
	now := time.Now().In(wibLoc)
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, wibLoc)
	cands := collectInvoiceRetries("zopoz_threads", "zopoz_blast_recipients", "zopoz_blast_logs", 0, startOfToday)
	type previewRow struct {
		Phone        string `json:"phone"`
		NamaOutlet   string `json:"nama_outlet"`
		NomerInvoice string `json:"nomer_invoice"`
		NextAttempt  int    `json:"next_attempt"`
	}
	out := []previewRow{}
	count2, count3 := 0, 0
	for _, c := range cands {
		switch c.nextAttempt {
		case 2:
			count2++
		case 3:
			count3++
		}
		out = append(out, previewRow{c.phone, c.namaOutlet, c.nomerInvoice, c.nextAttempt})
	}
	writeJSON(w, map[string]any{"rows": out, "count": len(out), "count2": count2, "count3": count3})
}

// zopozHandleRetryRunNow — trigger MANUAL: jalankan batch retry Zopoz sekarang (lewati
// guard window-jam). ?attempt=2|3 wajib pilih salah satu attempt. ?limit=N opsional.
func zopozHandleRetryRunNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	_, loggedIn, connected := zopozState.snapshot()
	if !loggedIn || !connected {
		httpErr(w, 400, "WhatsApp Zopoz belum terhubung — tidak ada yang dikirim.")
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	attempt := 0
	if v := r.URL.Query().Get("attempt"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && (n == 2 || n == 3) {
			attempt = n
		} else {
			httpErr(w, 400, "attempt harus 2 atau 3")
			return
		}
	}
	user, _ := userFromCtx(r.Context())
	go zopozProcessRetries(true, limit, attempt, user.Email, user.Name)
	log.Printf("zopoz retry: FORCE manual via API oleh %s (attempt=%d limit=%d)", user.Email, attempt, limit)
	writeJSON(w, map[string]any{"ok": true, "started": true, "attempt": attempt, "limit": limit})
}
