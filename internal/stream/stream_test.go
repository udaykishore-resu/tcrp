package stream_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/obs"
	"github.com/udaykishore-resu/tcrp/internal/stream"
)

var testTime = time.Date(2026, 9, 3, 16, 0, 0, 0, time.UTC)

func newBroker(t *testing.T, dir string) (*stream.Broker, *obs.Registry) {
	t.Helper()
	reg := obs.NewRegistry()
	clock := obs.NewManualClock(testTime)
	tr := obs.NewTracer("test", clock).Deterministic(true)
	b, err := stream.NewBroker(stream.Config{Dir: dir, Clock: clock, Tracer: tr, Metrics: reg})
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, reg
}

func TestOrderingWithinPartition(t *testing.T) {
	ctx := context.Background()
	b, _ := newBroker(t, "")
	if err := b.CreateTopic("events", 4); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	p := b.Producer("ingest-1")

	const n = 50
	var part int32 = -1
	for i := 0; i < n; i++ {
		pt, off, err := p.Produce(ctx, "events", stream.Record{
			Key:   []byte("CP-ALPHA"),
			Value: []byte(fmt.Sprintf(`{"i":%d}`, i)),
		})
		if err != nil {
			t.Fatalf("produce %d: %v", i, err)
		}
		if part == -1 {
			part = pt
		}
		if pt != part {
			t.Fatalf("same key landed on partition %d then %d", part, pt)
		}
		if off != int64(i) {
			t.Fatalf("offset %d, want %d", off, i)
		}
	}

	c, err := b.Join("readers", []string{"events"})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	got, err := c.Poll(ctx, 100)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(got) != n {
		t.Fatalf("polled %d records, want %d", len(got), n)
	}
	for i, r := range got {
		if r.Offset != int64(i) {
			t.Fatalf("record %d has offset %d", i, r.Offset)
		}
		if string(r.Value) != fmt.Sprintf(`{"i":%d}`, i) {
			t.Fatalf("record %d out of order: %s", i, r.Value)
		}
		if !r.Timestamp.Equal(testTime) {
			t.Fatalf("record %d stamped %s, want the manual clock", i, r.Timestamp)
		}
	}
}

func TestPartitionerIsDeterministic(t *testing.T) {
	keys := []string{"CP-ALPHA", "CP-BRAVO", "CP-CHARLIE", "CP-DELTA"}
	want := make([]int32, len(keys))
	for i, k := range keys {
		want[i] = stream.PartitionFor([]byte(k), nil, 8)
	}
	for round := 0; round < 3; round++ {
		for i, k := range keys {
			if got := stream.PartitionFor([]byte(k), nil, 8); got != want[i] {
				t.Fatalf("key %s hashed to %d then %d", k, want[i], got)
			}
		}
	}
}

func TestIdempotentProducerSuppressesRetry(t *testing.T) {
	ctx := context.Background()
	b, reg := newBroker(t, "")
	if err := b.CreateTopic("events", 2); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	p := b.Producer("ingest-1")

	rec := stream.Record{Key: []byte("k"), Value: []byte("v")}
	part, off, err := p.Produce(ctx, "events", rec)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	// The caller never learned the outcome, so it replays the same sequence.
	retry := rec
	retry.Seq = p.Sequence()
	part2, off2, err := p.Produce(ctx, "events", retry)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if part2 != part || off2 != off {
		t.Fatalf("retry landed at %d/%d, want the original %d/%d", part2, off2, part, off)
	}
	hw, err := b.HighWatermark(stream.TopicPartition{Topic: "events", Partition: part})
	if err != nil {
		t.Fatalf("HighWatermark: %v", err)
	}
	if hw != 1 {
		t.Fatalf("partition holds %d records, want 1", hw)
	}
	if got := reg.Counter("tcrp_stream_duplicates_suppressed_total", "", map[string]string{"topic": "events"}).Value(); got != 1 {
		t.Fatalf("duplicates suppressed = %v, want 1", got)
	}
}

