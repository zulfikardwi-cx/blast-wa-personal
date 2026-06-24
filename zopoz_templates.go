package main

// Template pesan KHUSUS Zopoz — terpisah total dari template majoo (attemptTemplates /
// closingTemplate di chat.go). Default branding "Zopreneurs / zopoz". Semua bisa di-override
// via env: ZOPOZ_TEMPLATE_ATTEMPT_1/2/3 dan ZOPOZ_INBOX_CLOSING_TEMPLATE (pakai \n utk newline).

import (
	"net/http"
	"os"
	"strings"
)

var (
	zopozAttemptTemplates [3]string
	zopozClosingTemplate  string
)

func loadZopozTemplates() {
	zopozAttemptTemplates[0] = pickTemplate("ZOPOZ_TEMPLATE_ATTEMPT_1", `Halo, Zopreneurs!

Terima kasih telah berlangganan aplikasi zopoz.
Untuk menjaga keakuratan dan keamanan data, kami perlu melakukan validasi untuk Invoice berikut:

Nama Outlet: {{nama_outlet}}
No. Invoice: {{nomer_invoice}}

Apabila Kakak bersedia mengikuti proses validasi, mohon balas pesan ini pada jam operasional kami (09.00–16.00 WIB) agar proses penjadwalan dapat segera dilakukan.
Tim Validator kami akan menghubungi Kakak melalui WhatsApp Call.

Terima kasih! 🙏`)

	zopozAttemptTemplates[1] = pickTemplate("ZOPOZ_TEMPLATE_ATTEMPT_2", `Halo, Zopreneurs!

Mohon maaf, ingin melakukan konfirmasi kembali
Untuk menjaga keakuratan dan keamanan data, kami perlu melakukan validasi untuk Invoice berikut:

Nama Outlet: {{nama_outlet}}
No. Invoice: {{nomer_invoice}}

Apabila Kakak bersedia mengikuti proses validasi, mohon balas pesan ini pada jam operasional kami (09.00–16.00 WIB) agar proses penjadwalan dapat segera dilakukan.
Tim Validator kami akan menghubungi Kakak melalui WhatsApp Call.

Terima kasih! 🙏`)

	zopozAttemptTemplates[2] = pickTemplate("ZOPOZ_TEMPLATE_ATTEMPT_3", `Halo, Zopreneurs!

Izin follow up kembali
Untuk menjaga keakuratan dan keamanan data, kami perlu melakukan validasi untuk Invoice berikut:

Nama Outlet: {{nama_outlet}}
No. Invoice: {{nomer_invoice}}

Apabila Kakak bersedia mengikuti proses validasi, mohon balas pesan ini pada jam operasional kami (09.00–16.00 WIB) agar proses penjadwalan dapat segera dilakukan.
Tim Validator kami akan menghubungi Kakak melalui WhatsApp Call.
Jika Kakak masih belum membalas pesan ini, maka penjadwalan kami tutup.

Terima kasih! 🙏`)
}

func loadZopozClosingTemplate() {
	t := os.Getenv("ZOPOZ_INBOX_CLOSING_TEMPLATE")
	if t == "" {
		t = `Baik, Terima kasih atas konfirmasinya, Kak. 😊
Untuk percakapan ini akan saya tutup.

Jika nantinya ada pertanyaan atau kendala terkait layanan zopoz, mohon tidak membalas atau menghubungi nomor ini karena nomor ini hanya digunakan untuk proses konfirmasi.

Terima kasih`
	}
	t = strings.ReplaceAll(t, `\n`, "\n")
	zopozClosingTemplate = t
}

// zopozGetAttemptTemplate — template attempt N (1,2,3). Default attempt 1 kalau out of range.
func zopozGetAttemptTemplate(attempt int) string {
	if attempt < 1 || attempt > 3 {
		attempt = 1
	}
	return zopozAttemptTemplates[attempt-1]
}

func zopozRenderClosingTemplate(namaOutlet string) string {
	return strings.ReplaceAll(zopozClosingTemplate, "{{nama_outlet}}", namaOutlet)
}

// zopozHandleTemplates — preview 3 template Zopoz + closing utk UI (route /api/zopoz/templates).
func zopozHandleTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"attempt_1":         zopozAttemptTemplates[0],
		"attempt_2":         zopozAttemptTemplates[1],
		"attempt_3":         zopozAttemptTemplates[2],
		"retry_window_hour": retryWindowHour,
		"closing":           zopozClosingTemplate,
	})
}
