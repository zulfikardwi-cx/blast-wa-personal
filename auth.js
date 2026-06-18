'use strict';

// auth.js — port dari auth.go.
// Google OAuth 2.0 (domain @majoo.id) + session cookie HMAC-SHA256 yang skema-nya
// PERSIS sama dengan versi Go: value = b64url(json(user)) + "." + b64url(hmac(p)).
// Cookie first-party, same-origin → jalan di semua browser.

const crypto = require('crypto');
const { OAuth2Client } = require('google-auth-library');

const SESSION_COOKIE = 'blast_session';
const STATE_COOKIE = 'blast_oauth_state';
const ALLOWED_DOMAIN = 'majoo.id';
const SESSION_TTL_SEC = 7 * 24 * 60 * 60; // 7 hari

let oauthClient = null;
let sessionSecret = null;
let redirectURL = '';
let frontendURL = '';
let allowedOrigins = [];

function initAuth() {
  const clientID = process.env.GOOGLE_CLIENT_ID;
  const clientSecret = process.env.GOOGLE_CLIENT_SECRET;
  redirectURL = process.env.OAUTH_REDIRECT_URL;
  const secret = process.env.SESSION_SECRET;
  frontendURL = (process.env.FRONTEND_URL || '').replace(/\/+$/, '');

  if (!clientID || !clientSecret || !redirectURL) {
    throw new Error('GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET, OAUTH_REDIRECT_URL wajib di-set');
  }
  if (!secret) {
    throw new Error('SESSION_SECRET wajib di-set (generate: openssl rand -hex 32)');
  }
  sessionSecret = Buffer.from(secret, 'utf8');

  allowedOrigins = (process.env.ALLOWED_ORIGINS || '')
    .split(',')
    .map((o) => o.trim().replace(/\/+$/, ''))
    .filter(Boolean);

  oauthClient = new OAuth2Client(clientID, clientSecret, redirectURL);

  console.log('=== OAUTH CONFIG ===');
  console.log('  CLIENT_ID:', clientID.slice(0, 20) + '...' + clientID.slice(-30));
  console.log('  REDIRECT_URL:', redirectURL);
  console.log('  FRONTEND_URL:', frontendURL);
  console.log('  ALLOWED_ORIGINS:', allowedOrigins);
  console.log('====================');
}

// ---- HTTPS / cookie helpers (port isSecureRequest + cookieSameSite) ----

function isSecureRequest(req) {
  if (req.secure) return true;
  if (req.headers['x-forwarded-proto'] === 'https') return true;
  const cfv = req.headers['cf-visitor'] || '';
  if (cfv.includes('"scheme":"https"')) return true;
  if (req.headers['cf-connecting-ip']) return true;
  return false;
}

function cookieOpts(req, maxAgeSec) {
  const secure = isSecureRequest(req);
  return {
    path: '/',
    httpOnly: true,
    secure,
    sameSite: secure ? 'none' : 'lax',
    maxAge: maxAgeSec * 1000,
  };
}

// clearOpts — atribut untuk MENGHAPUS cookie. WAJIB sama dgn saat set (path + sameSite
// + secure), kalau tidak browser mengabaikan Set-Cookie penghapus di konteks cross-site
// (frontend Pages → backend tunnel) → logout tidak jalan / tetap login.
function clearOpts(req) {
  const secure = isSecureRequest(req);
  return {
    path: '/',
    httpOnly: true,
    secure,
    sameSite: secure ? 'none' : 'lax',
  };
}

function parseCookies(req) {
  const header = req.headers.cookie || '';
  const out = {};
  for (const part of header.split(';')) {
    const i = part.indexOf('=');
    if (i < 0) continue;
    const k = part.slice(0, i).trim();
    const v = part.slice(i + 1).trim();
    if (k) out[k] = decodeURIComponent(v);
  }
  return out;
}

function b64url(buf) {
  return buf.toString('base64').replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}
function b64urlDecode(s) {
  s = s.replace(/-/g, '+').replace(/_/g, '/');
  return Buffer.from(s, 'base64');
}

function signSession(user) {
  const payload = b64url(Buffer.from(JSON.stringify(user), 'utf8'));
  const mac = crypto.createHmac('sha256', sessionSecret).update(payload).digest();
  return payload + '.' + b64url(mac);
}

function setSessionCookie(req, res, user) {
  res.cookie(SESSION_COOKIE, signSession(user), cookieOpts(req, SESSION_TTL_SEC));
}