func TestGroupRebalanceIsRangeAndDeterministic(t *testing.T) {
	b, _ := newBroker(t, "")
	if err := b.CreateTopic("events", 6); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	c1, err := b.Join("g", []string{"events"})
	if err != nil {
		t.Fatalf("join c1: %v", err)
	}
	if got := partitionIDs(c1.Assignment()); !equalInts(got, []int32{0, 1, 2, 3, 4, 5}) {
		t.Fatalf("sole member holds %v", got)
	}

	c2, err := b.Join("g", []string{"events"})
	if err != nil {
		t.Fatalf("join c2: %v", err)
	}
	if got := partitionIDs(c1.Assignment()); !equalInts(got, []int32{0, 1, 2}) {
		t.Fatalf("c1 holds %v after c2 joined", got)
	}
	if got := partitionIDs(c2.Assignment()); !equalInts(got, []int32{3, 4, 5}) {
		t.Fatalf("c2 holds %v", got)
	}

	c3, err := b.Join("g", []string{"events"})
	if err != nil {
		t.Fatalf("join c3: %v", err)
	}
	for i, want := range [][]int32{{0, 1}, {2, 3}, {4, 5}} {
		c := []*stream.Consumer{c1, c2, c3}[i]
		if got := partitionIDs(c.Assignment()); !equalInts(got, want) {
			t.Fatalf("member %d holds %v, want %v", i+1, got, want)
		}
	}

	if err := c2.Close(); err != nil {
		t.Fatalf("close c2: %v", err)
	}
	if got := partitionIDs(c1.Assignment()); !equalInts(got, []int32{0, 1, 2}) {
		t.Fatalf("c1 holds %v after c2 left", got)
	}
	if got := partitionIDs(c3.Assignment()); !equalInts(got, []int32{3, 4, 5}) {
		t.Fatalf("c3 holds %v after c2 left", got)
	}
}

