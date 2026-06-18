package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type RecipientStatus struct {
	Phone      string `json:"phone"`
	NamaOutlet string `json:"nama_outlet"`
	NomerInv   string `json:"nomer_invoice"`
	Message    string `json:"message,omitempty"`
	Status     string `json:"status"` // pending | sent | failed | skipped
	Error      string `json:"error,omitempty"`
	SentAt     string `json:"sent_at,omitempty"`
}

type BlastJob struct {
	mu        sync.RWMutex
	ID        string             `json:"id"`
	UserEmail string             `json:"user_email"`
	UserName  string             `json:"user_name"`
	Template  string             `json:"template"`
	StartedAt time.Time          `json:"started_at"`
	EndedAt   *time.Time         `json:"ended_at,omitempty"`
	Running   bool               `json:"running"`
	MinDelay  int                `json:"min_delay_sec"`
	MaxDelay  int                `json:"max_delay_sec"`
	Total     int                `json:"total"`
	Sent      int                `json:"sent"`
	Failed    int                `json:"failed"`
	Skipped   int                `json:"skipped"`
	Items     []*RecipientStatus `json:"items"`
	auditID   int64
	cancel    context.CancelFunc
}

func (j *BlastJob) snapshot() map[string]any {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return map[string]any{
		"id":            j.ID,
		"user_email":    j.UserEmail,
		"user_name":     j.UserName,
		"template":      j.Template,
		"started_at":    j.StartedAt,
		"ended_at":      j.EndedAt,
		"running":       j.Running,
		"min_delay_sec": j.MinDelay,
		"max_delay_sec": j.MaxDelay,
		"total":         j.Total,
		"sent":          j.Sent,
		"failed":        j.Failed,
		"skipped":       j.Skipped,
		"items":         j.Items,
	}
}

func handleBlast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	_, loggedIn, connected := state.snapshot()
	if !loggedIn || !connected {
		httpErr(w, 400, "WhatsApp belum terhubung. Scan QR dulu.")
		return
	}
	if state.job != nil && state.job.Running {
		httpErr(w, 409, "Ada blast yang sedang berjalan.")
		return
	}

	if err := r.ParseMultipartForm(10 << 20); err != nil {
		httpErr(w, 400, "form: %v", err)
		return
	}
	// Template attempt 1 dari backend (3 template di chat.go)
	// User-supplied template di-abaikan supaya konsisten dengan retry attempt 2/3
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
		ID:        fmt.Sprintf("job-%d", time.Now().Unix()),
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
	state.job = job

	id, err := recordBlastStart(job)
	if err != nil {
		fmt.Println("audit start failed:", err)
	}
	job.auditID = id

	go runBlast(ctx, job)

	writeJSON(w, map[string]any{"ok": true, "job_id": job.ID, "total": len(rows)})
}

func handleProgress(w http.ResponseWriter, r *http.Request) {
	if state.job == nil {
		writeJSON(w, map[string]any{"job": nil})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"job": state.job.snapshot()})
}

func parseCSV(r io.Reader) ([]*RecipientStatus, error) {
	rd := csv.NewReader(r)
	rd.TrimLeadingSpace = true
	rd.FieldsPerRecord = -1
	header, err := rd.Read()
	if err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	pi, ok1 := idx["phone"]
	ni, ok2 := idx["nama_outlet"]
	ii, ok3 := idx["nomer_invoice"]
	if !ok1 || !ok2 || !ok3 {
		return nil, fmt.Errorf("header wajib: phone, nama_outlet, nomer_invoice")
	}

	var out []*RecipientStatus
	for {
		row, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		phone := normalizePhone(row[pi])
		if phone == "" {
			continue
		}
		out = append(out, &RecipientStatus{
			Phone:      phone,
			NamaOutlet: safeAt(row, ni),
			NomerInv:   safeAt(row, ii),
			Status:     "pending",
		})
	}
	return out, nil
}

func safeAt(row []string, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

// normalizePhone: ambil digit saja, normalkan ke format internasional Indonesia (62...).
// Tangani 3 format umum dari CSV:
//   - "628xxx"  -> sudah benar, biarkan
//   - "08xxx"   -> ganti 0 jadi 62
//   - "8xxx"    -> tanpa 0/62 (sering dari Excel yang buang leading 0) -> tambah 62
// Tanpa ini, "8xxx" dicek sebagai "+8..." (mis. +82 = Korea) -> dianggap tidak terdaftar.
func normalizePhone(raw string) string {
	var b strings.Builder
	for _, ch := range strings.TrimSpace(raw) {
		if ch >= '0' && ch <= '9' {
			b.WriteRune(ch)
		}
	}
	s := b.String()
	if s == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(s, "62"):
		// sudah format internasional
	case strings.HasPrefix(s, "0"):
		s = "62" + s[1:]
	case strings.HasPrefix(s, "8"):
		s = "62" + s
	}
	return s
}

