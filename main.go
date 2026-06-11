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
	waLog "go.mau.fi/whatsmeow/util/log"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/mdp/qrterminal/v3"
)

type appState struct {
	mu         sync.RWMutex
	client     *whatsmeow.Client
	qrCode     string // last QR string (empty when logged in)
	loggedIn   bool
	connected  bool

	job *BlastJob // current/last blast job
}

var state = &appState{}

func main() {
	loadDotEnv()
	if err := initAuth(); err != nil {
		log.Fatalf("auth: %v", err)
	}
	if err := initAudit(); err != nil {
		log.Fatalf("audit: %v", err)
	}

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
			_ = e
		case *events.Disconnected:
			state.setConnected(false)
			log.Println("disconnected")
		case *events.LoggedOut:
			state.setLoggedIn(false)
			state.setConnected(false)
			log.Println("logged out — re-pair required")
		}
	})

	go connectAndPair(client)

	mux := http.NewServeMux()

	// Public routes (no CORS needed — server-to-browser navigation)
	mux.HandleFunc("/login", serveLogin)
	mux.HandleFunc("/auth/login", handleLogin)
	mux.HandleFunc("/auth/callback", handleCallback)
	mux.HandleFunc("/auth/logout", handleAuthLogout)
	mux.Handle("/static/assets/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	// Optional: serve frontend juga dari backend (fallback kalau Pages down)
	mux.HandleFunc("/", requirePage(serveIndex))

	// API endpoints — CORS-enabled untuk dipanggil dari GitHub Pages
	mux.HandleFunc("/api/me", corsMiddleware(handleMe))
	mux.HandleFunc("/api/status", corsMiddleware(requireAuth(handleStatus)))
	mux.HandleFunc("/api/qr", corsMiddleware(requireAuth(handleQR)))
	mux.HandleFunc("/api/logout", corsMiddleware(requireAuth(handleLogout)))
	mux.HandleFunc("/api/blast", corsMiddleware(requireAuth(handleBlast)))
	mux.HandleFunc("/api/progress", corsMiddleware(requireAuth(handleProgress)))
	mux.HandleFunc("/api/history", corsMiddleware(requireAuth(handleHistory)))

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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func connectAndPair(client *whatsmeow.Client) {
	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(context.Background())
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
