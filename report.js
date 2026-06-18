'use strict';

// report.js — port dari report.go. Report "Belum Respons" = thread after_blast +
// in_progress. Begitu user balas (→ open) / done / invalid, otomatis keluar dari list.

const { db } = require('./db');
const sheets = require('./sheets');

const HEADER = ['Phone', 'Nama Outlet', 'Nomor Invoice', 'Status Attempt 1', 'Status Attempt 2', 'Status Attempt 3'];

// attStatus — "No Response" kalau attempt ke-n sudah dikirim (current >= n), else "-".
function attStatus(n, current) {
  return current >= n ? 'No Response' : '-';
}

function queryUnresponsive() {
  const rows = db
    .prepare(
      `SELECT phone, COALESCE(nama_outlet,'') AS nama_outlet, COALESCE(nomer_invoice,'') AS nomer_invoice, current_attempt
       FROM chat_threads WHERE status IN ('after_blast','in_progress')
       ORDER BY current_attempt DESC, last_attempt_at ASC`
    )
    .all();
  return rows.map((r) => ({
    phone: r.phone,
    nama_outlet: r.nama_outlet,
    nomer_invoice: r.nomer_invoice,
    attempt1: attStatus(1, r.current_attempt),
    attempt2: attStatus(2, r.current_attempt),
    attempt3: attStatus(3, r.current_attempt),
  }));
}

function handleReportUnresponsive(req, res) {
  const list = queryUnresponsive();
  res.json({ rows: list, count: list.length });
}

function csvField(s) {
  s = String(s == null ? '' : s);
  if (/[",\n]/.test(s)) return '"' + s.replace(/"/g, '""') + '"';
  return s;
}

function handleReportUnresponsiveCSV(req, res) {
  const list = queryUnresponsive();
  res.setHeader('Content-Type', 'text/csv; charset=utf-8');
  res.setHeader('Content-Disposition', 'attachment; filename="belum-respons.csv"');
  const lines = [HEADER.map(csvField).join(',')];
  for (const r of list) {
    lines.push([r.phone, r.nama_outlet, r.nomer_invoice, r.attempt1, r.attempt2, r.attempt3].map(csvField).join(','));
  }
  res.send(lines.join('\n') + '\n');
}

async function handleReportExportSheet(req, res) {
  if (!sheets.enabled()) {
    return res.status(400).send('Sheets export belum dikonfigurasi. Set GOOGLE_SERVICE_ACCOUNT_JSON + GSHEET_SPREADSHEET_ID di .env, lalu restart.');
  }
  const list = queryUnresponsive();
  const sn = sheets.reportSheetName();
  try {
    await sheets.ensureSheetExists(sn);
    const values = [HEADER.slice()];
    for (const r of list) {
      values.push(["'" + r.phone, r.nama_outlet, r.nomer_invoice, r.attempt1, r.attempt2, r.attempt3]);
    }
    await sheets.clearAndWrite(sn, 'A:F', values);
  } catch (e) {
    return res.status(500).send('export report: ' + e.message + '. Pastikan service account punya akses Editor.');
  }
  res.json({ ok: true, rows: list.length, sheet_url: sheets.sheetURL(), sheet_name: sn });
}

module.exports = {
  handleReportUnresponsive,
  handleReportUnresponsiveCSV,
  handleReportExportSheet,
};
