package main

// ============================================================================
// BLASTER — nomor WA DISPOSABLE khusus pengirim blast (attempt 1/2/3).
//
// Arsitektur 2-nomor:
//   - INTI  (state.client, nomor majoo existing) = inbound-only, pegang Inbox +
//     balas + Done. TIDAK pernah blast. Dijaga stabil.
//   - BLASTER (client di file ini, nomor baru) = OUTBOUND-only. Semua blast dikirim
//     dari sini. Kalau kena banned, tinggal logout → scan QR nomor pengganti; INTI
//     tidak terganggu.
//
// Client ini punya session store TERPISAH (session/store-blaster.db) & state sendiri.
// Ia SENGAJA tidak memproses events.Message — inbound hanya ditangani INTI. Job blast
// tetap dipegang state.job (satu antrian blast global), yang berubah cuma CLIENT PENGIRIM
// (lihat sendOne di blast.go & sendRetryOne di retry.go).
// ============================================================================

import (
	"context"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/mdp/qrterminal/v3"
)

type blasterAppState struct {
	mu        sync.RWMutex
	client    *whatsmeow.Client
	qrCode    string
	loggedIn  bool
	connected bool
}

var blasterState = &blasterAppState{}

// guard supaya auto re-pair blaster tidak jalan dobel (mirror pola primary/zopoz)
var (
	blasterRepairMu  sync.Mutex
	blasterRepairing bool
)

func (s *blasterAppState) setQR(code string)   { s.mu.Lock(); s.qrCode = code; s.mu.Unlock() }
func (s *blasterAppState) setLoggedIn(v bool)  { s.mu.Lock(); s.loggedIn = v; s.mu.Unlock() }
func (s *blasterAppState) setConnected(v bool) { s.mu.Lock(); s.connected = v; s.mu.Unlock() }
func (s *blasterAppState) snapshot() (string, bool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.qrCode, s.loggedIn, s.connected
}

// initBlasterClient — buat client WA disposable dengan session store terpisah. Dipanggil
// dari main() setelah client INTI siap. Non-fatal: kalau gagal, log saja — blast mati tapi
// Inbox INTI tetap jalan.
func initBlasterClient(rootCtx context.Context) error {
	dbLog := waLog.Stdout("BlasterDB", "WARN", true)
	container, err := sqlstore.New(rootCtx, "sqlite3", "file:session/store-blaster.db?_foreign_keys=on", dbLog)
	if err != nil {
		return err
	}
	deviceStore, err := container.GetFirstDevice(rootCtx)
	if err != nil {
		return err
	}

	clientLog := waLog.Stdout("BlasterClient", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)
	blasterState.client = client

	// SENGAJA tanpa case *events.Message — blaster outbound-only, inbound ditangani INTI.
	client.AddEventHandler(func(evt interface{}) {
		switch evt.(type) {
		case *events.Connected:
			blasterState.setConnected(true)
			blasterState.setQR("")
			log.Println("blaster: connected")
		case *events.Disconnected:
			blasterState.setConnected(false)
			log.Println("blaster: disconnected")
		case *events.LoggedOut:
			blasterState.setLoggedIn(false)
			blasterState.setConnected(false)
			blasterState.setQR("")
			log.Println("blaster: logged out — auto re-pair: QR baru akan muncul (scan nomor blaster pengganti)")
			go blasterRepairAfterLogout(client)
		}
	})

	go blasterConnectAndPair(client)
	return nil
}

func blasterRepairAfterLogout(client *whatsmeow.Client) {
	blasterRepairMu.Lock()
	if blasterRepairing {
		blasterRepairMu.Unlock()
		return
	}
	blasterRepairing = true
	blasterRepairMu.Unlock()
	defer func() {
		blasterRepairMu.Lock()
		blasterRepairing = false
		blasterRepairMu.Unlock()
	}()

	client.Disconnect()
	time.Sleep(2 * time.Second)
	blasterConnectAndPair(client)
}

func blasterConnectAndPair(client *whatsmeow.Client) {
	if client.Store.ID != nil {
		blasterState.setLoggedIn(true)
		if err := client.Connect(); err != nil {
			log.Printf("blaster connect: %v", err)
		}
		return
	}
	// Belum login → tampilkan QR. Regenerate terus sampai ter-scan (nomor pengganti).
	for {
		if client.IsConnected() {
			client.Disconnect()
			time.Sleep(1 * time.Second)
		}
		qrChan, err := client.GetQRChannel(context.Background())
		if err != nil {
			log.Printf("blaster QR channel: %v", err)
			return
		}
		if err := client.Connect(); err != nil {
			log.Printf("blaster connect: %v", err)
			return
		}
		paired := false
		for evt := range qrChan {
			switch evt.Event {
			case "code":
				blasterState.setQR(evt.Code)
				log.Println("blaster: QR code updated — buka halaman Blast atau scan terminal")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			case "success":
				blasterState.setQR("")
				blasterState.setLoggedIn(true)
				log.Println("blaster: pairing success")
				paired = true
			case "timeout":
				log.Println("blaster: QR timeout — regenerate QR baru otomatis (tanpa restart)")
			}
		}
		if paired || client.Store.ID != nil {
			return
		}
	}
}

// ---- HTTP handlers (namespace /api/blaster/*) ----

func blasterHandleStatus(w http.ResponseWriter, r *http.Request) {
	qr, loggedIn, connected := blasterState.snapshot()
	writeJSON(w, map[string]any{
		"loggedIn":  loggedIn,
		"connected": connected,
		"hasQR":     qr != "",
	})
}

func blasterHandleQR(w http.ResponseWriter, r *http.Request) {
	qr, _, _ := blasterState.snapshot()
	writeJSON(w, map[string]any{"code": qr})
}

// blasterHandleLogout — tombol "Ganti Nomor Blaster": logout nomor sekarang → auto re-pair
// memunculkan QR baru untuk scan nomor pengganti.
func blasterHandleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if blasterState.client == nil {
		httpErr(w, 400, "Blaster client belum siap")
		return
	}
	if err := blasterState.client.Logout(r.Context()); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	blasterState.setLoggedIn(false)
	writeJSON(w, map[string]any{"ok": true})
}
