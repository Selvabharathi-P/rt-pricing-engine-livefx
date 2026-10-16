package store

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"pricing-engine/internal/model"
)

// Redis layout (all values are fixed-width little-endian binary), under a
// namespace per upstream source so e.g. simulator data can never be
// restored into a live-feed run:
//
//	<ns>b:<SYMBOL>  hash  field = unix minute  -> 52-byte bucket
//	<ns>last        hash  field = SYMBOL       -> 24-byte last quote
//
// Only changed buckets are written each flush (~1 per symbol per second),
// so write volume is independent of the upstream tick rate. Bucket keys
// expire 48h after their last write, cleaning up symbols that disappear.
const (
	bucketKeyPrefix = "b:"
	lastKey         = "last"
	bucketTTL       = 48 * time.Hour
	bucketSize      = 4*8 + 2*8 + 4
	lastSize        = 3 * 8
)

type Redis struct {
	c  *redis.Client
	ns string
}

// NewRedis creates a store whose keys all start with namespace.
func NewRedis(addr, password string, db int, namespace string) *Redis {
	return &Redis{ns: namespace, c: redis.NewClient(&redis.Options{
		Addr: addr, Password: password, DB: db,
		DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
	})}
}

func (r *Redis) Ping(ctx context.Context) error { return r.c.Ping(ctx).Err() }
func (r *Redis) Close() error                   { return r.c.Close() }

func (r *Redis) Save(ctx context.Context, deltas []model.Delta) error {
	_, err := r.c.TxPipelined(ctx, func(p redis.Pipeliner) error {
		for _, d := range deltas {
			key := r.ns + bucketKeyPrefix + d.Symbol
			if len(d.Buckets) > 0 {
				args := make([]any, 0, 2*len(d.Buckets))
				for _, b := range d.Buckets {
					args = append(args, strconv.FormatInt(b.Minute, 10), encodeBucket(b))
				}
				p.HSet(ctx, key, args...)
				p.Expire(ctx, key, bucketTTL)
			}
			if len(d.Deleted) > 0 {
				fields := make([]string, len(d.Deleted))
				for i, m := range d.Deleted {
					fields[i] = strconv.FormatInt(m, 10)
				}
				p.HDel(ctx, key, fields...)
			}
			if d.Last != nil {
				p.HSet(ctx, r.ns+lastKey, d.Symbol, encodeLast(*d.Last))
			}
		}
		return nil
	})
	return err
}

func (r *Redis) Load(ctx context.Context, symbols []string) (map[string]model.SymbolState, error) {
	cmds := make([]*redis.MapStringStringCmd, len(symbols))
	var lastCmd *redis.MapStringStringCmd
	_, err := r.c.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, s := range symbols {
			cmds[i] = p.HGetAll(ctx, r.ns+bucketKeyPrefix+s)
		}
		lastCmd = p.HGetAll(ctx, r.ns+lastKey)
		return nil
	})
	if err != nil {
		return nil, err
	}
	lasts := lastCmd.Val()
	out := make(map[string]model.SymbolState)
	for i, s := range symbols {
		var ss model.SymbolState
		for field, v := range cmds[i].Val() {
			m, err := strconv.ParseInt(field, 10, 64)
			if err != nil {
				continue
			}
			b, err := decodeBucket(m, []byte(v))
			if err != nil {
				continue // skip a corrupt field rather than fail the restore
			}
			ss.Buckets = append(ss.Buckets, b)
		}
		if v, ok := lasts[s]; ok {
			if l, err := decodeLast([]byte(v)); err == nil {
				ss.Last = l
			}
		}
		if len(ss.Buckets) > 0 || ss.Last.TS > 0 {
			out[s] = ss
		}
	}
	return out, nil
}

var le = binary.LittleEndian

func encodeBucket(b model.Bucket) []byte {
	buf := make([]byte, bucketSize)
	le.PutUint64(buf[0:], math.Float64bits(b.Open))
	le.PutUint64(buf[8:], math.Float64bits(b.High))
	le.PutUint64(buf[16:], math.Float64bits(b.Low))
	le.PutUint64(buf[24:], math.Float64bits(b.Close))
	le.PutUint64(buf[32:], uint64(b.OpenTS))
	le.PutUint64(buf[40:], uint64(b.CloseTS))
	le.PutUint32(buf[48:], b.Count)
	return buf
}

func decodeBucket(minute int64, buf []byte) (model.Bucket, error) {
	if len(buf) != bucketSize {
		return model.Bucket{}, errors.New("bad bucket size")
	}
	return model.Bucket{
		Minute:  minute,
		Open:    math.Float64frombits(le.Uint64(buf[0:])),
		High:    math.Float64frombits(le.Uint64(buf[8:])),
		Low:     math.Float64frombits(le.Uint64(buf[16:])),
		Close:   math.Float64frombits(le.Uint64(buf[24:])),
		OpenTS:  int64(le.Uint64(buf[32:])),
		CloseTS: int64(le.Uint64(buf[40:])),
		Count:   le.Uint32(buf[48:]),
	}, nil
}

func encodeLast(l model.Last) []byte {
	buf := make([]byte, lastSize)
	le.PutUint64(buf[0:], math.Float64bits(l.Buy))
	le.PutUint64(buf[8:], math.Float64bits(l.Sell))
	le.PutUint64(buf[16:], uint64(l.TS))
	return buf
}

func decodeLast(buf []byte) (model.Last, error) {
	if len(buf) != lastSize {
		return model.Last{}, errors.New("bad last size")
	}
	return model.Last{
		Buy:  math.Float64frombits(le.Uint64(buf[0:])),
		Sell: math.Float64frombits(le.Uint64(buf[8:])),
		TS:   int64(le.Uint64(buf[16:])),
	}, nil
}
