'use strict';

// util.js — helper bersama (port dari helper di blast.go / chat.go).

// normalizePhone: ambil digit, normalkan ke format internasional Indonesia (62...).
//   "628xxx" -> tetap; "08xxx" -> "62"+sisa; "8xxx" -> "62"+s.
// Tanpa ini "8xxx" dikira +8.. (mis. +82 Korea) -> dianggap tidak terdaftar.
function normalizePhone(raw) {
  const s = String(raw || '').replace(/\D/g, '');
  if (s === '') return '';
  if (s.startsWith('62')) return s;
  if (s.startsWith('0')) return '62' + s.slice(1);
  if (s.startsWith('8')) return '62' + s;
  return s;
}

// renderTemplate — substitusi {{nama_outlet}} & {{nomer_invoice}}.
function renderTemplate(tpl, namaOutlet, nomerInvoice) {
  return String(tpl)
    .split('{{nama_outlet}}').join(namaOutlet || '')
    .split('{{nomer_invoice}}').join(nomerInvoice || '');
}

function truncate(s, n) {
  s = String(s || '').trim();
  return s.length > n ? s.slice(0, n) + '…' : s;
}

// nowISO — timestamp RFC3339-compatible (UTC, diakhiri Z). Dipakai untuk kolom
// last_message_at / last_attempt_at / timestamp (parseable oleh luxon di retry).
function nowISO(d) {
  return (d instanceof Date ? d : new Date()).toISOString();
}

module.exports = { normalizePhone, renderTemplate, truncate, nowISO };
