// Package engine computes per-symbol quotes (buy, sell, rolling 24h high,
// low and % change) from raw ticks. The 24h statistics are derived purely
// from ticks we observed; upstream's precomputed 24h fields are ignored.
package engine

import (
	"math"
	"sync"

	"pricing-engine/internal/metrics"
	"pricing-engine/internal/model"
)

const (
	// Ticks stamped further than this ahead of local receive time are bad data.
	maxFutureSkewMs = 5 * 60_000
	// Recent tick fingerprints kept per symbol for duplicate detection.
	dedupeRing = 64
)

type tickKey struct {
	ts        int64
	buy, sell float64
}

type symbolState struct {
	w         window
	last      model.Last
	hasLast   bool
	lastDirty bool
	recent    [dedupeRing]tickKey
	ri        int
	quote     model.Quote
	touched   bool
	recvNano  int64
}

func (s *symbolState) seen(k tickKey) bool {
	for i := range s.recent {
		if s.recent[i] == k {
			return true
		}
	}
	s.recent[s.ri] = k
	s.ri = (s.ri + 1) % dedupeRing
	return false
}

// Engine is safe for concurrent use. Apply is called from a single pipeline
// goroutine; readers (snapshots, candles, persistence) take the lock briefly.
type Engine struct {
	mu      sync.RWMutex
	syms    map[string]*symbolState // fixed key set after New
	order   []string
	clockMs int64 // event-time clock: max valid tick timestamp seen
	seq     uint64
	m       *metrics.Set
}

func New(symbols []string, m *metrics.Set) *Engine {
	e := &Engine{syms: make(map[string]*symbolState, len(symbols)), m: m}
	for _, s := range symbols {
		if _, dup := e.syms[s]; dup {
			continue
		}
		e.syms[s] = &symbolState{w: newWindow(), quote: model.Quote{Symbol: s}}
		e.order = append(e.order, s)
	}
	return e
}

// Has reports whether symbol is tracked. The symbol set is immutable.
func (e *Engine) Has(symbol string) bool { _, ok := e.syms[symbol]; return ok }

// Symbols returns tracked symbols in configuration order.
func (e *Engine) Symbols() []string { return e.order }

func valid(t *model.Tick) bool {
	return t.Buy > 0 && t.Sell > 0 && !math.IsInf(t.Buy, 0) && !math.IsInf(t.Sell, 0) &&
		!math.IsNaN(t.Buy) && !math.IsNaN(t.Sell) && t.TS > 0 &&
		(t.RecvNano == 0 || t.TS <= t.RecvNano/1e6+maxFutureSkewMs)
}

// Apply ingests a batch of ticks and returns the quotes that changed.
func (e *Engine) Apply(ticks []model.Tick) []model.Quote {
	e.mu.Lock()
	defer e.mu.Unlock()

	prevMin := e.clockMs / BucketMillis
	for i := range ticks {
		if t := &ticks[i]; valid(t) && t.TS > e.clockMs && e.syms[t.Symbol] != nil {
			e.clockMs = t.TS
		}
	}
	nowMin := e.clockMs / BucketMillis

	var touched []*symbolState
	for i := range ticks {
		t := &ticks[i]
		st := e.syms[t.Symbol]
		if st == nil || !valid(t) {
			e.m.RejectedTotal.Inc()
			continue
		}
		if st.seen(tickKey{t.TS, t.Buy, t.Sell}) {
			e.m.DuplicatesTotal.Inc()
			continue
		}
		if !st.w.add(midPrice(t.Buy, t.Sell), t.TS, nowMin) {
			e.m.RejectedTotal.Inc() // older than the 24h window
			continue
		}
		e.m.TicksTotal.Inc()
		if !st.hasLast || t.TS >= st.last.TS {
			st.last = model.Last{Buy: t.Buy, Sell: t.Sell, TS: t.TS}
			st.hasLast, st.lastDirty = true, true
		} else {
			e.m.OutOfOrderTotal.Inc()
		}
		st.recvNano = max(st.recvNano, t.RecvNano)
		if !st.touched {
			st.touched = true
			touched = append(touched, st)
		}
	}

	// Crossing a minute boundary can expire old buckets for symbols that did
	// not tick (e.g. FX over the weekend), so re-evaluate all of them.
	if prevMin != 0 && nowMin > prevMin {
		for _, s := range e.order {
			if st := e.syms[s]; !st.touched && st.hasLast {
				st.touched = true
				touched = append(touched, st)
			}
		}
	}

	var out []model.Quote
	for _, st := range touched {
		if q, changed := e.refresh(st, nowMin); changed {
			out = append(out, q)
		}
		st.touched, st.recvNano = false, 0
	}
	return out
}

