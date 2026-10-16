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

  // ---- connection status --------------------------------------------------
  // "Live" is computed every second from what is really happening, not from
  // whether our socket happens to be open:
  //   socket closed            -> Reconnecting…       (red)
  //   upstream provider down   -> Market feed down    (red)
  //   no data yet              -> Connecting to market data… (amber)
  //   newest price > 15 s old  -> Stale · no updates for N  (amber)
  //   otherwise                -> Live                (green)
  const STALE_MS = 15000;
  let wsOpen = false, upDown = false, isLocked = false, retryAt = 0, lastDataTs = 0;

  function span(ms) {
    const s = Math.max(0, Math.round(ms / 1000));
    return s < 60 ? `${s} sec` : s < 3600 ? `${Math.floor(s / 60)} min` : `${Math.floor(s / 3600)} hr`;
  }

  function refreshStatus() {
    const now = Date.now();
    let kind, text, why;
    if (isLocked) [kind, text, why] = ['off', 'Locked', 'Live data requires approved KYC'];
    else if (!wsOpen) [kind, text, why] = ['off', retryAt > now ? `Reconnecting… (${Math.ceil((retryAt - now) / 1000)}s)` : 'Connecting…', 'Connection to this server is down'];
    else if (upDown) [kind, text, why] = ['off', 'Market feed down', 'The market data provider is unavailable; prices are frozen'];
    else if (!lastDataTs) [kind, text, why] = ['warn', 'Connecting to market data…', 'Waiting for the first price'];
    else if (now - lastDataTs > STALE_MS) [kind, text, why] = ['warn', `Stale · no updates for ${span(now - lastDataTs)}`, 'Connected, but no new prices are arriving'];
    else [kind, text, why] = ['on', 'Live', 'Prices are streaming'];
    $('dot').className = 'dot ' + kind;
    $('state').textContent = text;
    $('state').title = why;
  }

  // Upstream = LiveFXHub. Our socket can be fine while the provider is down,
  // in which case prices freeze: say so loudly instead of looking "live".
  function upstream(s) {
    if (s.up) {
      if (upDown) toast('ok', 'Live prices resumed.', { id: 'upstream' });
      upDown = false;
    } else if (s.reason !== 'connecting') {
      upDown = true;
      toast('warn', `Live prices paused: ${UPSTREAM_TEXT[s.reason] || 'market data unavailable'}. Prices shown may be stale.`,
        { id: 'upstream', sticky: true });
    }
    refreshStatus();
  }

  function locked(reason) {
    isLocked = true;
    refreshStatus();
    $('locked').classList.remove('hidden');
    $('locked-why').textContent = reason;
    $('live').classList.add('hidden');
  }

  let wasLive = false; // only announce a reconnect after a real disconnect
  function backoff(text) {
    if (wasLive) toast('warn', 'Live feed disconnected. Reconnecting…', { id: 'feed', sticky: true });
    wasLive = false;
    wsOpen = false;
    retryAt = Date.now() + retry;
    refreshStatus();
    setTimeout(connect, retry);
    retry = Math.min(retry * 2, 15000);
  }

  async function connect() {
    const t = await api('/api/v1/ws-ticket', { method: 'POST' });
    if (t.status === 401) { goLogin(); return; }
    if (t.status === 403) { locked(t.data?.kyc_status ? `KYC status: ${t.data.kyc_status}` : 'not permitted'); return; }
    if (!t.ok) { backoff('ticket unavailable'); return; }
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    ws = new WebSocket(`${proto}://${location.host}/ws?symbols=*&ticket=${encodeURIComponent(t.data.ticket)}`);
    ws.onopen = () => {
      if (document.querySelector('#toasts [data-tid="feed"]')) toast('ok', 'Live feed reconnected.', { id: 'feed' });
      wasLive = true;
      wsOpen = true;
      retry = 500;
      refreshStatus();
    };
    ws.onclose = (e) => {
      if (e.code === 4003) {
        toast('err', `Live access revoked: ${e.reason || 'your account is no longer permitted'}.`, { id: 'feed', sticky: true });
        wsOpen = false; locked(e.reason || 'access revoked'); return;
      }
      backoff('disconnected');
    };
    ws.onmessage = (ev) => {
      let m;
      try { m = JSON.parse(ev.data); } catch { return; }
      if (m.t === 'welcome' && m.upstream) { upstream(m.upstream); return; }
      if (m.t === 'upstream') { upstream(m); return; }
      if (m.t !== 'q') return;
      const now = Date.now();
      let added = false;
      for (const [sym, buy, sell, hi, lo, chg, ts] of m.d) {
        if (ts > lastDataTs) lastDataTs = ts; // freshness of the data itself, not of the message
        if (sym === chartSym) liveTick((buy + sell) / 2, ts); // live candle
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
  // Pagination (client-side: every quote already streams to the page).
  const PAGE_SIZES = [10, 25, 50];
  let page = 1, pageSize = 10;
  try { const s = Number(localStorage.getItem('pe_quotes_page_size')); if (PAGE_SIZES.includes(s)) pageSize = s; } catch { /* storage blocked */ }
  let lastPager = '';
  function goPage(p) { page = p; layout(); }
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
    const pages = Math.max(1, Math.ceil(syms.length / pageSize));
    page = Math.min(Math.max(1, page), pages);
    const visible = syms.slice((page - 1) * pageSize, page * pageSize);
    // Re-render the pager only when its numbers change: rebuilding it every
    // second (price-sorted re-ranking) would eat clicks and close the select.
    const pkey = `${page}/${pageSize}/${syms.length}`;
    if (pkey !== lastPager) { lastPager = pkey; pager($('q-pager'), { page, size: pageSize, total: syms.length, pages }, 'symbols', goPage, {
      sizes: PAGE_SIZES,
      onSize: (n) => {
        pageSize = n; page = 1;
        try { localStorage.setItem('pe_quotes_page_size', String(n)); } catch { /* storage blocked */ }
        layout();
      },
    }); }
    if (!visible.length) {
      body.innerHTML = `<tr><td class="empty" colspan="9">${quotes.size ? 'No symbols match.' : 'Waiting for prices…'}</td></tr>`;
      return;
    }
    const cur = body.children;
    if (cur.length === visible.length && visible.every((s, i) => cur[i] === rows.get(s))) return; // order unchanged: touch nothing
    body.replaceChildren(...visible.map(rowFor));
  }

  $('filter').addEventListener('input', () => { page = 1; layout(); });

  // Header sorting: one column at a time. Click = ascending, click again =
  // descending; clicking another column replaces the sort.
  document.querySelectorAll('#quotes th[data-key]').forEach((th) => th.querySelector('button').addEventListener('click', () => {
    const key = th.dataset.key;
    sortDir = key === sortKey ? -sortDir : 1;
    sortKey = key;
    page = 1;
    markSort();
    layout();
  }));
  markSort();

  setInterval(() => {
    refreshStatus();
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
      if (ts) {
        c.textContent = timeAgo(ts, now);
        c.title = 'Last update: ' + exactTime(ts);
      }
    }
  }, 1000);

  // ---- OHLC candles (plain canvas, no libraries) ----
  // The x-axis is real time: minutes with no data stay empty and gaps longer
  // than two candles are shaded "no data", so outages are visible. The newest
  // candle is updated live from WebSocket ticks; the server's 1-minute
  // candles are re-fetched every 30 s to stay in sync.
  const WINDOWS = { // label: [window minutes, candle minutes]
    '30m': [30, 1], '1h': [60, 1], '3h': [180, 5], '6h': [360, 5], '12h': [720, 15], '24h': [1440, 30],
  };
  let winKey = '1h';
  try { const w = localStorage.getItem('pe_chart_window'); if (WINDOWS[w]) winKey = w; } catch { /* storage blocked */ }
  let chartSym = null, base = [], loaded = false, hover = -1, chartTimer, chartMsg = '', drawQueued = false;
  const cv = $('cv');

  // Uses api() (not bare fetch) so an expired 15-minute session is refreshed
  // silently, exactly like every other call on the page.
  async function loadCandles() {
    const sym = chartSym;
    if (!sym) return;
    const r = await api(`/api/v1/candles?symbol=${encodeURIComponent(sym)}&limit=${WINDOWS[winKey][0]}`);
    if (sym !== chartSym) return; // user switched symbol while this was in flight
    if (r.status === 401) { goLogin(); return; }
    if (r.ok) {
      const fresh = (r.data.candles || []).map(([ts, o, h, l, c, n]) => ({ ts, o, h, l, c, n }));
      // keep a live-built candle that is newer than the server's latest
      const last = base[base.length - 1];
      if (last && fresh.length && last.ts > fresh[fresh.length - 1].ts) fresh.push(last);
      base = fresh; loaded = true;
      chartMsg = 'No candles yet — history builds one bar per minute.';
    } else if (r.status === 403) {
      base = []; chartMsg = 'Access revoked — KYC verification required.';
      toast('err', 'Candles unavailable: access revoked.', { id: 'candles' });
    } else if (!base.length) {
      chartMsg = `Could not load candles (${r.status ? 'HTTP ' + r.status : 'network error'}); retrying…`;
    } // transient error with data already drawn: keep the last chart
    drawChart();
  }

  // liveTick folds one streamed price into the 1-minute candles.
  function liveTick(mid, ts) {
    if (!loaded) return;
    const minute = Math.floor(ts / 60000) * 60000;
    let k = base[base.length - 1];
    if (!k || minute > k.ts) {
      base.push({ ts: minute, o: mid, h: mid, l: mid, c: mid, n: 1 });
      const cutoff = minute - WINDOWS['24h'][0] * 60000;
      while (base.length && base[0].ts < cutoff) base.shift();
    } else {
      for (let i = base.length - 1; i >= 0 && base[i].ts >= minute; i--) { // late tick: its own minute
        if (base[i].ts === minute) { k = base[i]; break; }
      }
      if (k.ts !== minute) return; // older than anything we hold
      k.h = Math.max(k.h, mid); k.l = Math.min(k.l, mid); k.n++;
      if (minute === base[base.length - 1].ts) k.c = mid;
    }
    if (!drawQueued) { drawQueued = true; requestAnimationFrame(() => { drawQueued = false; drawChart(); }); }
  }

  // aggregate merges 1-minute candles into bucket-minute candles.
  function aggregate(bucketMs, start) {
    const out = [];
    for (const k of base) {
      if (k.ts < start) continue;
      const t = Math.floor(k.ts / bucketMs) * bucketMs;
      const a = out[out.length - 1];
      if (a && a.ts === t) { a.h = Math.max(a.h, k.h); a.l = Math.min(a.l, k.l); a.c = k.c; a.n += k.n; }
      else out.push({ ts: t, o: k.o, h: k.h, l: k.l, c: k.c, n: k.n });
    }
    return out;
  }

  function renderWindowButtons() {
    $('c-win').innerHTML = Object.keys(WINDOWS).map((w) =>
      `<button type="button" data-w="${w}" class="${w === winKey ? 'on' : ''}" aria-pressed="${w === winKey}">${w}</button>`).join('');
    const [mins, bm] = WINDOWS[winKey];
    $('c-desc').textContent = `${bm}m candles · last ${winKey} · mid price`;
  }
  $('c-win').addEventListener('click', (e) => {
    const w = e.target.closest('button')?.dataset.w;
    if (!w || w === winKey) return;
    winKey = w;
    try { localStorage.setItem('pe_chart_window', w); } catch { /* storage blocked */ }
    renderWindowButtons();
    hover = -1;
    loadCandles();
  });

  function openChart(sym) {
    if (chartSym === sym && !$('chart').hidden) return;
    chartSym = sym;
    base = []; loaded = false; hover = -1; chartMsg = 'Loading…'; // never show the previous symbol's bars
    history.replaceState(null, '', '#' + sym);
    for (const [s, tr] of rows) tr.classList.toggle('sel', s === sym);
    $('chart').hidden = false;
    $('c-sym').textContent = sym;
    renderWindowButtons();
    drawChart();
    clearInterval(chartTimer);
    chartTimer = setInterval(loadCandles, 30000);
    loadCandles();
    $('chart').scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  }

  $('c-close').addEventListener('click', () => {
    chartSym = null; loaded = false; clearInterval(chartTimer); $('chart').hidden = true;
    history.replaceState(null, '', location.pathname);
    for (const tr of rows.values()) tr.classList.remove('sel');
  });

  const hhmm = (ts) => { const t = new Date(ts); return String(t.getHours()).padStart(2, '0') + ':' + String(t.getMinutes()).padStart(2, '0'); };

  function drawChart() {
    if (!chartSym) return;
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

    const [mins, bm] = WINDOWS[winKey];
    const bucket = bm * 60000, slots = mins / bm;
    const end = Math.floor(Date.now() / bucket) * bucket + bucket; // right edge = end of the current candle
    const start = end - mins * 60000;
    const candles = aggregate(bucket, start);
    if (!candles.length) {
      g.fillText(base.length ? `No data in the last ${winKey}.` : chartMsg, 8, 20);
      $('c-ohlc').textContent = '';
      cv._geom = null;
      return;
    }
    const padR = 70, padB = 20, padT = 8, pw = W - padR, ph = H - padB - padT;
    const step = pw / slots;
    const xOf = (ts) => (ts - start) / bucket * step;
    let lo = Infinity, hi = -Infinity;
    for (const k of candles) { lo = Math.min(lo, k.l); hi = Math.max(hi, k.h); }
    const spanV = (hi - lo) || hi * 1e-4 || 1;
    lo -= spanV * 0.08; hi += spanV * 0.08;
    const y = (v) => padT + ph - (v - lo) / (hi - lo) * ph;
    const d = Math.max(0, Math.min(8, Math.ceil(-Math.log10(spanV / 5)) + 1));

    // downtime: shade every stretch with no candles longer than two buckets,
    // including the one still running at the right edge.
    const gapMin = Math.max(2 * bucket, 120000);
    const gaps = [];
    for (let i = 1; i < candles.length; i++) {
      const from = candles[i - 1].ts + bucket, to = candles[i].ts;
      if (to - from >= gapMin - bucket) gaps.push([from, to]);
    }
    const lastEnd = candles[candles.length - 1].ts + bucket;
    if (Date.now() - lastEnd >= gapMin - bucket) gaps.push([lastEnd, end]);
    g.fillStyle = col('--line');
    for (const [from, to] of gaps) {
      const x1 = xOf(from), x2 = Math.min(pw, xOf(to));
      g.globalAlpha = 0.45; g.fillRect(x1, padT, x2 - x1, ph); g.globalAlpha = 1;
      if (x2 - x1 > 44) {
        g.fillStyle = col('--muted');
        g.fillText('no data', x1 + (x2 - x1) / 2 - 18, padT + 14);
        g.fillStyle = col('--line');
      }
    }

    // grid + price labels
    g.strokeStyle = col('--line'); g.lineWidth = 1; g.fillStyle = col('--muted');
    for (let i = 0; i <= 4; i++) {
      const v = lo + (hi - lo) * i / 4, yy = Math.round(y(v)) + 0.5;
      g.beginPath(); g.moveTo(0, yy); g.lineTo(pw, yy); g.stroke();
      g.fillText(v.toFixed(d), pw + 6, yy + 4);
    }
    // time labels on round clock times, at most ~7
    const labelMin = [5, 10, 15, 30, 60, 120, 180, 240, 360].find((m) => mins / m <= 7) || 360;
    const lstep = labelMin * 60000;
    for (let t = Math.ceil(start / lstep) * lstep; t < end; t += lstep) {
      const x = xOf(t);
      if (x > 14 && x < pw - 14) g.fillText(hhmm(t), x - 14, H - 5);
    }
    // candles
    const bw = Math.max(1, step * 0.65);
    for (const k of candles) {
      const cx = xOf(k.ts) + step / 2;
      g.strokeStyle = g.fillStyle = k.c >= k.o ? col('--up') : col('--down');
      g.beginPath(); g.moveTo(Math.round(cx) + 0.5, y(k.h)); g.lineTo(Math.round(cx) + 0.5, y(k.l)); g.stroke();
      const top = y(Math.max(k.o, k.c)), bot = y(Math.min(k.o, k.c));
      g.fillRect(cx - bw / 2, top, bw, Math.max(1, bot - top));
    }
    // hover crosshair: the slot under the cursor (may be an empty, no-data slot)
    let shown = candles[candles.length - 1], hoverTs = null;
    if (hover >= 0 && hover < slots) {
      hoverTs = start + hover * bucket;
      shown = candles.find((k) => k.ts === hoverTs) || null;
      g.strokeStyle = col('--muted'); g.setLineDash([3, 3]);
      const cx = Math.round(xOf(hoverTs) + step / 2) + 0.5;
      g.beginPath(); g.moveTo(cx, padT); g.lineTo(cx, padT + ph); g.stroke(); g.setLineDash([]);
    }
    $('c-ohlc').innerHTML = shown
      ? `${hhmm(shown.ts)} &nbsp; O <b>${shown.o.toFixed(d)}</b> H <b>${shown.h.toFixed(d)}</b> L <b>${shown.l.toFixed(d)}</b> C <b>${shown.c.toFixed(d)}</b> · ${shown.n} ticks`
      : `${hhmm(hoverTs)} &nbsp; no data (feed down or engine stopped)`;
    cv._geom = { step, slots };
  }

  cv.addEventListener('mousemove', (e) => {
    const gm = cv._geom; if (!gm) return;
    const i = Math.floor(e.offsetX / gm.step);
    if (i !== hover) { hover = i; drawChart(); }
  });
  cv.addEventListener('mouseleave', () => { hover = -1; drawChart(); });
  window.addEventListener('resize', () => { if (chartSym) drawChart(); });
  // keep the time axis (and a running "no data" band) moving even with no ticks
  setInterval(() => { if (chartSym && !drawQueued) drawChart(); }, 15000);

  connect();
})();
