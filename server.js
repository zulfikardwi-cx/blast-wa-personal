'use strict';

// server.js — port dari main.go. Bootstrap Express, daftar semua route (paritas penuh),
// serve docs/ same-origin, init WA (whatsapp-web.js), scheduler retry, graceful shutdown.

require('dotenv').config();

const path = require('path');
const express = require('express');
const multer = require('multer');

const dbmod = require('./db');
const auth = require('./auth');
const wa = require('./wa');
const templates = require('./templates');
const audit = require('./audit');
const chat = require('./chat');
const blast = require('./blast');
const retry = require('./retry');
const report = require('./report');
const sheets = require('./sheets');

const upload = multer({ storage: multer.memoryStorage(), limits: { fileSize: 10 * 1024 * 1024 } });

async function main() {
  dbmod.init();
  templates.load();
  auth.initAuth();
  await sheets.initSheets();

  // WA incoming → inbox state machine
  wa.setIncomingHandler(chat.handleIncoming);
  // filter backfill: hanya tarik ulang chat dari nomor yang pernah di-blast
  wa.setBlastedCheck(chat.isPhoneBlasted);

  // Init WA di background (jangan blok start server — QR muncul di log/endpoint)
  wa.init().catch((e) => console.log('wa init error:', e.message));

  retry.startScheduler();

  const app = express();
  app.set('trust proxy', true);
  app.use(express.json());
  app.use(express.urlencoded({ extended: true }));
  app.use(auth.corsMiddleware); // tambah header CORS + tangani preflight OPTIONS

  // ---- auth (public) ----
  app.get('/auth/login', auth.handleLogin);
  app.get('/auth/callback', auth.handleCallback);
  app.all('/auth/logout', auth.handleAuthLogout);

  // ---- API ----
  app.get('/api/me', auth.handleMe);

  app.get('/api/status', auth.requireAuth, (req, res) => {
    const s = wa.snapshot();
    res.json(s);
  });
  app.get('/api/qr', auth.requireAuth, (req, res) => {
    res.json({ code: wa.getQR() });
  });
  app.post('/api/logout', auth.requireAuth, async (req, res) => {
    try {
      await wa.logout();
      res.json({ ok: true });
    } catch (e) {
      res.status(500).send(e.message);
    }
  });

  app.post('/api/blast', auth.requireAuth, upload.single('csv'), blast.handleBlast);
  app.get('/api/progress', auth.requireAuth, blast.handleProgress);
  app.get('/api/history', auth.requireAuth, audit.handleHistory);

  app.get('/api/sheet-status', auth.requireAuth, sheets.handleSheetStatus);
  app.post('/api/export-sheet', auth.requireAuth, sheets.handleExportSheet);

  app.get('/api/templates', auth.requireAuth, chat.handleTemplates);
  app.get('/api/retry/preview', auth.requireAuth, retry.handleRetryPreview);
  app.post('/api/retry/run-now', auth.requireAuth, retry.handleRetryRunNow);

  app.get('/api/report/unresponsive', auth.requireAuth, report.handleReportUnresponsive);
  app.get('/api/report/unresponsive.csv', auth.requireAuth, report.handleReportUnresponsiveCSV);
  app.post('/api/report/export-sheet', auth.requireAuth, report.handleReportExportSheet);

  // ---- Inbox ----
  app.get('/api/inbox/threads', auth.requireAuth, chat.handleThreads);
  app.get('/api/inbox/messages', auth.requireAuth, chat.handleMessages);
  app.post('/api/inbox/read', auth.requireAuth, chat.handleMarkRead);
  app.post('/api/inbox/status', auth.requireAuth, upload.none(), chat.handleSetStatus);
  app.post('/api/inbox/reply', auth.requireAuth, upload.none(), chat.handleReply);
  app.post('/api/inbox/resolve', auth.requireAuth, chat.handleResolve);
  app.post('/api/inbox/sync-teams', auth.requireAuth, sheets.handleSyncTeams);

  // ---- Frontend (docs/) same-origin ----
  app.use(express.static(path.join(__dirname, 'docs')));

  const port = parseInt(process.env.PORT, 10) || 8080;
  const server = app.listen(port, () => {
    console.log(`listening on http://localhost:${port}`);
  });

  const shutdown = () => {
    console.log('shutting down');
    server.close(() => process.exit(0));
    setTimeout(() => process.exit(0), 3000).unref();
  };
  process.on('SIGINT', shutdown);
  process.on('SIGTERM', shutdown);
}

main().catch((e) => {
  console.error('fatal:', e.message);
  process.exit(1);
});
