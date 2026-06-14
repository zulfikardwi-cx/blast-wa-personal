package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math/rand"
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
	retryDelayHours  int
	retryMinJitter   int
	retryMaxJitter   int
)

// startRetryScheduler — jalankan goroutine background yang cek thread retry tiap
// RETRY_CHECK_INTERVAL_MINUTES menit.
func startRetryScheduler() {
	retryIntervalMin = atoiEnv("RETRY_CHECK_INTERVAL_MINUTES", 60)
	retryDelayHours = atoiEnv("RETRY_DELAY_HOURS", 24)
	retryMinJitter = atoiEnv("RETRY_SEND_MIN_DELAY", 20)
	retryMaxJitter = atoiEnv("RETRY_SEND_MAX_DELAY", 40)

	log.Printf("retry scheduler: interval=%dm delay=%dh jitter=%d-%ds",
		retryIntervalMin, retryDelayHours, retryMinJitter, retryMaxJitter)

	go func() {
		// Delay awal 2 menit setelah start, biar WA connect dulu
		time.Sleep(2 * time.Minute)
		ticker := time.NewTicker(time.Duration(retryIntervalMin) * time.Minute)
		defer ticker.Stop()
		for {
			processRetries()
			<-ticker.C
		}
	}()
}

// processRetries — cek semua thread yang perlu retry, kirim attempt berikutnya.
// Lock supaya tidak double-run (kalau interval terlalu pendek dan batch besar).
func processRetries() {
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

	// Cek WA connected
	_, loggedIn, connected := state.snapshot()
	if !loggedIn || !connected {
		return
	}

	cutoffTime := time.Now().Add(-time.Duration(retryDelayHours) * time.Hour).Format(time.RFC3339)

	rows, err := auditDB.Query(`
SELECT phone, COALESCE(nama_outlet, ''), COALESCE(nomer_invoice, ''), current_attempt
FROM chat_threads
WHERE status = 'after_blast'
  AND current_attempt < 3
  AND (last_attempt_at IS NULL OR last_attempt_at < ?)
ORDER BY last_attempt_at ASC
LIMIT 100`, cutoffTime)
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
		if err := rows.Scan(&r.phone, &r.namaOutlet, &r.nomerInvoice, &r.currentAttempt); err == nil {
			batch = append(batch, r)
		}
	}
	rows.Close()

	if len(batch) == 0 {
		return
	}

	log.Printf("retry: processing %d threads (cutoff=%s)", len(batch), cutoffTime)

	sent, failed := 0, 0
	for i, r := range batch {
		// Re-check status — kalau user balas antara query dan send, skip
		if !stillNeedsRetry(r.phone) {
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
			log.Printf("retry: phone=%s reached MAX ATTEMPTS — no more auto-retry akan dijalankan untuk nomor ini", r.phone)
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

// stillNeedsRetry — re-check status di DB sebelum send (race condition guard)
func stillNeedsRetry(phone string) bool {
	var status string
	var ca int
	err := auditDB.QueryRow(`SELECT status, current_attempt FROM chat_threads WHERE phone = ?`, phone).Scan(&status, &ca)
	if err != nil {
		return false
	}
	return status == "after_blast" && ca < 3
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

// avoid unused
var _ = sql.ErrNoRows
