'use strict';

// audit.js — port dari audit.go. Catat blast_logs + blast_recipients, endpoint history.

const { db } = require('./db');
const { nowISO } = require('./util');

function recordBlastStart(job) {
  const info = db
    .prepare(
      `INSERT INTO blast_logs (user_email, user_name, started_at, template, total, min_delay, max_delay)
       VALUES (?, ?, ?, ?, ?, ?, ?)`
    )
    .run(job.userEmail, job.userName, nowISO(job.startedAt), job.template, job.total, job.minDelay, job.maxDelay);
  return info.lastInsertRowid;
}

function recordRecipient(blastLogID, rec) {
  if (!blastLogID) return;
  db.prepare(
    `INSERT INTO blast_recipients (blast_log_id, phone, nama_outlet, nomer_invoice, status, error, message, sent_at)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
  ).run(
    blastLogID,
    rec.phone,
    rec.nama_outlet,
    rec.nomer_invoice,
    rec.status,
    rec.error || '',
    rec.message || '',
    rec.sent_at || ''
  );
}

function recordBlastEnd(id, job) {
  if (!id) return;
  db.prepare(`UPDATE blast_logs SET ended_at = ?, sent = ?, failed = ?, skipped = ? WHERE id = ?`).run(
    job.endedAt ? nowISO(job.endedAt) : '',
    job.sent,
    job.failed,
    job.skipped,
    id
  );
}

function handleHistory(req, res) {
  const rows = db
    .prepare(
      `SELECT id, user_email, COALESCE(user_name,'') AS user_name, started_at, COALESCE(ended_at,'') AS ended_at,
              template, total, sent, failed, skipped, COALESCE(min_delay,0) AS min_delay, COALESCE(max_delay,0) AS max_delay
       FROM blast_logs ORDER BY id DESC LIMIT 100`
    )
    .all();
  res.json({ logs: rows });
}

module.exports = { recordBlastStart, recordRecipient, recordBlastEnd, handleHistory };
