package main

// ============================================================================
// ZOPOZ — second WhatsApp line, fully isolated from the primary blast/inbox.
//
// Design constraint: NOTHING here touches the existing `state`/`client`,
// `chat_*` tables, or any running handler. It is purely additive:
//   - its own whatsmeow client + session store (session/store-zopoz.db)
//   - its own connection state (zopozState)
//   - its own tables (zopoz_threads / zopoz_messages / zopoz_blast_*)
//   - its own /api/zopoz/* endpoints + media dir (session/media-zopoz)
// Only pure, stateless helpers from the existing code are reused
// (normalizePhone, parseCSV, unwrapMessage, extractTextFromMessage, truncate,
// auth middleware, GetAttemptTemplate, renderClosingTemplate, …).
// ============================================================================

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
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/mdp/qrterminal/v3"
)

type zopozAppState struct {
	mu        sync.RWMutex
	client    *whatsmeow.Client
	qrCode    string
	loggedIn  bool
	connected bool

	job *BlastJob // current/last Zopoz blast job
}

var zopozState = &zopozAppState{}

// guard supaya auto re-pair Zopoz tidak jalan dobel (mirror repairing guard primary)
var (
	zopozRepairMu  sync.Mutex
	zopozRepairing bool
)

func (s *zopozAppState) setQR(code string)   { s.mu.Lock(); s.qrCode = code; s.mu.Unlock() }
func (s *zopozAppState) setLoggedIn(v bool)  { s.mu.Lock(); s.loggedIn = v; s.mu.Unlock() }
func (s *zopozAppState) setConnected(v bool) { s.mu.Lock(); s.connected = v; s.mu.Unlock() }
func (s *zopozAppState) snapshot() (string, bool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.qrCode, s.loggedIn, s.connected
}

// initZopozClient — buat client WA kedua dengan session store TERPISAH. Dipanggil dari
// main() setelah client primary siap. Non-fatal: kalau gagal, log saja — fitur Zopoz mati
// tapi blast/inbox utama tetap jalan.
func initZopozClient(rootCtx context.Context) error {
	dbLog := waLog.Stdout("ZopozDB", "WARN", true)
	container, err := sqlstore.New(rootCtx, "sqlite3", "file:session/store-zopoz.db?_foreign_keys=on", dbLog)
	if err != nil {
		return err
	}
	deviceStore, err := container.GetFirstDevice(rootCtx)
	if err != nil {
		return err
	}

	clientLog := waLog.Stdout("ZopozClient", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)
	zopozState.client = client

	client.AddEventHandler(func(evt interface{}) {
		switch e := evt.(type) {
		case *events.Connected:
			zopozState.setConnected(true)
			zopozState.setQR("")
			log.Println("zopoz: connected")
		case *events.Disconnected:
			zopozState.setConnected(false)
			log.Println("zopoz: disconnected")
		case *events.LoggedOut:
			zopozState.setLoggedIn(false)
			zopozState.setConnected(false)
			zopozState.setQR("")
			log.Println("zopoz: logged out — auto re-pair: QR baru akan muncul (scan dari HP nomor Zopoz)")
			go zopozRepairAfterLogout(client)
		case *events.Message:
			zopozHandleIncoming(e)
		}
	})

	go zopozConnectAndPair(client)
	return nil
}

func zopozRepairAfterLogout(client *whatsmeow.Client) {
	zopozRepairMu.Lock()
	if zopozRepairing {
		zopozRepairMu.Unlock()
		return
	}
	zopozRepairing = true
	zopozRepairMu.Unlock()
	defer func() {
		zopozRepairMu.Lock()
		zopozRepairing = false
		zopozRepairMu.Unlock()
	}()

	client.Disconnect()
	time.Sleep(2 * time.Second)
	zopozConnectAndPair(client)
}

func zopozConnectAndPair(client *whatsmeow.Client) {
	if client.Store.ID == nil {
		qrChan, err := client.GetQRChannel(context.Background())
		if err != nil {
			log.Printf("zopoz QR channel: %v", err)
			return
		}
		if err := client.Connect(); err != nil {
			log.Printf("zopoz connect: %v", err)
			return
		}
		for evt := range qrChan {
			switch evt.Event {
			case "code":
				zopozState.setQR(evt.Code)
				log.Println("zopoz: QR code updated — buka halaman Zopoz Blast atau scan terminal")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			case "success":
				zopozState.setQR("")
				zopozState.setLoggedIn(true)
				log.Println("zopoz: pairing success")
			case "timeout":
				log.Println("zopoz: QR timeout — restart untuk retry")
			}
		}
	} else {
		zopozState.setLoggedIn(true)
		if err := client.Connect(); err != nil {
			log.Printf("zopoz connect: %v", err)
		}
	}
}

// ---- WA connection handlers (separate namespace dari /api/status global) ----

func zopozHandleStatus(w http.ResponseWriter, r *http.Request) {
	qr, loggedIn, connected := zopozState.snapshot()
	writeJSON(w, map[string]any{
		"loggedIn":  loggedIn,
		"connected": connected,
		"hasQR":     qr != "",
	})
}

func zopozHandleQR(w http.ResponseWriter, r *http.Request) {
	qr, _, _ := zopozState.snapshot()
	writeJSON(w, map[string]any{"code": qr})
}

func zopozHandleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if zopozState.client == nil {
		httpErr(w, 400, "Zopoz client belum siap")
		return
	}
	if err := zopozState.client.Logout(r.Context()); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	zopozState.setLoggedIn(false)
	writeJSON(w, map[string]any{"ok": true})
}

// ---- incoming message → zopoz_threads / zopoz_messages ----

// zopozResolveSenderPhone — sama seperti resolveSenderPhone tapi pakai client Zopoz utk
// LID lookup (store LID-nya beda device).
func zopozResolveSenderPhone(info types.MessageInfo) string {
	if info.Sender.Server == types.DefaultUserServer && info.Sender.User != "" {
		return info.Sender.User
	}
	if info.SenderAlt.Server == types.DefaultUserServer && info.SenderAlt.User != "" {
		return info.SenderAlt.User
	}
	if info.Sender.Server == types.HiddenUserServer && zopozState.client != nil && zopozState.client.Store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if lids := zopozState.client.Store.LIDs; lids != nil {
			if pnJID, err := lids.GetPNForLID(ctx, info.Sender); err == nil && pnJID.User != "" {
				return pnJID.User
			}
		}
	}
	return ""
}

func zopozHandleIncoming(e *events.Message) {
	if e.Info.IsFromMe || e.Info.IsGroup {
		return
	}
	phone := zopozResolveSenderPhone(e.Info)
	if phone == "" {
		log.Printf("zopoz: skip incoming — tidak bisa resolve phone (sender=%s alt=%s)", e.Info.Sender.String(), e.Info.SenderAlt.String())
		return
	}
	if !zopozIsPhoneBlasted(phone) {
		log.Println("zopoz: skip incoming —", phone, "tidak ada di zopoz_threads (belum pernah di-blast Zopoz)")
		return
	}
	eff := unwrapMessage(e.Message)
	body, mediaType := extractTextFromMessage(eff)
	if mediaType == "unknown" || (body == "" && mediaType == "") {
		return // pesan kontrol/protokol — skip
	}
	if err := zopozRecordChatMessage(phone, "in", body, mediaType, e.Info.ID, e.Info.Timestamp, 0, "", ""); err != nil {
		log.Println("zopoz warn: recordChatMessage incoming:", err)
	}
	if err := zopozUpsertThreadIncoming(phone, body, e.Info.Timestamp); err != nil {
		log.Println("zopoz warn: upsertThreadIncoming:", err)
	}
	if isDownloadableMedia(mediaType) {
		go zopozDownloadAndStoreMedia(e.Info.ID, eff, mediaType)
	}
	log.Println("zopoz inbox: incoming from", phone, "—", truncate(body, 40))
}

// ---- media (dir + token TERPISAH dari inbox; id-space zopoz_messages beda) ----

const zopozMediaDir = "session/media-zopoz"

func zopozMediaToken(msgID int64) string {
	mac := hmac.New(sha256.New, sessionSecret)
	mac.Write([]byte("zmedia:" + strconv.FormatInt(msgID, 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func zopozMediaTokenValid(msgID int64, tok string) bool {
	if tok == "" {
		return false
	}
	return hmac.Equal([]byte(tok), []byte(zopozMediaToken(msgID)))
}

func zopozMediaURLPath(msgID int64) string {
	return "/api/zopoz/media?id=" + strconv.FormatInt(msgID, 10) + "&t=" + zopozMediaToken(msgID)
}

func zopozDownloadAndStoreMedia(waMsgID string, msg *waProto.Message, mediaType string) {
	if zopozState.client == nil || msg == nil || waMsgID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	data, err := zopozState.client.DownloadAny(ctx, msg)
	if err != nil {
		log.Printf("zopoz media: download gagal id=%s type=%s: %v", waMsgID, mediaType, err)
		return
	}
	if err := os.MkdirAll(zopozMediaDir, 0o755); err != nil {
		log.Printf("zopoz media: mkdir %s: %v", zopozMediaDir, err)
		return
	}
	fpath := filepath.Join(zopozMediaDir, sanitizeFilename(waMsgID)+extForMediaType(mediaType))
	if err := os.WriteFile(fpath, data, 0o644); err != nil {
		log.Printf("zopoz media: write %s: %v", fpath, err)
		return
	}
	if _, err := auditDB.Exec(`UPDATE zopoz_messages SET media_path = ? WHERE wa_message_id = ?`, fpath, waMsgID); err != nil {
		log.Printf("zopoz media: update media_path id=%s: %v", waMsgID, err)
		return
	}
	log.Printf("zopoz media: tersimpan id=%s type=%s (%d bytes) → %s", waMsgID, mediaType, len(data), fpath)
}

func zopozHandleMedia(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || !zopozMediaTokenValid(id, r.URL.Query().Get("t")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var mediaPath, mediaType, body string
	err = auditDB.QueryRow(`SELECT COALESCE(media_path,''), COALESCE(media_type,''), COALESCE(body,'') FROM zopoz_messages WHERE id = ?`, id).
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
		name := strings.TrimSpace(strings.TrimPrefix(body, "[Dokumen]"))
		if name == "" {
			name = "dokumen"
		}
		w.Header().Set("Content-Disposition", "inline; filename=\""+sanitizeFilename(name)+"\"")
	}
	http.ServeContent(w, r, filepath.Base(mediaPath), fi.ModTime(), f)
}