// userFrom — validasi + decode cookie. Return user object atau null.
function userFrom(req) {
  const c = parseCookies(req)[SESSION_COOKIE];
  if (!c) return null;
  const parts = c.split('.');
  if (parts.length !== 2) return null;
  const expected = b64url(crypto.createHmac('sha256', sessionSecret).update(parts[0]).digest());
  const a = Buffer.from(expected);
  const b = Buffer.from(parts[1]);
  if (a.length !== b.length || !crypto.timingSafeEqual(a, b)) return null;
  let user;
  try {
    user = JSON.parse(b64urlDecode(parts[0]).toString('utf8'));
  } catch {
    return null;
  }
  if (!user || typeof user.iat !== 'number') return null;
  if (Date.now() / 1000 - user.iat > SESSION_TTL_SEC) return null;
  return user;
}

// ---- CORS (port corsMiddleware) ----

function isOriginAllowed(origin) {
  origin = (origin || '').replace(/\/+$/, '');
  return allowedOrigins.includes(origin);
}

function corsMiddleware(req, res, next) {
  const origin = req.headers.origin;
  if (origin && isOriginAllowed(origin)) {
    res.setHeader('Access-Control-Allow-Origin', origin);
    res.setHeader('Vary', 'Origin');
    res.setHeader('Access-Control-Allow-Credentials', 'true');
    res.setHeader('Access-Control-Allow-Methods', 'GET, POST, OPTIONS');
    res.setHeader('Access-Control-Allow-Headers', 'Content-Type');
    res.setHeader('Access-Control-Max-Age', '86400');
  }
  if (req.method === 'OPTIONS') {
    res.status(204).end();
    return;
  }
  next();
}

// requireAuth — middleware. JSON 401 kalau belum login. Set req.user.
function requireAuth(req, res, next) {
  const user = userFrom(req);
  if (!user) {
    res.status(401).json({ error: 'unauthorized' });
    return;
  }
  req.user = user;
  next();
}

// ---- OAuth handlers ----

function handleLogin(req, res) {
  const state = crypto.randomBytes(16).toString('hex');
  res.cookie(STATE_COOKIE, state, cookieOpts(req, 300));
  const url = oauthClient.generateAuthUrl({
    access_type: 'online',
    scope: ['openid', 'email', 'profile'],
    state,
    hd: ALLOWED_DOMAIN,
    prompt: 'select_account',
  });
  res.redirect(url);
}

async function handleCallback(req, res) {
  const q = req.query;
  if (q.error) return renderAuthError(res, 'Login dibatalkan: ' + q.error);

  const stateC = parseCookies(req)[STATE_COOKIE];
  if (!stateC || stateC !== q.state) {
    return renderAuthError(res, 'State OAuth tidak valid. Coba login ulang.');
  }
  res.clearCookie(STATE_COOKIE, clearOpts(req));

  try {
    const { tokens } = await oauthClient.getToken(q.code);
    const resp = await fetch('https://openidconnect.googleapis.com/v1/userinfo', {
      headers: { Authorization: 'Bearer ' + tokens.access_token },
    });
    if (!resp.ok) return renderAuthError(res, 'Gagal ambil profile: HTTP ' + resp.status);
    const profile = await resp.json();

    if (!profile.email_verified) return renderAuthError(res, 'Email Google belum verified.');
    if (!String(profile.email || '').toLowerCase().endsWith('@' + ALLOWED_DOMAIN)) {
      return renderAuthError(
        res,
        `Akses ditolak. Hanya email @${ALLOWED_DOMAIN} yang diizinkan. Akun Anda: ${profile.email}`
      );
    }

    const user = {
      email: profile.email,
      name: profile.name || '',
      picture: profile.picture || '',
      iat: Math.floor(Date.now() / 1000),
    };
    setSessionCookie(req, res, user);
    console.log('login:', profile.email);
    res.redirect(frontendURL || '/');
  } catch (e) {
    return renderAuthError(res, 'Gagal exchange token: ' + e.message);
  }
}

function handleAuthLogout(req, res) {
  res.clearCookie(SESSION_COOKIE, clearOpts(req));
  if (req.method === 'POST') {
    res.json({ ok: true });
    return;
  }
  res.redirect(frontendURL || '/login.html');
}

function handleMe(req, res) {
  res.json({ user: userFrom(req) });
}

function renderAuthError(res, msg) {
  res.status(403).type('html').send(
    `<!doctype html><html><head><meta charset="utf-8"><title>Login error</title>
<style>body{font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#0b1220;color:#e6ecff;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}
.box{background:#121a2b;border:1px solid #223;border-radius:10px;padding:32px;max-width:480px;text-align:center}
h1{font-size:18px;margin:0 0 16px}.msg{color:#fca5a5;margin-bottom:20px}a{color:#2DBDB6}</style></head><body>
<div class="box"><h1>Login gagal</h1><p class="msg">${msg}</p><a href="/login.html">Coba lagi</a></div></body></html>`
  );
}

module.exports = {
  initAuth,
  corsMiddleware,
  requireAuth,
  userFrom,
  handleLogin,
  handleCallback,
  handleAuthLogout,
  handleMe,
};
