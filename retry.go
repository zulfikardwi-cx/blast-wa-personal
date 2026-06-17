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
	retryMinJitter   int
	retryMaxJitter   int
	wibLoc           *time.Location
)

// startRetryScheduler — jalankan goroutine background yang cek thread retry tiap
// RETRY_CHECK_INTERVAL_MINUTES menit. Blast attempt 2/3 hanya dijalankan saat masuk
// window jam RETRY_WINDOW_HOUR (WIB), maksimal 1x/hari kalender per thread.
func startRetryScheduler() {
	retryIntervalMin = atoiEnv("RETRY_CHECK_INTERVAL_MINUTES", 30)
	retryWindowHour = atoiEnv("RETRY_WINDOW_HOUR", 9)
	retryMinJitter = atoiEnv("RETRY_SEND_MIN_DELAY", 20)
	retryMaxJitter = atoiEnv("RETRY_SEND_MAX_DELAY", 40)

	var err error
	wibLoc, err = time.LoadLocation("Asia/Jakarta")
	if err != nil {
		wibLoc = time.FixedZone("WIB", 7*3600)
		log.Printf("retry scheduler: LoadLocation Asia/Jakarta gagal (%v), fallback fixed UTC+7", err)
	}

	log.Printf("retry scheduler: interval=%dm window=%02d:00 WIB jitter=%d-%ds (max 1x/hari per thread)",
		retryIntervalMin, retryWindowHour, retryMinJitter, retryMaxJitter)

	go func() {
		// Delay awal 2 menit setelah start, biar WA connect dulu
		time.Sleep(2 * time.Minute)
		ticker := time.NewTicker(time.Duration(retryIntervalMin) * time.Minute)
		defer ticker.Stop()
		for {
			processRetries(false, 0)
			<-ticker.C
		}
	}()
}

// processRetries — cek semua thread yang perlu retry, kirim attempt berikutnya.
// force=true → lewati window-jam guard (dipakai endpoint manual /api/retry/run-now).
// limit>0 → batasi jumlah thread yang diproses (mis. tes batch kecil dulu).
// Lock supaya tidak double-run (kalau interval terlalu pendek dan batch besar).
func processRetries(force bool, limit int) {
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
	rows, err := auditDB.Query(`
SELECT phone, COALESCE(nama_outlet, ''), COALESCE(nomer_invoice, ''), current_attempt, COALESCE(last_attempt_at, '')
FROM chat_threads
WHERE status IN ('after_blast', 'in_progress')
  AND current_attempt < 3
ORDER BY current_attempt DESC, last_attempt_at ASC`)
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
	log.Printf("retry: %s — antrikan %d threads (after_blast + in_progress)", mode, len(batch))

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
			continue
		}

		now := time.Now()
		if err := upsertThreadRetry(r.phone, body, nextAttempt, now); err != nil {
			log.Printf("retry: upsertThreadRetry error: %v", err)
		}
		if err := recordChatMessage(r.phone, "out", body, "", "", now, 0, "system@retry", fmt.Sprintf("Auto Attempt %d", nextAttempt)); err != nil {
			log.Printf("retry: recordChatMessage error: %v", err)
		}

		log.Printf("retry: phone=%s attempt %d sent", r.phone, nextAttempt)
		if nextAttempt >= 3 {
			// Attempt 3 (terakhir) terkirim & tetap belum ada respons → pindah dari
			// after_blast ke force_close. Hanya kalau masih after_blast (kalau sudah
			// balas/diubah, jangan timpa). Otomatis keluar dari auto-retry & Belum Respons.
			if _, err := auditDB.Exec(`UPDATE chat_threads SET status='force_close', updated_at=datetime('now') WHERE phone=? AND status='after_blast'`, r.phone); err != nil {
				log.Printf("retry: set force_close error: %v", err)
			}
			log.Printf("retry: phone=%s attempt 3 terkirim → force_close (no response)", r.phone)
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

	log.Printf("retry: batch done — sent=%d failed=%d", sent, failed)
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
	go processRetries(true, limit)
	log.Printf("retry: FORCE dipicu manual via API (limit=%d)", limit)
	writeJSON(w, map[string]any{"ok": true, "started": true, "limit": limit})
}

// avoid unused
var _ = sql.ErrNoRows
