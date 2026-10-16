// Package metrics is a tiny, dependency-free Prometheus text-format registry.
// It covers exactly what this service needs: counters, gauges and
// fixed-bucket histograms, all lock-free on the hot path.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
)

type Counter struct{ v atomic.Uint64 }

func (c *Counter) Inc()          { c.v.Add(1) }
func (c *Counter) Add(n uint64)  { c.v.Add(n) }
func (c *Counter) Value() uint64 { return c.v.Load() }

type Gauge struct{ v atomic.Int64 }

func (g *Gauge) Set(n int64)  { g.v.Store(n) }
func (g *Gauge) Add(n int64)  { g.v.Add(n) }
func (g *Gauge) Value() int64 { return g.v.Load() }

type Histogram struct {
	bounds  []float64
	counts  []atomic.Uint64 // len(bounds)+1, last is +Inf
	sumBits atomic.Uint64
	n       atomic.Uint64
}

func NewHistogram(bounds ...float64) *Histogram {
	return &Histogram{bounds: bounds, counts: make([]atomic.Uint64, len(bounds)+1)}
}

func (h *Histogram) Observe(v float64) {
	i := sort.SearchFloat64s(h.bounds, v)
	h.counts[i].Add(1)
	h.n.Add(1)
	for {
		old := h.sumBits.Load()
		if h.sumBits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+v)) {
			return
		}
	}
}

// Quantile estimates q (0..1) from bucket counts (upper bound of the bucket).
func (h *Histogram) Quantile(q float64) float64 {
	total := h.n.Load()
	if total == 0 {
		return 0
	}
	target := uint64(math.Ceil(q * float64(total)))
	var cum uint64
	for i := range h.counts {
		cum += h.counts[i].Load()
		if cum >= target {
			if i < len(h.bounds) {
				return h.bounds[i]
			}
			return math.Inf(1)
		}
	}
	return math.Inf(1)
}

type entry struct {
	name, help, kind string
	c                *Counter
	g                *Gauge
	h                *Histogram
}

type Registry struct {
	mu      sync.Mutex
	entries []entry
}

func (r *Registry) Counter(name, help string) *Counter {
	c := &Counter{}
	r.add(entry{name: name, help: help, kind: "counter", c: c})
	return c
}

func (r *Registry) Gauge(name, help string) *Gauge {
	g := &Gauge{}
	r.add(entry{name: name, help: help, kind: "gauge", g: g})
	return g
}

func (r *Registry) Histogram(name, help string, bounds ...float64) *Histogram {
	h := NewHistogram(bounds...)
	r.add(entry{name: name, help: help, kind: "histogram", h: h})
	return h
}

func (r *Registry) add(e entry) {
	r.mu.Lock()
	r.entries = append(r.entries, e)
	r.mu.Unlock()
}

// WritePrometheus writes all metrics in Prometheus text exposition format.
func (r *Registry) WritePrometheus(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", e.name, e.help, e.name, e.kind)
		switch e.kind {
		case "counter":
			fmt.Fprintf(w, "%s %d\n", e.name, e.c.Value())
		case "gauge":
			fmt.Fprintf(w, "%s %d\n", e.name, e.g.Value())
		case "histogram":
			var cum uint64
			for i, b := range e.h.bounds {
				cum += e.h.counts[i].Load()
				fmt.Fprintf(w, "%s_bucket{le=\"%s\"} %d\n", e.name, strconv.FormatFloat(b, 'g', -1, 64), cum)
			}
			cum += e.h.counts[len(e.h.bounds)].Load()
			fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", e.name, cum)
			fmt.Fprintf(w, "%s_sum %g\n%s_count %d\n", e.name, math.Float64frombits(e.h.sumBits.Load()), e.name, e.h.n.Load())
		}
	}
}

// LatencyBounds are histogram buckets in seconds, dense around the 50ms target.
var LatencyBounds = []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 1}

// SizeBounds are histogram buckets in bytes.
var SizeBounds = []float64{256, 512, 1024, 2048, 4096, 8192, 10240, 65536, 262144, 1048576}

// Set is every metric the service exports.
type Set struct {
	Registry *Registry

	TicksTotal         *Counter
	DuplicatesTotal    *Counter
	OutOfOrderTotal    *Counter
	RejectedTotal      *Counter
	UpstreamFrames     *Counter
	UpstreamReconn     *Counter
	UpstreamUp         *Gauge
	UpstreamBytes      *Histogram
	ParseErrors        *Counter
	UnrecognisedFrames *Counter
	ProcessLatency     *Histogram // receive -> handed to hub
	WSClients          *Gauge
	WSFramesTotal      *Counter
	WSFrameBytes       *Histogram
	WSSendLatency      *Histogram // receive -> written to client socket
	WSConflated        *Counter
	WSSlowDisconnect   *Counter
	StoreFlushErrors   *Counter
	StoreFlushTime     *Histogram
}

func NewSet() *Set {
	r := &Registry{}
	return &Set{
		Registry:           r,
		TicksTotal:         r.Counter("pe_ticks_total", "Ticks accepted by the engine."),
		DuplicatesTotal:    r.Counter("pe_ticks_duplicate_total", "Duplicate ticks dropped."),
		OutOfOrderTotal:    r.Counter("pe_ticks_out_of_order_total", "Ticks older than the latest seen for their symbol."),
		RejectedTotal:      r.Counter("pe_ticks_rejected_total", "Invalid or out-of-window ticks rejected."),
		UpstreamFrames:     r.Counter("pe_upstream_frames_total", "Frames read from upstream."),
		UpstreamReconn:     r.Counter("pe_upstream_reconnects_total", "Upstream reconnect attempts."),
		UpstreamUp:         r.Gauge("pe_upstream_connected", "1 when the upstream session is live."),
		UpstreamBytes:      r.Histogram("pe_upstream_frame_bytes", "Upstream frame size.", SizeBounds...),
		ParseErrors:        r.Counter("pe_upstream_parse_errors_total", "Upstream frames that failed to parse."),
		UnrecognisedFrames: r.Counter("pe_upstream_unrecognised_frames_total", "Valid JSON upstream frames containing no recognised ticks."),
		ProcessLatency:     r.Histogram("pe_process_latency_seconds", "Upstream receive to hub publish.", LatencyBounds...),
		WSClients:          r.Gauge("pe_ws_clients", "Connected downstream WebSocket clients."),
		WSFramesTotal:      r.Counter("pe_ws_frames_total", "Frames written to downstream clients."),
		WSFrameBytes:       r.Histogram("pe_ws_frame_bytes", "Downstream frame size (pre-compression).", SizeBounds...),
		WSSendLatency:      r.Histogram("pe_ws_send_latency_seconds", "Upstream receive to downstream socket write.", LatencyBounds...),
		WSConflated:        r.Counter("pe_ws_conflated_total", "Quotes superseded before a slow client could receive them."),
		WSSlowDisconnect:   r.Counter("pe_ws_slow_disconnects_total", "Clients dropped for failing to accept writes."),
		StoreFlushErrors:   r.Counter("pe_store_flush_errors_total", "Failed state flushes."),
		StoreFlushTime:     r.Histogram("pe_store_flush_seconds", "State flush duration.", LatencyBounds...),
	}
}
