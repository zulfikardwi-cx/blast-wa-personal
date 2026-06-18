'use strict';

// retry.js — port dari retry.go. Scheduler harian window jam RETRY_WINDOW_HOUR (WIB),
// proses antrian "Belum Respons" (after_blast + in_progress, attempt<3), maks 1x/hari
// per thread. RETRY_ENABLED=false → auto-cron mati, tapi force manual tetap jalan.

const { DateTime } = require('luxon');
const { db } = require('./db');
const wa = require('./wa');
const chat = require('./chat');
const templates = require('./templates');
const { renderTemplate, nowISO } = require('./util');

const ZONE = 'Asia/Jakarta';

let intervalMin = 30;
let windowHour = 9;
let minJitter = 20;
let maxJitter = 40;
let running = false; // single-flight

function atoiEnv(key, def) {
  const v = parseInt(process.env[key], 10);
  return isNaN(v) ? def : v;
}
function boolEnv(key, def) {
  const v = process.env[key];
  if (v === undefined || v === '') return def;
  return ['true', '1', 'yes', 'on'].includes(v.toLowerCase());
}

function retryWindowHour() {
  return windowHour;
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

function startScheduler() {
  intervalMin = atoiEnv('RETRY_CHECK_INTERVAL_MINUTES', 30);
  windowHour = atoiEnv('RETRY_WINDOW_HOUR', 9);
  minJitter = atoiEnv('RETRY_SEND_MIN_DELAY', 20);
  maxJitter = atoiEnv('RETRY_SEND_MAX_DELAY', 40);

  if (!boolEnv('RETRY_ENABLED', true)) {
    console.log('retry scheduler: DISABLED (RETRY_ENABLED=false) — auto-cron jam-9 MATI. Force manual tetap bisa.');
    return;
  }

  console.log(
    `retry scheduler: interval=${intervalMin}m window=${String(windowHour).padStart(2, '0')}:00 WIB ` +
      `jitter=${minJitter}-${maxJitter}s (max 1x/hari per thread)`
  );

  setTimeout(() => {
    processRetries(false, 0).catch((e) => console.log('retry tick error:', e.message));
    setInterval(() => {
      processRetries(false, 0).catch((e) => console.log('retry tick error:', e.message));
    }, intervalMin * 60 * 1000);
  }, 2 * 60 * 1000); // delay awal 2 menit (tunggu WA connect)
}

// startOfTodayMillis — instant 00:00 WIB hari ini.
function startOfTodayMillis() {
  return DateTime.now().setZone(ZONE).startOf('day').toMillis();
}

// attemptedToday — true kalau last_attempt_at jatuh di hari kalender yang sama (WIB).
function attemptedToday(lastAt, startMillis) {
  if (!lastAt) return false;
  const dt = DateTime.fromISO(lastAt);
  if (!dt.isValid) return false;
  return dt.toMillis() >= startMillis;
}

// stillNeedsRetry — re-check sebelum send (race guard).
function stillNeedsRetry(phone, startMillis) {
  const row = db
    .prepare(`SELECT status, current_attempt, COALESCE(last_attempt_at,'') AS last_attempt_at FROM chat_threads WHERE phone = ?`)
    .get(phone);
  if (!row) return false;
  if (row.status !== 'after_blast' && row.status !== 'in_progress') return false;
  if (row.current_attempt >= 3) return false;
  return !attemptedToday(row.last_attempt_at, startMillis);
}

async function processRetries(force, limit) {
  if (running) {
    console.log('retry: previous batch still running, skip');
    return;
  }
  running = true;
  try {
    const now = DateTime.now().setZone(ZONE);
    if (!force && now.hour !== windowHour) return;

    const snap = wa.snapshot();
    if (!snap.loggedIn || !snap.connected) return;

    const startMillis = startOfTodayMillis();
    const rows = db
      .prepare(
        `SELECT phone, COALESCE(nama_outlet,'') AS nama_outlet, COALESCE(nomer_invoice,'') AS nomer_invoice,
                current_attempt, COALESCE(last_attempt_at,'') AS last_attempt_at
         FROM chat_threads
         WHERE status IN ('after_blast','in_progress') AND current_attempt < 3
         ORDER BY current_attempt DESC, last_attempt_at ASC`
      )
      .all();

    let batch = rows.filter((r) => !attemptedToday(r.last_attempt_at, startMillis));
    if (limit > 0 && batch.length > limit) batch = batch.slice(0, limit);
    if (batch.length === 0) return;

    const mode = force ? 'FORCE (manual)' : `window ${String(windowHour).padStart(2, '0')}:00 WIB`;
    console.log(`retry: ${mode} — antrikan ${batch.length} threads (after_blast + in_progress)`);

    let sent = 0;
    let failed = 0;
    for (let i = 0; i < batch.length; i++) {
      const r = batch[i];
      if (!stillNeedsRetry(r.phone, startMillis)) continue;

      const nextAttempt = r.current_attempt + 1;
      const body = renderTemplate(templates.getAttemptTemplate(nextAttempt), r.nama_outlet, r.nomer_invoice);

      try {
        await wa.sendText(r.phone, body);
      } catch (e) {
        console.log(`retry: phone=${r.phone} attempt=${nextAttempt} FAILED: ${e.message}`);
        failed++;
        continue;
      }

      const now2 = new Date();
      try {
        chat.upsertThreadRetry(r.phone, body, nextAttempt, now2);
      } catch (e) {
        console.log('retry: upsertThreadRetry error:', e.message);
      }
      try {
        chat.recordChatMessage(r.phone, 'out', body, '', '', now2, 0, 'system@retry', `Auto Attempt ${nextAttempt}`);
      } catch (e) {
        console.log('retry: recordChatMessage error:', e.message);
      }
      console.log(`retry: phone=${r.phone} attempt ${nextAttempt} sent`);

      if (nextAttempt >= 3) {
        // Attempt 3 terkirim & masih after_blast → force_close (keluar dari antrian).
        db.prepare(
          `UPDATE chat_threads SET status='force_close', updated_at=datetime('now') WHERE phone=? AND status='after_blast'`
        ).run(r.phone);
        console.log(`retry: phone=${r.phone} attempt 3 terkirim → force_close (no response)`);
      }
      sent++;

      if (i < batch.length - 1) {
        const d = maxJitter > minJitter ? minJitter + Math.floor(Math.random() * (maxJitter - minJitter + 1)) : minJitter;
        await sleep(d * 1000);
      }
    }
    console.log(`retry: batch done — sent=${sent} failed=${failed}`);
  } finally {
    running = false;
  }
}

// ---- HTTP handlers ----

function handleRetryPreview(req, res) {
  const startMillis = startOfTodayMillis();
  const rows = db
    .prepare(
      `SELECT phone, COALESCE(nama_outlet,'') AS nama_outlet, COALESCE(nomer_invoice,'') AS nomer_invoice,
              current_attempt, status, COALESCE(last_attempt_at,'') AS last_attempt_at
       FROM chat_threads
       WHERE status IN ('after_blast','in_progress') AND current_attempt < 3
       ORDER BY current_attempt DESC, last_attempt_at ASC`
    )
    .all();
  const out = [];
  for (const r of rows) {
    if (attemptedToday(r.last_attempt_at, startMillis)) continue;
    out.push({
      phone: r.phone,
      nama_outlet: r.nama_outlet,
      nomer_invoice: r.nomer_invoice,
      current_attempt: r.current_attempt,
      next_attempt: r.current_attempt + 1,
      status: r.status,
    });
  }
  res.json({ rows: out, count: out.length });
}

function handleRetryRunNow(req, res) {
  const snap = wa.snapshot();
  if (!snap.loggedIn || !snap.connected) {
    return res.status(400).send('WhatsApp belum terhubung — tidak ada yang dikirim.');
  }
  let limit = 0;
  const v = parseInt(req.query.limit, 10);
  if (!isNaN(v) && v > 0) limit = v;
  processRetries(true, limit).catch((e) => console.log('retry run-now error:', e.message));
  console.log(`retry: FORCE dipicu manual via API (limit=${limit})`);
  res.json({ ok: true, started: true, limit });
}

module.exports = { startScheduler, processRetries, handleRetryPreview, handleRetryRunNow, retryWindowHour };
