package feed

import (
	"bytes"
	"errors"
	json "github.com/goccy/go-json"
	"math"
	"strconv"
	"strings"
	"time"

	"pricing-engine/internal/model"
)

// The upstream framing is not formally specified, so the parser accepts the
// common shapes: a single tick object, an array of ticks, a wrapper object
// ({"data": ...}, {"prices": ...}, ...) or a symbol-keyed map. Numbers may be
// JSON numbers or strings. low_24h / high_24h / change_24h are never decoded.

type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	b = unquote(b)
	if len(b) == 0 || string(b) == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return err
	}
	*f = flexFloat(v)
	return nil
}

// flexTS normalises seconds / ms / µs / ns epoch values or RFC 3339 to unix ms.
type flexTS int64

func (t *flexTS) UnmarshalJSON(b []byte) error {
	quoted := len(b) > 0 && b[0] == '"'
	b = unquote(b)
	if len(b) == 0 || string(b) == "null" {
		*t = 0
		return nil
	}
	if v, err := strconv.ParseFloat(string(b), 64); err == nil {
		*t = flexTS(epochToMillis(v))
		return nil
	}
	if !quoted {
		return errors.New("invalid timestamp")
	}
	ts, err := time.Parse(time.RFC3339Nano, string(b))
	if err != nil {
		return err
	}
	*t = flexTS(ts.UnixMilli())
	return nil
}

func epochToMillis(v float64) int64 {
	switch a := math.Abs(v); {
	case a < 1e11: // seconds
		return int64(v * 1e3)
	case a < 1e14: // milliseconds
		return int64(v)
	case a < 1e17: // microseconds
		return int64(v / 1e3)
	default: // nanoseconds
		return int64(v / 1e6)
	}
}

func unquote(b []byte) []byte {
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		return b[1 : len(b)-1]
	}
	return b
}

// The live LiveFXHub feed sends one object per frame:
//
//	{"event":"marketPriceSnapshot","symbol":"AUDUSD","bid":0.69563,"ask":0.69588,
//	 "high":0.69631,"low":0.69194,"percentage":0.57}
//
// i.e. bid/ask instead of buy/sell and no timestamp (receive time is used).
// high/low/percentage are upstream 24h figures and are deliberately ignored.
type wireTick struct {
	Symbol    string    `json:"symbol"`
	Buy       flexFloat `json:"buy"`
	Sell      flexFloat `json:"sell"`
	Ask       flexFloat `json:"ask"` // client buys at the ask
	Bid       flexFloat `json:"bid"` // client sells at the bid
	Timestamp flexTS    `json:"timestamp"`
	TS        flexTS    `json:"ts"`
}

type envelope struct {
	wireTick
	Data    json.RawMessage `json:"data"`
	Prices  json.RawMessage `json:"prices"`
	Payload json.RawMessage `json:"payload"`
	Result  json.RawMessage `json:"result"`
	Ticks   json.RawMessage `json:"ticks"`
}

// Parse extracts all ticks from one upstream frame. A frame with no price
// data (acks, heartbeats) returns (nil, nil).
func Parse(b []byte, recvNano int64) ([]model.Tick, error) {
	p := parser{recvNano: recvNano, nowMs: recvNano / 1e6}
	if err := p.value(b, 0); err != nil {
		return nil, err
	}
	return p.out, nil
}

type parser struct {
	recvNano, nowMs int64
	out             []model.Tick
}

func (p *parser) value(b []byte, depth int) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || depth > 3 {
		return nil
	}
	switch b[0] {
	case '[':
		var ticks []wireTick
		if err := json.Unmarshal(b, &ticks); err == nil {
			for i := range ticks {
				p.add(&ticks[i])
			}
			return nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(b, &items); err != nil {
			return err
		}
		for _, it := range items {
			if err := p.value(it, depth+1); err != nil {
				return err
			}
		}
		return nil
	case '{':
		var env envelope
		if err := json.Unmarshal(b, &env); err != nil {
			return err
		}
		if env.Symbol != "" {
			p.add(&env.wireTick)
			return nil
		}
		found := false
		for _, raw := range []json.RawMessage{env.Data, env.Prices, env.Payload, env.Result, env.Ticks} {
			if len(raw) > 0 && string(raw) != "null" {
				found = true
				if err := p.value(raw, depth+1); err != nil {
					return err
				}
			}
		}
		if !found {
			// Symbol-keyed map: {"EURUSD": {"buy":..,"sell":..}, ...}.
			// Anything else (acks, errors) simply yields no ticks.
			var m map[string]wireTick
			if json.Unmarshal(b, &m) == nil {
				for sym, t := range m {
					if t.Symbol == "" {
						t.Symbol = sym
					}
					p.add(&t)
				}
			}
		}
		return nil
	}
	return nil
}

func (p *parser) add(w *wireTick) {
	if w.Buy == 0 {
		w.Buy = w.Ask
	}
	if w.Sell == 0 {
		w.Sell = w.Bid
	}
	sym := strings.ToUpper(strings.TrimSpace(w.Symbol))
	if sym == "" || (w.Buy == 0 && w.Sell == 0) {
		return
	}
	ts := int64(w.Timestamp)
	if ts == 0 {
		ts = int64(w.TS)
	}
	if ts == 0 {
		ts = p.nowMs
	}
	p.out = append(p.out, model.Tick{
		Symbol: sym, Buy: float64(w.Buy), Sell: float64(w.Sell), TS: ts, RecvNano: p.recvNano,
	})
}
