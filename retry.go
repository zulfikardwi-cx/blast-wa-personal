package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

var (
	retryMu          sync.Mutex
	retryRunning     bool
	retryIntervalMin int
	retryWindowHour  int
	retryRejectHour  int
	retryMinJitter   int
	retryMaxJitter   int
	retryEnabled     bool
	wibLoc           *time.Location
)

// startRetryScheduler — jalankan goroutine background yang cek thread retry tiap
// RETRY_CHECK_INTERVAL_MINUTES menit. Blast attempt 2/3 hanya dijalankan saat masuk
// window jam RETRY_WINDOW_HOUR (WIB), maksimal 1x/hari kalender per thread.
func startRetryScheduler() {
	retryIntervalMin = atoiEnv("RETRY_CHECK_INTERVAL_MINUTES", 30)
	retryWindowHour = atoiEnv("RETRY_WINDOW_HOUR", 9)
	retryRejectHour = atoiEnv("RETRY_REJECT_HOUR", 16)
	retryMinJitter = atoiEnv("RETRY_SEND_MIN_DELAY", 20)
	retryMaxJitter = atoiEnv("RETRY_SEND_MAX_DELAY", 40)

	var err error
	wibLoc, err = time.LoadLocation("Asia/Jakarta")
	if err != nil {
		wibLoc = time.FixedZone("WIB", 7*3600)
		log.Printf("retry scheduler: LoadLocation Asia/Jakarta gagal (%v), fallback fixed UTC+7", err)
	}

	// Saklar pause auto-cron SEND. RETRY_ENABLED=false → burst Attempt 2/3 jam-9 MATI
	// (mencegah ban berulang). Endpoint force manual /api/retry/run-now TETAP jalan.
	// CATATAN: reject-sweep (16:00) hanya update DB (tidak kirim pesan) → SELALU jalan,
	// termasuk saat RETRY_ENABLED=false.
	retryEnabled = boolEnv("RETRY_ENABLED", true)
	if !retryEnabled {
		log.Printf("retry scheduler: SEND DISABLED (RETRY_ENABLED=false) — auto-cron jam-%02d MATI. Reject-sweep %02d:00 & force manual tetap jalan.", retryWindowHour, retryRejectHour)
	} else {
		log.Printf("retry scheduler: interval=%dm window=%02d:00 WIB reject=%02d:00 WIB jitter=%d-%ds (max 1x/hari per thread)",
			retryIntervalMin, retryWindowHour, retryRejectHour, retryMinJitter, retryMaxJitter)
	}

	go func() {
		// Delay awal 2 menit setelah start, biar WA connect dulu
		time.Sleep(2 * time.Minute)
		ticker := time.NewTicker(time.Duration(retryIntervalMin) * time.Minute)
		defer ticker.Stop()
		for {
			if retryEnabled {
				// Pisah per-attempt (2 dulu, lalu 3) → tiap batch jadi 1 entri Riwayat
				// dengan attempt & template yang jelas. Sekuensial (single-flight lock).
				processRetries(false, 0, 2, "system@retry", "Auto Retry (cron)")
				processRetries(false, 0, 3, "system@retry", "Auto Retry (cron)")
			}
			processRejectSweep()
			<-ticker.C
		}
	}()
}

