'use strict';

// templates.js — port dari bagian template di chat.go.
// 3 attempt template + closing. Default hardcoded (spec majoo), override via env
// TEMPLATE_ATTEMPT_1/2/3 dan INBOX_CLOSING_TEMPLATE (pakai \n untuk newline).

const { renderTemplate } = require('./util');

const DEFAULT_ATTEMPT_1 = `Halo, Majoopreneurs!

Terima kasih telah berlangganan aplikasi majoo.
Untuk menjaga keakuratan dan keamanan data, kami perlu melakukan validasi untuk Invoice berikut:

Nama Outlet: {{nama_outlet}}
No. Invoice: {{nomer_invoice}}

Apabila Kakak bersedia mengikuti proses validasi, mohon balas pesan ini pada jam operasional kami (09.00–16.00 WIB) agar proses penjadwalan dapat segera dilakukan.
Tim Validator kami akan menghubungi Kakak untuk melakukan sesi Google Meet atau WhatsApp Call.

Terima kasih! 🙏`;

const DEFAULT_ATTEMPT_2 = `Halo, Majoopreneurs!

Mohon maaf, ingin melakukan konfirmasi kembali
Untuk menjaga keakuratan dan keamanan data, kami perlu melakukan validasi untuk Invoice berikut:

Nama Outlet: {{nama_outlet}}
No. Invoice: {{nomer_invoice}}

Apabila Kakak bersedia mengikuti proses validasi, mohon balas pesan ini pada jam operasional kami (09.00–16.00 WIB) agar proses penjadwalan dapat segera dilakukan.
Tim Validator kami akan menghubungi Kakak untuk melakukan sesi Google Meet atau WhatsApp Call.

Terima kasih! 🙏`;

const DEFAULT_ATTEMPT_3 = `Halo, Majoopreneurs!

Izin follow up kembali
Untuk menjaga keakuratan dan keamanan data, kami perlu melakukan validasi untuk Invoice berikut:

Nama Outlet: {{nama_outlet}}
No. Invoice: {{nomer_invoice}}

Apabila Kakak bersedia mengikuti proses validasi, mohon balas pesan ini pada jam operasional kami (09.00–16.00 WIB) agar proses penjadwalan dapat segera dilakukan.
Tim Validator kami akan menghubungi Kakak untuk melakukan sesi Google Meet atau WhatsApp Call.
Jika Kakak masih belum membalas pesan ini, maka penjadwalan kami tutup. Jika terdapat permintaan dan informasi lainnya, silahkan menghubungi Hotline Majoo pada nomer 0811-500-460

Terima kasih! 🙏`;

const DEFAULT_CLOSING = `Baik, Terima kasih atas konfirmasinya, Kak. 😊
Untuk percakapan ini akan saya tutup.

Jika nantinya ada pertanyaan atau kendala terkait layanan majoo, mohon tidak membalas atau menghubungi nomor ini karena nomor ini hanya digunakan untuk proses konfirmasi.

Untuk bantuan lebih lanjut, Kakak dapat menghubungi Hotline majoo di 0811-500-460.

Terima kasih`;

let attemptTemplates = ['', '', ''];
let closingTemplate = '';

function pickTemplate(envKey, def) {
  const t = process.env[envKey];
  if (!t) return def;
  return t.split('\\n').join('\n');
}

function load() {
  attemptTemplates = [
    pickTemplate('TEMPLATE_ATTEMPT_1', DEFAULT_ATTEMPT_1),
    pickTemplate('TEMPLATE_ATTEMPT_2', DEFAULT_ATTEMPT_2),
    pickTemplate('TEMPLATE_ATTEMPT_3', DEFAULT_ATTEMPT_3),
  ];
  closingTemplate = pickTemplate('INBOX_CLOSING_TEMPLATE', DEFAULT_CLOSING);
}

// getAttemptTemplate — template untuk attempt N (1,2,3); default ke 1 kalau out of range.
function getAttemptTemplate(attempt) {
  if (attempt < 1 || attempt > 3) attempt = 1;
  return attemptTemplates[attempt - 1];
}

function renderClosing(namaOutlet) {
  return renderTemplate(closingTemplate, namaOutlet, '');
}

module.exports = {
  load,
  getAttemptTemplate,
  renderClosing,
  getAttemptTemplates: () => attemptTemplates.slice(),
  getClosing: () => closingTemplate,
};
