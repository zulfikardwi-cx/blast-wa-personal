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
	"go.mau.fi/whatsmeow/store"
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

// blasterContainer — sqlstore container blaster, disimpan supaya re-pair setelah logout bisa
// mengambil DEVICE STORE BARU (device lama sudah dihapus whatsmeow saat LoggedOut).
var blasterContainer *sqlstore.Container

// initBlasterClient — buat client WA disposable dengan session store terpisah. Dipanggil
// dari main() setelah client INTI siap. Non-fatal: kalau gagal, log saja — blast mati tapi
// Inbox INTI tetap jalan.
func initBlasterClient(rootCtx context.Context) error {
	dbLog := waLog.Stdout("BlasterDB", "WARN", true)
	container, err := sqlstore.New(rootCtx, "sqlite3", "file:session/store-blaster.db?_foreign_keys=on", dbLog)
	if err != nil {
		return err
	}
	blasterContainer = container
	deviceStore, err := container.GetFirstDevice(rootCtx)
	if err != nil {
		return err
	}
	blasterState.client = newBlasterClient(deviceStore)
	go blasterConnectAndPair(blasterState.client)
	return nil
}

// newBlasterClient — buat client + pasang event handler. Dipakai saat init DAN saat re-pair
// setelah logout. Wajib client + device store BARU tiap re-pair: whatsmeow menghapus device
// saat LoggedOut, jadi memakai ulang client lama → "invalid use of deleted device" (bikin QR
// tak pernah muncul / tools mentok "Menghubungkan..."). SENGAJA tanpa case *events.Message —
// blaster outbound-only, inbound ditangani INTI.
func newBlasterClient(deviceStore *store.Device) *whatsmeow.Client {
	clientLog := waLog.Stdout("BlasterClient", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)
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
			log.Println("blaster: logged out — auto re-pair: bikin device baru, QR baru akan muncul")
			go blasterRepairAfterLogout(client)
		}
	})
	return client
}

// blasterRepairAfterLogout — setelah nomor blaster di-logout/banned, siapkan client + device
// BARU lalu tampilkan QR pengganti. `old` = client lama (device-nya sudah dihapus) → dibuang.
func blasterRepairAfterLogout(old *whatsmeow.Client) {
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

	if old != nil {
		old.Disconnect()
	}
	time.Sleep(2 * time.Second)

	if blasterContainer == nil {
		log.Println("blaster: container nil — tak bisa re-pair, restart backend.")
		return
	}
	// Ambil device BARU (GetFirstDevice bikin fresh kalau store kosong setelah device lama
	// dihapus). Client baru → pairing bersih, hindari 'invalid use of deleted device'.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	deviceStore, err := blasterContainer.GetFirstDevice(ctx)
	cancel()
	if err != nil {
		log.Printf("blaster: gagal ambil device baru utk re-pair: %v", err)
		return
	}
	client := newBlasterClient(deviceStore)
	blasterState.client = client
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
