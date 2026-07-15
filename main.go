package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/mdp/qrterminal/v3"
)

type appState struct {
	mu        sync.RWMutex
	client    *whatsmeow.Client
	qrCode    string // last QR string (empty when logged in)
	loggedIn  bool
	connected bool

	job *BlastJob // current/last blast job
}

var state = &appState{}

// guard supaya auto re-pair (repairAfterLogout) tidak jalan dobel
var (
	repairMu  sync.Mutex
	repairing bool
)

func main() {
	loadDotEnv()
	if err := initAuth(); err != nil {
		log.Fatalf("auth: %v", err)
	}
	if err := initAudit(); err != nil {
		log.Fatalf("audit: %v", err)
	}
	if err := initUsers(); err != nil {
		log.Fatalf("users: %v", err)
	}
	if err := initSheets(); err != nil {
		log.Fatalf("sheets: %v", err)
	}
	if err := initChat(); err != nil {
		log.Fatalf("chat: %v", err)
	}
	// Inbox CLM — layer assignment manual di atas Inbox (tabel clm_assignments).
	if err := initCLM(); err != nil {
		log.Fatalf("clm: %v", err)
	}
	// Zopoz (line WA kedua) — tabel terpisah di auditDB. Non-fatal kalau gagal.
	if err := initZopozChat(); err != nil {
		log.Fatalf("zopoz chat: %v", err)
	}
	if err := initZopozBlastAudit(); err != nil {
		log.Fatalf("zopoz blast audit: %v", err)
	}
	initZopozSheetName()
	if err := initExclusions(); err != nil {
		log.Fatalf("exclusions: %v", err)
	}
	if err := initResolvedInvoices(); err != nil {
		log.Fatalf("resolved invoices: %v", err)
	}
	// Token validasi (korelasi balasan INTI ↔ invoice yang di-blast dari BLASTER).
	if err := initTokens(); err != nil {
		log.Fatalf("tokens: %v", err)
	}
	closeStaleRunningBlasts()
	// Stempel attempt per-recipient (urutan kronologis kirim) — betulkan data lama yang
	// mislabel attempt supaya kolom Attempt di report Belum Respons & antrian akurat.
	backfillRecipientAttempts()
	// Self-heal: recipient 'sent' yang di-insert di luar runBlast (import/SQL manual) tak punya
	// thread → invisible di Inbox & di-drop dari antrian No Response. Buatkan threadnya di sini.
	// Jalan SETELAH initResolvedInvoices supaya klasifikasi after_blast/done akurat.
	if err := backfillMissingThreads(); err != nil {
		fmt.Println("warn: backfillMissingThreads:", err)
	}
	startRetryScheduler()
	startZopozRetryScheduler()

	rootCtx := context.Background()
	dbLog := waLog.Stdout("Database", "WARN", true)
	container, err := sqlstore.New(rootCtx, "sqlite3", "file:session/store.db?_foreign_keys=on", dbLog)
	if err != nil {
		log.Fatalf("sqlstore: %v", err)
	}
	deviceStore, err := container.GetFirstDevice(rootCtx)
	if err != nil {
		log.Fatalf("device: %v", err)
	}

	clientLog := waLog.Stdout("Client", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)
	state.client = client

	client.AddEventHandler(func(evt interface{}) {
		switch e := evt.(type) {
		case *events.Connected:
			state.setConnected(true)
			state.setQR("")
			log.Println("connected")
		case *events.Disconnected:
			state.setConnected(false)
			log.Println("disconnected")
		case *events.LoggedOut:
			state.setLoggedIn(false)
			state.setConnected(false)
			state.setQR("")
			log.Println("logged out — auto re-pair: QR baru akan muncul, scan dari HP (tanpa perlu restart)")
			go repairAfterLogout(client)
		case *events.Message:
			handleIncomingWA(e)
		case *events.UndecryptableMessage:
			log.Printf("event.UndecryptableMessage: from=%s id=%s — decrypt failed, mungkin re-pair", e.Info.Sender.String(), e.Info.ID)
		case *events.Receipt:
			// receipt (delivered/read) — too noisy untuk di-log, skip
		}
	})

	go connectAndPair(client)

	// Zopoz: client WA kedua dengan session store terpisah (session/store-zopoz.db).
	// Non-fatal — kalau gagal, blast/inbox utama tetap jalan.
	if err := initZopozClient(rootCtx); err != nil {
		log.Printf("zopoz client init gagal (fitur Zopoz mati, blast/inbox utama tetap jalan): %v", err)
	}

	// BLASTER: client WA disposable khusus pengirim blast (session/store-blaster.db).
	// Non-fatal — kalau gagal, Inbox INTI tetap jalan (cuma blast yang butuh scan QR blaster).
	if err := initBlasterClient(rootCtx); err != nil {
		log.Printf("blaster client init gagal (blast mati sampai QR blaster di-scan, Inbox INTI tetap jalan): %v", err)
	}

	mux := http.NewServeMux()

	// Public auth routes
	mux.HandleFunc("/auth/login", handleLogin)
	mux.HandleFunc("/auth/callback", handleCallback)
	mux.HandleFunc("/auth/logout", handleAuthLogout)
	mux.HandleFunc("/auth/password-login", corsMiddleware(handlePasswordLogin))
	mux.HandleFunc("/auth/change-password", corsMiddleware(requireAuth(handleChangePassword)))

	// Serve frontend (docs/) SAME-ORIGIN dari backend. Cookie session jadi first-party
	// -> jalan di semua browser. (Akses via GitHub Pages = beda domain -> cookie pihak
	// ketiga -> diblokir Safari/Firefox, cuma jalan di Chrome.) Auth di-handle client-side:
	// tiap halaman fetch /api/me lalu redirect ke login.html kalau belum login. Data tetap
	// aman karena semua endpoint /api dilindungi requireAuth.
	mux.Handle("/", http.FileServer(http.Dir("docs")))

	// API endpoints — CORS-enabled untuk dipanggil dari GitHub Pages
	mux.HandleFunc("/api/me", corsMiddleware(handleMe))
	mux.HandleFunc("/api/status", corsMiddleware(requireAuth(handleStatus)))
	mux.HandleFunc("/api/qr", corsMiddleware(requireAuth(handleQR)))
	mux.HandleFunc("/api/logout", corsMiddleware(requireAuth(handleLogout)))
	mux.HandleFunc("/api/blast", corsMiddleware(requireAuth(handleBlast)))
	mux.HandleFunc("/api/generate-links", corsMiddleware(requireAuth(handleGenerateLinks)))
	mux.HandleFunc("/api/belum-respons", corsMiddleware(requireAuth(handleBelumResponsStats)))
	mux.HandleFunc("/api/belum-respons/export", corsMiddleware(requireAuth(handleBelumResponsExport)))
	// PUBLIK (tanpa login): customer konfirmasi kode referensi dari halaman web.
	mux.HandleFunc("/api/konfirmasi", corsMiddleware(handleKonfirmasi))
	mux.HandleFunc("/api/konfirmasi-coba", corsMiddleware(handleKonfirmasiCoba))
	mux.HandleFunc("/api/progress", corsMiddleware(requireAuth(handleProgress)))
	mux.HandleFunc("/api/history", corsMiddleware(requireAuth(handleHistory)))
	mux.HandleFunc("/api/sheet-status", corsMiddleware(requireAuth(handleSheetStatus)))
	mux.HandleFunc("/api/export-sheet", corsMiddleware(requireAuth(handleExportSheet)))

	mux.HandleFunc("/api/templates", corsMiddleware(requireAuth(handleTemplates)))
	mux.HandleFunc("/api/retry/preview", corsMiddleware(requireAuth(handleRetryPreview)))
	mux.HandleFunc("/api/retry/run-now", corsMiddleware(requireAuth(handleRetryRunNow)))
	mux.HandleFunc("/api/retry/exclude", corsMiddleware(requireAuth(handleRetryExclude)))
	mux.HandleFunc("/api/retry/include", corsMiddleware(requireAuth(handleRetryInclude)))
	mux.HandleFunc("/api/retry/excluded", corsMiddleware(requireAuth(handleRetryExcluded)))

	// Report belum-respons (sudah di-blast 2/3x tapi nomor tidak pernah membalas)
	mux.HandleFunc("/api/report/unresponsive", corsMiddleware(requireAuth(handleReportUnresponsive)))
	mux.HandleFunc("/api/report/unresponsive.csv", corsMiddleware(requireAuth(handleReportUnresponsiveCSV)))
	mux.HandleFunc("/api/report/export-sheet", corsMiddleware(requireAuth(handleReportExportSheet)))
	mux.HandleFunc("/api/report/resolved", corsMiddleware(requireAuth(handleReportResolved)))
	mux.HandleFunc("/api/report/resolved.csv", corsMiddleware(requireAuth(handleReportResolvedCSV)))
	mux.HandleFunc("/api/report/resolved/export-sheet", corsMiddleware(requireAuth(handleReportResolvedExportSheet)))

	// Inbox Chat endpoints
	mux.HandleFunc("/api/inbox/threads", corsMiddleware(requireAuth(handleThreads)))
	mux.HandleFunc("/api/inbox/messages", corsMiddleware(requireAuth(handleMessages)))
	mux.HandleFunc("/api/inbox/read", corsMiddleware(requireAuth(handleMarkRead)))
	mux.HandleFunc("/api/inbox/status", corsMiddleware(requireAuth(handleSetStatus)))
	mux.HandleFunc("/api/inbox/invoices", corsMiddleware(requireAuth(handleThreadInvoices)))
	mux.HandleFunc("/api/inbox/match-token", corsMiddleware(requireAuth(handleMatchToken)))
	mux.HandleFunc("/api/inbox/reply", corsMiddleware(requireAuth(handleReply)))
	mux.HandleFunc("/api/inbox/note", corsMiddleware(requireAuth(handleNote)))
	mux.HandleFunc("/api/inbox/resolve", corsMiddleware(requireAuth(handleResolve)))
	mux.HandleFunc("/api/inbox/sync-teams", corsMiddleware(requireAuth(handleSyncTeams)))

	// Inbox CLM (independen dari Inbox; per-invoice, timeline & reply sendiri)
	mux.HandleFunc("/api/clm/threads", corsMiddleware(requireAuth(clmHandleThreads)))
	mux.HandleFunc("/api/clm/assigned", corsMiddleware(requireAuth(clmHandleAssigned)))
	mux.HandleFunc("/api/clm/assign", corsMiddleware(requireAuth(clmHandleAssign)))
	mux.HandleFunc("/api/clm/status", corsMiddleware(requireAuth(clmHandleSetStatus)))
	mux.HandleFunc("/api/clm/messages", corsMiddleware(requireAuth(clmHandleMessages)))
	mux.HandleFunc("/api/clm/reply", corsMiddleware(requireAuth(clmHandleReply)))
	mux.HandleFunc("/api/clm/note", corsMiddleware(requireAuth(clmHandleNote)))
	// Media file — TANPA requireAuth (di-load via <img>/<video> lintas-domain), diproteksi
	// token HMAC di query string (?id=&t=). Lihat media.go.
	mux.HandleFunc("/api/inbox/media", handleInboxMedia)

	// Blaster (nomor disposable pengirim blast) — status/QR/logout(ganti nomor)
	mux.HandleFunc("/api/blaster/wa-status", corsMiddleware(requireAuth(blasterHandleStatus)))
	mux.HandleFunc("/api/blaster/qr", corsMiddleware(requireAuth(blasterHandleQR)))
	mux.HandleFunc("/api/blaster/wa-logout", corsMiddleware(requireAuth(blasterHandleLogout)))

	// ---- Zopoz (line WA kedua) — namespace /api/zopoz/* terpisah total ----
	mux.HandleFunc("/api/zopoz/wa-status", corsMiddleware(requireAuth(zopozHandleStatus)))
	mux.HandleFunc("/api/zopoz/qr", corsMiddleware(requireAuth(zopozHandleQR)))
	mux.HandleFunc("/api/zopoz/wa-logout", corsMiddleware(requireAuth(zopozHandleLogout)))
	mux.HandleFunc("/api/zopoz/templates", corsMiddleware(requireAuth(zopozHandleTemplates)))
	mux.HandleFunc("/api/zopoz/blast", corsMiddleware(requireAuth(zopozHandleBlast)))
	mux.HandleFunc("/api/zopoz/progress", corsMiddleware(requireAuth(zopozHandleProgress)))
	mux.HandleFunc("/api/zopoz/history", corsMiddleware(requireAuth(zopozHandleHistory)))
	mux.HandleFunc("/api/zopoz/sheet-status", corsMiddleware(requireAuth(zopozHandleSheetStatus)))
	mux.HandleFunc("/api/zopoz/export-sheet", corsMiddleware(requireAuth(zopozHandleExportSheet)))
	mux.HandleFunc("/api/zopoz/retry/preview", corsMiddleware(requireAuth(zopozHandleRetryPreview)))
	mux.HandleFunc("/api/zopoz/retry/run-now", corsMiddleware(requireAuth(zopozHandleRetryRunNow)))
	mux.HandleFunc("/api/zopoz/retry/exclude", corsMiddleware(requireAuth(zopozHandleRetryExclude)))
	mux.HandleFunc("/api/zopoz/retry/include", corsMiddleware(requireAuth(zopozHandleRetryInclude)))
	mux.HandleFunc("/api/zopoz/retry/excluded", corsMiddleware(requireAuth(zopozHandleRetryExcluded)))
	mux.HandleFunc("/api/zopoz/report/unresponsive", corsMiddleware(requireAuth(zopozHandleReportUnresponsive)))
	mux.HandleFunc("/api/zopoz/report/unresponsive.csv", corsMiddleware(requireAuth(zopozHandleReportUnresponsiveCSV)))
	mux.HandleFunc("/api/zopoz/report/export-sheet", corsMiddleware(requireAuth(zopozHandleReportExportSheet)))
	mux.HandleFunc("/api/zopoz/threads", corsMiddleware(requireAuth(zopozHandleThreads)))
	mux.HandleFunc("/api/zopoz/messages", corsMiddleware(requireAuth(zopozHandleMessages)))
	mux.HandleFunc("/api/zopoz/read", corsMiddleware(requireAuth(zopozHandleMarkRead)))
	mux.HandleFunc("/api/zopoz/status", corsMiddleware(requireAuth(zopozHandleSetStatus)))
	mux.HandleFunc("/api/zopoz/reply", corsMiddleware(requireAuth(zopozHandleReply)))
	mux.HandleFunc("/api/zopoz/media", zopozHandleMedia)

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		log.Printf("listening on http://localhost%s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")
	client.Disconnect()
	if zopozState.client != nil {
		zopozState.client.Disconnect()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// repairAfterLogout — dipanggil saat *events.LoggedOut. Tanpa ini backend nyangkut
// "hang tanpa QR" (connectAndPair cuma jalan sekali saat startup). Di-guard supaya
// tidak ada dua flow pairing berjalan barengan.
func repairAfterLogout(client *whatsmeow.Client) {
	repairMu.Lock()
	if repairing {
		repairMu.Unlock()
		return
	}
	repairing = true
	repairMu.Unlock()
	defer func() {
		repairMu.Lock()
		repairing = false
		repairMu.Unlock()
	}()

	client.Disconnect()
	time.Sleep(2 * time.Second) // beri waktu socket tutup & store terhapus (Store.ID -> nil)
	connectAndPair(client)
}

func connectAndPair(client *whatsmeow.Client) {
	if client.Store.ID == nil {
		qrChan, err := client.GetQRChannel(context.Background())
		if err != nil {
			log.Printf("QR channel: %v", err)
			return
		}
		if err := client.Connect(); err != nil {
			log.Printf("connect: %v", err)
			return
		}
		for evt := range qrChan {
			switch evt.Event {
			case "code":
				state.setQR(evt.Code)
				log.Println("QR code updated — open /qr or scan terminal")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			case "success":
				state.setQR("")
				state.setLoggedIn(true)
				log.Println("pairing success")
			case "timeout":
				log.Println("QR timeout — restart to retry")
			}
		}
	} else {
		state.setLoggedIn(true)
		if err := client.Connect(); err != nil {
			log.Printf("connect: %v", err)
		}
	}
}

// ---- state helpers ----

func (s *appState) setQR(code string) {
	s.mu.Lock()
	s.qrCode = code
	s.mu.Unlock()
}
func (s *appState) setLoggedIn(v bool) {
	s.mu.Lock()
	s.loggedIn = v
	s.mu.Unlock()
}
func (s *appState) setConnected(v bool) {
	s.mu.Lock()
	s.connected = v
	s.mu.Unlock()
}
func (s *appState) snapshot() (qr string, loggedIn, connected bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.qrCode, s.loggedIn, s.connected
}

// ---- handlers ----

func handleStatus(w http.ResponseWriter, r *http.Request) {
	qr, loggedIn, connected := state.snapshot()
	writeJSON(w, map[string]any{
		"loggedIn":  loggedIn,
		"connected": connected,
		"hasQR":     qr != "",
	})
}

func handleQR(w http.ResponseWriter, r *http.Request) {
	qr, _, _ := state.snapshot()
	writeJSON(w, map[string]any{"code": qr})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if err := state.client.Logout(r.Context()); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	state.setLoggedIn(false)
	writeJSON(w, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, status int, format string, args ...any) {
	http.Error(w, fmt.Sprintf(format, args...), status)
}

// handleIncomingWA — di-call dari whatsmeow event handler. Simpan reply ke chat_messages
// dan update thread. Skip group, skip outgoing, skip dari nomor yang tidak pernah di-blast.
func handleIncomingWA(e *events.Message) {
	// Debug log — semua event Message dilog untuk diagnose
	log.Printf("event.Message: from=%s chat=%s alt=%s isFromMe=%v isGroup=%v id=%s",
		e.Info.Sender.String(), e.Info.Chat.String(), e.Info.SenderAlt.String(), e.Info.IsFromMe, e.Info.IsGroup, e.Info.ID)

	if e.Info.IsFromMe {
		log.Println("  → skipped: from me")
		return
	}
	if e.Info.IsGroup {
		log.Println("  → skipped: group")
		return
	}

	// Resolve nomor pengirim asli (bisa "" kalau LID belum ter-mapping ke phone).
	senderPhone := resolveSenderPhone(e.Info)

	// Unwrap pembungkus (view-once/ephemeral/device-sent) dulu supaya TOKEN & media di
	// dalamnya terdeteksi. eff dipakai utk extract DAN download (DownloadAny butuh pesan benar).
	eff := unwrapMessage(e.Message)
	body, mediaType := extractTextFromMessage(eff)
	// Pesan kontrol/protokol WA (EPHEMERAL_SYNC_RESPONSE, revoke, reaction, dsb.) BUKAN
	// isi chat dari customer. Jangan dicatat / dimunculkan ("[Pesan tidak didukung]") dan
	// jangan geser thread ke Open / tambah unread. Skip total.
	if mediaType == "unknown" || (body == "" && mediaType == "") {
		log.Printf("  → skip: pesan kontrol/tidak didukung from=%s id=%s raw: %.200s", senderPhone, e.Info.ID, eff.String())
		return
	}

	if senderPhone == "" {
		log.Printf("  → skipped: tak bisa resolve nomor pengirim (sender=%s alt=%s)",
			e.Info.Sender.String(), e.Info.SenderAlt.String())
		return
	}

	// Sudah ada thread non-blast (inbound_non_blast belum dicocokkan, atau outside_blast) untuk
	// nomor ini? Tetap di bucket itu — jangan promosikan ke Open (append pesan saja).
	if st := threadStatus(senderPhone); st == "inbound_non_blast" || st == "outside_blast" {
		if err := recordChatMessage(senderPhone, "in", body, mediaType, e.Info.ID, e.Info.Timestamp, 0, "", ""); err != nil {
			log.Println("warn: recordChatMessage inbound_non_blast:", err)
		}
		if err := upsertThreadInboundNonBlast(senderPhone, body, e.Info.Timestamp); err != nil {
			log.Println("warn: upsertThreadInboundNonBlast:", err)
		}
		clmOnIncoming(senderPhone, body, e.Info.Timestamp, e.Info.ID) // additif — no-op kalau nomor belum di-assign CLM
		if isDownloadableMedia(mediaType) {
			go downloadAndStoreMedia(e.Info.ID, eff, mediaType)
		}
		log.Println("  → inbound_non_blast (belum dicocokkan) from", senderPhone, "—", truncate(body, 40))
		return
	}

	// Arsitektur 2-nomor: pesan ini masuk ke INTI dari pelanggan yang di-blast oleh nomor
	// BLASTER. Kaitkan ke invoice via TOKEN (baris "Kode Referensi") lebih dulu, fallback ke
	// nomor pengirim kalau token hilang/diedit. phone = canonical (key thread = nomor yang
	// di-blast).
	phone, ok := resolveInboundThread(body, senderPhone)
	if !ok {
		// Tak bisa dikaitkan ke invoice (chat manual dari nomor tak dikenal, tanpa Kode
		// Referensi valid) → TAMPUNG di bucket 'inbound_non_blast' (bukan di-skip). Agent akan
		// minta Kode Referensi lalu cocokkan (handleMatchToken).
		if err := recordChatMessage(senderPhone, "in", body, mediaType, e.Info.ID, e.Info.Timestamp, 0, "", ""); err != nil {
			log.Println("warn: recordChatMessage inbound_non_blast:", err)
		}
		if err := upsertThreadInboundNonBlast(senderPhone, body, e.Info.Timestamp); err != nil {
			log.Println("warn: upsertThreadInboundNonBlast:", err)
		}
		if isDownloadableMedia(mediaType) {
			go downloadAndStoreMedia(e.Info.ID, eff, mediaType)
		}
		log.Printf("  → inbound_non_blast (baru) from %s token=%q — %s", senderPhone, parseTokenFromBody(body), truncate(body, 40))
		return
	}

	if err := recordChatMessage(phone, "in", body, mediaType, e.Info.ID, e.Info.Timestamp, 0, "", ""); err != nil {
		log.Println("warn: recordChatMessage incoming:", err)
	}
	if err := upsertThreadIncoming(phone, body, e.Info.Timestamp); err != nil {
		log.Println("warn: upsertThreadIncoming:", err)
	}
	clmOnIncoming(phone, body, e.Info.Timestamp, e.Info.ID) // additif — customer balas → CLM 'open' (no-op kalau belum di-assign)
	// Pelanggan chat dari nomor BEDA dari yang di-blast (dikaitkan via token) → simpan JID
	// pengirim asli supaya balasan agent terkirim ke nomor itu, bukan nomor yang di-blast.
	if senderPhone != "" && senderPhone != phone {
		if err := setThreadReplyJID(phone, senderPhone); err != nil {
			log.Println("warn: setThreadReplyJID:", err)
		}
	}
	// Media (gambar/video/audio/stiker/dokumen) → unduh & simpan async supaya bisa
	// ditampilkan di chatbox. Jangan blok event handler.
	if isDownloadableMedia(mediaType) {
		go downloadAndStoreMedia(e.Info.ID, eff, mediaType)
	}
	log.Println("  → inbox: incoming from", senderPhone, "→ thread", phone, "—", truncate(body, 40))
}

func serveLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := userFrom(r); ok {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	http.ServeFile(w, r, "static/login.html")
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "static/index.html")
}

func serveInbox(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "static/inbox.html")
}

// loadDotEnv minimal — KEY=VALUE per baris, # untuk komentar.
func loadDotEnv() {
	f, err := os.Open(".env")
	if err != nil {
		return
	}
	defer f.Close()
	buf := make([]byte, 8192)
	n, _ := f.Read(buf)
	for _, line := range splitLines(string(buf[:n])) {
		line = trim(line)
		if line == "" || line[0] == '#' {
			continue
		}
		i := indexByte(line, '=')
		if i < 0 {
			continue
		}
		k := trim(line[:i])
		v := trim(line[i+1:])
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