// processRetries — cek semua thread yang perlu retry, kirim attempt berikutnya.
// force=true → lewati window-jam guard (dipakai endpoint manual /api/retry/run-now).
// limit>0 → batasi jumlah thread yang diproses (mis. tes batch kecil dulu).
// targetAttempt 2/3 → HANYA kirim attempt itu (current_attempt = targetAttempt-1).
// targetAttempt 0 → semua attempt berikutnya (perilaku auto-cron lama, 2 & 3 sekaligus).
// actorEmail/actorName → siapa yang menjalankan (untuk dicatat di Riwayat Blast).
// Lock supaya tidak double-run (kalau interval terlalu pendek dan batch besar).
func processRetries(force bool, limit, targetAttempt int, actorEmail, actorName string) {
	retryMu.Lock()
	if retryRunning {
		retryMu.Unlock()
		log.Println("retry: previous batch still running, skip")
		return
	}
	retryRunning = true
	retryMu.Unlock()
	defer func() {
		retryMu.Lock()
		retryRunning = false
		retryMu.Unlock()
	}()

	// Window guard: blast attempt hanya distart saat masuk jam window (WIB). Batch yang
	// sudah jalan boleh lanjut lewat jam window (di-proteksi single-flight lock di atas).
	// force=true (trigger manual) melewati guard ini.
	now := time.Now().In(wibLoc)
	if !force && now.Hour() != retryWindowHour {
		return
	}

	// Cek WA connected
	_, loggedIn, connected := state.snapshot()
	if !loggedIn || !connected {
		return
	}

	// "max 1x/hari per thread": eligible kalau belum pernah di-attempt pada hari kalender
	// ini (WIB). startOfToday = 00:00 WIB hari ini.
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, wibLoc)

	// List "Belum Respons" = after_blast + in_progress (sama persis dgn report.go).
	// Begitu user balas → status pindah ke 'open' → otomatis keluar antrian.
	q := `
SELECT phone, COALESCE(nama_outlet, ''), COALESCE(nomer_invoice, ''), current_attempt, COALESCE(last_attempt_at, '')
FROM chat_threads
WHERE status IN ('after_blast', 'in_progress')
  AND current_attempt < 3`
	var qargs []any
	if targetAttempt == 2 || targetAttempt == 3 {
		// hanya thread yang attempt BERIKUTNYA == targetAttempt
		q += ` AND current_attempt = ?`
		qargs = append(qargs, targetAttempt-1)
	}
	q += ` ORDER BY current_attempt DESC, last_attempt_at ASC`

	rows, err := auditDB.Query(q, qargs...)
	if err != nil {
		log.Printf("retry: query error: %v", err)
		return
	}

	type retryRow struct {
		phone, namaOutlet, nomerInvoice string
		currentAttempt                  int
	}
	var batch []retryRow
	for rows.Next() {
		var r retryRow
		var lastAt string
		if err := rows.Scan(&r.phone, &r.namaOutlet, &r.nomerInvoice, &r.currentAttempt, &lastAt); err != nil {
			continue
		}
		if attemptedToday(lastAt, startOfToday) {
			continue // sudah dikirimi attempt hari ini, skip
		}
		batch = append(batch, r)
	}
	rows.Close()

	if limit > 0 && len(batch) > limit {
		batch = batch[:limit]
	}

	if len(batch) == 0 {
		return
	}

	mode := fmt.Sprintf("window %02d:00 WIB", retryWindowHour)
	if force {
		mode = "FORCE (manual)"
	}
	if targetAttempt == 2 || targetAttempt == 3 {
		mode += fmt.Sprintf(" Attempt %d", targetAttempt)
	}
	log.Printf("retry: %s — antrikan %d threads (after_blast + in_progress)", mode, len(batch))

	// Catat batch ini ke Riwayat Blast (blast_logs + blast_recipients) untuk report &
	// monitoring — attempt 2/3 yang benar-benar terkirim ikut tercatat seperti attempt 1.
	batchStart := time.Now()
	auditEmail := actorEmail
	if auditEmail == "" {
		auditEmail = "system@retry"
	}
	// template = template attempt ASLI (raw, dengan {{...}}) → tampil di Riwayat seperti
	// attempt 1. Kolom Attempt + User yang membedakan attempt berapa & manual/auto.
	auditTemplate := GetAttemptTemplate(targetAttempt)
	auditAttempt := targetAttempt
	if targetAttempt != 2 && targetAttempt != 3 {
		// fallback — tak terjadi via UI/auto (selalu per-attempt), tapi jaga-jaga.
		auditTemplate = "Retry Attempt 2 & 3"
		auditAttempt = 0
	}
	retryLogID, err := recordRetryBatchStart(auditEmail, actorName, auditTemplate, auditAttempt, len(batch), batchStart)
	if err != nil {
		log.Printf("retry: recordRetryBatchStart error: %v", err)
	}

	sent, failed := 0, 0
	for i, r := range batch {
		// Re-check sebelum send — kalau user balas (→ open) atau sudah dikirimi hari ini
		// (run lain) antara query dan send, skip.
		if !stillNeedsRetry(r.phone, startOfToday) {
			continue
		}

		nextAttempt := r.currentAttempt + 1
		template := GetAttemptTemplate(nextAttempt)
		body := renderTemplateWithVars(template, r.namaOutlet, r.nomerInvoice)

		if err := sendRetryOne(r.phone, body); err != nil {
			log.Printf("retry: phone=%s attempt=%d FAILED: %v", r.phone, nextAttempt, err)
			failed++
			_ = recordRetryRecipient(retryLogID, r.phone, r.namaOutlet, r.nomerInvoice, "failed", err.Error(), body, time.Now())
			continue
		}

		now := time.Now()
		if err := upsertThreadRetry(r.phone, body, nextAttempt, now); err != nil {
			log.Printf("retry: upsertThreadRetry error: %v", err)
		}
		if err := recordChatMessage(r.phone, "out", body, "", "", now, 0, "system@retry", fmt.Sprintf("Auto Attempt %d", nextAttempt)); err != nil {
			log.Printf("retry: recordChatMessage error: %v", err)
		}
		_ = recordRetryRecipient(retryLogID, r.phone, r.namaOutlet, r.nomerInvoice, "sent", "", body, now)

		log.Printf("retry: phone=%s attempt %d sent", r.phone, nextAttempt)
		if nextAttempt >= 3 {
			// Attempt 3 (terakhir) terkirim. JANGAN langsung close — beri tenggang sampai
			// jam reject (16:00 WIB) hari ini. processRejectSweep yang akan menandai
			// 'rejected' kalau tetap tidak ada respons. Kalau user balas dulu → 'open'.
			log.Printf("retry: phone=%s attempt 3 terkirim → tunggu respons s/d %02d:00 WIB (reject-sweep)", r.phone, retryRejectHour)
		}
		sent++

		// Jitter delay antara send (kecuali yang terakhir)
		if i < len(batch)-1 {
			d := retryMinJitter
			if retryMaxJitter > retryMinJitter {
				d = retryMinJitter + rand.Intn(retryMaxJitter-retryMinJitter+1)
			}
			time.Sleep(time.Duration(d) * time.Second)
		}
	}

	// Tutup entri Riwayat Blast: kalau ada yang benar-benar dikirim/gagal → update
	// sent/failed/ended_at; kalau semua ke-skip (0/0) → hapus entri biar tidak kosong.
	if sent+failed > 0 {
		if err := recordRetryBatchEnd(retryLogID, sent, failed, time.Now()); err != nil {
			log.Printf("retry: recordRetryBatchEnd error: %v", err)
		}
	} else {
		deleteEmptyBlastLog(retryLogID)
	}

	log.Printf("retry: batch done — sent=%d failed=%d", sent, failed)
}

