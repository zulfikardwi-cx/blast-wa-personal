package main

// ============================================================================
// KONFIRMASI VALIDASI VIA WEB (pengganti "chat ke Inti" yang di-reject Meta)
//
// Customer terima blast (dari Tools Blast Resmi Majoo) berisi KODE REFERENSI +
// tombol statik ke halaman konfirmasi. Di halaman itu customer mengetik kodenya.
// POST /api/konfirmasi (PUBLIK, tanpa login) memvalidasi kode → thread pindah ke
// bucket 'open' (= sudah respons, menunggu agent) + penanda di Inbox, jadi work
// order tim validator. Ekuivalen web dari balasan WA (upsertThreadIncoming).
// ============================================================================

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// konfirmasiURL — URL statik halaman konfirmasi (dipakai sebagai tombol di template
// WABA & kolom link CSV). Override via env KONFIRMASI_URL.
func konfirmasiURL() string {
	if v := strings.TrimSpace(os.Getenv("KONFIRMASI_URL")); v != "" {
		return v
	}
	return "https://blastvalidasi.cxmajoo.my.id/konfirmasi.html"
}

// ---- rate limiter sederhana per-IP (anti brute-force kode) ----
var (
	konfMu     sync.Mutex
	konfHits   = map[string][]time.Time{}
	konfLimit  = 20          // maks percobaan
	konfWindow = time.Minute // per jendela waktu
)

func konfirmasiRateOK(ip string) bool {
	now := time.Now()
	konfMu.Lock()
	defer konfMu.Unlock()
	cutoff := now.Add(-konfWindow)
	kept := konfHits[ip][:0]
	for _, t := range konfHits[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= konfLimit {
		konfHits[ip] = kept
		return false
	}
	konfHits[ip] = append(kept, now)
	return true
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if cf := r.Header.Get("CF-Connecting-IP"); cf != "" {
		return cf
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host
}

// handleKonfirmasi — POST /api/konfirmasi (publik). Body JSON {"kode":"XXXX"} atau
// form field "kode". Return {ok, nama_outlet, nomer_invoice} kalau cocok.
func handleKonfirmasi(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if !konfirmasiRateOK(clientIP(r)) {
		writeJSON(w, map[string]any{"ok": false, "error": "Terlalu banyak percobaan. Coba lagi sebentar lagi."})
		return
	}

	kode := ""
	// Terima JSON atau form.
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var in struct {
			Kode string `json:"kode"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		kode = in.Kode
	} else {
		_ = r.ParseForm()
		kode = r.FormValue("kode")
	}
	input := strings.ToUpper(strings.TrimSpace(kode))
	if input == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "Nomor Invoice atau Kode Referensi belum diisi."})
		return
	}

	// Terima Kode Referensi (token) ATAU Nomor Invoice.
	phone, invoice, outlet, ok := lookupToken(input)
	token := input
	if !ok || phone == "" {
		phone, invoice, outlet, token, ok = lookupByInvoice(input)
	}
	if !ok || phone == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "Informasi invoice atau kode referensi anda tidak valid."})
		return
	}

	now := time.Now()
	body := fmt.Sprintf("[Konfirmasi Validasi via Web] Kode %s — Customer siap divalidasi (Invoice %s).", token, invoice)

	// Idempoten: hanya catat penanda & pindah bucket sekali. Kalau sudah pernah konfirmasi,
	// tetap balas sukses tanpa dobel penanda / dobel unread.
	var already int
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM chat_messages WHERE phone=? AND direction='in' AND body LIKE '[Konfirmasi Validasi via Web]%'`, phone).Scan(&already)
	if already == 0 {
		if err := recordChatMessage(phone, "in", body, "", "", now, 0, "", ""); err != nil {
			fmt.Println("warn: konfirmasi recordChatMessage:", err)
		}
		// Pindah thread ke 'open' (kecuali terminal) — sama seperti balasan WA masuk.
		if err := upsertThreadIncoming(phone, body, now); err != nil {
			fmt.Println("warn: konfirmasi upsertThreadIncoming:", err)
		}
	}

	// Link wa.me ke INTI dengan pesan prefilled (Kode Referensi) — customer diarahkan chat
	// ke Inti setelah konfirmasi supaya Inti dapat inbound ASLI (window terbuka → validator
	// bisa balas normal, tak kena 463). Token asli dipakai (bukan input mentah).
	waLink := buildTriggerLink(phone, invoice, outlet, token)

	writeJSON(w, map[string]any{"ok": true, "nama_outlet": outlet, "nomer_invoice": invoice, "wa_link": waLink})
}
