package store

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"pricing-engine/internal/model"
)

const testNS = "pe:test:"

func TestRedisRoundTrip(t *testing.T) {
	mr := miniredis.RunT(t)
	r := NewRedis(mr.Addr(), "", 0, testNS)
	defer r.Close()
	ctx := context.Background()

	b1 := model.Bucket{Minute: 100, Open: 1.1, High: 1.2, Low: 1.0, Close: 1.15, OpenTS: 6000000, CloseTS: 6059999, Count: 7}
	b2 := model.Bucket{Minute: 101, Open: 1.15, High: 1.15, Low: 1.15, Close: 1.15, OpenTS: 6060000, CloseTS: 6060000, Count: 1}
	last := model.Last{Buy: 1.16, Sell: 1.14, TS: 6060000}
	if err := r.Save(ctx, []model.Delta{{Symbol: "EURUSD", Buckets: []model.Bucket{b1, b2}, Last: &last}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(ctx, []model.Delta{{Symbol: "EURUSD", Deleted: []int64{100}}}); err != nil {
		t.Fatal(err)
	}
	got, err := r.Load(ctx, []string{"EURUSD", "GBPUSD"})
	if err != nil {
		t.Fatal(err)
	}
	ss, ok := got["EURUSD"]
	if !ok || len(got) != 1 {
		t.Fatalf("loaded %v", got)
	}
	if len(ss.Buckets) != 1 || ss.Buckets[0] != b2 {
		t.Fatalf("buckets %+v", ss.Buckets)
	}
	if ss.Last != last {
		t.Fatalf("last %+v", ss.Last)
	}
	if ttl := mr.TTL(testNS + bucketKeyPrefix + "EURUSD"); ttl <= 0 {
		t.Fatal("bucket key has no TTL")
	}
}

func TestRedisCorruptFieldSkipped(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.HSet(testNS+bucketKeyPrefix+"EURUSD", "100", "short")
	r := NewRedis(mr.Addr(), "", 0, testNS)
	defer r.Close()
	got, err := r.Load(context.Background(), []string{"EURUSD"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("corrupt data restored: %+v", got)
	}
}

func TestRedisDown(t *testing.T) {
	mr := miniredis.RunT(t)
	r := NewRedis(mr.Addr(), "", 0, testNS)
	defer r.Close()
	mr.Close()
	if err := r.Save(context.Background(), []model.Delta{{Symbol: "X", Last: &model.Last{TS: 1}}}); err == nil {
		t.Fatal("expected error with redis down")
	}
}

// Data saved for one upstream must be invisible to another.
func TestRedisNamespaceIsolation(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()
	sim := NewRedis(mr.Addr(), "", 0, "pe:simulator:9001:")
	live := NewRedis(mr.Addr(), "", 0, "pe:v3-prod.livefxhub.com:")
	defer sim.Close()
	defer live.Close()
	l := model.Last{Buy: 1, Sell: 1, TS: 1}
	if err := sim.Save(ctx, []model.Delta{{Symbol: "EURUSD", Buckets: []model.Bucket{{Minute: 1, Count: 1}}, Last: &l}}); err != nil {
		t.Fatal(err)
	}
	got, err := live.Load(ctx, []string{"EURUSD"})
	if err != nil || len(got) != 0 {
		t.Fatalf("live run restored simulator data: %v %v", got, err)
	}
}
