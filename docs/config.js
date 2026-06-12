// Konfigurasi runtime — edit URL backend di sini setelah Cloudflare Tunnel siap.
// File ini di-load sebelum semua script aplikasi, jadi window.APP_CONFIG global tersedia.
window.APP_CONFIG = {
  // Wajib: URL backend (Cloudflare Tunnel ke Mac yang jalanin Go app).
  // Contoh: "https://blast-wa-api.majoo.id"
  // BIARKAN kosong "" hanya saat development di mana frontend & backend sama host.
  API_BASE: "https://mid-dennis-luther-named.trycloudflare.com",
};
