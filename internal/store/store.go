// Package store persists engine state so it survives restarts.
package store

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"pricing-engine/internal/metrics"
	"pricing-engine/internal/model"
)

type Store interface {
	Load(ctx context.Context, symbols []string) (map[string]model.SymbolState, error)
	Save(ctx context.Context, deltas []model.Delta) error
	Ping(ctx context.Context) error
	Close() error
}

// Mem is an in-process Store used by tests and as a no-persistence fallback.
type Mem struct {
	mu       sync.Mutex
	buckets  map[string]map[int64]model.Bucket
	last     map[string]model.Last
	FailSave error // when set, Save returns it (fault injection in tests)
}

func NewMem() *Mem {
	return &Mem{buckets: map[string]map[int64]model.Bucket{}, last: map[string]model.Last{}}
}

func (m *Mem) Load(_ context.Context, symbols []string) (map[string]model.SymbolState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]model.SymbolState{}
	for _, s := range symbols {
		var ss model.SymbolState
		for _, b := range m.buckets[s] {
			ss.Buckets = append(ss.Buckets, b)
		}
		ss.Last = m.last[s]
		if len(ss.Buckets) > 0 || ss.Last.TS > 0 {
			out[s] = ss
		}
	}
	return out, nil
}

func (m *Mem) Save(_ context.Context, deltas []model.Delta) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailSave != nil {
		return m.FailSave
	}
	for _, d := range deltas {
		bs := m.buckets[d.Symbol]
		if bs == nil {
			bs = map[int64]model.Bucket{}
			m.buckets[d.Symbol] = bs
		}
		for _, b := range d.Buckets {
			bs[b.Minute] = b
		}
		for _, minute := range d.Deleted {
			delete(bs, minute)
		}
		if d.Last != nil {
			m.last[d.Symbol] = *d.Last
		}
	}
	return nil
}

func (m *Mem) Ping(context.Context) error { return nil }
func (m *Mem) Close() error               { return nil }

// pending accumulates deltas that have not been persisted yet.
type pending struct {
	buckets map[int64]model.Bucket
	deleted map[int64]struct{}
	last    *model.Last
}

// Flusher periodically drains engine changes into the Store. If a save
// fails (e.g. Redis restarting) the changes are kept and merged with the
// next batch, so nothing is lost while the process stays up.
type Flusher struct {
	Store    Store
	Source   func() []model.Delta
	Interval time.Duration
	Log      *slog.Logger
	Metrics  *metrics.Set

	pend map[string]*pending
}

func (f *Flusher) Run(ctx context.Context) {
	t := time.NewTicker(f.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Final flush on shutdown, with its own deadline.
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := f.Flush(fctx); err != nil {
				f.Log.Error("final state flush failed", "err", err)
			} else {
				f.Log.Info("final state flush complete")
			}
			cancel()
			return
		case <-t.C:
			// Bounded so a hung store can't stall flushing indefinitely.
			fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := f.Flush(fctx); err != nil && ctx.Err() == nil {
				f.Log.Warn("state flush failed; will retry", "err", err)
			}
			cancel()
		}
	}
}

// Flush persists everything changed since the last successful flush.
func (f *Flusher) Flush(ctx context.Context) error {
	if f.pend == nil {
		f.pend = map[string]*pending{}
	}
	for _, d := range f.Source() {
		p := f.pend[d.Symbol]
		if p == nil {
			p = &pending{buckets: map[int64]model.Bucket{}, deleted: map[int64]struct{}{}}
			f.pend[d.Symbol] = p
		}
		for _, b := range d.Buckets {
			p.buckets[b.Minute] = b
			delete(p.deleted, b.Minute)
		}
		for _, m := range d.Deleted {
			delete(p.buckets, m)
			p.deleted[m] = struct{}{}
		}
		if d.Last != nil {
			p.last = d.Last
		}
	}
	if len(f.pend) == 0 {
		return nil
	}
	deltas := make([]model.Delta, 0, len(f.pend))
	for s, p := range f.pend {
		d := model.Delta{Symbol: s, Last: p.last}
		for _, b := range p.buckets {
			d.Buckets = append(d.Buckets, b)
		}
		for m := range p.deleted {
			d.Deleted = append(d.Deleted, m)
		}
		deltas = append(deltas, d)
	}
	start := time.Now()
	if err := f.Store.Save(ctx, deltas); err != nil {
		f.Metrics.StoreFlushErrors.Inc()
		return err
	}
	f.Metrics.StoreFlushTime.Observe(time.Since(start).Seconds())
	clear(f.pend)
	return nil
}
