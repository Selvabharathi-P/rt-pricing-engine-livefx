package engine

import "pricing-engine/internal/model"

const (
	BucketMillis  = 60_000 // 1-minute buckets
	WindowBuckets = 1440   // 24h
)

// window is a fixed ring of 1440 one-minute buckets for a single symbol.
//
// Cost model:
//   - every tick: O(1) — update one bucket, compare against cached aggregates
//   - once per minute (when the clock crosses a bucket boundary): O(1440)
//     rescan of closed buckets to drop the bucket that left the window
//
// The window edge therefore has 1-minute granularity: a price is counted in
// the 24h high/low until its whole minute bucket is older than 24h.
type window struct {
	b [WindowBuckets]model.Bucket

	// Aggregates over closed buckets (minute < aggMinute) inside the window.
	aggMinute int64
	aggValid  bool
	hasAgg    bool
	hi, lo    float64
	refMinute int64   // oldest bucket in window
	ref       float64 // its open = price 24h ago, for % change

	dirty   map[int64]struct{} // minutes changed since last flush
	deleted []int64            // minutes evicted since last flush
}

func newWindow() window { return window{dirty: make(map[int64]struct{})} }

func inWindow(minute, nowMin int64) bool {
	return minute > nowMin-WindowBuckets && minute <= nowMin
}

// add records price at ts. It returns false when ts is already outside the
// 24h window relative to nowMin.
func (w *window) add(price float64, ts, nowMin int64) bool {
	minute := ts / BucketMillis
	if !inWindow(minute, nowMin) {
		return false
	}
	slot := &w.b[minute%WindowBuckets]
	if slot.Count == 0 || slot.Minute != minute {
		if slot.Count > 0 {
			w.deleted = append(w.deleted, slot.Minute)
		}
		*slot = model.Bucket{Minute: minute, Open: price, High: price, Low: price, Close: price, OpenTS: ts, CloseTS: ts, Count: 1}
	} else {
		slot.High = max(slot.High, price)
		slot.Low = min(slot.Low, price)
		if ts < slot.OpenTS {
			slot.Open, slot.OpenTS = price, ts
		}
		if ts >= slot.CloseTS {
			slot.Close, slot.CloseTS = price, ts
		}
		slot.Count++
	}
	w.dirty[minute] = struct{}{}

	// A late tick landed in an already-closed bucket: fold it into the
	// cached aggregates instead of rescanning.
	if w.aggValid && minute < w.aggMinute {
		if !w.hasAgg {
			w.hasAgg, w.hi, w.lo, w.refMinute, w.ref = true, price, price, minute, slot.Open
		} else {
			w.hi, w.lo = max(w.hi, price), min(w.lo, price)
			if minute <= w.refMinute {
				w.refMinute, w.ref = minute, slot.Open
			}
		}
	}
	return true
}

// put restores a persisted bucket verbatim.
func (w *window) put(bk model.Bucket) {
	slot := &w.b[bk.Minute%WindowBuckets]
	if slot.Count == 0 || bk.Minute >= slot.Minute {
		*slot = bk
	}
	w.aggValid = false
}

func (w *window) recompute(nowMin int64) {
	w.hasAgg = false
	for i := range w.b {
		bk := &w.b[i]
		if bk.Count == 0 || bk.Minute >= nowMin || !inWindow(bk.Minute, nowMin) {
			continue
		}
		if !w.hasAgg {
			w.hasAgg, w.hi, w.lo, w.refMinute, w.ref = true, bk.High, bk.Low, bk.Minute, bk.Open
			continue
		}
		w.hi, w.lo = max(w.hi, bk.High), min(w.lo, bk.Low)
		if bk.Minute < w.refMinute {
			w.refMinute, w.ref = bk.Minute, bk.Open
		}
	}
	w.aggMinute, w.aggValid = nowMin, true
}

// stats returns the rolling 24h high, low and reference (oldest) price.
func (w *window) stats(nowMin int64) (hi, lo, ref float64, ok bool) {
	if !w.aggValid || w.aggMinute != nowMin {
		w.recompute(nowMin)
	}
	hi, lo, ref, ok = w.hi, w.lo, w.ref, w.hasAgg
	if cur := &w.b[nowMin%WindowBuckets]; cur.Count > 0 && cur.Minute == nowMin {
		if !ok {
			return cur.High, cur.Low, cur.Open, true
		}
		hi, lo = max(hi, cur.High), min(lo, cur.Low)
	}
	return hi, lo, ref, ok
}

// candles returns the buckets inside the window, oldest first, at most limit.
func (w *window) candles(nowMin int64, limit int) []model.Bucket {
	out := make([]model.Bucket, 0, min(limit, WindowBuckets))
	for m := max(nowMin-int64(limit)+1, nowMin-WindowBuckets+1); m <= nowMin; m++ {
		if bk := w.b[m%WindowBuckets]; bk.Count > 0 && bk.Minute == m {
			out = append(out, bk)
		}
	}
	return out
}

// drainDirty returns changed buckets and evicted minutes, then resets tracking.
func (w *window) drainDirty() (changed []model.Bucket, deleted []int64) {
	for m := range w.dirty {
		if bk := w.b[m%WindowBuckets]; bk.Count > 0 && bk.Minute == m {
			changed = append(changed, bk)
		}
		delete(w.dirty, m)
	}
	deleted, w.deleted = w.deleted, nil
	return changed, deleted
}
