'use strict';
// Live quotes page. Opens the WebSocket with a single-use ticket obtained
// from the logged-in session, so no API secret is ever exposed to JS.
(async () => {
  const me = await requireMe();
  if (!me) return;
  renderNav(me, 'quotes', 'Live Quotes');
  const quotes = new Map();   // symbol -> {buy, sell, hi, lo, chg, ts}
  const rows = new Map();     // symbol -> <tr>
  let updates = 0, latSum = 0, latN = 0, ws, retry = 500;

  const dec = (v) => { const s = String(v); const i = s.indexOf('.'); return i < 0 ? 0 : s.length - i - 1; };
  const fmt = (v, d) => Number.isFinite(v) ? v.toFixed(d) : '–';

  // Upstream = LiveFXHub. Our socket can be fine while the provider is down,
  // in which case prices freeze: say so loudly instead of looking "live".
  let upDown = false;
  function upstream(s, initial) {
    if (s.up) {
      if (upDown) toast('ok', 'Live prices resumed.', { id: 'upstream' });
      upDown = false;
      setState(true, 'live');
      return;
    }
    if (s.reason === 'connecting') { if (initial) setState(false, 'waiting for market data'); return; }
    upDown = true;
    setState(false, 'market feed down');
    toast('warn', `Live prices paused: ${UPSTREAM_TEXT[s.reason] || 'market data unavailable'}. Prices shown may be stale.`,
      { id: 'upstream', sticky: true });
  }

  function setState(on, text) {
    $('dot').className = 'dot ' + (on ? 'on' : 'off');
    $('state').textContent = text;
  }

  function locked(reason) {
    $('locked').classList.remove('hidden');
    $('locked-why').textContent = reason;
    $('live').classList.add('hidden');
  }

  let wasLive = false; // only announce a reconnect after a real disconnect
  function backoff(text) {
    if (wasLive) toast('warn', 'Live feed disconnected. Reconnecting…', { id: 'feed', sticky: true });
    wasLive = false;
    setState(false, `${text} · retry in ${Math.round(retry / 1000)}s`);
    setTimeout(connect, retry);
    retry = Math.min(retry * 2, 15000);
  }

  async function connect() {
    const t = await api('/api/v1/ws-ticket', { method: 'POST' });
    if (t.status === 401) { goLogin(); return; }
    if (t.status === 403) { setState(false, 'locked'); locked(t.data?.kyc_status ? `KYC status: ${t.data.kyc_status}` : 'not permitted'); return; }
    if (!t.ok) { backoff('ticket unavailable'); return; }
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    ws = new WebSocket(`${proto}://${location.host}/ws?symbols=*&ticket=${encodeURIComponent(t.data.ticket)}`);
    ws.onopen = () => {
      if (document.querySelector('#toasts [data-tid="feed"]')) toast('ok', 'Live feed reconnected.', { id: 'feed' });
      wasLive = true;
      setState(true, upDown ? 'market feed down' : 'live'); retry = 500;
    };
    ws.onclose = (e) => {
      if (e.code === 4003) {
        toast('err', `Live access revoked: ${e.reason || 'your account is no longer permitted'}.`, { id: 'feed', sticky: true });
        setState(false, 'access revoked'); locked(e.reason || 'access revoked'); return;
      }
      backoff('disconnected');
    };
    ws.onmessage = (ev) => {
      let m;
      try { m = JSON.parse(ev.data); } catch { return; }
      if (m.t === 'welcome' && m.upstream) { upstream(m.upstream, true); return; }
      if (m.t === 'upstream') { upstream(m, false); return; }
      if (m.t !== 'q') return;
      $('fsize').textContent = ev.data.length.toLocaleString() + ' B';
      const now = Date.now();
      let added = false;
      for (const [sym, buy, sell, hi, lo, chg, ts] of m.d) {
        const prev = quotes.get(sym);
        if (!prev) added = true;
        quotes.set(sym, { buy, sell, hi, lo, chg, ts, dir: prev ? Math.sign(buy - prev.buy) : 0 });
        render(sym); // updates cells in place; never moves rows
        updates++; latSum += now - ts; latN++;
      }
      // Re-order only when the set of symbols changes. Moving rows on every
      // tick (it used to be ~25x/s) cancels clicks: the browser drops a click
      // whose row is detached between mouse press and release.
      if (added) layout();
      const want = decodeURIComponent(location.hash.slice(1)).toUpperCase();
      if (want && !chartSym && quotes.has(want)) openChart(want);
    };
  }

  function rowFor(sym) {
    let tr = rows.get(sym);
    if (!tr) {
      tr = document.createElement('tr');
      tr.innerHTML = '<td class="sym"></td><td></td><td></td><td></td><td></td><td></td><td></td><td></td><td class="age"></td>';
      tr.cells[0].textContent = sym;
      tr.addEventListener('click', () => openChart(sym));
      rows.set(sym, tr);
    }
    return tr;
  }

  function render(sym) {
    const q = quotes.get(sym), c = rowFor(sym).cells;
    const d = Math.max(dec(q.buy), dec(q.sell), 2);
    c[1].textContent = fmt(q.buy, d);
    c[2].textContent = fmt(q.sell, d);
    // 24h stats are on mid, so show mid too: buy can legitimately exceed the 24h high.
    c[3].textContent = fmt((q.buy + q.sell) / 2, d + 1);
    c[4].textContent = fmt(q.buy - q.sell, d);
    c[5].textContent = fmt(q.hi, d + 1);
    c[6].textContent = fmt(q.lo, d + 1);
    c[7].textContent = (q.chg > 0 ? '+' : '') + fmt(q.chg, 2) + '%';
    c[7].className = q.chg > 0 ? 'pos' : q.chg < 0 ? 'neg' : '';
    c[8].dataset.ts = q.ts;
    if (q.dir) {
      const cls = q.dir > 0 ? 'flash-up' : 'flash-down';
      for (const i of [1, 2]) { c[i].classList.remove('flash-up', 'flash-down'); void c[i].offsetWidth; c[i].classList.add(cls); }
      setTimeout(() => { c[1].classList.remove(cls); c[2].classList.remove(cls); }, 350);
    }
  }

  // Rows are never moved while a mouse button is held over the table.
  let pressing = false, pendingLayout = false;
  $('rows').addEventListener('pointerdown', () => { pressing = true; });
  window.addEventListener('pointerup', () => {
    pressing = false;
    // setTimeout: runs after this pointerup's click event has been dispatched
    if (pendingLayout) { pendingLayout = false; setTimeout(layout, 0); }
  });

  let sortKey = 'symbol', sortDir = 1; // 1 = ascending, -1 = descending
  const SORT_VALUE = {
    buy: (q) => q.buy, sell: (q) => q.sell, mid: (q) => (q.buy + q.sell) / 2, spread: (q) => q.buy - q.sell,
    hi: (q) => q.hi, lo: (q) => q.lo, chg: (q) => q.chg, ts: (q) => q.ts,
  };

  function markSort() {
    document.querySelectorAll('#quotes th[data-key]').forEach((th) => {
      const on = th.dataset.key === sortKey;
      th.classList.toggle('sorted', on);
      th.setAttribute('aria-sort', on ? (sortDir > 0 ? 'ascending' : 'descending') : 'none');
      th.querySelector('.arr').textContent = on ? (sortDir > 0 ? '▲' : '▼') : '';
    });
  }

  function layout() {
    if (pressing) { pendingLayout = true; return; }
    const f = $('filter').value.trim().toUpperCase();
    const syms = [...quotes.keys()].filter((s) => !f || s.includes(f));
    const val = SORT_VALUE[sortKey];
    syms.sort((a, b) => {
      if (sortKey === 'symbol') return sortDir * a.localeCompare(b);
      const d = val(quotes.get(a)) - val(quotes.get(b));
      return d !== 0 ? sortDir * d : a.localeCompare(b); // ties: stable by symbol
    });
    const body = $('rows');
    $('nsym').textContent = quotes.size;
    if (!syms.length) {
      body.innerHTML = `<tr><td class="empty" colspan="9">${quotes.size ? 'No symbols match.' : 'Waiting for prices…'}</td></tr>`;
      return;
    }
    const cur = body.children;
    if (cur.length === syms.length && syms.every((s, i) => cur[i] === rows.get(s))) return; // order unchanged: touch nothing
    body.replaceChildren(...syms.map(rowFor));
  }

  $('filter').addEventListener('input', layout);

  // Header sorting: one column at a time. Click = ascending, click again =
  // descending; clicking another column replaces the sort.
  document.querySelectorAll('#quotes th[data-key]').forEach((th) => th.querySelector('button').addEventListener('click', () => {
    const key = th.dataset.key;
    sortDir = key === sortKey ? -sortDir : 1;
    sortKey = key;
    markSort();
    layout();
  }));
  markSort();

  setInterval(() => {
    if (sortKey !== 'symbol') layout(); // price-sorted views re-rank once a second, not per tick
    $('ups').textContent = updates;
    // Server and browser clocks can differ by ~1 ms (Docker VM vs host), so
    // near-zero latency may come out negative: show it as "<1 ms".
    const lat = latN ? Math.round(latSum / latN) : null;
    $('lat').textContent = lat === null ? '–' : lat < 1 ? '<1 ms' : lat + ' ms';
    updates = 0; latSum = 0; latN = 0;
    const now = Date.now();
    for (const tr of rows.values()) {
      const c = tr.cells[8], ts = +c.dataset.ts;
      if (ts) c.textContent = ago(now - ts);
    }
  }, 1000);

  // ---- 1m OHLC candles (plain canvas, no libraries) ----
  let chartSym = null, candles = [], hover = -1, chartTimer, chartMsg = '';
  const cv = $('cv');

  // Uses api() (not bare fetch) so an expired 15-minute session is refreshed
  // silently, exactly like every other call on the page.
  async function loadCandles() {
    const sym = chartSym;
    if (!sym) return;
    let r;
    try {
      r = await api(`/api/v1/candles?symbol=${encodeURIComponent(sym)}&limit=120`);
    } catch {
      r = { ok: false, status: 0 };
    }
    if (sym !== chartSym) return; // user switched symbol while this was in flight
    if (r.status === 401) { goLogin(); return; }
    if (r.ok) {
      candles = (r.data.candles || []).map(([ts, o, h, l, c, n]) => ({ ts, o, h, l, c, n }));
      chartMsg = 'No candles yet — history builds one bar per minute.';
    } else if (r.status === 403) {
      candles = []; chartMsg = 'Access revoked — KYC verification required.';
      toast('err', 'Candles unavailable: access revoked.', { id: 'candles' });
    } else if (!candles.length) {
      chartMsg = `Could not load candles (${r.status ? 'HTTP ' + r.status : 'network error'}); retrying…`;
    } // transient error with data already drawn: keep the last chart
    drawChart();
  }

  function openChart(sym) {
    if (chartSym === sym && !$('chart').hidden) return;
    chartSym = sym;
    candles = []; hover = -1; chartMsg = 'Loading…'; // never show the previous symbol's bars
    history.replaceState(null, '', '#' + sym);
    for (const [s, tr] of rows) tr.classList.toggle('sel', s === sym);
    $('chart').hidden = false;
    $('c-sym').textContent = sym;
    drawChart();
    clearInterval(chartTimer);
    chartTimer = setInterval(loadCandles, 5000);
    loadCandles();
    $('chart').scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  }

  $('c-close').addEventListener('click', () => {
    chartSym = null; clearInterval(chartTimer); $('chart').hidden = true;
    history.replaceState(null, '', location.pathname);
    for (const tr of rows.values()) tr.classList.remove('sel');
  });

  function drawChart() {
    const css = getComputedStyle(document.documentElement);
    const col = (v) => css.getPropertyValue(v).trim();
    const dpr = window.devicePixelRatio || 1;
    const W = cv.clientWidth, H = cv.clientHeight;
    cv.width = W * dpr; cv.height = H * dpr;
    const g = cv.getContext('2d');
    g.setTransform(dpr, 0, 0, dpr, 0, 0);
    g.clearRect(0, 0, W, H);
    g.font = '11px system-ui, sans-serif';
    g.fillStyle = col('--muted');
    if (!candles.length) {
      g.fillText(chartMsg, 8, 20);
      $('c-ohlc').textContent = '';
      return;
    }
    const padR = 70, padB = 20, padT = 8, pw = W - padR, ph = H - padB - padT;
    let lo = Infinity, hi = -Infinity;
    for (const k of candles) { lo = Math.min(lo, k.l); hi = Math.max(hi, k.h); }
    const span = (hi - lo) || hi * 1e-4 || 1;
    lo -= span * 0.08; hi += span * 0.08;
    const y = (v) => padT + ph - (v - lo) / (hi - lo) * ph;
    const slots = Math.max(candles.length, 30), step = pw / slots;
    const x0 = pw - candles.length * step;
    const d = Math.max(0, Math.min(8, Math.ceil(-Math.log10(span / 5)) + 1));

    // grid + price labels
    g.strokeStyle = col('--line'); g.lineWidth = 1;
    for (let i = 0; i <= 4; i++) {
      const v = lo + (hi - lo) * i / 4, yy = Math.round(y(v)) + 0.5;
      g.beginPath(); g.moveTo(0, yy); g.lineTo(pw, yy); g.stroke();
      g.fillText(v.toFixed(d), pw + 6, yy + 4);
    }
    // time labels
    const every = Math.ceil(candles.length / 6);
    candles.forEach((k, i) => {
      if (i % every) return;
      const t = new Date(k.ts);
      g.fillText(String(t.getHours()).padStart(2, '0') + ':' + String(t.getMinutes()).padStart(2, '0'), x0 + i * step, H - 5);
    });
    // candles
    const bw = Math.max(1, step * 0.65);
    candles.forEach((k, i) => {
      const cx = x0 + i * step + step / 2;
      g.strokeStyle = g.fillStyle = k.c >= k.o ? col('--up') : col('--down');
      g.beginPath(); g.moveTo(Math.round(cx) + 0.5, y(k.h)); g.lineTo(Math.round(cx) + 0.5, y(k.l)); g.stroke();
      const top = y(Math.max(k.o, k.c)), bot = y(Math.min(k.o, k.c));
      g.fillRect(cx - bw / 2, top, bw, Math.max(1, bot - top));
    });
    // hover crosshair
    const k = candles[hover >= 0 && hover < candles.length ? hover : candles.length - 1];
    if (hover >= 0 && hover < candles.length) {
      g.strokeStyle = col('--muted'); g.setLineDash([3, 3]);
      const cx = Math.round(x0 + hover * step + step / 2) + 0.5;
      g.beginPath(); g.moveTo(cx, padT); g.lineTo(cx, padT + ph); g.stroke(); g.setLineDash([]);
    }
    const t = new Date(k.ts);
    $('c-ohlc').innerHTML = `${t.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })} &nbsp;
      O <b>${k.o.toFixed(d)}</b> H <b>${k.h.toFixed(d)}</b> L <b>${k.l.toFixed(d)}</b> C <b>${k.c.toFixed(d)}</b> · ${k.n} ticks`;
    cv._geom = { x0, step };
  }

  cv.addEventListener('mousemove', (e) => {
    const gm = cv._geom; if (!gm) return;
    const i = Math.floor((e.offsetX - gm.x0) / gm.step);
    if (i !== hover) { hover = i; drawChart(); }
  });
  cv.addEventListener('mouseleave', () => { hover = -1; drawChart(); });
  window.addEventListener('resize', () => { if (chartSym) drawChart(); });

  connect();
})();
