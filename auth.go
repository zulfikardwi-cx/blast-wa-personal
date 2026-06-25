package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	sessionCookie = "blast_session"
	stateCookie   = "blast_oauth_state"
	allowedDomain = "majoo.id"
	sessionTTL    = 7 * 24 * time.Hour
)

type SessionUser struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Picture  string `json:"picture"`
	IssuedAt int64  `json:"iat"`
}

var oauthCfg *oauth2.Config
var sessionSecret []byte
var frontendURL string
var allowedOrigins []string

// Login alternatif (non-OAuth): password bersama dari env APP_LOGIN_PASSWORD. Dipakai saat
// OAuth tidak bisa (mis. domain baru tanpa akses Google Console). Email yang boleh masuk:
// daftar APP_LOGIN_EMAILS (koma) kalau di-set, kalau kosong → wajib @majoo.id (sama OAuth).
var appLoginPassword string
var appLoginEmails []string

func initAuth() error {
	clientID := os.Getenv("GOOGLE_CLIENT_ID")
	clientSecret := os.Getenv("GOOGLE_CLIENT_SECRET")
	redirect := os.Getenv("OAUTH_REDIRECT_URL")
	secret := os.Getenv("SESSION_SECRET")
	frontendURL = strings.TrimRight(os.Getenv("FRONTEND_URL"), "/")
	ao := os.Getenv("ALLOWED_ORIGINS")

	if secret == "" {
		return errors.New("SESSION_SECRET wajib di-set (generate: openssl rand -hex 32)")
	}
	sessionSecret = []byte(secret)

	for _, o := range strings.Split(ao, ",") {
		o = strings.TrimSpace(strings.TrimRight(o, "/"))
		if o != "" {
			allowedOrigins = append(allowedOrigins, o)
		}
	}

	appLoginPassword = os.Getenv("APP_LOGIN_PASSWORD")
	for _, e := range strings.Split(os.Getenv("APP_LOGIN_EMAILS"), ",") {
		e = strings.TrimSpace(strings.ToLower(e))
		if e != "" {
			appLoginEmails = append(appLoginEmails, e)
		}
	}

	// OAuth Google OPSIONAL — aktif hanya kalau GOOGLE_* + OAUTH_REDIRECT_URL lengkap.
	// Kalau tidak, login murni via email+password (APP_LOGIN_PASSWORD).
	if clientID != "" && clientSecret != "" && redirect != "" {
		oauthCfg = &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirect,
			Scopes:       []string{"openid", "email", "profile"},
			Endpoint:     google.Endpoint,
		}
		fmt.Println("OAuth Google: AKTIF — redirect", redirect)
	} else {
		fmt.Println("OAuth Google: NONAKTIF (GOOGLE_* tidak lengkap) — login via email+password")
	}
	if oauthCfg == nil && appLoginPassword == "" {
		return errors.New("tidak ada metode login aktif: set APP_LOGIN_PASSWORD, atau GOOGLE_* + OAUTH_REDIRECT_URL")
	}
	fmt.Println("login: FRONTEND_URL =", frontendURL, "| ALLOWED_ORIGINS =", allowedOrigins, "| password-login =", appLoginPassword != "")
	return nil
}

func isOriginAllowed(origin string) bool {
	origin = strings.TrimRight(origin, "/")
	for _, o := range allowedOrigins {
		if o == origin {
			return true
		}
	}
	return false
}

// corsMiddleware: echo Origin if allowed, allow credentials.
func corsMiddleware(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && isOriginAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if oauthCfg == nil {
		http.Error(w, "Login Google dinonaktifkan. Gunakan login email + password.", http.StatusNotFound)
		return
	}
	stateBytes := make([]byte, 16)
	_, _ = rand.Read(stateBytes)
	state := hex.EncodeToString(stateBytes)

	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    state,
		Path:     "/",
		MaxAge:   300,
		HttpOnly: true,
		SameSite: cookieSameSite(r),
		Secure:   isSecureRequest(r),
	})

	url := oauthCfg.AuthCodeURL(state, oauth2.AccessTypeOnline, oauth2.SetAuthURLParam("hd", allowedDomain), oauth2.SetAuthURLParam("prompt", "select_account"))
	http.Redirect(w, r, url, http.StatusFound)
}

