'use strict';
(async () => {
  const me = await requireMe();
  if (!me) return;
  if (me.role !== 'admin') { flash('err', 'Admin access required. You were redirected to your account.'); location.href = '/account'; return; }
  renderNav(me, 'admin', 'Admin Console');

  const PAGE_SIZE = 10;
  let merchants = [], current = null, counts = {}, mPage = 1, aPage = 1, mInfo = null, aInfo = null;
  const syms = (list) => !list || !list.length ? '–' : list[0] === '*' ? 'all symbols' : list.map(esc).join(', ');

  // pager renders "‹ Prev  Page x of y · n items  Next ›" and calls go(page).
  function pager(el, info, noun, go) {
    if (!info || info.total === 0) { el.innerHTML = ''; return; }
    const from = (info.page - 1) * info.size + 1, to = Math.min(info.page * info.size, info.total);
    el.innerHTML = `<button class="btn ghost" type="button" data-p="prev" ${info.page <= 1 ? 'disabled' : ''}>‹ Prev</button>
      <span class="small muted">${from}–${to} of ${info.total} ${noun} · page ${info.page} of ${info.pages}</span>
      <button class="btn ghost" type="button" data-p="next" ${info.page >= info.pages ? 'disabled' : ''}>Next ›</button>`;
    el.querySelector('[data-p=prev]').addEventListener('click', () => go(info.page - 1));
    el.querySelector('[data-p=next]').addEventListener('click', () => go(info.page + 1));
  }

  let lastUp = null;
  function upstreamTile(hz) {
    const s = hz?.upstream_status;
    if (!s) return ['Upstream feed', '?'];
    if (lastUp !== null && lastUp !== s.up) {
      if (s.up) toast('ok', 'Upstream market feed recovered.', { id: 'upstream' });
      else toast('warn', `Upstream market feed down: ${UPSTREAM_TEXT[s.reason] || s.reason}.`, { id: 'upstream', sticky: true });
    } else if (lastUp === null && !s.up && s.reason !== 'connecting') {
      toast('warn', `Upstream market feed down: ${UPSTREAM_TEXT[s.reason] || s.reason}.`, { id: 'upstream', sticky: true });
    }
    lastUp = s.up;
    return ['Upstream feed', s.up ? 'up' : (s.reason || 'down').replace(/_/g, ' ')];
  }

  function tiles(ss, hz) {
    $('tiles').innerHTML = [
      ['Merchants', counts.merchants || 0],
      ['Pending review', counts.pending || 0],
      ['Verified', counts.verified || 0],
      ['Disabled', counts.disabled || 0],
      ['Live sessions', ss.length],
      upstreamTile(hz),
    ].map(([l, v]) => `<div class="tile"><b class="${typeof v === 'number' ? '' : 'txt'}">${esc(v)}</b><span>${esc(l)}</span></div>`).join('');
  }

  function table() {
    $('merchants').innerHTML = merchants.length ? merchants.map((m) => `<tr>
        <td><b>${esc(m.name)}</b><br><span class="small muted">${esc(m.email)}</span></td>
        <td>${badge(m.role)}${m.disabled ? ' ' + badge('disabled') : ''}</td>
        <td>${badge(m.kyc.status)}${m.kyc.expires_at ? `<br><span class="small muted">until ${fmtTime(m.kyc.expires_at)}</span>` : ''}</td>
        <td>${badge(m.can_trade ? 'yes' : 'no')}</td>
        <td class="mono small">${m.api_key ? esc(m.api_key.id) + `<br><span class="muted">used ${fmtTime(m.api_key.last_used_at)}</span>` : '–'}</td>
        <td>${m.sessions ? `<b>${m.sessions}</b> · ${syms(m.symbols)}` : '<span class="muted">offline</span>'}</td>
        <td class="small">${fmtTime(m.created_at)}</td>
        <td>${m.role === 'admin' ? '' : `<button class="btn ghost" type="button" data-id="${esc(m.id)}">Review</button>`}</td>
      </tr>`).join('') : '<tr><td colspan="8" class="empty">No merchants match.</td></tr>';
    $('merchants').querySelectorAll('button[data-id]').forEach((b) => b.addEventListener('click', () => openReview(b.dataset.id)));
    pager($('m-pager'), mInfo, 'merchants', (p) => { mPage = p; loadMerchants(); });
  }

  function sessions(ss) {
    $('sessions').innerHTML = ss.length ? ss.map((s) => `<tr>
        <td>${s.email ? esc(s.email) : '<span class="muted">(unauthenticated)</span>'}</td>
        <td class="mono small">${esc(s.remote)}</td><td>${badge(s.authenticated ? 'yes' : 'no')}</td>
        <td>${esc(s.format)}</td><td>${syms(s.symbols)}</td><td>${s.frames}</td><td class="small">${fmtTime(s.connected_at)}</td>
      </tr>`).join('') : '<tr><td colspan="7" class="empty">No live connections.</td></tr>';
  }

  async function loadMerchants() {
    const qs = new URLSearchParams({ page: mPage, size: PAGE_SIZE, q: $('filter').value.trim(), status: $('status').value });
    const r = await api('/api/v1/admin/merchants?' + qs);
    if (r.status === 401) { goLogin(); return false; }
    if (r.status === 403) { flash('err', 'Admin access required.'); location.href = '/account'; return false; }
    if (!r.ok) return true;
    merchants = r.data.items; counts = r.data.counts || {}; mInfo = r.data;
    mPage = r.data.page; // server clamps past-the-end pages (e.g. after a filter shrinks the list)
    table();
    return true;
  }

  async function audit() {
    const r = await api(`/api/v1/admin/audit?page=${aPage}&size=${PAGE_SIZE}`);
    if (!r.ok) return;
    aInfo = r.data; aPage = r.data.page;
    $('audit').innerHTML = r.data.items.map((e) => `<tr>
        <td class="small">${fmtTime(e.ts)}</td><td>${esc(e.actor)}</td><td><code>${esc(e.action)}</code></td>
        <td class="mono small">${esc(e.target || '')}</td><td class="small">${esc(e.detail || '')}</td></tr>`).join('')
      || '<tr><td colspan="5" class="empty">No events.</td></tr>';
    pager($('a-pager'), aInfo, 'events', (p) => { aPage = p; audit(); });
  }

  async function refresh() {
    if (!(await loadMerchants())) return;
    const [s, hzr] = await Promise.all([api('/api/v1/admin/sessions'), api('/healthz')]);
    const ss = s.ok ? s.data : [];
    tiles(ss, hzr.ok ? hzr.data : null); sessions(ss); audit();
  }

  async function openReview(id) {
    const r = await api('/api/v1/admin/merchants/' + encodeURIComponent(id));
    if (!r.ok) return;
    current = r.data;
    const p = current.profile, pii = current.pii || {};
    $('review').classList.remove('hidden');
    $('rv-title').textContent = 'Review: ' + p.name;
    $('rv-profile').innerHTML = `
      <dt>Email</dt><dd>${esc(p.email)}</dd><dt>Role</dt><dd>${badge(p.role)}${p.disabled ? ' ' + badge('disabled') : ''}</dd>
      <dt>KYC status</dt><dd>${badge(p.kyc.status)}</dd><dt>Reason</dt><dd>${esc(p.kyc.reason || '–')}</dd>
      <dt>Valid until</dt><dd>${fmtTime(p.kyc.expires_at)}</dd><dt>Submitted</dt><dd>${fmtTime(p.kyc.submitted_at)}</dd>
      <dt>API key</dt><dd class="mono">${p.api_key ? esc(p.api_key.id) : '–'}</dd>`;
    $('rv-pii').innerHTML = Object.keys(pii).length ? `
      <dt>Legal name</dt><dd>${esc(pii.legal_name)}</dd><dt>Date of birth</dt><dd>${esc(pii.date_of_birth)}</dd>
      <dt>Country</dt><dd>${esc(p.kyc.country)}</dd><dt>Document</dt><dd>${esc(p.kyc.doc_type)} · <span class="mono">${esc(pii.document_number)}</span></dd>`
      : '<dd class="muted">Nothing submitted yet.</dd>';
    $('rv-doc').innerHTML = current.document
      ? `<a href="/api/v1/admin/merchants/${encodeURIComponent(p.id)}/document" target="_blank" rel="noopener">Open document scan</a> (${esc(current.document.mime)}, ${current.document.size < 1024 ? current.document.size + " B" : Math.round(current.document.size / 1024) + " KB"})`
      : '<span class="muted">No scan uploaded.</span>';
    $('rv-sessions').innerHTML = current.sessions.length
      ? current.sessions.map((s) => `${esc(s.remote)} · ${esc(s.format)} · ${syms(s.symbols)} · ${s.frames} frames`).join('<br>')
      : '<span class="muted">offline</span>';
    $('rv-disable').textContent = p.disabled ? 'Enable account' : 'Disable account';
    $('review').scrollIntoView({ behavior: 'smooth', block: 'start' });
  }

  async function act(path, body, okText, method = 'POST') {
    const r = await api(path, { method, body });
    if (r.ok) { toast('ok', okText); await refresh(); await openReview(current.profile.id); }
    else if (r.status) toast('err', errText(r, 'Action failed.'));
  }
  const base = () => '/api/v1/admin/merchants/' + encodeURIComponent(current.profile.id);

  $('rv-close').addEventListener('click', () => { $('review').classList.add('hidden'); current = null; });
  $('rv-approve').addEventListener('click', () => act(base() + '/kyc', { decision: 'approve', valid_days: Number($('rv-days').value), reason: '' }, 'Approved.'));
  $('rv-reject').addEventListener('click', () => act(base() + '/kyc', { decision: 'reject', reason: $('rv-reason').value, valid_days: 0 }, 'Rejected. Live sessions were closed.'));
  $('rv-reverify').addEventListener('click', () => act(base() + '/kyc', { decision: 'reverify', reason: $('rv-reason').value, valid_days: 0 }, 'Sent back to pending. Live sessions were closed.'));
  $('rv-revoke').addEventListener('click', () => act(base() + '/apikey', undefined, 'API key revoked.', 'DELETE'));
  $('rv-disable').addEventListener('click', () => act(base() + (current.profile.disabled ? '/enable' : '/disable'), undefined, 'Updated.'));
  let searchTimer;
  $('filter').addEventListener('input', () => { clearTimeout(searchTimer); searchTimer = setTimeout(() => { mPage = 1; loadMerchants(); }, 250); });
  $('status').addEventListener('change', () => { mPage = 1; loadMerchants(); });

  await refresh();
  setInterval(refresh, 5000);
})();
