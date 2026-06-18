// Konfigurasi runtime — URL backend untuk environment STAGING (Node.js).
// File ini di-load sebelum semua script aplikasi, jadi window.APP_CONFIG global tersedia.
window.APP_CONFIG = {
  // Wajib NON-KOSONG (frontend menolak API_BASE kosong).
  // - Lokal dev (mesin staging sendiri): "http://localhost:8080"
  // - Setelah Cloudflare Tunnel staging siap, ganti ke: "https://<tunnel-staging>"
  //   (URL harus SAMA dengan host halaman ini → tetap same-origin / first-party cookie).
  API_BASE: "http://localhost:8080",
};
