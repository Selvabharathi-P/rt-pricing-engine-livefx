package server

import (
	"encoding/binary"
	"math"
	"strconv"

	"pricing-engine/internal/model"
)

// Wire formats. Both carry only the symbols that changed since the client's
// last frame, and every frame is split to stay under MaxFrameBytes.
//
// JSON (default), positional rows to avoid repeating key names:
//
//	{"t":"q","d":[["EURUSD",buy,sell,high24h,low24h,changePct,ts],...]}
//
// Binary (?format=binary), little-endian:
//
//	u8 'Q' | u8 version=1 | u16 count | count × row
//	row = u8 len | symbol | f64 buy | f64 sell | f64 high | f64 low | f64 chg | i64 ts
var JSONFields = []string{"symbol", "buy", "sell", "high24h", "low24h", "change24hPct", "ts"}

const (
	jsonHead = `{"t":"q","d":[`
	jsonTail = `]}`
	binHead  = 4
	binVer   = 1
)

func appendJSONRow(b []byte, q *model.Quote) []byte {
	b = append(b, '[', '"')
	b = append(b, q.Symbol...)
	b = append(b, '"', ',')
	for _, v := range [...]float64{q.Buy, q.Sell, q.High24h, q.Low24h} {
		b = strconv.AppendFloat(b, v, 'f', -1, 64)
		b = append(b, ',')
	}
	b = strconv.AppendFloat(b, q.ChangePct, 'f', -1, 64)
	b = append(b, ',')
	b = strconv.AppendInt(b, q.TS, 10)
	return append(b, ']')
}

func appendBinRow(b []byte, q *model.Quote) []byte {
	b = append(b, byte(len(q.Symbol)))
	b = append(b, q.Symbol...)
	for _, v := range [...]float64{q.Buy, q.Sell, q.High24h, q.Low24h, q.ChangePct} {
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(v))
	}
	return binary.LittleEndian.AppendUint64(b, uint64(q.TS))
}

// EncodeFrames packs quotes into as few frames as possible, none larger than maxBytes.
func EncodeFrames(qs []model.Quote, maxBytes int, bin bool) [][]byte {
	var frames [][]byte
	var cur []byte
	count := 0
	var row []byte

	start := func() {
		cur = make([]byte, 0, min(maxBytes, 64+len(qs)*96))
		if bin {
			cur = append(cur, 'Q', binVer, 0, 0)
		} else {
			cur = append(cur, jsonHead...)
		}
		count = 0
	}
	finish := func() {
		if count == 0 {
			return
		}
		if bin {
			binary.LittleEndian.PutUint16(cur[2:], uint16(count))
		} else {
			cur = append(cur, jsonTail...)
		}
		frames = append(frames, cur)
	}

	start()
	for i := range qs {
		row = row[:0]
		if bin {
			row = appendBinRow(row, &qs[i])
		} else {
			if count > 0 {
				row = append(row, ',')
			}
			row = appendJSONRow(row, &qs[i])
		}
		extra := 0
		if !bin {
			extra = len(jsonTail)
		}
		if count > 0 && len(cur)+len(row)+extra > maxBytes {
			finish()
			start()
			if !bin {
				row = row[1:] // drop the leading comma
			}
		}
		cur = append(cur, row...)
		count++
		if bin && count == math.MaxUint16 {
			finish()
			start()
		}
	}
	finish()
	return frames
}
