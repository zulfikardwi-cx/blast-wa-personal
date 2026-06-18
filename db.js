'use strict';

// db.js — port dari audit.go + chat.go (CREATE TABLE).
// Satu file SQLite (session/audit.db). store.db whatsmeow sudah tidak ada (diganti
// LocalAuth folder whatsapp-web.js). Skema identik dengan versi Go supaya frontend
// docs/ dan semua query lain tidak perlu berubah.

const fs = require('fs');
const path = require('path');
const Database = require('better-sqlite3');

const DB_DIR = path.join(__dirname, 'session');
const DB_PATH = path.join(DB_DIR, 'audit.db');

if (!fs.existsSync(DB_DIR)) fs.mkdirSync(DB_DIR, { recursive: true });

const db = new Database(DB_PATH);
db.pragma('journal_mode = WAL');
db.pragma('foreign_keys = ON');

function init() {
  db.exec(`
    CREATE TABLE IF NOT EXISTS blast_logs (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      user_email TEXT NOT NULL,
      user_name TEXT,
      started_at TEXT NOT NULL,
      ended_at TEXT,
      template TEXT NOT NULL,
      total INTEGER NOT NULL DEFAULT 0,
      sent INTEGER NOT NULL DEFAULT 0,
      failed INTEGER NOT NULL DEFAULT 0,
      skipped INTEGER NOT NULL DEFAULT 0,
      min_delay INTEGER,
      max_delay INTEGER
    );
    CREATE INDEX IF NOT EXISTS idx_blast_logs_started ON blast_logs(started_at DESC);
    CREATE INDEX IF NOT EXISTS idx_blast_logs_user ON blast_logs(user_email);

    CREATE TABLE IF NOT EXISTS blast_recipients (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      blast_log_id INTEGER NOT NULL,
      phone TEXT NOT NULL,
      nama_outlet TEXT,
      nomer_invoice TEXT,
      status TEXT NOT NULL,
      error TEXT,
      message TEXT,
      sent_at TEXT,
      created_at TEXT NOT NULL DEFAULT (datetime('now'))
    );
    CREATE INDEX IF NOT EXISTS idx_recipients_blast ON blast_recipients(blast_log_id);
    CREATE INDEX IF NOT EXISTS idx_recipients_phone ON blast_recipients(phone);

    CREATE TABLE IF NOT EXISTS chat_threads (
      phone TEXT PRIMARY KEY,
      nama_outlet TEXT,
      last_blast_id INTEGER,
      last_message_at TEXT,
      last_message_preview TEXT,
      last_message_direction TEXT,
      unread_count INTEGER NOT NULL DEFAULT 0,
      status TEXT NOT NULL DEFAULT 'open',
      assigned_email TEXT,
      assigned_name TEXT,
      created_at TEXT NOT NULL DEFAULT (datetime('now')),
      updated_at TEXT NOT NULL DEFAULT (datetime('now'))
    );
    CREATE INDEX IF NOT EXISTS idx_threads_updated ON chat_threads(updated_at DESC);
    CREATE INDEX IF NOT EXISTS idx_threads_status ON chat_threads(status);

    CREATE TABLE IF NOT EXISTS chat_messages (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      phone TEXT NOT NULL,
      direction TEXT NOT NULL,
      body TEXT,
      is_media INTEGER NOT NULL DEFAULT 0,
      media_type TEXT,
      wa_message_id TEXT,
      timestamp TEXT NOT NULL,
      blast_log_id INTEGER,
      sender_email TEXT,
      sender_name TEXT,
      created_at TEXT NOT NULL DEFAULT (datetime('now'))
    );
    CREATE INDEX IF NOT EXISTS idx_messages_phone ON chat_messages(phone, id);
    CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_wa_id ON chat_messages(wa_message_id) WHERE wa_message_id IS NOT NULL;
  `);

  // Migrasi kolom retry/team ke chat_threads (idempoten — SQLite < 3.35 tidak punya
  // ADD COLUMN IF NOT EXISTS, jadi cek lewat PRAGMA table_info).
  const cols = new Set(db.prepare(`PRAGMA table_info(chat_threads)`).all().map((c) => c.name));
  const addColumns = [
    ['nomer_invoice', 'TEXT'],
    ['current_attempt', 'INTEGER NOT NULL DEFAULT 1'],
    ['last_attempt_at', 'TEXT'],
    ['team', 'TEXT'],
    ['area', 'TEXT'],
  ];
  for (const [col, def] of addColumns) {
    if (!cols.has(col)) db.exec(`ALTER TABLE chat_threads ADD COLUMN ${col} ${def}`);
  }
}

module.exports = { db, init, DB_PATH };
