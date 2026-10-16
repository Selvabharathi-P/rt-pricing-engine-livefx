// Package model holds the plain data types shared across the pipeline.
package model

// Tick is one normalised price update from the upstream feed.
// Upstream 24h fields (low_24h, high_24h, change_24h) are intentionally
// never decoded — the engine computes them itself.
type Tick struct {
	Symbol   string
	Buy      float64
	Sell     float64
	TS       int64 // event time, unix milliseconds
	RecvNano int64 // local receive time, unix nanoseconds (latency tracking)
}

// Quote is the engine's computed view of a symbol, published to clients.
type Quote struct {
	Symbol    string
	Buy       float64
	Sell      float64
	High24h   float64
	Low24h    float64
	ChangePct float64 // 24h % change
	TS        int64   // event time of the latest tick, unix ms
	Seq       uint64  // global, monotonically increasing publish sequence
	RecvNano  int64   // receive time of the tick that produced this quote
}

// Bucket is one minute of aggregated prices (also served as a 1m OHLC candle).
type Bucket struct {
	Minute  int64 // unix minute (TS / 60000)
	Open    float64
	High    float64
	Low     float64
	Close   float64
	OpenTS  int64
	CloseTS int64
	Count   uint32
}

// Last is the latest (by event time) top-of-book for a symbol.
type Last struct {
	Buy  float64
	Sell float64
	TS   int64
}

// SymbolState is everything needed to rebuild one symbol after a restart.
type SymbolState struct {
	Buckets []Bucket
	Last    Last
}

// Delta is the set of changes to persist for one symbol since the last flush.
type Delta struct {
	Symbol  string
	Buckets []Bucket // new or updated buckets
	Deleted []int64  // minutes that left the window and can be removed
	Last    *Last    // nil when unchanged
}
