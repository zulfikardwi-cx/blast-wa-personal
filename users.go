package main

// Akun per-user: tiap email punya password sendiri (bcrypt). Roster diambil dari
// APP_LOGIN_EMAILS (.env). Email di roster yang belum ada di tabel di-seed dengan password
// default "admin123" (must_change=1). User ganti passwordnya sendiri via menu Profil.

import (
	"log"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const defaultUserPassword = "admin123"

func initUsers() error {
	_, err := auditDB.Exec(`
CREATE TABLE IF NOT EXISTS app_users (
	email TEXT PRIMARY KEY,
	pass_hash TEXT NOT NULL,
	must_change INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);`)
	if err != nil {
		return err
	}
	// Seed dari roster APP_LOGIN_EMAILS (sudah lowercase di initAuth). Hanya insert yang
	// belum ada — tidak menimpa password user yang sudah pernah diganti.
	for _, email := range appLoginEmails {
		var c int
		_ = auditDB.QueryRow(`SELECT COUNT(*) FROM app_users WHERE email=?`, email).Scan(&c)
		if c > 0 {
			continue
		}
		h, err := bcrypt.GenerateFromPassword([]byte(defaultUserPassword), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		if _, err := auditDB.Exec(`INSERT INTO app_users (email, pass_hash, must_change) VALUES (?, ?, 1)`, email, string(h)); err != nil {
			return err
		}
		log.Printf("seed user: %s (password default '%s')", email, defaultUserPassword)
	}
	return nil
}

// verifyUserPassword — true kalau email ada & password cocok. mustChange = masih pakai default.
func verifyUserPassword(email, password string) (ok bool, mustChange bool) {
	var hash string
	var mc int
	if err := auditDB.QueryRow(`SELECT pass_hash, must_change FROM app_users WHERE email=?`, email).Scan(&hash, &mc); err != nil {
		return false, false
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return false, false
	}
	return true, mc == 1
}

func setUserPassword(email, newPassword string) error {
	h, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = auditDB.Exec(`UPDATE app_users SET pass_hash=?, must_change=0, updated_at=datetime('now') WHERE email=?`, string(h), email)
	return err
}

// nameFromEmail — "zulfikar.dwi@majoo.id" → "Zulfikar Dwi" (untuk display & audit).
func nameFromEmail(email string) string {
	local := email
	if i := strings.IndexByte(email, '@'); i > 0 {
		local = email[:i]
	}
	local = strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(local)
	parts := strings.Fields(local)
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	if len(parts) == 0 {
		return email
	}
	return strings.Join(parts, " ")
}

// handleChangePassword — user ganti password sendiri (wajib tahu password lama).
func handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	user, ok := userFromCtx(r.Context())
	if !ok {
		httpErr(w, 401, "unauthorized")
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		if err := r.ParseForm(); err != nil {
			httpErr(w, 400, "form: %v", err)
			return
		}
	}
	cur := r.FormValue("current_password")
	nw := strings.TrimSpace(r.FormValue("new_password"))
	if len(nw) < 6 {
		httpErr(w, 400, "Password baru minimal 6 karakter.")
		return
	}
	okPass, _ := verifyUserPassword(user.Email, cur)
	if !okPass {
		httpErr(w, 401, "Password lama salah.")
		return
	}
	if err := setUserPassword(user.Email, nw); err != nil {
		httpErr(w, 500, "update: %v", err)
		return
	}
	log.Println("password changed:", user.Email)
	writeJSON(w, map[string]any{"ok": true})
}