func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }

// midPrice is the statistic basis for high/low/change. Rounding to 1e-8
// removes float noise (1.1580750000000002) that would bloat payloads.
func midPrice(buy, sell float64) float64 { return math.Round((buy+sell)/2*1e8) / 1e8 }

// refresh recomputes st's quote and reports whether anything visible changed.
func (e *Engine) refresh(st *symbolState, nowMin int64) (model.Quote, bool) {
	if !st.hasLast {
		return model.Quote{}, false
	}
	mid := midPrice(st.last.Buy, st.last.Sell)
	hi, lo, ref, ok := st.w.stats(nowMin)
	if !ok {
		hi, lo, ref = mid, mid, mid
	}
	chg := 0.0
	if ref != 0 {
		chg = round4((mid - ref) / ref * 100)
	}
	q := model.Quote{
		Symbol: st.quote.Symbol, Buy: st.last.Buy, Sell: st.last.Sell,
		High24h: hi, Low24h: lo, ChangePct: chg, TS: st.last.TS,
	}
	prev := st.quote
	if prev.Seq != 0 && q.Buy == prev.Buy && q.Sell == prev.Sell && q.High24h == prev.High24h &&
		q.Low24h == prev.Low24h && q.ChangePct == prev.ChangePct && q.TS == prev.TS {
		return prev, false
	}
	e.seq++
	q.Seq, q.RecvNano = e.seq, st.recvNano
	st.quote = q
	return q, true
}

// Snapshot returns current quotes for symbols (all when symbols is nil).
func (e *Engine) Snapshot(symbols []string) []model.Quote {
	if symbols == nil {
		symbols = e.order
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]model.Quote, 0, len(symbols))
	for _, s := range symbols {
		if st := e.syms[s]; st != nil && st.quote.Seq != 0 {
			q := st.quote
			q.RecvNano = 0
			out = append(out, q)
		}
	}
	return out
}

// Candles returns up to limit 1-minute OHLC candles (mid price), oldest first.
func (e *Engine) Candles(symbol string, limit int) []model.Bucket {
	st := e.syms[symbol]
	if st == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return st.w.candles(e.clockMs/BucketMillis, limit)
}

// DrainDirty returns all state changed since the previous call.
func (e *Engine) DrainDirty() []model.Delta {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []model.Delta
	for _, s := range e.order {
		st := e.syms[s]
		changed, deleted := st.w.drainDirty()
		if len(changed) == 0 && len(deleted) == 0 && !st.lastDirty {
			continue
		}
		d := model.Delta{Symbol: s, Buckets: changed, Deleted: deleted}
		if st.lastDirty {
			l := st.last
			d.Last = &l
			st.lastDirty = false
		}
		out = append(out, d)
	}
	return out
}

// Restore loads persisted state. Call before Apply. It returns the number of
// buckets restored; expired buckets are dropped and scheduled for deletion.
func (e *Engine) Restore(states map[string]model.SymbolState) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	for s, ss := range states {
		if e.syms[s] == nil {
			continue
		}
		e.clockMs = max(e.clockMs, ss.Last.TS)
		for _, bk := range ss.Buckets {
			e.clockMs = max(e.clockMs, bk.CloseTS)
		}
	}
	nowMin := e.clockMs / BucketMillis
	n := 0
	for s, ss := range states {
		st := e.syms[s]
		if st == nil {
			continue
		}
		for _, bk := range ss.Buckets {
			if !inWindow(bk.Minute, nowMin) {
				st.w.deleted = append(st.w.deleted, bk.Minute)
				continue
			}
			st.w.put(bk)
			n++
		}
		if ss.Last.TS > 0 {
			st.last, st.hasLast = ss.Last, true
		}
	}
	for _, s := range e.order {
		e.refresh(e.syms[s], nowMin)
	}
	return n
}
