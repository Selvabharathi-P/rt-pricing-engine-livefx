package engine

import (
	"context"
	"math"
	"math/rand/v2"
	"testing"

	"pricing-engine/internal/metrics"
	"pricing-engine/internal/model"
	"pricing-engine/internal/store"
)

const (
	t0     = int64(1_749_999_960_000) // fixed epoch ms, minute-aligned
	oneMin = int64(60_000)
	day    = 24 * 60 * oneMin
)

func tick(sym string, ts int64, mid float64) model.Tick {
	return model.Tick{Symbol: sym, Buy: mid + 0.0001, Sell: mid - 0.0001, TS: ts}
}

func quoteOf(t *testing.T, e *Engine, sym string) model.Quote {
	t.Helper()
	qs := e.Snapshot([]string{sym})
	if len(qs) != 1 {
		t.Fatalf("no quote for %s", sym)
	}
	return qs[0]
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestRollingStats(t *testing.T) {
	m := metrics.NewSet()
	e := New([]string{"EURUSD"}, m)
	e.Apply([]model.Tick{tick("EURUSD", t0, 1.10)})
	e.Apply([]model.Tick{tick("EURUSD", t0+10*oneMin, 1.20)})
	e.Apply([]model.Tick{tick("EURUSD", t0+20*oneMin, 1.05)})
	q := quoteOf(t, e, "EURUSD")
	if !near(q.High24h, 1.20) || !near(q.Low24h, 1.05) {
		t.Fatalf("high/low = %v/%v", q.High24h, q.Low24h)
	}
	// change vs the oldest price in window (1.10)
	want := math.Round((1.05-1.10)/1.10*100*1e4) / 1e4
	if q.ChangePct != want {
		t.Fatalf("change = %v want %v", q.ChangePct, want)
	}
	if !near(q.Buy, 1.0501) || !near(q.Sell, 1.0499) {
		t.Fatalf("buy/sell = %v/%v", q.Buy, q.Sell)
	}
}

func TestWindowExpiry(t *testing.T) {
	e := New([]string{"EURUSD", "BTCUSD"}, metrics.NewSet())
	e.Apply([]model.Tick{tick("EURUSD", t0, 2.0), tick("BTCUSD", t0, 100)})
	e.Apply([]model.Tick{tick("EURUSD", t0+oneMin, 1.0)})
	// 24h + 2min later only BTC ticks: EURUSD's old buckets must expire too.
	out := e.Apply([]model.Tick{tick("BTCUSD", t0+day+2*oneMin, 101)})
	var sawEUR bool
	for _, q := range out {
		if q.Symbol == "EURUSD" {
			sawEUR = true
		}
	}
	if !sawEUR {
		t.Fatal("expected EURUSD to be republished after its window expired")
	}
	q := quoteOf(t, e, "EURUSD")
	if !near(q.High24h, 1.0) || !near(q.Low24h, 1.0) || q.ChangePct != 0 {
		t.Fatalf("after expiry: %+v", q)
	}
	b := quoteOf(t, e, "BTCUSD")
	if !near(b.High24h, 101) || !near(b.Low24h, 101) {
		t.Fatalf("btc window not rolled: %+v", b)
	}
}

func TestDuplicatesAndOutOfOrder(t *testing.T) {
	m := metrics.NewSet()
	e := New([]string{"EURUSD"}, m)
	e.Apply([]model.Tick{tick("EURUSD", t0+5000, 1.10)})
	if out := e.Apply([]model.Tick{tick("EURUSD", t0+5000, 1.10)}); len(out) != 0 {
		t.Fatalf("duplicate produced output: %v", out)
	}
	if m.DuplicatesTotal.Value() != 1 {
		t.Fatalf("duplicates = %d", m.DuplicatesTotal.Value())
	}
	// Late tick with a higher price: counts toward the high, not the last price.
	e.Apply([]model.Tick{tick("EURUSD", t0+1000, 1.30)})
	q := quoteOf(t, e, "EURUSD")
	if !near(q.High24h, 1.30) {
		t.Fatalf("late tick ignored for high: %v", q.High24h)
	}
	if !near(q.Buy, 1.1001) || q.TS != t0+5000 {
		t.Fatalf("late tick overwrote last price: %+v", q)
	}
	if m.OutOfOrderTotal.Value() != 1 {
		t.Fatalf("ooo = %d", m.OutOfOrderTotal.Value())
	}
	// The late tick was the earliest -> it is the 24h reference price.
	want := math.Round((1.10-1.30)/1.30*100*1e4) / 1e4
	if q.ChangePct != want {
		t.Fatalf("change = %v want %v", q.ChangePct, want)
	}
}

func TestRejectsBadTicks(t *testing.T) {
	m := metrics.NewSet()
	e := New([]string{"EURUSD"}, m)
	recv := t0 * 1e6
	bad := []model.Tick{
		{Symbol: "XXXYYY", Buy: 1, Sell: 1, TS: t0},                            // unknown symbol
		{Symbol: "EURUSD", Buy: 0, Sell: 1, TS: t0},                            // zero price
		{Symbol: "EURUSD", Buy: math.NaN(), Sell: 1, TS: t0},                   // NaN
		{Symbol: "EURUSD", Buy: 1, Sell: 1, TS: t0 + 3600_000, RecvNano: recv}, // far future
	}
	if out := e.Apply(bad); len(out) != 0 {
		t.Fatalf("bad ticks produced output: %v", out)
	}
	if m.RejectedTotal.Value() != 4 {
		t.Fatalf("rejected = %d", m.RejectedTotal.Value())
	}
	// A tick older than the window is rejected once the clock has moved on.
	e.Apply([]model.Tick{tick("EURUSD", t0+2*day, 1.1)})
	e.Apply([]model.Tick{tick("EURUSD", t0, 9.9)})
	if q := quoteOf(t, e, "EURUSD"); q.High24h > 2 {
		t.Fatalf("stale tick entered window: %+v", q)
	}
}

func TestNoChangeNoPublish(t *testing.T) {
	e := New([]string{"EURUSD"}, metrics.NewSet())
	e.Apply([]model.Tick{tick("EURUSD", t0, 1.1)})
	// Different timestamp => TS changes => publish. Same values & TS => nothing.
	if out := e.Apply([]model.Tick{tick("EURUSD", t0+1, 1.1)}); len(out) != 1 {
		t.Fatalf("want 1 quote, got %d", len(out))
	}
}

func TestSeqMonotonic(t *testing.T) {
	e := New([]string{"A", "B"}, metrics.NewSet())
	var last uint64
	for i := int64(0); i < 100; i++ {
		for _, q := range e.Apply([]model.Tick{tick("A", t0+i*1000, 1+float64(i)/1000), tick("B", t0+i*1000, 2)}) {
			if q.Seq <= last {
				t.Fatalf("seq not increasing: %d after %d", q.Seq, last)
			}
			last = q.Seq
		}
	}
}

// TestAgainstBruteForce compares the bucketed window with a naive
// recomputation over every raw tick, including late and duplicate ticks.
func TestAgainstBruteForce(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	e := New([]string{"X"}, metrics.NewSet())
	type raw struct {
		ts  int64
		mid float64
	}
	var all []raw
	seen := map[[2]float64]bool{}
	ts := t0
	price := 100.0
	for i := 0; i < 20000; i++ {
		ts += r.Int64N(20_000) // up to 20s between ticks -> spans ~2.3 days
		price *= 1 + r.NormFloat64()*1e-3
		tt := ts
		if r.Float64() < 0.05 {
			tt -= r.Int64N(10 * oneMin) // late tick
		}
		k := tick("X", tt, price)
		e.Apply([]model.Tick{k})
		key := [2]float64{float64(tt), k.Buy}
		if !seen[key] {
			seen[key] = true
			all = append(all, raw{tt, midPrice(k.Buy, k.Sell)})
		}
		if i%500 != 0 {
			continue
		}
		nowMin := e.clockMs / BucketMillis
		hi, lo := math.Inf(-1), math.Inf(1)
		refTS, ref := int64(math.MaxInt64), 0.0
		refMin := int64(math.MaxInt64)
		for _, x := range all {
			m := x.ts / BucketMillis
			if m <= nowMin-WindowBuckets {
				continue
			}
			hi, lo = math.Max(hi, x.mid), math.Min(lo, x.mid)
			if m < refMin || (m == refMin && x.ts < refTS) {
				refMin, refTS, ref = m, x.ts, x.mid
			}
		}
		q := quoteOf(t, e, "X")
		lastMid := midPrice(q.Buy, q.Sell)
		wantChg := math.Round((lastMid-ref)/ref*100*1e4) / 1e4
		if !near(q.High24h, hi) || !near(q.Low24h, lo) || q.ChangePct != wantChg {
			t.Fatalf("i=%d got hi=%v lo=%v chg=%v want hi=%v lo=%v chg=%v", i, q.High24h, q.Low24h, q.ChangePct, hi, lo, wantChg)
		}
	}
}

func TestPersistAndRestore(t *testing.T) {
	ctx := context.Background()
	m := metrics.NewSet()
	e := New([]string{"EURUSD", "GBPUSD"}, m)
	for i := int64(0); i < 300; i++ {
		e.Apply([]model.Tick{tick("EURUSD", t0+i*10_000, 1.1+float64(i%17)/1000), tick("GBPUSD", t0+i*10_000, 1.3)})
	}
	st := store.NewMem()
	f := &store.Flusher{Store: st, Source: e.DrainDirty, Metrics: m}
	if err := f.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	before := e.Snapshot(nil)

	// "Restart"
	e2 := New([]string{"EURUSD", "GBPUSD"}, metrics.NewSet())
	states, _ := st.Load(ctx, []string{"EURUSD", "GBPUSD"})
	if n := e2.Restore(states); n == 0 {
		t.Fatal("nothing restored")
	}
	after := e2.Snapshot(nil)
	if len(after) != len(before) {
		t.Fatalf("restored %d quotes, want %d", len(after), len(before))
	}
	for i := range before {
		b, a := before[i], after[i]
		if b.Symbol != a.Symbol || b.Buy != a.Buy || b.Sell != a.Sell || b.High24h != a.High24h ||
			b.Low24h != a.Low24h || b.ChangePct != a.ChangePct || b.TS != a.TS {
			t.Fatalf("mismatch after restore:\n before %+v\n after  %+v", b, a)
		}
	}
	// Continuing after restore keeps the window consistent.
	e2.Apply([]model.Tick{tick("EURUSD", t0+300*10_000, 1.5)})
	if q := quoteOf(t, e2, "EURUSD"); !near(q.High24h, 1.5) || !near(q.Low24h, 1.1) {
		t.Fatalf("post-restore: %+v", q)
	}
}

func TestFlusherKeepsDataWhenStoreFails(t *testing.T) {
	ctx := context.Background()
	m := metrics.NewSet()
	e := New([]string{"EURUSD"}, m)
	st := store.NewMem()
	st.FailSave = context.DeadlineExceeded
	f := &store.Flusher{Store: st, Source: e.DrainDirty, Metrics: m}
	e.Apply([]model.Tick{tick("EURUSD", t0, 1.1)})
	if f.Flush(ctx) == nil {
		t.Fatal("expected failure")
	}
	e.Apply([]model.Tick{tick("EURUSD", t0+oneMin, 1.2)})
	st.FailSave = nil
	if err := f.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	states, _ := st.Load(ctx, []string{"EURUSD"})
	if got := len(states["EURUSD"].Buckets); got != 2 {
		t.Fatalf("persisted %d buckets, want 2 (one from the failed flush)", got)
	}
}

func TestCandles(t *testing.T) {
	e := New([]string{"EURUSD"}, metrics.NewSet())
	for i := int64(0); i < 5; i++ {
		e.Apply([]model.Tick{tick("EURUSD", t0+i*oneMin, 1+float64(i)), tick("EURUSD", t0+i*oneMin+30_000, 1.5+float64(i))})
	}
	cs := e.Candles("EURUSD", 3)
	if len(cs) != 3 {
		t.Fatalf("got %d candles", len(cs))
	}
	if c := cs[2]; !near(c.Open, 5) || !near(c.Close, 5.5) || !near(c.High, 5.5) || c.Count != 2 {
		t.Fatalf("last candle %+v", c)
	}
}

func BenchmarkApply61Symbols(b *testing.B) {
	syms := make([]string, 61)
	for i := range syms {
		syms[i] = string(rune('A'+i/26)) + string(rune('A'+i%26)) + "USD"
	}
	e := New(syms, metrics.NewSet())
	batch := make([]model.Tick, len(syms))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ts := t0 + int64(i)*100
		for j, s := range syms {
			batch[j] = tick(s, ts, 1+float64(i%1000)/1e4)
		}
		e.Apply(batch)
	}
}
