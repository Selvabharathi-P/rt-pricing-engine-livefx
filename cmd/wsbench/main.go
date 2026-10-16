// Command wsbench opens N downstream clients against the engine and reports
// end-to-end latency (upstream tick timestamp -> client receive) and frame
// sizes. Run it on the same host as the simulator so clocks agree.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	url := flag.String("url", "ws://localhost:8080/ws", "engine WebSocket URL")
	clients := flag.Int("clients", 50, "concurrent clients")
	dur := flag.Duration("duration", 30*time.Second, "test duration")
	format := flag.String("format", "json", "json or binary")
	key := flag.String("key", "", "API key (pk_...) when the server requires auth")
	secret := flag.String("secret", "", "API secret (sk_...)")
	flag.Parse()

	var (
		mu        sync.Mutex
		lat       []float64
		maxFrame  atomic.Int64
		frames    atomic.Int64
		bytes     atomic.Int64
		rows      atomic.Int64
		connected atomic.Int64
		wg        sync.WaitGroup
	)
	deadline := time.Now().Add(*dur)
	for i := 0; i < *clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u := *url + "?symbols=*"
			if *format == "binary" {
				u += "&format=binary"
			}
			conn, _, err := websocket.DefaultDialer.Dial(u, nil)
			if err != nil {
				fmt.Fprintln(os.Stderr, "dial:", err)
				return
			}
			defer conn.Close()
			if *key != "" {
				if err := conn.WriteJSON(map[string]string{"action": "auth", "api_key": *key, "secret": *secret}); err != nil {
					fmt.Fprintln(os.Stderr, "auth:", err)
					return
				}
			}
			connected.Add(1)
			local := make([]float64, 0, 4096)
			warm := time.Now().Add(2 * time.Second) // skip the initial snapshot
			for time.Now().Before(deadline) {
				conn.SetReadDeadline(deadline)
				kind, msg, err := conn.ReadMessage()
				if err != nil {
					break
				}
				now := time.Now().UnixMilli()
				var tss []int64
				if kind == websocket.BinaryMessage {
					tss = binTS(msg)
				} else {
					tss = jsonTS(msg)
				}
				if len(tss) == 0 {
					continue // control message
				}
				frames.Add(1)
				bytes.Add(int64(len(msg)))
				rows.Add(int64(len(tss)))
				for {
					m := maxFrame.Load()
					if int64(len(msg)) <= m || maxFrame.CompareAndSwap(m, int64(len(msg))) {
						break
					}
				}
				if time.Now().After(warm) {
					for _, ts := range tss {
						local = append(local, float64(now-ts))
					}
				}
			}
			mu.Lock()
			lat = append(lat, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	slices.Sort(lat)
	q := func(p float64) float64 {
		if len(lat) == 0 {
			return math.NaN()
		}
		return lat[min(len(lat)-1, int(p*float64(len(lat))))]
	}
	secs := dur.Seconds()
	fmt.Printf("clients=%d connected=%d duration=%s format=%s\n", *clients, connected.Load(), *dur, *format)
	fmt.Printf("frames=%d (%.0f/s)  rows=%d  avg_frame=%.0fB  max_frame=%dB\n",
		frames.Load(), float64(frames.Load())/secs, rows.Load(), float64(bytes.Load())/math.Max(1, float64(frames.Load())), maxFrame.Load())
	fmt.Printf("e2e latency ms (tick ts -> client recv): p50=%.0f p95=%.0f p99=%.0f max=%.0f samples=%d\n",
		q(0.50), q(0.95), q(0.99), q(1), len(lat))
}

func jsonTS(msg []byte) []int64 {
	var f struct {
		T string              `json:"t"`
		D [][]json.RawMessage `json:"d"`
	}
	if json.Unmarshal(msg, &f) != nil || f.T != "q" {
		return nil
	}
	out := make([]int64, 0, len(f.D))
	for _, row := range f.D {
		if len(row) == 7 {
			var ts int64
			json.Unmarshal(row[6], &ts)
			out = append(out, ts)
		}
	}
	return out
}

func binTS(msg []byte) []int64 {
	if len(msg) < 4 || msg[0] != 'Q' {
		return nil
	}
	n := int(binary.LittleEndian.Uint16(msg[2:]))
	out := make([]int64, 0, n)
	p := 4
	for i := 0; i < n && p < len(msg); i++ {
		l := int(msg[p])
		p += 1 + l + 5*8
		if p+8 > len(msg) {
			break
		}
		out = append(out, int64(binary.LittleEndian.Uint64(msg[p:])))
		p += 8
	}
	return out
}
