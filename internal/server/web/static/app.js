// Shared helpers for every page. No framework, no external assets.
'use strict';

const $ = (id) => document.getElementById(id);

// esc makes user-controlled text safe for innerHTML.
function esc(v) {
  return String(v ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function badge(text) {
  const t = String(text || '');
  return `<span class="badge b-${esc(t)}">${esc(t.replace(/_/g, ' '))}</span>`;
}

// timeAgo(ts, now) -> "just now", "25 sec ago", "5 min ago", "2 hr ago",
// then a short date + time ("16 Oct, 17:32") once it is a day or older.
function timeAgo(ts, now = Date.now()) {
  const s = Math.max(0, Math.round((now - ts) / 1000));
  if (s < 10) return 'just now';
  if (s < 60) return `${s} sec ago`;
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} hr ago`;
  return new Date(ts).toLocaleString([], { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' });
}

// exactTime(ts) -> "16 Oct 2026, 17:32:05" (local time), for tooltips.
function exactTime(ts) {
  return new Date(ts).toLocaleString([], { day: 'numeric', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

function fmtTime(s) {
  if (!s) return '–';
  const d = new Date(s);
  return isNaN(d) ? '–' : d.toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' });
}

// ---- toasts ------------------------------------------------------------------
// toast(kind, text, {id, sticky}) — kind: 'ok' | 'err' | 'warn' | 'info'.
// Text is set with textContent (never innerHTML), so it is safe for any input.
// A toast with an id replaces the previous toast with the same id (used for
// live status such as the feed connection).
const TOAST_MS = { ok: 3500, info: 3500, warn: 6000, err: 7000 };
const TOAST_ICON = { ok: '✓', info: 'i', warn: '!', err: '✕' };

function toast(kind, text, { id, sticky = false } = {}) {
  let box = document.getElementById('toasts');
  if (!box) {
    box = document.createElement('div');
    box.id = 'toasts';
    box.setAttribute('aria-live', 'polite');
    document.body.appendChild(box);
  }
  if (id) box.querySelector(`[data-tid="${CSS.escape(id)}"]`)?.remove();
  while (box.children.length >= 4) box.firstElementChild.remove();
  const el = document.createElement('div');
  el.className = 'toast t-' + kind;
  el.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  if (id) el.dataset.tid = id;
  const icon = document.createElement('span');
  icon.className = 'ti';
  icon.textContent = TOAST_ICON[kind] || 'i';
  const msg = document.createElement('span');
  msg.className = 'tm';
  msg.textContent = text;
  const x = document.createElement('button');
  x.type = 'button';
  x.className = 'tx';
  x.setAttribute('aria-label', 'Dismiss');
  x.textContent = '×';
  const close = () => { el.classList.add('out'); setTimeout(() => el.remove(), 200); };
  x.addEventListener('click', close);
  el.append(icon, msg, x);
  box.appendChild(el);
  if (!sticky) {
    let t = setTimeout(close, TOAST_MS[kind] || 4000);
    el.addEventListener('mouseenter', () => clearTimeout(t)); // pause while reading
    el.addEventListener('mouseleave', () => { t = setTimeout(close, 2000); });
  }
  return el;
}

function dismissToast(id) {
  document.querySelector(`#toasts [data-tid="${CSS.escape(id)}"]`)?.remove();
}

// flash shows a toast on the *next* page (survives a redirect).
function flash(kind, text) {
  try { sessionStorage.setItem('pe_flash', JSON.stringify([kind, text])); } catch { /* storage blocked: skip */ }
}

document.addEventListener('DOMContentLoaded', () => {
  try {
    const f = sessionStorage.getItem('pe_flash');
    if (f) { sessionStorage.removeItem('pe_flash'); const [k, t] = JSON.parse(f); toast(k, t); }
  } catch { /* ignore */ }
});

// Human text for the upstream (market data provider) status reasons.
const UPSTREAM_TEXT = {
  session_in_use: 'the market data provider allows one connection per API key and another client is using it',
  auth_rejected: 'the market data provider rejected our API credentials',
  disconnected: 'the connection to the market data provider was lost; reconnecting',
  connecting: 'connecting to the market data provider',
};

let refreshing = null;

// api calls a JSON endpoint with the session cookie. On 401 it tries one
// silent refresh (rotating refresh token), then retries once. It never
// throws: network failures come back as { ok: false, status: 0 }.
// Rate limiting (429) and server errors (5xx) raise a toast here, once for
// every caller.
async function api(path, { method = 'GET', body, form, retry = true } = {}) {
  const opts = { method, credentials: 'same-origin', headers: {} };
  if (form) opts.body = form;
  else if (body !== undefined) { opts.body = JSON.stringify(body); opts.headers['Content-Type'] = 'application/json'; }
  let res;
  try {
    res = await fetch(path, opts);
  } catch {
    toast('err', 'Network error: cannot reach the server. Check your connection.', { id: 'net' });
    return { ok: false, status: 0, data: { error: 'network error' } };
  }
  if (res.status === 401 && retry && !path.startsWith('/api/v1/auth/')) {
    refreshing = refreshing || fetch('/api/v1/auth/refresh', { method: 'POST', credentials: 'same-origin' })
      .catch(() => ({ ok: false })).finally(() => { setTimeout(() => { refreshing = null; }, 0); });
    const r = await refreshing;
    if (r.ok) return api(path, { method, body, form, retry: false });
  }
  dismissToast('net');
  let data = null;
  const text = await res.text();
  try { data = text ? JSON.parse(text) : null; } catch { data = { error: text }; }
  if (res.status === 429) toast('warn', 'Too many requests. Please wait a moment and try again.', { id: 'rate' });
  else if (res.status >= 500) toast('err', 'Server error. Please try again shortly.', { id: 'server' });
  return { ok: res.ok, status: res.status, data };
}

// pager renders "‹ Prev · 1–10 of N noun · page x of y · Next ›" into el and
// calls go(page). With opts.sizes it adds a "Rows per page" selector that
// calls opts.onSize(size). info = {page, size, total, pages}.
function pager(el, info, noun, go, opts = {}) {
  if (!info || info.total === 0) { el.innerHTML = ''; return; }
  const from = (info.page - 1) * info.size + 1, to = Math.min(info.page * info.size, info.total);
  const sizes = opts.sizes
    ? `<label class="small muted pager-size">Rows per page <select data-p="size">${opts.sizes.map((s) =>
        `<option value="${s}" ${s === info.size ? 'selected' : ''}>${s}</option>`).join('')}</select></label>`
    : '';
  el.innerHTML = `${sizes}<button class="btn ghost" type="button" data-p="prev" ${info.page <= 1 ? 'disabled' : ''}>‹ Prev</button>
    <span class="small muted">${from}–${to} of ${info.total} ${esc(noun)} · page ${info.page} of ${info.pages}</span>
    <button class="btn ghost" type="button" data-p="next" ${info.page >= info.pages ? 'disabled' : ''}>Next ›</button>`;
  el.querySelector('[data-p=prev]').addEventListener('click', () => go(info.page - 1));
  el.querySelector('[data-p=next]').addEventListener('click', () => go(info.page + 1));
  el.querySelector('[data-p=size]')?.addEventListener('change', (e) => opts.onSize(Number(e.target.value)));
}

// errText turns an API error into a readable sentence.
function errText(r, fallback) {
  const e = r.data?.error;
  return e ? e.charAt(0).toUpperCase() + e.slice(1) : fallback;
}

// goLogin redirects to /login, explaining why when a session just ended.
function goLogin() {
  let had = false;
  try { had = sessionStorage.getItem('pe_authed') === '1'; sessionStorage.removeItem('pe_authed'); } catch { /* ignore */ }
  if (had) flash('warn', 'Your session expired. Please log in again.');
  location.href = '/login';
}

// requireMe returns the logged-in profile or redirects to /login.
async function requireMe() {
  const r = await api('/api/v1/me');
  if (!r.ok) {
    if (r.status === 401) goLogin();
    else if (r.status !== 0) toast('err', errText(r, 'Could not load your profile'));
    return null;
  }
  try { sessionStorage.setItem('pe_authed', '1'); } catch { /* ignore */ }
  return r.data;
}

// renderNav fills <header class="top"> with links appropriate to the role.
function renderNav(me, active, title) {
  const h = document.querySelector('header.top');
  const links = [['/', 'Live quotes', 'quotes'], ['/account', 'My account', 'account']];
  if (me && me.role === 'admin') links.push(['/admin', 'Admin', 'admin']);
  h.innerHTML = `<h1>${esc(title)}</h1>
    <nav class="links">${links.map(([href, label, key]) => `<a href="${href}" class="${key === active ? 'on' : ''}">${label}</a>`).join('')}</nav>
    <div class="who" id="who"></div>`;
  if (me) {
    $('who').innerHTML = `<span>${esc(me.email)}</span>${badge(me.role)}<button type="button" id="logout">Log out</button>`;
    $('logout').addEventListener('click', async () => {
      await api('/api/v1/auth/logout', { method: 'POST' });
      try { sessionStorage.removeItem('pe_authed'); } catch { /* ignore */ }
      flash('info', 'You have been logged out.');
      location.href = '/login';
    });
  }
}