// processRejectSweep — tandai 'rejected' semua nomor yang sudah dikirimi Attempt 3 tapi
// belum merespons hingga jam RETRY_REJECT_HOUR (default 16:00 WIB) pada HARI Attempt 3
// dikirim. Hanya update DB (tidak kirim pesan) → aman dijalankan kapan saja, termasuk saat
// RETRY_ENABLED=false. Begitu user balas → status 'open' → otomatis tidak kena sweep.
// Status 'rejected' membuat nomor keluar dari antrian auto-retry & tampil di Log Status
// Update dengan kolom Rejected = "reject".
func processRejectSweep() {
	now := time.Now().In(wibLoc)

	rows, err := auditDB.Query(`
SELECT phone, COALESCE(last_attempt_at, '')
FROM chat_threads
WHERE status IN ('after_blast', 'in_progress') AND current_attempt >= 3`)
	if err != nil {
		log.Printf("reject-sweep: query error: %v", err)
		return
	}
	type rrow struct{ phone, lastAt string }
	var batch []rrow
	for rows.Next() {
		var r rrow
		if err := rows.Scan(&r.phone, &r.lastAt); err != nil {
			continue
		}
		batch = append(batch, r)
	}
	rows.Close()

	rejected := 0
	for _, r := range batch {
		if r.lastAt == "" {
			continue
		}
		t3, err := time.Parse(time.RFC3339, r.lastAt)
		if err != nil {
			continue
		}
		// cutoff = jam reject WIB pada hari Attempt 3 dikirim. now < cutoff → masih ada
		// kesempatan respons hari ini, skip. (Kalau sweep ketinggalan beberapa hari, hari
		// Attempt 3 sudah lewat → now pasti > cutoff → tetap ke-reject.)
		t3 = t3.In(wibLoc)
		cutoff := time.Date(t3.Year(), t3.Month(), t3.Day(), retryRejectHour, 0, 0, 0, wibLoc)
		if now.Before(cutoff) {
			continue
		}
		// Guard status di WHERE: kalau balasan masuk antara query & update (→ 'open'),
		// jangan timpa.
		reason := fmt.Sprintf("Tidak ada respons s/d %02d:00 WIB (Attempt 3)", retryRejectHour)
		res, err := auditDB.Exec(`UPDATE chat_threads SET status='rejected', rejected_at=?, reject_reason=?, updated_at=datetime('now') WHERE phone=? AND status IN ('after_blast','in_progress') AND current_attempt >= 3`,
			now.Format(time.RFC3339), reason, r.phone)
		if err != nil {
			log.Printf("reject-sweep: update %s error: %v", r.phone, err)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			rejected++
		}
	}
	if rejected > 0 {
		log.Printf("reject-sweep: %d nomor → rejected (Attempt 3 tanpa respons s/d %02d:00 WIB)", rejected, retryRejectHour)
	}
}

