'use strict';

// sheets.js — port dari sheets.go + teams.go. Export Blast Log ke Google Sheets via
// service account, ensureSheetExists, dan sync mapping team dari tab "Query Blast".

const fs = require('fs');
const { google } = require('googleapis');
const { db } = require('./db');
const { normalizePhone } = require('./util');

let sheetsSvc = null;
let spreadsheetID = '';
let sheetName = 'Blast Log';

async function initSheets() {
  const saPath = process.env.GOOGLE_SERVICE_ACCOUNT_JSON;
  spreadsheetID = process.env.GSHEET_SPREADSHEET_ID || '';
  sheetName = process.env.GSHEET_SHEET_NAME || 'Blast Log';

  if (!saPath || !spreadsheetID) {
    console.log('Sheets export disabled (set GOOGLE_SERVICE_ACCOUNT_JSON + GSHEET_SPREADSHEET_ID di .env untuk aktifkan)');
    return;
  }
  if (!fs.existsSync(saPath)) {
    throw new Error(`service account JSON tidak ditemukan di ${saPath}`);
  }
  const auth = new google.auth.GoogleAuth({
    keyFile: saPath,
    scopes: ['https://www.googleapis.com/auth/spreadsheets'],
  });
  sheetsSvc = google.sheets({ version: 'v4', auth });
  console.log('Sheets export enabled — spreadsheet:', spreadsheetID, '/ sheet:', sheetName);
}

function enabled() {
  return sheetsSvc !== null;
}
function getSpreadsheetID() {
  return spreadsheetID;
}
function sheetURL() {
  return `https://docs.google.com/spreadsheets/d/${spreadsheetID}/edit`;
}
function reportSheetName() {
  return process.env.GSHEET_REPORT_SHEET_NAME || 'Belum Respons';
}
function querySheetName() {
  return process.env.GSHEET_QUERY_SHEET_NAME || 'Query Blast';
}

async function ensureSheetExists(name) {
  const ss = await sheetsSvc.spreadsheets.get({ spreadsheetId: spreadsheetID });
  for (const s of ss.data.sheets || []) {
    if (s.properties && s.properties.title === name) return;
  }
  await sheetsSvc.spreadsheets.batchUpdate({
    spreadsheetId: spreadsheetID,
    requestBody: { requests: [{ addSheet: { properties: { title: name } } }] },
  });
}

async function clearAndWrite(name, clearCols, values) {
  await sheetsSvc.spreadsheets.values.clear({ spreadsheetId: spreadsheetID, range: `${name}!${clearCols}` });
  await sheetsSvc.spreadsheets.values.update({
    spreadsheetId: spreadsheetID,
    range: `${name}!A1`,
    valueInputOption: 'USER_ENTERED',
    requestBody: { values },
  });
}

async function handleExportSheet(req, res) {
  if (!sheetsSvc) {
    return res.status(400).send('Sheets export belum dikonfigurasi. Set GOOGLE_SERVICE_ACCOUNT_JSON + GSHEET_SPREADSHEET_ID di .env, lalu restart.');
  }
  const rows = db
    .prepare(
      `SELECT b.started_at, COALESCE(b.user_name,'') AS user_name, b.user_email, r.phone,
              COALESCE(r.nama_outlet,'') AS nama_outlet, COALESCE(r.nomer_invoice,'') AS nomer_invoice,
              r.status, COALESCE(r.error,'') AS error, COALESCE(r.sent_at,'') AS sent_at,
              COALESCE(r.message,'') AS message, r.blast_log_id
       FROM blast_recipients r JOIN blast_logs b ON r.blast_log_id = b.id ORDER BY r.id ASC`
    )
    .all();

  const values = [
    ['Waktu Blast', 'User', 'Email', 'Phone', 'Nama Outlet', 'Nomor Invoice', 'Status', 'Error', 'Sent At', 'Pesan', 'Blast ID'],
  ];
  for (const r of rows) {
    values.push([
      r.started_at, r.user_name, r.user_email, "'" + r.phone, r.nama_outlet, r.nomer_invoice,
      r.status, r.error, r.sent_at, r.message, r.blast_log_id,
    ]);
  }

  try {
    await clearAndWrite(sheetName, 'A:K', values);
  } catch (e) {
    return res.status(500).send('export sheet: ' + e.message + '. Pastikan service account punya akses Editor.');
  }
  res.json({ ok: true, rows: rows.length, sheet_url: sheetURL(), sheet_name: sheetName });
}

function handleSheetStatus(req, res) {
  res.json({ enabled: sheetsSvc !== null, spreadsheet: spreadsheetID, sheet_name: sheetName, sheet_url: sheetURL() });
}

// syncTeamsFromSheet — baca tab "Query Blast", update kolom team/area di chat_threads
// dengan cocokkan by phone (62...). Return jumlah thread ter-update.
async function syncTeamsFromSheet() {
  if (!sheetsSvc) throw new Error('Sheets belum dikonfigurasi (GOOGLE_SERVICE_ACCOUNT_JSON + GSHEET_SPREADSHEET_ID)');
  const sn = querySheetName();
  const resp = await sheetsSvc.spreadsheets.values.get({ spreadsheetId: spreadsheetID, range: `${sn}!A:Z` });
  const vals = resp.data.values || [];
  if (vals.length < 2) throw new Error(`tab '${sn}' kosong / tidak ada baris data`);

  const idx = {};
  vals[0].forEach((c, i) => (idx[String(c).trim().toLowerCase()] = i));
  if (!('phone' in idx) || !('team' in idx)) {
    throw new Error(`kolom 'Phone' & 'Team' wajib ada di tab '${sn}'`);
  }
  const cell = (row, i) => (i >= 0 && i < row.length ? String(row[i]).trim() : '');

  const map = new Map(); // phone -> {team, area}; baris terakhir menang
  for (let r = 1; r < vals.length; r++) {
    const row = vals[r];
    const phone = normalizePhone(cell(row, idx.phone));
    const team = cell(row, idx.team);
    if (!phone || !team) continue;
    const area = 'area' in idx ? cell(row, idx.area) : '';
    map.set(phone, { team, area });
  }

  const upd = db.prepare(`UPDATE chat_threads SET team = ?, area = ? WHERE phone = ?`);
  let updated = 0;
  for (const [phone, v] of map) {
    const info = upd.run(v.team, v.area, phone);
    if (info.changes > 0) updated++;
  }
  return updated;
}

async function handleSyncTeams(req, res) {
  try {
    const n = await syncTeamsFromSheet();
    res.json({ ok: true, updated: n, sheet: querySheetName() });
  } catch (e) {
    res.status(500).send(e.message);
  }
}

module.exports = {
  initSheets,
  enabled,
  getSpreadsheetID,
  sheetURL,
  ensureSheetExists,
  clearAndWrite,
  reportSheetName,
  handleExportSheet,
  handleSheetStatus,
  handleSyncTeams,
};