func TestRebalanceHandsOverAtTheCommittedOffset(t *testing.T) {
	ctx := context.Background()
	b, _ := newBroker(t, "")
	if err := b.CreateTopic("events", 2); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	p := b.Producer("ingest-1")
	for i := 0; i < 20; i++ {
		if _, _, err := p.Produce(ctx, "events", stream.Record{
			Key: []byte(fmt.Sprintf("k%d", i)), Value: []byte(fmt.Sprintf("%d", i)),
		}); err != nil {
			t.Fatalf("produce: %v", err)
		}
	}
	c1, err := b.Join("g", []string{"events"})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := c1.Poll(ctx, 100); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if err := c1.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	c2, err := b.Join("g", []string{"events"})
	if err != nil {
		t.Fatalf("join c2: %v", err)
	}
	got, err := c2.Poll(ctx, 100)
	if err != nil {
		t.Fatalf("poll c2: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("c2 re-read %d committed records", len(got))
	}
}

func TestRestartResumesFromCommittedOffset(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	b1, _ := newBroker(t, dir)
	if err := b1.CreateTopic("events", 1); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	p := b1.Producer("ingest-1")
	for i := 0; i < 10; i++ {
		if _, _, err := p.Produce(ctx, "events", stream.Record{
			Key: []byte("k"), Value: []byte(fmt.Sprintf("%d", i)),
		}); err != nil {
			t.Fatalf("produce: %v", err)
		}
	}
	c, err := b1.Join("pipeline", []string{"events"})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	first, err := c.Poll(ctx, 6)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(first) != 6 {
		t.Fatalf("polled %d, want 6", len(first))
	}
	if err := c.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := b1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	b2, _ := newBroker(t, dir)
	if got := b2.Topics(); len(got) != 1 || got[0] != "events" {
		t.Fatalf("restored topics %v", got)
	}
	c2, err := b2.Join("pipeline", []string{"events"})
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	rest, err := c2.Poll(ctx, 100)
	if err != nil {
		t.Fatalf("poll after restart: %v", err)
	}
	if len(rest) != 4 {
		t.Fatalf("resumed with %d records, want the 4 uncommitted ones", len(rest))
	}
	for i, r := range rest {
		if want := fmt.Sprintf("%d", i+6); string(r.Value) != want {
			t.Fatalf("record %d is %s, want %s", i, r.Value, want)
		}
	}
}

func TestRestartRestoresIdempotenceTable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	b1, _ := newBroker(t, dir)
	if err := b1.CreateTopic("events", 1); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	p := b1.Producer("ingest-1")
	if _, _, err := p.Produce(ctx, "events", stream.Record{Key: []byte("k"), Value: []byte("v")}); err != nil {
		t.Fatalf("produce: %v", err)
	}
	if err := b1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	b2, _ := newBroker(t, dir)
	p2 := b2.Producer("ingest-1")
	if _, _, err := p2.Produce(ctx, "events", stream.Record{Key: []byte("k"), Value: []byte("v"), Seq: 1}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	hw, err := b2.HighWatermark(stream.TopicPartition{Topic: "events", Partition: 0})
	if err != nil {
		t.Fatalf("HighWatermark: %v", err)
	}
	if hw != 1 {
		t.Fatalf("replayed produce appended a second copy: %d records", hw)
	}
}

func TestConcurrentProducers(t *testing.T) {
	ctx := context.Background()
	b, _ := newBroker(t, t.TempDir())
	if err := b.CreateTopic("events", 4); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	const writers, each = 8, 100
	var wg sync.WaitGroup
	wg.Add(writers)
	errs := make([]error, writers)
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			p := b.Producer(fmt.Sprintf("ingest-%d", w))
			for i := 0; i < each; i++ {
				if _, _, err := p.Produce(ctx, "events", stream.Record{
					Key:     []byte(fmt.Sprintf("w%d-k%d", w, i)),
					Value:   []byte(fmt.Sprintf("w%d-%d", w, i)),
					Headers: map[string]string{"writer": fmt.Sprintf("%d", w)},
				}); err != nil {
					errs[w] = err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	for w, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", w, err)
		}
	}

	var total int64
	for part := 0; part < 4; part++ {
		hw, err := b.HighWatermark(stream.TopicPartition{Topic: "events", Partition: int32(part)})
		if err != nil {
			t.Fatalf("HighWatermark: %v", err)
		}
		total += hw
	}
	if total != writers*each {
		t.Fatalf("broker holds %d records, want %d", total, writers*each)
	}

	c, err := b.Join("g", []string{"events"})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	got, err := c.Poll(ctx, writers*each*2)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(got) != writers*each {
		t.Fatalf("polled %d, want %d", len(got), writers*each)
	}
	// Ordering is only promised within a partition: each producer's records
	// must appear in sequence order there.
	lastSeq := map[string]map[int32]int64{}
	for _, r := range got {
		per, ok := lastSeq[r.ProducerID]
		if !ok {
			per = map[int32]int64{}
			lastSeq[r.ProducerID] = per
		}
		if r.Seq <= per[r.Partition] {
			t.Fatalf("producer %s went backwards on partition %d: %d after %d",
				r.ProducerID, r.Partition, r.Seq, per[r.Partition])
		}
		per[r.Partition] = r.Seq
	}
	if c.Lag() != 0 {
		t.Fatalf("lag %d after draining", c.Lag())
	}
}

func TestUnknownTopicIsAnError(t *testing.T) {
	ctx := context.Background()
	b, _ := newBroker(t, "")
	if _, _, err := b.Producer("p").Produce(ctx, "nope", stream.Record{}); err == nil {
		t.Fatal("produce to an uncreated topic succeeded")
	}
	if _, err := b.Join("g", []string{"nope"}); err == nil {
		t.Fatal("join on an uncreated topic succeeded")
	}
}

func partitionIDs(tps []stream.TopicPartition) []int32 {
	out := make([]int32, len(tps))
	for i, tp := range tps {
		out[i] = tp.Partition
	}
	return out
}

func equalInts(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