func renderTemplate(tpl string, r *RecipientStatus) string {
	out := tpl
	out = strings.ReplaceAll(out, "{{nama_outlet}}", r.NamaOutlet)
	out = strings.ReplaceAll(out, "{{nomer_invoice}}", r.NomerInv)
	return out
}

func runBlast(ctx context.Context, job *BlastJob) {
	defer func() {
		now := time.Now()
		job.mu.Lock()
		job.Running = false
		job.EndedAt = &now
		job.mu.Unlock()
		if err := recordBlastEnd(job.auditID, job); err != nil {
			fmt.Println("audit end failed:", err)
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

		if err := sendOne(rec.Phone, msg); err != nil {
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

		// Persist per-recipient detail untuk export ke Sheets nanti
		if err := recordRecipient(job.auditID, rec); err != nil {
			fmt.Println("warn: recordRecipient failed for", rec.Phone, ":", err)
		}

		// Update chat thread & message (untuk Inbox).
		if rec.Status == "sent" {
			now := time.Now()
			if err := upsertThreadBlast(rec.Phone, rec.NamaOutlet, rec.NomerInv, job.auditID, msg, now); err != nil {
				fmt.Println("warn: upsertThreadBlast:", err)
			}
			if err := recordChatMessage(rec.Phone, "out", msg, "", "", now, job.auditID, job.UserEmail, job.UserName); err != nil {
				fmt.Println("warn: recordChatMessage outgoing blast:", err)
			}
		} else if rec.Status == "failed" {
			// Attempt 1 gagal kirim → tandai thread 'rejected' supaya muncul di Log Status
			// Update (Attempt 1 = "Rejected", kolom Rejected = "reject") utk di-reject tim WO.
			if err := upsertThreadBlastFailed(rec.Phone, rec.NamaOutlet, rec.NomerInv, job.auditID, rec.Error, time.Now()); err != nil {
				fmt.Println("warn: upsertThreadBlastFailed:", err)
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

func sendOne(phone, body string) error {
	jid := types.NewJID(phone, types.DefaultUserServer)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	// 1) cek nomor terdaftar WA
	res, err := state.client.IsOnWhatsApp(ctx, []string{"+" + phone})
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	if len(res) == 0 || !res[0].IsIn {
		return fmt.Errorf("nomor tidak terdaftar di WhatsApp")
	}

	// 1b) Resolve PN -> LID (lihat resolveToLID). Wajib juga di retry attempt 2/3.
	jid = resolveToLID(ctx, jid)

	// 2) pre-fetch device list — bootstrap Signal session ke semua device penerima.
	// Tanpa ini, kirim pertama ke nomor baru bisa "Waiting for this message" karena
	// session belum ter-establish. Error di sini non-fatal — coba tetap kirim.
	if _, e := state.client.GetUserDevicesContext(ctx, []types.JID{jid}); e != nil {
		fmt.Println("warn: prefetch devices failed for", phone, ":", e)
	}

	// 3) kirim
	msg := &waProto.Message{Conversation: proto.String(body)}
	_, err = state.client.SendMessage(ctx, jid, msg)
	if err != nil {
		return err
	}
	return nil
}

// resolveToLID — WhatsApp migrasi ke sistem LID. Kalau penerima sudah punya LID,
// whatsmeow meng-enkripsi session di bawah identitas LID, tapi tujuan kirim tetap PN
// selama LIDMigrationTimestamp akun pengirim masih 0 (send.go:325) -> mismatch ->
// penerima tidak bisa dekripsi ("Waiting for this message", kosong di HP). Resolve ke
// JID LID supaya alamat + enkripsi konsisten dan device list lengkap (termasuk primary
// :0). Return JID asli kalau LID tidak tersedia (nomor lama belum migrasi -> PN aman).
func resolveToLID(ctx context.Context, jid types.JID) types.JID {
	if lid, e := state.client.Store.LIDs.GetLIDForPN(ctx, jid); e == nil && !lid.IsEmpty() {
		return lid
	}
	if info, e := state.client.GetUserInfo(ctx, []types.JID{jid}); e == nil && !info[jid].LID.IsEmpty() {
		return info[jid].LID
	}
	return jid
}

func atoiOr(s string, def int) int {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return def
	}
	return n
}

// suppress unused import warnings on some platforms
var _ = whatsmeow.Client{}
