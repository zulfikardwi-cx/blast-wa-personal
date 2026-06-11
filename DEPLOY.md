# Deploy Guide — Step by Step

Total waktu: **60–90 menit** untuk first-time. Subsequent updates: **5 menit** (git push, frontend auto-deploy).

Urutan langkah penting — **jangan loncat**, karena URL OAuth callback dan CORS origins saling tergantung.

---

## Overview urutan setup

```
1. Persiapan tools           (10 menit)
2. Buat repo GitHub           (5 menit)
3. Setup Google OAuth         (15 menit)
4. Setup Cloudflare Tunnel    (15 menit)
5. Isi .env & test backend    (5 menit)
6. Enable GitHub Pages        (5 menit)
7. Edit config.js & push      (5 menit)
8. Smoke test end-to-end      (5 menit)
```

---

## 1. Persiapan tools (10 menit)

### 1.1. Cek prerequisites

```bash
go version          # harus go1.22+
brew --version
git --version
gh --version        # GitHub CLI — install kalau belum: brew install gh
```

Install yang missing:
```bash
brew install go git gh cloudflared
```

### 1.2. Generate session secret (catat output-nya, dipakai di step 5)

```bash
openssl rand -hex 32
```

Contoh output: `a1b2c3d4e5f6...9z0a` (64 char hex).

### 1.3. Login GitHub CLI

```bash
gh auth login
```

Pilih: GitHub.com → HTTPS → Login with web browser → ikuti instruksi.

Verify:
```bash
gh auth status
```

---

## 2. Buat repo GitHub (5 menit)

### 2.1. Masuk ke folder project

```bash
cd "/Users/m1/Documents/Claude/Projects/Blast WA Personal"
```

### 2.2. Verifikasi tidak ada secret yang akan ke-commit

```bash
cat .gitignore     # pastikan ada: .env, session/, *.db
ls -la .env        # file ini ADA (dari setup sebelumnya) tapi WAJIB ignored
```

### 2.3. Init git + first commit

```bash
git init
git branch -m main
git add .gitignore .env.example README.md DEPLOY.md
git add go.mod go.sum main.go auth.go audit.go blast.go
git add docs/ static/ sample.csv
git status                                # double-check: .env, session/, *.db TIDAK ada
git commit -m "Initial commit: blast WA personal with GitHub Pages + OAuth"
```

**Penting**: cek `git status` sebelum commit — kalau `.env` atau `session/store.db` muncul di staging, **STOP** dan benarkan gitignore.

### 2.4. Create repo di GitHub & push

```bash
gh repo create blast-wa-personal --public --source=. --remote=origin --push
```

Output akan tampilkan URL repo, mis. `https://github.com/zulfikardwnsyh19/blast-wa-personal`.

**Catat USERNAME GitHub Anda** (mis. `zulfikardwnsyh19`) — dipakai di step 3 untuk URL Pages.

---

## 3. Setup Google OAuth (15 menit)

### 3.1. Buka GCP Console

https://console.cloud.google.com/ → login pakai akun majoo atau pribadi.

### 3.2. Create project

Pojok kiri atas → klik dropdown project → **NEW PROJECT**.
- Name: `blast-wa-personal`
- Organization: pilih `majoo.id` kalau ada akses, atau skip
- **CREATE** → tunggu ~10 detik → **pilih project** baru di dropdown

### 3.3. OAuth consent screen

Sidebar → **APIs & Services** → **OAuth consent screen**.

- **User Type**: pilih **Internal** (kalau project di org majoo) atau **External** (kalau personal)
- **CREATE**

Isi form:

| Field | Value |
|---|---|
| App name | `Blast WA Personal` |
| User support email | email Anda |
| Authorized domains | klik +ADD DOMAIN → `majoo.id` |
| Developer contact email | email Anda |

**SAVE AND CONTINUE**.

**Scopes**: klik **ADD OR REMOVE SCOPES** → centang `openid`, `userinfo.email`, `userinfo.profile` → **UPDATE** → **SAVE AND CONTINUE**.

**Test users** (kalau pilih External): tambah email Anda + 2-3 tim untuk testing → **SAVE**.

**BACK TO DASHBOARD**.

### 3.4. Create OAuth Client ID

Sidebar → **Credentials** → **+ CREATE CREDENTIALS** → **OAuth client ID**.

- **Application type**: Web application
- **Name**: `Blast WA Personal — Web`
- **Authorized redirect URIs** → klik **+ ADD URI** dan tambah **DUA** URI:
  ```
  http://localhost:8080/auth/callback
  https://blast-wa-api.majoo.id/auth/callback
  ```

**CREATE**.

### 3.5. Catat credentials

Popup tampilkan **Client ID** + **Client Secret**. **CATAT KEDUANYA** — secret tidak bisa dilihat ulang.

---

## 4. Setup Cloudflare Tunnel (15 menit)

> **Prasyarat**: Anda atau tim infra harus punya akses dashboard Cloudflare untuk domain `majoo.id`. Kalau tidak, koordinasi dulu.

### 4.1. Login cloudflared

```bash
cloudflared tunnel login
```

