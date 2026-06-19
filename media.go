package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
)

const mediaDir = "session/media"

// isDownloadableMedia — tipe media yang byte-nya bisa diunduh dari WA.
// location/contact/unknown TIDAK termasuk (bukan file).
func isDownloadableMedia(mt string) bool {
	switch mt {
	case "image", "video", "audio", "sticker", "document":
		return true
	}
	return false
}

// mediaToken — token tak-bisa-ditebak untuk akses media TANPA cookie (supaya <img>/<video>
// jalan lintas-domain dari GitHub Pages). HMAC(message_id) dengan SESSION_SECRET.
func mediaToken(msgID int64) string {
	mac := hmac.New(sha256.New, sessionSecret)
	mac.Write([]byte("media:" + strconv.FormatInt(msgID, 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func mediaTokenValid(msgID int64, tok string) bool {
	if tok == "" {
		return false
	}
	return hmac.Equal([]byte(tok), []byte(mediaToken(msgID)))
}

// mediaURLPath — path relatif (frontend prepend API_BASE) untuk pesan media. "" kalau bukan media.
func mediaURLPath(msgID int64) string {
	return "/api/inbox/media?id=" + strconv.FormatInt(msgID, 10) + "&t=" + mediaToken(msgID)
}

func extForMediaType(mt string) string {
	switch mt {
	case "image":
		return ".jpg"
	case "video":
		return ".mp4"
	case "audio":
		return ".ogg"
	case "sticker":
		return ".webp"
	default:
		return ".bin"
	}
}

func contentTypeForMedia(mt string) string {
	switch mt {
	case "image":
		return "image/jpeg"
	case "video":
		return "video/mp4"
	case "audio":
		return "audio/ogg"
	case "sticker":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// sanitizeFilename — hanya alnum, '-', '_' (cegah path traversal dari wa_message_id).
func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteRune(c)
		}
	}
	out := b.String()
	if out == "" {
		out = "media"
	}
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

// downloadAndStoreMedia — unduh byte media dari WA, simpan ke disk, lalu update kolom
// media_path pesan. Dipanggil ASYNC (goroutine) dari handleIncomingWA supaya event handler
// tidak ke-blok unduhan. Pesan sudah tercatat (placeholder) sebelum ini, jadi muncul dulu;
// media menyusul saat fetch berikutnya.
func downloadAndStoreMedia(waMsgID string, msg *waProto.Message, mediaType string) {
	if state.client == nil || msg == nil || waMsgID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	data, err := state.client.DownloadAny(ctx, msg)
	if err != nil {
		log.Printf("media: download gagal id=%s type=%s: %v", waMsgID, mediaType, err)
		return
	}
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		log.Printf("media: mkdir %s: %v", mediaDir, err)
		return
	}
	fpath := filepath.Join(mediaDir, sanitizeFilename(waMsgID)+extForMediaType(mediaType))
	if err := os.WriteFile(fpath, data, 0o644); err != nil {
		log.Printf("media: write %s: %v", fpath, err)
		return
	}
	if _, err := auditDB.Exec(`UPDATE chat_messages SET media_path = ? WHERE wa_message_id = ?`, fpath, waMsgID); err != nil {
		log.Printf("media: update media_path id=%s: %v", waMsgID, err)
		return
	}
	log.Printf("media: tersimpan id=%s type=%s (%d bytes) → %s", waMsgID, mediaType, len(data), fpath)
}

// handleInboxMedia — serve file media. TANPA requireAuth (cross-origin <img>/<video>):
// diproteksi token HMAC di query (?id=&t=). Dukung Range request (seek video) via ServeContent.
func handleInboxMedia(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || !mediaTokenValid(id, r.URL.Query().Get("t")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var mediaPath, mediaType, body string
	err = auditDB.QueryRow(`SELECT COALESCE(media_path,''), COALESCE(media_type,''), COALESCE(body,'') FROM chat_messages WHERE id = ?`, id).
		Scan(&mediaPath, &mediaType, &body)
	if err != nil || mediaPath == "" {
		http.Error(w, "media belum tersedia", http.StatusNotFound)
		return
	}
	f, err := os.Open(mediaPath)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "stat error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentTypeForMedia(mediaType))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	if mediaType == "document" {
		// "[Dokumen] namafile.pdf" → ambil nama file utk download
		name := strings.TrimSpace(strings.TrimPrefix(body, "[Dokumen]"))
		if name == "" {
			name = "dokumen"
		}
		w.Header().Set("Content-Disposition", "inline; filename=\""+sanitizeFilename(name)+"\"")
	}
	http.ServeContent(w, r, filepath.Base(mediaPath), fi.ModTime(), f)
}
