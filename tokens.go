package main

// ============================================================================
// TOKEN VALIDASI — korelasi balasan pelanggan (di INTI) ke invoice yang di-blast
// (dari BLASTER).
//
// Alur: BLASTER kirim pesan trigger berisi link wa.me ke INTI + baris
// "Kode Referensi : <token>". Pelanggan klik → chat masuk ke INTI dengan teks
// prefilled itu → handleIncomingWA (main.go) mem-parse token → lookupToken →
// dapat (phone, invoice, outlet) canonical → thread Inbox terkait invoice yang benar.
//
// Token di-reuse per (phone, invoice) lintas attempt (link tetap konsisten).
// Token di-mark "used" saat thread di-Done (lihat markTokenUsed dipanggil dari
// jalur Done di chat.go). Retry attempt 2/3 sudah otomatis berhenti utk invoice yang
// Done via resolved_invoices — token status "used" jadi penanda eksplisit + audit.
// ============================================================================

import (
	"crypto/rand"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

func initTokens() error {
	loadPrefillTemplate()
	_, err := auditDB.Exec(`
CREATE TABLE IF NOT EXISTS validation_tokens (
	token         TEXT PRIMARY KEY,
	phone         TEXT NOT NULL,
	nomer_invoice TEXT NOT NULL DEFAULT '',
	nama_outlet   TEXT NOT NULL DEFAULT '',
	status        TEXT NOT NULL DEFAULT 'pending',
	created_at    TEXT NOT NULL DEFAULT (datetime('now')),
	used_at       TEXT,
	UNIQUE(phone, nomer_invoice)
)`)
	return err
}

// prefillTemplate — teks yang otomatis terisi di chat pelanggan saat mereka klik link wa.me
// ke INTI. Placeholder: {{nama_outlet}}, {{nomer_invoice}}, {{token}} (alias {{kode_referal}}).
// Override via env INTI_PREFILL_TEMPLATE (pakai \n untuk newline).
var prefillTemplate string

// tokenLineRe — regex pencari baris token pada pesan MASUK. Default cocok utk label
// "Kode Referal" & "Kode Referensi"; di-override otomatis dari prefillTemplate (loadPrefillTemplate)
// supaya label parser SELALU sama dgn label yang dipakai di link — tak mungkin drift.
var tokenLineRe = regexp.MustCompile(`(?i)Kode\s+Refera(?:l|nsi)\s*:\s*([A-Za-z0-9]{4,16})`)

func loadPrefillTemplate() {
	t := os.Getenv("INTI_PREFILL_TEMPLATE")
	if t == "" {
		t = `Halo,
Saya mau validasi atas invoice :
Nama Outlet: {{nama_outlet}}
No. Invoice: {{nomer_invoice}}
Kode Referensi: {{token}}`
	}
	t = strings.ReplaceAll(t, `\n`, "\n")
	// {{kode_referal}} = alias placeholder token supaya template lebih ramah dibaca.
	t = strings.ReplaceAll(t, "{{kode_referal}}", "{{token}}")
	prefillTemplate = t
	if re := deriveTokenLineRe(t); re != nil {
		tokenLineRe = re
	}
}

// deriveTokenLineRe — bangun regex parser dari BARIS yang memuat {{token}} di prefillTemplate.
// Label apa pun yang dipakai user (mis. "Kode Referal", "Kode Referensi", "Ref") otomatis
// jadi pola parser → tak perlu ubah kode kalau ganti wording. Return nil kalau tak ketemu.
func deriveTokenLineRe(tpl string) *regexp.Regexp {
	for _, line := range strings.Split(tpl, "\n") {
		i := strings.Index(line, "{{token}}")
		if i < 0 {
			continue
		}
		label := strings.TrimRight(line[:i], " ")   // "Kode Referal:"
		label = strings.TrimRight(label, ":")        // "Kode Referal"
		label = strings.TrimSpace(label)
		if label == "" {
			return nil
		}
		pat := strings.ReplaceAll(regexp.QuoteMeta(label), " ", `\s+`)
		return regexp.MustCompile(`(?i)` + pat + `\s*:\s*([A-Za-z0-9]{4,16})`)
	}
	return nil
}

// tokenAlphabet — alfanumerik tanpa karakter ambigu (0/O, 1/I/L) supaya token yang
// tampak di chat gampang dibaca & tidak salah ketik.
const tokenAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

const tokenLen = 8

func newTokenCode() string {
	b := make([]byte, tokenLen)
	if _, err := rand.Read(b); err != nil {
		// fallback sangat jarang: pakai timestamp nano biar tetap unik-ish
		return "T" + time.Now().Format("150405.000000")
	}
	for i := range b {
		b[i] = tokenAlphabet[int(b[i])%len(tokenAlphabet)]
	}
	return string(b)
}

// getOrCreateToken — kembalikan token yang sudah ada utk (phone, invoice), atau bikin baru.
// Idempoten: attempt 2/3 pakai token yang sama dengan attempt 1.
func getOrCreateToken(phone, invoice, outlet string) string {
	var tok string
	err := auditDB.QueryRow(`SELECT token FROM validation_tokens WHERE phone=? AND nomer_invoice=?`, phone, invoice).Scan(&tok)
	if err == nil && tok != "" {
		return tok
	}
	// generate + insert (retry beberapa kali kalau bentrok PK, sangat jarang)
	for i := 0; i < 5; i++ {
		tok = newTokenCode()
		_, err = auditDB.Exec(`INSERT INTO validation_tokens (token, phone, nomer_invoice, nama_outlet) VALUES (?,?,?,?)`,
			tok, phone, invoice, outlet)
		if err == nil {
			return tok
		}
		// Bentrok UNIQUE(phone,invoice) karena race → ambil yang sudah ada.
		if err2 := auditDB.QueryRow(`SELECT token FROM validation_tokens WHERE phone=? AND nomer_invoice=?`, phone, invoice).Scan(&tok); err2 == nil && tok != "" {
			return tok
		}
	}
	return tok
}

// intiNumber — nomor WA INTI (tujuan link wa.me). Prioritas env INTI_WA_NUMBER (digit saja),
// fallback ke Store ID client INTI setelah login.
func intiNumber() string {
	if v := normalizePhone(os.Getenv("INTI_WA_NUMBER")); v != "" {
		return v
	}
	if state != nil && state.client != nil && state.client.Store != nil && state.client.Store.ID != nil {
		return state.client.Store.ID.User
	}
	return ""
}

// buildTriggerLink — link wa.me ke INTI dengan teks prefilled (dari prefillTemplate) berisi
// token. Kalau nomor INTI belum diketahui (belum login & env kosong), kembalikan "" —
// pemanggil (applyLink) menghapus baris {{link}}.
func buildTriggerLink(phone, invoice, outlet, token string) string {
	inti := intiNumber()
	if inti == "" {
		return ""
	}
	prefilled := prefillTemplate
	prefilled = strings.ReplaceAll(prefilled, "{{nama_outlet}}", outlet)
	prefilled = strings.ReplaceAll(prefilled, "{{nomer_invoice}}", invoice)
	prefilled = strings.ReplaceAll(prefilled, "{{token}}", token)
	return "https://wa.me/" + inti + "?text=" + url.QueryEscape(prefilled)
}

// applyLink — ganti placeholder {{link}} pada body blast dengan link trigger wa.me ke INTI.
// Token di-reuse per (phone,invoice). Kalau nomor INTI belum diketahui (belum login & env
// kosong), hapus baris {{link}} supaya tidak ada placeholder mentah terkirim. Dipanggil HANYA
// di jalur blast/retry majoo (bukan Zopoz) — renderTemplate/renderTemplateWithVars tetap generik.
func applyLink(body, phone, invoice, outlet string) string {
	needLink := strings.Contains(body, "{{link}}")
	needCode := strings.Contains(body, "{{kode_referensi}}") || strings.Contains(body, "{{token}}")
	if !needLink && !needCode {
		return body
	}
	// Satu token per (phone,invoice) dipakai untuk kedua tempat: teks Kode Referensi di badan
	// pesan DAN di dalam link wa.me → konsisten (agent lihat kode sama dari mana pun customer masuk).
	token := getOrCreateToken(phone, invoice, outlet)
	body = strings.ReplaceAll(body, "{{kode_referensi}}", token)
	body = strings.ReplaceAll(body, "{{token}}", token)
	if needLink {
		link := buildTriggerLink(phone, invoice, outlet, token)
		if link == "" {
			body = strings.ReplaceAll(body, "{{link}}\n", "")
			body = strings.ReplaceAll(body, "{{link}}", "")
		} else {
			body = strings.ReplaceAll(body, "{{link}}", link)
		}
	}
	return body
}

// lookupToken — cari konteks invoice dari token.
func lookupToken(token string) (phone, invoice, outlet string, ok bool) {
	if token == "" {
		return "", "", "", false
	}
	err := auditDB.QueryRow(`SELECT phone, COALESCE(nomer_invoice,''), COALESCE(nama_outlet,'') FROM validation_tokens WHERE token=?`, token).
		Scan(&phone, &invoice, &outlet)
	if err != nil {
		return "", "", "", false
	}
	return phone, invoice, outlet, true
}

// resolveInboundThread — tentukan canonical phone (key thread INTI) untuk pesan masuk.
//  1. Token di teks ("Kode Referensi") → phone dari validation_tokens (paling presisi;
//     jalan walau pelanggan chat dari nomor beda).
//  2. Fallback: nomor pengirim, kalau pernah di-blast (ada thread) atau pernah dikirimi token.
// Return ("", false) kalau stranger (tak ada token valid & tak pernah di-blast) → skip.
func resolveInboundThread(body, senderPhone string) (string, bool) {
	if tok := parseTokenFromBody(body); tok != "" {
		if phone, _, _, ok := lookupToken(tok); ok && phone != "" {
			return phone, true
		}
	}
	if senderPhone != "" {
		if isPhoneBlasted(senderPhone) || phoneHasToken(senderPhone) {
			return senderPhone, true
		}
		// Kasus BEDA NOMOR: customer chat dari nomor lain dari yang di-blast. Pesan PERTAMA
		// (bertoken) sudah nyangkut ke thread canonical (nomor blast) + simpan wa_jid=senderPhone.
		// Pesan LANJUTAN tanpa token dari nomor ini harus ikut ke thread canonical itu (reverse
		// lookup wa_jid), bukan bikin thread baru → cegah history percakapan terpecah.
		if canonical := threadByReplyJID(senderPhone); canonical != "" {
			return canonical, true
		}
	}
	return "", false
}

// phoneHasToken — true kalau nomor pernah dikirimi token (fallback gate inbound bila token
// di teks hilang/diedit customer).
func phoneHasToken(phone string) bool {
	var c int
	_ = auditDB.QueryRow(`SELECT COUNT(*) FROM validation_tokens WHERE phone=?`, phone).Scan(&c)
	return c > 0
}

// markTokenUsed — tandai token (phone,invoice) used saat thread di-Done. Kalau invoice
// kosong, tandai semua token milik phone (best-effort).
func markTokenUsed(phone, invoice string) {
	now := time.Now().Format(time.RFC3339)
	if strings.TrimSpace(invoice) != "" {
		_, _ = auditDB.Exec(`UPDATE validation_tokens SET status='used', used_at=? WHERE phone=? AND nomer_invoice=? AND status!='used'`, now, phone, invoice)
		return
	}
	_, _ = auditDB.Exec(`UPDATE validation_tokens SET status='used', used_at=? WHERE phone=? AND status!='used'`, now, phone)
}

// parseTokenFromBody — ekstrak token dari baris label token (default "Kode Referensi : XXXX")
// pada teks masuk. Regex-nya (tokenLineRe) diturunkan dari prefillTemplate (lihat di atas).
func parseTokenFromBody(body string) string {
	m := tokenLineRe.FindStringSubmatch(body)
	if len(m) == 2 {
		return strings.ToUpper(m[1])
	}
	return ""
}