Browser kebuka → pilih domain `majoo.id` → **Authorize**. Cert tersimpan di `~/.cloudflared/cert.pem`.

### 4.2. Create tunnel

```bash
cloudflared tunnel create blast-wa
```

Output: `Created tunnel blast-wa with id <UUID>` dan path credentials JSON. **Catat UUID**.

### 4.3. Buat config tunnel

```bash
nano ~/.cloudflared/config.yml
```

Isi (ganti `<UUID>` dengan yang dari step 4.2):

```yaml
tunnel: <UUID>
credentials-file: /Users/m1/.cloudflared/<UUID>.json

ingress:
  - hostname: blast-wa-api.majoo.id
    service: http://localhost:8080
  - service: http_status:404
```

Save: `Ctrl+O` → Enter → `Ctrl+X`.

### 4.4. Route DNS

```bash
cloudflared tunnel route dns blast-wa blast-wa-api.majoo.id
```

Verifikasi di Cloudflare dashboard → DNS → CNAME `blast-wa-api` ke `<UUID>.cfargotunnel.com` muncul.

---

## 5. Isi `.env` & test backend (5 menit)

### 5.1. Copy & edit `.env`

```bash
cd "/Users/m1/Documents/Claude/Projects/Blast WA Personal"
cp .env.example .env
nano .env
```

Isi (ganti dengan nilai dari step sebelumnya — `<USERNAME>` = GitHub username Anda dari step 2.4):

```bash
GOOGLE_CLIENT_ID=<dari step 3.5>
GOOGLE_CLIENT_SECRET=<dari step 3.5>
OAUTH_REDIRECT_URL=https://blast-wa-api.majoo.id/auth/callback
SESSION_SECRET=<dari step 1.2>
FRONTEND_URL=https://<USERNAME>.github.io/blast-wa-personal
ALLOWED_ORIGINS=https://<USERNAME>.github.io
```

Save & tutup.

### 5.2. Build & jalankan

Tab terminal 1 — backend:
```bash
go build -o blast-wa-personal ./...
./blast-wa-personal
```

Tab terminal 2 — tunnel:
```bash
cloudflared tunnel run blast-wa
```

### 5.3. Smoke test backend

```bash
curl -i https://blast-wa-api.majoo.id/api/me
# Harus return: 200 OK, body {"user":null}
```

Kalau dapat:
- **Connection refused** → tunnel tidak jalan / backend mati
- **Error 1033** → tunnel belum stable, tunggu 30 detik
- **404 / SSL error** → DNS belum propagate, tunggu 1-2 menit

---

## 6. Enable GitHub Pages (5 menit)

### 6.1. Buka repo settings

Browser → `https://github.com/<USERNAME>/blast-wa-personal/settings/pages`.

### 6.2. Konfigurasi source

- **Source**: Deploy from a branch
- **Branch**: `main` / `/docs`
- **SAVE**

GitHub akan build & deploy. Tunggu ~30-60 detik.

### 6.3. Cek URL Pages

Refresh halaman → muncul banner hijau:
> Your site is live at `https://<USERNAME>.github.io/blast-wa-personal/`

Buka URL itu di browser. **Halaman akan blank dulu** atau redirect ke login — wajar, karena `config.js` masih point ke URL placeholder. Lanjut step 7.

---

## 7. Edit `docs/config.js` & push (5 menit)

### 7.1. Edit config.js

```bash
nano docs/config.js
```

Pastikan `API_BASE` benar (sesuai backend Cloudflare Tunnel):

```js
window.APP_CONFIG = {
  API_BASE: "https://blast-wa-api.majoo.id",
};
```

Save.

### 7.2. Commit & push

```bash
git add docs/config.js
git commit -m "Set API_BASE to production backend"
git push
```

GitHub Pages otomatis rebuild dalam ~30-60 detik.

---

## 8. Smoke test end-to-end (5 menit)

### 8.1. Buka URL frontend

`https://<USERNAME>.github.io/blast-wa-personal/`

- Harus redirect otomatis ke `login.html`
- Logo majoo muncul
- Tombol "Sign in with Google" enabled

### 8.2. Test login

Klik **Sign in with Google** → pilih akun `@majoo.id` → consent.

Flow yang terjadi:
1. Browser → `https://blast-wa-api.majoo.id/auth/login`
2. Backend redirect ke Google
3. Google → `https://blast-wa-api.majoo.id/auth/callback?code=...`
4. Backend set cookie + redirect ke `FRONTEND_URL`
5. Browser landing di `https://<USERNAME>.github.io/blast-wa-personal/` dengan cookie aktif
6. `index.html` → fetch `/api/me` (cross-site dengan credentials) → tampilkan user

Kalau ada error:

| Gejala | Solusi |
|---|---|
| Tombol login tidak respond | `config.js` belum di-push / Pages belum rebuild. Hard refresh (Cmd+Shift+R). |
| Google "redirect_uri_mismatch" | URI di GCP (step 3.4) tidak persis sama dengan `OAUTH_REDIRECT_URL` di `.env`. |
| "Akses ditolak. Hanya email @majoo.id" | Login pakai email selain majoo.id. |
| Redirect ke Pages tapi `/api/me` return user:null | Cookie tidak ter-set cross-site. Cek: backend HTTPS aktif (cookie Secure butuh HTTPS), `ALLOWED_ORIGINS` cover Pages URL. |
| `/api/me` CORS error di console | Origin Anda tidak match `ALLOWED_ORIGINS`. Cek URL exact (https vs http, trailing slash). |
| `/api/status` 401 di console | Cookie tidak terkirim. Browser block third-party cookies → cek setting browser, atau pakai browser yang allow cross-site cookies. |

### 8.3. Pairing WhatsApp & blast test

1. Setelah login, scan QR di card "Koneksi WhatsApp" (Linked Devices di HP)
2. Upload `sample.csv` (sudah ada di repo)
3. **Edit row pertama** dengan nomor Anda sendiri
4. Klik **Mulai Blast** → verify pesan masuk WA Anda
5. Cek tab **Riwayat Blast** → entry muncul dengan nama + email Anda

---

## Operasional sehari-hari

### Update kode frontend

```bash
# edit file di docs/
git add docs/
git commit -m "Update UI"
git push
# Pages auto-rebuild ~30 detik
```

### Update kode backend

```bash
# edit *.go
go build -o blast-wa-personal ./...
# stop server (Ctrl+C di terminal 1), jalankan ulang:
./blast-wa-personal
```

### Start sistem (setelah Mac restart)

Tab 1:
```bash
cd "/Users/m1/Documents/Claude/Projects/Blast WA Personal" && ./blast-wa-personal
```

Tab 2:
```bash
cloudflared tunnel run blast-wa
```

Atau install sebagai service biar auto-start:
```bash
sudo cloudflared service install
```

### Share URL ke team

Kirim ke Slack/grup WA:
> **Blast WA Personal**
> URL: https://<USERNAME>.github.io/blast-wa-personal/
> Login: akun Google @majoo.id Anda
> Audit log: tab "Riwayat Blast"

---

## Checklist produksi

- [ ] Repo di-tag release pertama (`git tag v1.0.0 && git push --tags`)
- [ ] `.env` di-backup ke 1Password / vault majoo (kalau hilang, semua user logout paksa + ribet re-pair OAuth)
- [ ] OAuth consent screen status: **In production** (kalau External), atau **Internal** (kalau org)
- [ ] Nomor WA sender dedicated (bukan personal admin)
- [ ] Slot Linked Device WA cukup (max 4 per nomor — koordinasi dengan service WA majoo lain)
- [ ] PIC pantau HP sender untuk handle reply
- [ ] Sign-off legal/compliance untuk PDP UU 27/2022
- [ ] Test 2-3 nomor (termasuk diri sendiri) sebelum batch besar
- [ ] Setup `launchd` agar app auto-start saat Mac booting
- [ ] (Opsional) UptimeRobot monitor `https://blast-wa-api.majoo.id/api/me`

---

## Troubleshooting matrix

| Layer | Symptom | Check |
|---|---|---|
| GitHub Pages | URL 404 | Settings → Pages → Source = main /docs |
| Pages cache | Update tidak muncul | Hard refresh (Cmd+Shift+R), tunggu 60s deploy |
| Backend tunnel | "Bad gateway" | Tab 1 (`./blast-wa-personal`) jalan? |
| Backend tunnel | Cloudflare 1033 | Tab 2 (`cloudflared tunnel run`) jalan? |
| OAuth | redirect_uri_mismatch | GCP redirect URI ↔ `.env` `OAUTH_REDIRECT_URL` identik? |
| OAuth | App not verified | External + production butuh verification Google; pakai Internal untuk org majoo |
| CORS | Browser console CORS error | `ALLOWED_ORIGINS` cover origin exact (https/http, no trailing slash) |
| Cookie | Login bouncing back to /login | Cookie tidak ter-set: cek HTTPS aktif, SameSite=None+Secure (auto saat HTTPS) |
| WA | "nomor tidak terdaftar" | Cek format CSV — phone harus nomor WA aktif |
| WA | Logout otomatis dari WA | Banned/limit. Turunkan rate, variasikan template, ganti sender |

---

## Architecture flow (reference)

```
[User browser]
     │
     │ (1) GET https://<USER>.github.io/blast-wa-personal/
     ↓
[GitHub Pages CDN]
     │ serve docs/index.html + config.js
     ↓
[Browser executes index.html]
     │ (2) fetch https://blast-wa-api.majoo.id/api/me (credentials: include)
     ↓
[Cloudflare Tunnel]
     │ proxy ke localhost:8080
     ↓
[Mac: blast-wa-personal]
     │ check cookie HMAC-signed
     │ if invalid → 401 → frontend redirect ke login.html
     │ if valid → return user → render app
     ↓
[User klik "Mulai Blast"]
     │ POST /api/blast (multipart) ke backend
     ↓
[Backend: whatsmeow]
     │ kirim WA satu per satu dengan jitter
     │ tulis ke session/audit.db
     ↓
[Frontend poll /api/progress tiap 1.5 detik]
```
