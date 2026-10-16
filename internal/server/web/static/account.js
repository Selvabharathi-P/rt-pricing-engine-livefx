'use strict';
(async () => {
  let me = await requireMe();
  let shownSecret = null; // survives periodic re-renders until the page is left
  if (!me) return;
  renderNav(me, 'account', 'My Account');

  function steps(me) {
    const s = me.kyc.status;
    const submitted = s !== 'not_submitted';
    const verified = s === 'approved';
    const failed = s === 'rejected' || s === 'expired';
    const items = [
      ['Registered', 'done'],
      ['KYC submitted', submitted ? 'done' : 'now'],
      [failed ? `KYC ${s}` : 'KYC verified', verified ? 'done' : failed ? 'bad' : submitted ? 'now' : ''],
      ['Role: verified merchant', me.role === 'verified_merchant' ? 'done' : ''],
      ['Trading enabled', me.can_trade ? 'done' : ''],
    ];
    $('steps').innerHTML = items.map(([t, c]) => `<span class="step ${c}">${esc(t)}</span>`).join('<span class="arrow">→</span>');
  }

  function profile(me) {
    $('profile').innerHTML = `
      <dt>Name</dt><dd>${esc(me.name)}</dd>
      <dt>Email</dt><dd>${esc(me.email)}</dd>
      <dt>Role</dt><dd>${badge(me.role)}</dd>
      <dt>Can trade</dt><dd>${badge(me.can_trade ? 'yes' : 'no')}</dd>
      <dt>Member since</dt><dd>${fmtTime(me.created_at)}</dd>`;
  }

  function kyc(me) {
    const k = me.kyc;
    const lines = {
      not_submitted: 'You have not submitted KYC yet. Submit your identity details to request trading access.',
      pending: 'Your documents are waiting for review by an administrator.',
      approved: `Verified. Valid until <b>${fmtTime(k.expires_at)}</b>.`,
      rejected: `Rejected${k.reason ? ': <b>' + esc(k.reason) + '</b>' : ''}. Fix the issue and resubmit.`,
      expired: 'Your verification has expired. Resubmit to restore trading access.',
    };
    $('kyc-status').innerHTML = `<p>${badge(k.status)} ${lines[k.status] || ''}</p>` +
      (k.submitted_at ? `<p class="small muted">Last submitted ${fmtTime(k.submitted_at)} · ${esc(k.doc_type || '')} · ${esc(k.country || '')}${k.has_document ? ' · scan attached' : ''}</p>` : '');
    const canSubmit = me.role !== 'admin' && ['not_submitted', 'rejected', 'expired'].includes(k.status);
    $('f-kyc').classList.toggle('hidden', !canSubmit);
  }

  function access(me) {
    const el = $('access');
    if (me.role === 'admin') {
      el.innerHTML = '<p>Admins can <a href="/">view live quotes</a> for oversight, but cannot trade or hold API keys (separation of duties).</p>';
      return;
    }
    if (!me.can_trade) {
      el.innerHTML = `<p>${badge('no')} Locked. API keys and the live price feed are available to <b>verified merchants</b> only.
        Current KYC status: ${badge(me.kyc.status)}.</p>`;
      return;
    }
    const k = me.api_key;
    el.innerHTML = `
      <p>${badge('yes')} Trading enabled. <a href="/">Open live quotes →</a></p>
      <h3>API key</h3>
      ${k ? `<dl class="kv"><dt>Key</dt><dd class="mono">${esc(k.id)}</dd>
        <dt>Created</dt><dd>${fmtTime(k.created_at)}</dd><dt>Last used</dt><dd>${fmtTime(k.last_used_at)}</dd></dl>`
        : '<p class="muted">No API key yet.</p>'}
      <div class="actions">
        <button class="btn" type="button" id="k-issue">${k ? 'Rotate key' : 'Generate API key'}</button>
        ${k ? '<button class="btn danger" type="button" id="k-revoke">Revoke key</button>' : ''}
      </div>
      <div id="k-secret"></div>
      <h3>Connect to the WebSocket feed</h3>
      <pre>ws://${esc(location.host)}/ws
→ {"action":"auth","api_key":"pk_…","secret":"sk_…"}
← {"t":"authenticated"}
→ {"action":"subscribe","symbols":["EURUSD","XAUUSD"]}
← {"t":"q","d":[["EURUSD",buy,sell,high24h,low24h,change24hPct,ts]]}</pre>
      <p class="small muted">REST: send <code>X-API-Key</code> / <code>X-API-Secret</code> headers to <code>/api/v1/quotes</code>, <code>/api/v1/candles</code>, <code>/api/v1/orders</code>.</p>`;
    $('k-issue').addEventListener('click', async () => {
      if (k && !confirm('Rotating disconnects any client using the current key. Continue?')) return;
      const r = await api('/api/v1/apikey', { method: 'POST' });
      if (!r.ok) { showMsg($('key-msg'), false, r.data?.error || 'Failed'); return; }
      shownSecret = r.data;
      await reload();
    });
    if (shownSecret) {
      const d = shownSecret;
      $('k-secret').innerHTML = `<div class="secret"><b>Copy your secret now — it will not be shown again.</b>
        <dl class="kv"><dt>API key</dt><dd><code>${esc(d.api_key)}</code></dd>
        <dt>Secret</dt><dd><code>${esc(d.secret)}</code></dd></dl>
        <div class="actions"><button class="btn ghost" type="button" id="copy">Copy secret</button>
        <button class="btn ghost" type="button" id="hide">I have saved it</button></div></div>`;
      $('copy').addEventListener('click', () => navigator.clipboard?.writeText(d.secret));
      $('hide').addEventListener('click', () => { shownSecret = null; render(); });
    }
    $('k-revoke')?.addEventListener('click', async () => {
      if (!confirm('Revoke this key? Connected clients are disconnected immediately.')) return;
      const r = await api('/api/v1/apikey', { method: 'DELETE' });
      if (r.ok) { shownSecret = null; await reload(); showMsg($('key-msg'), true, 'Key revoked.'); } else showMsg($('key-msg'), false, r.data?.error || 'Failed');
    });
  }

  function render() { steps(me); profile(me); kyc(me); access(me); }
  async function reload() { const r = await api('/api/v1/me'); if (r.ok) { me = r.data; render(); } }
  render();

  $('f-kyc').addEventListener('submit', async (e) => {
    e.preventDefault();
    const f = new FormData();
    f.append('legal_name', $('k-name').value);
    f.append('date_of_birth', $('k-dob').value);
    f.append('country', $('k-country').value);
    f.append('doc_type', $('k-type').value);
    f.append('doc_number', $('k-num').value);
    if ($('k-doc').files[0]) f.append('document', $('k-doc').files[0]);
    const r = await api('/api/v1/kyc', { method: 'POST', form: f });
    if (r.ok) { me = r.data; render(); showMsg($('kyc-msg'), true, 'Submitted. An administrator will review it.'); $('f-kyc').reset(); }
    else showMsg($('kyc-msg'), false, r.data?.error || 'Submission failed');
  });

  $('f-order').addEventListener('submit', async (e) => {
    e.preventDefault();
    const r = await api('/api/v1/orders', { method: 'POST',
      body: { symbol: $('o-sym').value, side: $('o-side').value, qty: Number($('o-qty').value) } });
    if (r.ok) showMsg($('order-msg'), true, `Filled (simulated): ${r.data.side} ${r.data.qty} ${r.data.symbol} @ ${r.data.price} · ${r.data.order_id}`);
    else showMsg($('order-msg'), false, `HTTP ${r.status}: ${r.data?.error || 'rejected'}${r.data?.kyc_status ? ' (kyc_status=' + r.data.kyc_status + ')' : ''}`);
  });

  // Status changes made by an admin show up without a reload.
  setInterval(reload, 15000);
})();