// attemptedToday — true kalau last_attempt_at jatuh pada hari kalender yang sama
// (instant >= startOfToday WIB). Kosong/unparseable = belum pernah → false (eligible).
// Perbandingan instant absolut, jadi aman walau offset timestamp tersimpan beda-beda.
func attemptedToday(lastAt string, startOfToday time.Time) bool {
	if lastAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, lastAt)
	if err != nil {
		return false
	}
	return !t.Before(startOfToday)
}

// stillNeedsRetry — re-check sebelum send (race guard): masih di bucket belum-respons,
// belum mentok 3 attempt, dan belum dikirimi hari ini.
func stillNeedsRetry(phone string, startOfToday time.Time) bool {
	var status, lastAt string
	var ca int
	err := auditDB.QueryRow(`SELECT status, current_attempt, COALESCE(last_attempt_at, '') FROM chat_threads WHERE phone = ?`, phone).Scan(&status, &ca, &lastAt)
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

func sendRetryOne(phone, body string) error {
	jid := types.NewJID(phone, types.DefaultUserServer)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	// Cek nomor masih di WA
	res, err := state.client.IsOnWhatsApp(ctx, []string{"+" + phone})
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	if len(res) == 0 || !res[0].IsIn {
		return fmt.Errorf("nomor tidak terdaftar di WhatsApp")
	}

	// Resolve PN -> LID (sama seperti sendOne) — tanpa ini attempt 2/3 undecryptable.
	jid = resolveToLID(ctx, jid)

	// Bootstrap session
	_, _ = state.client.GetUserDevicesContext(ctx, []types.JID{jid})

	msg := &waProto.Message{Conversation: proto.String(body)}
	_, err = state.client.SendMessage(ctx, jid, msg)
	return err
}

func boolEnv(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v == "true" || v == "1" || v == "yes" || v == "on"
}

func atoiEnv(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// handleRetryPreview — daftar nomor yang BENAR-BENAR akan diblast kalau Run ditekan:
// after_blast/in_progress, attempt<3, belum dikirim hari ini. Tidak mengirim apa pun.
func handleRetryPreview(w http.ResponseWriter, r *http.Request) {
	now := time.Now().In(wibLoc)
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, wibLoc)
	rows, err := auditDB.Query(`
SELECT phone, COALESCE(nama_outlet,''), COALESCE(nomer_invoice,''), current_attempt, status, COALESCE(last_attempt_at,'')
FROM chat_threads
WHERE status IN ('after_blast','in_progress') AND current_attempt < 3
ORDER BY current_attempt DESC, last_attempt_at ASC`)
	if err != nil {
		httpErr(w, 500, "query: %v", err)
		return
	}
	defer rows.Close()
	type previewRow struct {
		Phone          string `json:"phone"`
		NamaOutlet     string `json:"nama_outlet"`
		NomerInvoice   string `json:"nomer_invoice"`
		CurrentAttempt int    `json:"current_attempt"`
		NextAttempt    int    `json:"next_attempt"`
		Status         string `json:"status"`
	}
	out := []previewRow{}
	count2, count3 := 0, 0
	for rows.Next() {
		var p previewRow
		var lastAt string
		if err := rows.Scan(&p.Phone, &p.NamaOutlet, &p.NomerInvoice, &p.CurrentAttempt, &p.Status, &lastAt); err != nil {
			continue
		}
		if attemptedToday(lastAt, startOfToday) {
			continue
		}
		p.NextAttempt = p.CurrentAttempt + 1
		switch p.NextAttempt {
		case 2:
			count2++
		case 3:
			count3++
		}
		out = append(out, p)
	}
	// count = total (kompat lama); count2/count3 = eligible per attempt utk 2 tombol terpisah
	writeJSON(w, map[string]any{"rows": out, "count": len(out), "count2": count2, "count3": count3})
}

// handleRetryRunNow — trigger MANUAL: jalankan batch retry sekarang juga, lewati
// guard window-jam. Query opsional ?limit=N untuk batasi jumlah (mis. tes batch kecil).
// Tetap hormati: WA harus connected, attempt<3, belum dikirimi hari ini, jitter 20-40s.
func handleRetryRunNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	_, loggedIn, connected := state.snapshot()
	if !loggedIn || !connected {
		httpErr(w, 400, "WhatsApp belum terhubung — tidak ada yang dikirim.")
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	// attempt=2 atau 3 → hanya kirim attempt itu. Kosong/0 → semua (2 & 3) seperti dulu.
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
	go processRetries(true, limit, attempt, user.Email, user.Name)
	log.Printf("retry: FORCE dipicu manual via API oleh %s (attempt=%d limit=%d)", user.Email, attempt, limit)
	writeJSON(w, map[string]any{"ok": true, "started": true, "attempt": attempt, "limit": limit})
}

// avoid unused
var _ = sql.ErrNoRows