func handleCallback(w http.ResponseWriter, r *http.Request) {
	if oauthCfg == nil {
		renderAuthError(w, "Login Google dinonaktifkan. Gunakan login email + password.")
		return
	}
	q := r.URL.Query()
	if errParam := q.Get("error"); errParam != "" {
		renderAuthError(w, "Login dibatalkan: "+errParam)
		return
	}

	stateQ := q.Get("state")
	stateC, err := r.Cookie(stateCookie)
	if err != nil || stateC.Value == "" || stateC.Value != stateQ {
		renderAuthError(w, "State OAuth tidak valid. Coba login ulang.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/", MaxAge: -1})

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	tok, err := oauthCfg.Exchange(ctx, q.Get("code"))
	if err != nil {
		renderAuthError(w, "Gagal exchange token: "+err.Error())
		return
	}

	client := oauthCfg.Client(ctx, tok)
	resp, err := client.Get("https://openidconnect.googleapis.com/v1/userinfo")
	if err != nil {
		renderAuthError(w, "Gagal ambil profile: "+err.Error())
		return
	}
	defer resp.Body.Close()

	var profile struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
		HD            string `json:"hd"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		renderAuthError(w, "Gagal parse profile: "+err.Error())
		return
	}

	if !profile.EmailVerified {
		renderAuthError(w, "Email Google belum verified.")
		return
	}
	if !strings.HasSuffix(strings.ToLower(profile.Email), "@"+allowedDomain) {
		renderAuthError(w, fmt.Sprintf("Akses ditolak. Hanya email @%s yang diizinkan. Akun Anda: %s", allowedDomain, profile.Email))
		return
	}

	user := SessionUser{
		Email:    profile.Email,
		Name:     profile.Name,
		Picture:  profile.Picture,
		IssuedAt: time.Now().Unix(),
	}
	if err := setSessionCookie(w, r, user); err != nil {
		renderAuthError(w, "Gagal set session: "+err.Error())
		return
	}
	log.Println("login:", profile.Email)
	dest := frontendURL
	if dest == "" {
		dest = "/"
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// handlePasswordLogin — login non-OAuth (email + password bersama). Issue session yang
// SAMA dengan OAuth sehingga semua endpoint & audit log (atas nama email) jalan normal.
func handlePasswordLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if appLoginPassword == "" {
		httpErr(w, 403, "Login password belum diaktifkan. Set APP_LOGIN_PASSWORD di .env lalu restart backend.")
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		if err := r.ParseForm(); err != nil {
			httpErr(w, 400, "form: %v", err)
			return
		}
	}
	email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
	pass := r.FormValue("password")
	name := strings.TrimSpace(r.FormValue("name"))
	if email == "" || pass == "" {
		httpErr(w, 400, "email & password wajib")
		return
	}
	// constant-time compare biar tidak bocor lewat timing
	if subtle.ConstantTimeCompare([]byte(pass), []byte(appLoginPassword)) != 1 {
		time.Sleep(500 * time.Millisecond) // sedikit perlambat brute force
		httpErr(w, 401, "Email atau password salah.")
		return
	}
	if !emailAllowedForPasswordLogin(email) {
		httpErr(w, 403, "Email tidak diizinkan untuk login.")
		return
	}
	if name == "" {
		name = email
	}
	user := SessionUser{Email: email, Name: name, IssuedAt: time.Now().Unix()}
	if err := setSessionCookie(w, r, user); err != nil {
		httpErr(w, 500, "set session: %v", err)
		return
	}
	log.Println("login (password):", email)
	writeJSON(w, map[string]any{"ok": true})
}

func emailAllowedForPasswordLogin(email string) bool {
	if len(appLoginEmails) > 0 {
		for _, e := range appLoginEmails {
			if e == email {
				return true
			}
		}
		return false
	}
	return strings.HasSuffix(email, "@"+allowedDomain)
}

func handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: cookieSameSite(r),
		Secure:   isSecureRequest(r),
	})
	if r.Method == http.MethodPost {
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	dest := frontendURL
	if dest == "" {
		dest = "/login.html"
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

func handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r)
	if !ok {
		writeJSON(w, map[string]any{"user": nil})
		return
	}
	writeJSON(w, map[string]any{"user": user})
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, u SessionUser) error {
	payload, err := json.Marshal(u)
	if err != nil {
		return err
	}
	p := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, sessionSecret)
	mac.Write([]byte(p))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	value := p + "." + sig

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: cookieSameSite(r),
		Secure:   isSecureRequest(r),
	})
	return nil
}

func userFrom(r *http.Request) (SessionUser, bool) {
	var u SessionUser
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return u, false
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 {
		return u, false
	}
	mac := hmac.New(sha256.New, sessionSecret)
	mac.Write([]byte(parts[0]))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(parts[1])) {
		return u, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return u, false
	}
	if err := json.Unmarshal(payload, &u); err != nil {
		return u, false
	}
	if time.Since(time.Unix(u.IssuedAt, 0)) > sessionTTL {
		return u, false
	}
	return u, true
}

// requireAuth wraps API handlers — JSON 401 if not logged in.
func requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := userFrom(r)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "unauthorized"})
			return
		}
		ctx := context.WithValue(r.Context(), ctxUserKey{}, u)
		h(w, r.WithContext(ctx))
	}
}

// requirePage wraps page handlers — redirect to /login if not logged in.
func requirePage(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := userFrom(r); !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		h(w, r)
	}
}

type ctxUserKey struct{}

func userFromCtx(ctx context.Context) (SessionUser, bool) {
	u, ok := ctx.Value(ctxUserKey{}).(SessionUser)
	return u, ok
}

func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if r.Header.Get("X-Forwarded-Proto") == "https" {
		return true
	}
	// Cloudflare Quick Tunnel sets Cf-Visitor instead of X-Forwarded-Proto
	if strings.Contains(r.Header.Get("Cf-Visitor"), `"scheme":"https"`) {
		return true
	}
	// Cloudflare also sets Cf-Connecting-Ip on all tunneled requests — kalau ada
	// header ini, request pasti datang via Cloudflare yang selalu HTTPS dari sisi client.
	if r.Header.Get("Cf-Connecting-Ip") != "" {
		return true
	}
	return false
}

// cookieSameSite: None saat HTTPS (perlu untuk cross-site dari GitHub Pages),
// Lax kalau lokal HTTP supaya tetap jalan untuk dev.
func cookieSameSite(r *http.Request) http.SameSite {
	if isSecureRequest(r) {
		return http.SameSiteNoneMode
	}
	return http.SameSiteLaxMode
}

func renderAuthError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Login error</title>
<style>body{font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#0b1220;color:#e6ecff;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}
.box{background:#121a2b;border:1px solid #223;border-radius:10px;padding:32px;max-width:480px;text-align:center}
h1{font-size:18px;margin:0 0 16px}.msg{color:#fca5a5;margin-bottom:20px}a{color:#2DBDB6}</style></head><body>
<div class="box"><h1>Login gagal</h1><p class="msg">%s</p><a href="/login.html">Coba lagi</a></div></body></html>`, msg)
}
