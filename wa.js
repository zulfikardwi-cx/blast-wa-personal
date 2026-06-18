'use strict';

// wa.js — pengganti seluruh layer whatsmeow (main.go event handler + blast.go sendOne).
// whatsapp-web.js menyederhanakan banyak hal: registrasi & serialisasi nomor (termasuk
// LID) ditangani internal via getNumberId(), jadi resolveToLID/GetUserDevices TIDAK perlu.
// Sesi disimpan LocalAuth (folder browser) — bukan store.db. Logout = hapus folder ini.

const path = require('path');
const { Client, LocalAuth } = require('whatsapp-web.js');
const qrcodeTerminal = require('qrcode-terminal');

const DATA_PATH = process.env.WWEBJS_DATA_PATH || path.join(__dirname, '.wwebjs_auth');

const state = {
  loggedIn: false,
  connected: false,
  qr: '', // string QR terakhir (kosong saat sudah login)
};

let client = null;
let incomingHandler = null; // di-set server.js → chat.handleIncoming
let reinitTimer = null;

function setIncomingHandler(fn) {
  incomingHandler = fn;
}

function buildClient() {
  const puppeteer = {
    headless: true,
    args: [
      '--no-sandbox',
      '--disable-setuid-sandbox',
      '--disable-dev-shm-usage',
      '--disable-gpu',
    ],
  };
  if (process.env.PUPPETEER_EXECUTABLE_PATH) {
    puppeteer.executablePath = process.env.PUPPETEER_EXECUTABLE_PATH;
  }

  const c = new Client({
    authStrategy: new LocalAuth({ dataPath: DATA_PATH }),
    puppeteer,
  });

  c.on('qr', (qr) => {
    state.qr = qr;
    state.loggedIn = false;
    state.connected = false;
    console.log('QR code updated — buka /api/qr atau scan dari terminal di bawah:');
    qrcodeTerminal.generate(qr, { small: true });
  });

  c.on('authenticated', () => {
    state.loggedIn = true;
    console.log('authenticated');
  });

  c.on('auth_failure', (msg) => {
    state.loggedIn = false;
    console.log('auth_failure:', msg);
  });

  c.on('ready', () => {
    state.loggedIn = true;
    state.connected = true;
    state.qr = '';
    console.log('connected (ready)');
  });

  c.on('disconnected', (reason) => {
    state.connected = false;
    state.loggedIn = false;
    state.qr = '';
    console.log('disconnected:', reason, '— auto re-init: QR baru akan muncul (scan dari HP, tanpa restart)');
    scheduleReinit();
  });

  c.on('message', (msg) => {
    handleIncomingWA(msg).catch((e) => console.log('warn: handleIncomingWA:', e.message));
  });

  return c;
}

// scheduleReinit — port repairAfterLogout. Destroy client lama lalu bangun ulang supaya
// 'qr' event keluar lagi tanpa perlu restart proses.
function scheduleReinit() {
  if (reinitTimer) return;
  reinitTimer = setTimeout(async () => {
    reinitTimer = null;
    try {
      if (client) await client.destroy().catch(() => {});
    } catch (_) {}
    client = buildClient();
    client.initialize().catch((e) => console.log('reinit initialize error:', e.message));
  }, 3000);
}

function init() {
  client = buildClient();
  return client.initialize();
}

// ---- status helpers ----

function snapshot() {
  return { loggedIn: state.loggedIn, connected: state.connected, hasQR: state.qr !== '' };
}
function getQR() {
  return state.qr;
}

// ---- kirim ----

// isRegistered — return serialized id (62xxx@c.us) atau null. Ganti IsOnWhatsApp.
async function isRegistered(phone) {
  return client.getNumberId(phone); // null kalau tidak terdaftar
}

