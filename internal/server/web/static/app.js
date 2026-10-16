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

function fmtTime(s) {
  if (!s) return '–';
  const d = new Date(s);
  return isNaN(d) ? '–' : d.toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' });
}

let refreshing = null;

// api calls a JSON endpoint with the session cookie. On 401 it tries one
// silent refresh (rotating refresh token), then retries once.
async function api(path, { method = 'GET', body, form, retry = true } = {}) {
  const opts = { method, credentials: 'same-origin', headers: {} };
  if (form) opts.body = form;
  else if (body !== undefined) { opts.body = JSON.stringify(body); opts.headers['Content-Type'] = 'application/json'; }
  const res = await fetch(path, opts);
  if (res.status === 401 && retry && !path.startsWith('/api/v1/auth/')) {
    refreshing = refreshing || fetch('/api/v1/auth/refresh', { method: 'POST', credentials: 'same-origin' }).finally(() => { setTimeout(() => { refreshing = null; }, 0); });
    const r = await refreshing;
    if (r.ok) return api(path, { method, body, form, retry: false });
  }
  let data = null;
  const text = await res.text();
  try { data = text ? JSON.parse(text) : null; } catch { data = { error: text }; }
  return { ok: res.ok, status: res.status, data };
}

function showMsg(el, ok, text) {
  el.className = 'msg show ' + (ok ? 'ok' : 'err');
  el.textContent = text;
}

// requireMe returns the logged-in profile or redirects to /login.
async function requireMe() {
  const r = await api('/api/v1/me');
  if (!r.ok) { location.href = '/login'; return null; }
  return r.data;
}

// renderNav fills <header class="top"> with links appropriate to the role.
function renderNav(me, active, title) {
  const h = document.querySelector('header.top');
  const links = [['/', 'Live quotes', 'quotes']];

  if (me && me.role === 'admin') {
    links.push(['/admin', 'Admin', 'admin']);
  } else {
    links.push(['/account', 'My account', 'account']);
  }
  h.innerHTML = `<h1>${esc(title)}</h1>
    <nav class="links">${links.map(([href, label, key]) => `<a href="${href}" class="${key === active ? 'on' : ''}">${label}</a>`).join('')}</nav>
    <div class="who" id="who"></div>`;
  if (me) {
    $('who').innerHTML = `<span>${esc(me.email)}</span>${badge(me.role)}<button type="button" id="logout">Log out</button>`;
    $('logout').addEventListener('click', async () => {
      await api('/api/v1/auth/logout', { method: 'POST' });
      location.href = '/login';
    });
  }
}
