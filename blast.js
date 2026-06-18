'use strict';

// blast.js — port dari blast.go. Parse CSV, jalankan blast attempt-1 dengan jitter,
// track progress, tulis audit + thread + message. sendOne → wa.sendText (whatsapp-web.js
// menangani cek-registrasi + LID internal, jadi tidak ada resolveToLID lagi).

const audit = require('./audit');
const chat = require('./chat');
const wa = require('./wa');
const templates = require('./templates');
const { normalizePhone, renderTemplate, nowISO } = require('./util');

let currentJob = null; // satu blast aktif pada satu waktu

function getJob() {
  return currentJob;
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

// parseCSVText — parser CSV minimal (handle quote "..." dan koma di dalam quote, CRLF).
function parseCSVText(text) {
  const rows = [];
  let row = [];
  let field = '';
  let inQuotes = false;
  for (let i = 0; i < text.length; i++) {
    const ch = text[i];
    if (inQuotes) {
      if (ch === '"') {
        if (text[i + 1] === '"') {
          field += '"';
          i++;
        } else {
          inQuotes = false;
        }
      } else {
        field += ch;
      }
    } else if (ch === '"') {
      inQuotes = true;
    } else if (ch === ',') {
      row.push(field);
      field = '';
    } else if (ch === '\n') {
      row.push(field);
      rows.push(row);
      row = [];
      field = '';
    } else if (ch === '\r') {
      // skip (CRLF)
    } else {
      field += ch;
    }
  }
  if (field !== '' || row.length > 0) {
    row.push(field);
    rows.push(row);
  }
  return rows;
}

// parseCSV — return array recipient {phone, nama_outlet, nomer_invoice, status}.
function parseCSV(buffer) {
  const rows = parseCSVText(buffer.toString('utf8'));
  if (rows.length === 0) throw new Error('csv kosong');
  const header = rows[0].map((h) => h.trim().toLowerCase());
  const idx = {};
  header.forEach((h, i) => (idx[h] = i));
  if (!('phone' in idx) || !('nama_outlet' in idx) || !('nomer_invoice' in idx)) {
    throw new Error('header wajib: phone, nama_outlet, nomer_invoice');
  }
  const out = [];
  for (let r = 1; r < rows.length; r++) {
    const row = rows[r];
    const at = (i) => (i >= 0 && i < row.length ? String(row[i]).trim() : '');
    const phone = normalizePhone(at(idx.phone));
    if (phone === '') continue;
    out.push({
      phone,
      nama_outlet: at(idx.nama_outlet),
      nomer_invoice: at(idx.nomer_invoice),
      message: '',
      status: 'pending',
      error: '',
      sent_at: '',
    });
  }
  return out;
}

function jobSnapshot(job) {
  return {
    id: job.id,
    user_email: job.userEmail,
    user_name: job.userName,
    template: job.template,
    started_at: nowISO(job.startedAt),
    ended_at: job.endedAt ? nowISO(job.endedAt) : null,
    running: job.running,
    min_delay_sec: job.minDelay,
    max_delay_sec: job.maxDelay,
    total: job.total,
    sent: job.sent,
    failed: job.failed,
    skipped: job.skipped,
    items: job.items,
  };
}

async function runBlast(job) {
  try {
    for (let i = 0; i < job.items.length; i++) {
      if (job.cancelled) break;
      const rec = job.items[i];
      const msg = renderTemplate(job.template, rec.nama_outlet, rec.nomer_invoice);
      rec.message = msg;

      try {
        await wa.sendText(rec.phone, msg);
        rec.status = 'sent';
        rec.sent_at = nowISO();
        job.sent++;
      } catch (e) {
        rec.status = 'failed';
        rec.error = e.message;
        job.failed++;
      }

      try {
        audit.recordRecipient(job.auditID, rec);
      } catch (e) {
        console.log('warn: recordRecipient failed for', rec.phone, ':', e.message);
      }

      if (rec.status === 'sent') {
        const now = new Date();
        try {
          chat.upsertThreadBlast(rec.phone, rec.nama_outlet, rec.nomer_invoice, job.auditID, msg, now);
        } catch (e) {
          console.log('warn: upsertThreadBlast:', e.message);
        }
        try {
          chat.recordChatMessage(rec.phone, 'out', msg, '', '', now, job.auditID, job.userEmail, job.userName);
        } catch (e) {
          console.log('warn: recordChatMessage outgoing blast:', e.message);
        }
      }

      if (i < job.items.length - 1 && !job.cancelled) {
        const d = job.minDelay + Math.floor(Math.random() * (job.maxDelay - job.minDelay + 1));
        await sleep(d * 1000);
      }
    }
  } finally {
    // recipient yang masih pending (kalau cancel) → skipped
    for (const it of job.items) {
      if (it.status === 'pending') {
        it.status = 'skipped';
        it.error = 'cancelled';
        job.skipped++;
      }
    }
    job.running = false;
    job.endedAt = new Date();
    try {
      audit.recordBlastEnd(job.auditID, job);
    } catch (e) {
      console.log('audit end failed:', e.message);
    }
  }
}

function handleBlast(req, res) {
  const snap = wa.snapshot();
  if (!snap.loggedIn || !snap.connected) {
    return res.status(400).send('WhatsApp belum terhubung. Scan QR dulu.');
  }
  if (currentJob && currentJob.running) {
    return res.status(409).send('Ada blast yang sedang berjalan.');
  }
  if (!req.file) return res.status(400).send('csv: file wajib (field "csv")');

  let rows;
  try {
    rows = parseCSV(req.file.buffer);
  } catch (e) {
    return res.status(400).send('csv: ' + e.message);
  }
  if (rows.length === 0) return res.status(400).send('csv kosong');

  // Template attempt-1 dari backend (konsisten dgn retry 2/3). Template user diabaikan.
  const template = templates.getAttemptTemplate(1);
  let minDelay = parseInt(req.body.min_delay, 10);
  let maxDelay = parseInt(req.body.max_delay, 10);
  if (isNaN(minDelay)) minDelay = 20;
  if (isNaN(maxDelay)) maxDelay = 40;
  if (minDelay < 2) minDelay = 2;
  if (maxDelay < minDelay) maxDelay = minDelay + 4;

  const user = req.user;
  const job = {
    id: 'job-' + Math.floor(Date.now() / 1000),
    userEmail: user.email,
    userName: user.name,
    template,
    startedAt: new Date(),
    endedAt: null,
    running: true,
    minDelay,
    maxDelay,
    total: rows.length,
    sent: 0,
    failed: 0,
    skipped: 0,
    items: rows,
    cancelled: false,
    auditID: 0,
  };
  currentJob = job;

  try {
    job.auditID = audit.recordBlastStart(job);
  } catch (e) {
    console.log('audit start failed:', e.message);
  }

  runBlast(job); // tidak di-await — jalan di background

  res.json({ ok: true, job_id: job.id, total: rows.length });
}

function handleProgress(req, res) {
  if (!currentJob) return res.json({ job: null });
  res.json({ job: jobSnapshot(currentJob) });
}

module.exports = { handleBlast, handleProgress, parseCSV, getJob };