// sendText — kirim pesan teks ke nomor (digit, format 62xxx). Throw kalau gagal /
// nomor tidak terdaftar. Return { id } (wa_message_id).
async function sendText(phone, body) {
  const numberId = await client.getNumberId(phone);
  if (!numberId) throw new Error('nomor tidak terdaftar di WhatsApp');
  const msg = await client.sendMessage(numberId._serialized, body);
  return { id: (msg && msg.id && (msg.id.id || msg.id._serialized)) || '' };
}

async function logout() {
  await client.logout();
  state.loggedIn = false;
  state.connected = false;
}

// ---- incoming ----

function digitsOnly(s) {
  return String(s || '').replace(/\D/g, '');
}

function placeholder(p, caption) {
  return caption ? p + ' — ' + caption : p;
}

// extractIncoming — ambil (body, mediaType) dari pesan whatsapp-web.js.
// Mirror extractTextFromMessage di chat.go.
function extractIncoming(msg) {
  switch (msg.type) {
    case 'chat':
      return { body: msg.body || '', mediaType: '' };
    case 'image':
      return { body: placeholder('[Gambar]', msg.body), mediaType: 'image' };
    case 'video':
      return { body: placeholder('[Video]', msg.body), mediaType: 'video' };
    case 'ptt':
    case 'audio':
      return { body: '[Pesan suara]', mediaType: 'audio' };
    case 'document':
      return { body: '[Dokumen] ' + (msg.body || ''), mediaType: 'document' };
    case 'sticker':
      return { body: '[Stiker]', mediaType: 'sticker' };
    case 'location':
      return { body: '[Lokasi]', mediaType: 'location' };
    case 'vcard':
    case 'multi_vcard':
    case 'contact_card':
      return { body: '[Kontak]', mediaType: 'contact' };
    default:
      if (msg.body) return { body: msg.body, mediaType: '' };
      return { body: '[Pesan tidak didukung]', mediaType: 'unknown' };
  }
}

// resolvePhone — ambil nomor asli (62xxx) dari pesan masuk.
//   - DM normal: from = 62xxx@c.us → langsung.
//   - LID (privacy WA terbaru): from = <acak>@lid → resolve ke PN via API resmi
//     client.getContactLidAndPhone([lid]) → { lid, pn }. Tanpa ini, nomor yang kebaca
//     adalah angka LID acak (mis. 18649033228449) yang tidak match thread → reply
//     customer tidak masuk Inbox. (Padanan resolveSenderPhone/GetPNForLID di versi Go.)
async function resolvePhone(msg) {
  if (msg.from && msg.from.endsWith('@c.us')) {
    return digitsOnly(msg.from.replace('@c.us', ''));
  }
  if (msg.from && msg.from.endsWith('@lid')) {
    try {
      const res = await client.getContactLidAndPhone([msg.from]);
      const pn = res && res[0] && res[0].pn;
      if (pn) return digitsOnly(pn.replace('@c.us', ''));
    } catch (e) {
      console.log('warn: getContactLidAndPhone gagal untuk', msg.from, ':', e.message);
    }
  }
  // fallback terakhir: contact.number (kadang sudah ter-resolve ke PN)
  try {
    const c = await msg.getContact();
    if (c && c.number) return digitsOnly(c.number);
  } catch (_) {}
  return '';
}

async function handleIncomingWA(msg) {
  if (msg.fromMe) return;
  if (msg.from && msg.from.endsWith('@g.us')) return; // group
  if (msg.isStatus) return; // status/broadcast

  const phone = await resolvePhone(msg);
  if (!phone) {
    console.log('  → skip incoming: tidak bisa resolve phone dari', msg.from);
    return;
  }
  const { body, mediaType } = extractIncoming(msg);
  const ts = msg.timestamp ? new Date(msg.timestamp * 1000) : new Date();
  if (incomingHandler) {
    incomingHandler({
      phone,
      body,
      mediaType,
      waMessageId: (msg.id && msg.id.id) || '',
      timestamp: ts,
    });
  }
}

module.exports = {
  init,
  snapshot,
  getQR,
  isRegistered,
  sendText,
  logout,
  setIncomingHandler,
};
