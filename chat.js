'use strict';

// chat.js — port dari chat.go. Inbox state machine (7 bucket), thread upserts,
// recordChatMessage, handler incoming (dipanggil dari wa.js), dan endpoint inbox.

const { db } = require('./db');
const wa = require('./wa');
const templates = require('./templates');
const { truncate, nowISO } = require('./util');

function blank(s) {
  return s === '' || s === undefined || s === null ? null : s;
}

// ---- write helpers ----

// recordChatMessage — return jumlah baris yang BENAR-BENAR di-insert (1 = baru, 0 = sudah
// ada / di-ignore karena wa_message_id duplikat). Dipakai untuk gate update thread supaya
// pesan dobel (event live + backfill saat reconnect) tidak menambah unread/last_message 2x.
function recordChatMessage(phone, direction, body, mediaType, waMsgID, ts, blastID, senderEmail, senderName) {
  const isMedia = mediaType ? 1 : 0;
  const info = db.prepare(
    `INSERT OR IGNORE INTO chat_messages
       (phone, direction, body, is_media, media_type, wa_message_id, timestamp, blast_log_id, sender_email, sender_name)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
  ).run(
    phone,
    direction,
    body,
    isMedia,
    blank(mediaType),
    blank(waMsgID),
    nowISO(ts),
    blastID ? blastID : null,
    blank(senderEmail),
    blank(senderName)
  );
  return info.changes;
}

function isPhoneBlasted(phone) {
  const row = db.prepare(`SELECT COUNT(*) AS c FROM chat_threads WHERE phone = ?`).get(phone);
  return row.c > 0;
}

// upsertThreadBlast — saat BLAST outgoing (attempt 1). SELALU set status=after_blast,
// reset current_attempt=1. Termasuk reset dari done.
function upsertThreadBlast(phone, namaOutlet, nomerInvoice, blastID, preview, ts) {
  const tsStr = nowISO(ts);
  const prev = truncate(preview, 80);
  db.prepare(
    `INSERT INTO chat_threads
       (phone, nama_outlet, nomer_invoice, last_blast_id, last_message_at, last_message_preview,
        last_message_direction, status, assigned_email, assigned_name, unread_count, current_attempt, last_attempt_at, updated_at)
     VALUES (?, ?, ?, ?, ?, ?, 'out', 'after_blast', NULL, NULL, 0, 1, ?, ?)
     ON CONFLICT(phone) DO UPDATE SET
       nama_outlet = COALESCE(NULLIF(excluded.nama_outlet, ''), nama_outlet),
       nomer_invoice = COALESCE(NULLIF(excluded.nomer_invoice, ''), nomer_invoice),
       last_blast_id = COALESCE(excluded.last_blast_id, last_blast_id),
       last_message_at = excluded.last_message_at,
       last_message_preview = excluded.last_message_preview,
       last_message_direction = 'out',
       status = 'after_blast',
       assigned_email = NULL,
       assigned_name = NULL,
       unread_count = 0,
       current_attempt = 1,
       last_attempt_at = excluded.last_attempt_at,
       updated_at = excluded.updated_at`
  ).run(phone, namaOutlet, nomerInvoice, blastID ? blastID : null, tsStr, prev, tsStr, tsStr);
}

// upsertThreadRetry — saat scheduler kirim attempt 2/3. HANYA update last_message +
// current_attempt + last_attempt_at. Tidak ubah status.
function upsertThreadRetry(phone, preview, attemptNum, ts) {
  const tsStr = nowISO(ts);
  db.prepare(
    `UPDATE chat_threads SET
       last_message_at = ?, last_message_preview = ?, last_message_direction = 'out',
       current_attempt = ?, last_attempt_at = ?, updated_at = ?
     WHERE phone = ?`
  ).run(tsStr, truncate(preview, 80), attemptNum, tsStr, tsStr, phone);
}

// upsertThreadAgentReply — agent balas via web → in_progress (assign ke agent).
// done/invalid/on_going LOCKED (status & assignment tidak berubah).
function upsertThreadAgentReply(phone, preview, agentEmail, agentName, ts) {
  const tsStr = nowISO(ts);
  db.prepare(
    `UPDATE chat_threads SET
       last_message_at = ?, last_message_preview = ?, last_message_direction = 'out',
       status = CASE WHEN status IN ('done','invalid','on_going') THEN status ELSE 'in_progress' END,
       assigned_email = CASE WHEN status IN ('done','invalid','on_going') THEN assigned_email ELSE ? END,
       assigned_name = CASE WHEN status IN ('done','invalid','on_going') THEN assigned_name ELSE ? END,
       updated_at = ?
     WHERE phone = ?`
  ).run(tsStr, truncate(preview, 80), agentEmail, agentName, tsStr, phone);
}

// upsertThreadIncoming — user balas. done/invalid/on_going LOCKED, selain itu → open.
function upsertThreadIncoming(phone, preview, ts) {
  const tsStr = nowISO(ts);
  db.prepare(
    `UPDATE chat_threads SET
       last_message_at = ?, last_message_preview = ?, last_message_direction = 'in',
       unread_count = unread_count + 1,
       status = CASE WHEN status IN ('done','invalid','on_going') THEN status ELSE 'open' END,
       updated_at = ?
     WHERE phone = ?`
  ).run(tsStr, truncate(preview, 80), tsStr, phone);
}

// ---- incoming (dipanggil wa.js) ----

// handleIncoming — dipanggil untuk pesan masuk, baik live (event message) MAUPUN
// backfill saat reconnect. Update thread (unread/last_message/status) HANYA kalau pesan
// benar-benar baru (recordChatMessage return 1), supaya pesan yang sudah tercatat tidak
// di-hitung ulang. Inilah yang membuat chat yang masuk saat service OFF ikut muncul saat
// service ON: backfill mengumpankan pesan terlewat, yang baru tersimpan & menggerakkan thread.
function handleIncoming({ phone, body, mediaType, waMessageId, timestamp }) {
  if (!isPhoneBlasted(phone)) {
    return; // bukan nomor yang pernah di-blast → abaikan (tidak masuk inbox)
  }
  let inserted = 1;
  try {
    inserted = recordChatMessage(phone, 'in', body, mediaType, waMessageId, timestamp, 0, '', '');
  } catch (e) {
    console.log('warn: recordChatMessage incoming:', e.message);
  }
  if (!inserted) return; // sudah pernah tercatat (event dobel / backfill ulang) → stop
  try {
    upsertThreadIncoming(phone, body, timestamp);
  } catch (e) {
    console.log('warn: upsertThreadIncoming:', e.message);
  }
  console.log('  → inbox: incoming from', phone, '—', truncate(body, 40));
}

// ---- HTTP handlers ----

function handleThreads(req, res) {
  const status = req.query.status || '';
  const team = req.query.team || '';
  const conds = [];
  const args = [];
  if (status && status !== 'all') {
    conds.push('status = ?');
    args.push(status);
  }
  if (team && team !== 'all') {
    conds.push('team = ?');
    args.push(team);
  }
  const where = conds.length ? 'WHERE ' + conds.join(' AND ') : '';
  // Urut by last_message_at (bukan updated_at) supaya buka/klik thread tidak ubah urutan.
  const q =
    `SELECT phone, COALESCE(nama_outlet,'') AS nama_outlet, COALESCE(last_blast_id,0) AS last_blast_id,
            COALESCE(last_message_at,'') AS last_message_at, COALESCE(last_message_preview,'') AS last_preview,
            COALESCE(last_message_direction,'') AS last_direction, unread_count, status,
            COALESCE(assigned_email,'') AS assigned_email, COALESCE(assigned_name,'') AS assigned_name,
            COALESCE(team,'') AS team, COALESCE(area,'') AS area
     FROM chat_threads ${where} ORDER BY last_message_at DESC, phone ASC LIMIT 200`;
  const threads = db.prepare(q).all(...args);

  const cnt = (s) =>
    db.prepare(`SELECT COUNT(*) AS c FROM chat_threads WHERE status = ?`).get(s).c;
  const counts = {
    after_blast: cnt('after_blast'),
    open: cnt('open'),
    in_progress: cnt('in_progress'),
    on_going: cnt('on_going'),
    force_close: cnt('force_close'),
    done: cnt('done'),
    invalid: cnt('invalid'),
    unread: db.prepare(`SELECT COALESCE(SUM(unread_count),0) AS c FROM chat_threads`).get().c,
  };

  const teams = db
    .prepare(`SELECT DISTINCT team FROM chat_threads WHERE team IS NOT NULL AND team != '' ORDER BY team`)
    .all()
    .map((r) => r.team);

  res.json({ threads, counts, teams });
}

function handleMessages(req, res) {
  const phone = req.query.phone;
  if (!phone) return res.status(400).send('phone required');
  const rows = db
    .prepare(
      `SELECT id, direction, COALESCE(body,'') AS body, is_media, COALESCE(media_type,'') AS media_type,
              timestamp, COALESCE(sender_email,'') AS sender_email, COALESCE(sender_name,'') AS sender_name
       FROM chat_messages WHERE phone = ? ORDER BY id ASC LIMIT 500`
    )
    .all(phone)
    .map((m) => ({ ...m, is_media: m.is_media === 1 }));

  const meta =
    db
      .prepare(
        `SELECT COALESCE(nama_outlet,'') AS nama_outlet, status, COALESCE(assigned_email,'') AS assigned_email,
                COALESCE(assigned_name,'') AS assigned_name FROM chat_threads WHERE phone = ?`
      )
      .get(phone) || { nama_outlet: '', status: '', assigned_email: '', assigned_name: '' };

  res.json({
    phone,
    nama_outlet: meta.nama_outlet,
    status: meta.status,
    assigned_email: meta.assigned_email,
    assigned_name: meta.assigned_name,
    messages: rows,
    closing_template: templates.renderClosing(meta.nama_outlet),
  });
}

function handleMarkRead(req, res) {
  const phone = req.query.phone;
  if (!phone) return res.status(400).send('phone required');
  db.prepare(`UPDATE chat_threads SET unread_count = 0, updated_at = datetime('now') WHERE phone = ?`).run(phone);
  res.json({ ok: true });
}

const VALID_STATUS = ['open', 'in_progress', 'done', 'invalid', 'on_going', 'force_close'];

function handleSetStatus(req, res) {
  const phone = req.query.phone;
  if (!phone) return res.status(400).send('phone required');
  const status = (req.body && req.body.status) || '';
  if (!VALID_STATUS.includes(status)) {
    return res.status(400).send('status invalid (open|in_progress|done|invalid|on_going|force_close)');
  }
  const user = req.user;
  let assignedEmail = null;
  let assignedName = null;
  if (status === 'in_progress' || status === 'on_going') {
    assignedEmail = user.email;
    assignedName = user.name;
  }
  db.prepare(
    `UPDATE chat_threads SET status = ?, assigned_email = ?, assigned_name = ?, updated_at = datetime('now') WHERE phone = ?`
  ).run(status, assignedEmail, assignedName, phone);
  res.json({ ok: true });
}

function handleTemplates(req, res) {
  const t = templates.getAttemptTemplates();
  res.json({
    attempt_1: t[0],
    attempt_2: t[1],
    attempt_3: t[2],
    retry_window_hour: require('./retry').retryWindowHour(),
    closing: templates.getClosing(),
  });
}

async function handleReply(req, res) {
  const snap = wa.snapshot();
  if (!snap.loggedIn || !snap.connected) return res.status(400).send('WhatsApp belum terhubung');
  const phone = req.query.phone;
  if (!phone) return res.status(400).send('phone required');
  const body = ((req.body && req.body.body) || '').trim();
  if (!body) return res.status(400).send('body kosong');
  const user = req.user;
  let result;
  try {
    result = await wa.sendText(phone, body);
  } catch (e) {
    return res.status(500).send('send: ' + e.message);
  }
  const now = new Date();
  try {
    recordChatMessage(phone, 'out', body, '', result.id, now, 0, user.email, user.name);
  } catch (e) {
    console.log('warn: recordChatMessage:', e.message);
  }
  try {
    upsertThreadAgentReply(phone, body, user.email, user.name, now);
  } catch (e) {
    console.log('warn: upsertThreadAgentReply:', e.message);
  }
  res.json({ ok: true, id: result.id });
}

// handleResolve — set done + kirim closing. Setelah done, reply user tidak ubah status.
async function handleResolve(req, res) {
  const snap = wa.snapshot();
  if (!snap.loggedIn || !snap.connected) return res.status(400).send('WhatsApp belum terhubung');
  const phone = req.query.phone;
  if (!phone) return res.status(400).send('phone required');
  const user = req.user;

  const row = db.prepare(`SELECT COALESCE(nama_outlet,'') AS n FROM chat_threads WHERE phone = ?`).get(phone);
  const closing = templates.renderClosing(row ? row.n : '');

  let result;
  try {
    result = await wa.sendText(phone, closing);
  } catch (e) {
    return res.status(500).send('send closing: ' + e.message);
  }
  const now = new Date();
  const tsStr = nowISO(now);
  try {
    recordChatMessage(phone, 'out', closing, '', result.id, now, 0, user.email, user.name);
  } catch (e) {
    console.log('warn: recordChatMessage closing:', e.message);
  }
  db.prepare(
    `UPDATE chat_threads SET last_message_at = ?, last_message_preview = ?, last_message_direction = 'out',
       status = 'done', assigned_email = ?, assigned_name = ?, updated_at = ? WHERE phone = ?`
  ).run(tsStr, truncate(closing, 80), user.email, user.name, tsStr, phone);

  res.json({ ok: true, closing_sent: true });
}

module.exports = {
  recordChatMessage,
  isPhoneBlasted,
  upsertThreadBlast,
  upsertThreadRetry,
  upsertThreadAgentReply,
  upsertThreadIncoming,
  handleIncoming,
  handleThreads,
  handleMessages,
  handleMarkRead,
  handleSetStatus,
  handleTemplates,
  handleReply,
  handleResolve,
};
