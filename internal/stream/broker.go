package stream

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Config configures a broker. Every field is optional: the zero Config is an
// in-memory broker on the real clock with no telemetry.
type Config struct {
	// Dir, when set, is the directory holding topic segments and committed
	// offsets. An empty Dir keeps everything in memory and a restart starts
	// from zero.
	Dir string
	// Clock is the broker's only source of time; records with no timestamp are
	// stamped from it.
	Clock obs.Clock
	// Tracer, when set, receives a span per produce and per poll.
	Tracer *obs.Tracer
	// Metrics, when set, receives produced/consumed counters and a lag gauge.
	Metrics *obs.Registry
}

// Broker holds topics, partitions and consumer groups. It is safe for
// concurrent use by any number of producers and consumers.
type Broker struct {
	mu      sync.Mutex
	dir     string
	clock   obs.Clock
	tracer  *obs.Tracer
	metrics *obs.Registry

	topics map[string]*topicState
	groups map[string]*groupState
	// dedupe maps a producer's (id, seq) to where the record landed, which is
	// what lets a retried produce return the original coordinates.
	dedupe map[string]dedupeEntry
	closed bool
}

type dedupeEntry struct {
	Partition int32 `json:"partition"`
	Offset    int64 `json:"offset"`
}

type topicState struct {
	name  string
	parts []*partitionState
}

type partitionState struct {
	id   int32
	recs []Record
	seg  *os.File
}

// NewBroker opens a broker. When cfg.Dir is set the directory is created if it
// does not exist and any segments already in it are replayed, restoring both
// the records and the idempotence table.
func NewBroker(cfg Config) (*Broker, error) {
	clock := cfg.Clock
	if clock == nil {
		clock = obs.RealClock{}
	}
	b := &Broker{
		dir:     cfg.Dir,
		clock:   clock,
		tracer:  cfg.Tracer,
		metrics: cfg.Metrics,
		topics:  map[string]*topicState{},
		groups:  map[string]*groupState{},
		dedupe:  map[string]dedupeEntry{},
	}
	if b.dir == "" {
		return b, nil
	}
	for _, sub := range []string{"topics", "groups"} {
		if err := os.MkdirAll(filepath.Join(b.dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("stream: create %s: %w", sub, err)
		}
	}
	if err := b.restore(); err != nil {
		return nil, err
	}
	return b, nil
}

// Clock exposes the broker's clock so a pipeline built on it shares one time
// source.
func (b *Broker) Clock() obs.Clock { return b.clock }

// CreateTopic creates a topic with the given number of partitions. Creating an
// existing topic with the same partition count is a no-op; with a different
// count it is an error, because changing the count silently remaps every key.
func (b *Broker) CreateTopic(name string, partitions int) error {
	if partitions <= 0 {
		return ErrBadPartitionCount
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	if t, ok := b.topics[name]; ok {
		if len(t.parts) != partitions {
			return fmt.Errorf("%w: %s has %d", ErrTopicExists, name, len(t.parts))
		}
		return nil
	}
	t := &topicState{name: name, parts: make([]*partitionState, partitions)}
	for i := range t.parts {
		t.parts[i] = &partitionState{id: int32(i)}
	}
	b.topics[name] = t
	if b.dir == "" {
		return nil
	}
	dir := filepath.Join(b.dir, "topics", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("stream: create topic dir: %w", err)
	}
	meta, err := json.Marshal(map[string]int{"partitions": partitions})
	if err != nil {
		return fmt.Errorf("stream: encode topic meta: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "topic.json"), append(meta, '\n'), 0o644); err != nil {
		return fmt.Errorf("stream: write topic meta: %w", err)
	}
	for _, p := range t.parts {
		if err := b.openSegment(name, p); err != nil {
			return err
		}
	}
	return nil
}

// Topics returns the topic names in sorted order.
func (b *Broker) Topics() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.topics))
	for n := range b.topics {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Partitions returns the partition count of a topic.
func (b *Broker) Partitions(topic string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.topics[topic]
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrUnknownTopic, topic)
	}
	return len(t.parts), nil
}

// HighWatermark returns the offset the next record in a partition will take.
func (b *Broker) HighWatermark(tp TopicPartition) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.topics[tp.Topic]
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrUnknownTopic, tp.Topic)
	}
	if int(tp.Partition) >= len(t.parts) || tp.Partition < 0 {
		return 0, fmt.Errorf("%w: %s has no partition %d", ErrUnknownTopic, tp.Topic, tp.Partition)
	}
	return int64(len(t.parts[tp.Partition].recs)), nil
}

// Produce appends a record. It is the low-level entry point; callers wanting
// idempotence use Producer, which allocates the sequence numbers.
func (b *Broker) Produce(ctx context.Context, topic string, r Record) (int32, int64, error) {
	if b.tracer != nil {
		var sp *obs.Span
		ctx, sp = b.tracer.Start(ctx, "stream.produce", obs.KindProducer)
		defer sp.End()
		sp.SetAttrs(map[string]any{
			"messaging.system":           "kafka",
			"messaging.operation.name":   "send",
			"messaging.destination.name": topic,
		})
		part, off, err := b.produce(ctx, topic, r)
		if err != nil {
			sp.RecordError(err)
			return part, off, err
		}
		sp.SetAttrs(map[string]any{
			"messaging.destination.partition.id": int64(part),
			"messaging.kafka.offset":             off,
		})
		return part, off, nil
	}
	return b.produce(ctx, topic, r)
}

func (b *Broker) produce(ctx context.Context, topic string, r Record) (int32, int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, 0, ErrClosed
	}
	t, ok := b.topics[topic]
	if !ok {
		return 0, 0, fmt.Errorf("%w: %s", ErrUnknownTopic, topic)
	}
	if r.ProducerID != "" && r.Seq > 0 {
		k := dedupeKey(r.ProducerID, r.Seq)
		if got, seen := b.dedupe[k]; seen {
			b.counter("tcrp_stream_duplicates_suppressed_total",
				"Produce calls suppressed by the idempotent producer.",
				map[string]string{"topic": topic}).Inc()
			return got.Partition, got.Offset, nil
		}
	}
	r.Topic = topic
	r.Partition = PartitionFor(r.Key, r.Value, len(t.parts))
	if r.Timestamp.IsZero() {
		r.Timestamp = b.clock.Now()
	}
	p := t.parts[r.Partition]
	r.Offset = int64(len(p.recs))
	p.recs = append(p.recs, r)
	if p.seg != nil {
		line, err := json.Marshal(r)
		if err != nil {
			return 0, 0, fmt.Errorf("stream: encode record: %w", err)
		}
		if _, err := p.seg.Write(append(line, '\n')); err != nil {
			return 0, 0, fmt.Errorf("stream: append segment: %w", err)
		}
	}
	if r.ProducerID != "" && r.Seq > 0 {
		b.dedupe[dedupeKey(r.ProducerID, r.Seq)] = dedupeEntry{Partition: r.Partition, Offset: r.Offset}
	}
	b.counter("tcrp_stream_records_produced_total", "Records accepted by the broker.",
		map[string]string{"topic": topic}).Inc()
	return r.Partition, r.Offset, nil
}

// Close flushes and closes the segment files. The broker is unusable
// afterwards; reopening the same directory restores its state.
func (b *Broker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	var firstErr error
	names := make([]string, 0, len(b.topics))
	for n := range b.topics {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, p := range b.topics[n].parts {
			if p.seg == nil {
				continue
			}
			if err := p.seg.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
			p.seg = nil
		}
	}
	return firstErr
}

func (b *Broker) openSegment(topic string, p *partitionState) error {
	path := filepath.Join(b.dir, "topics", topic, strconv.Itoa(int(p.id))+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("stream: open segment: %w", err)
	}
	p.seg = f
	return nil
}

func (b *Broker) restore() error {
	entries, err := os.ReadDir(filepath.Join(b.dir, "topics"))
	if err != nil {
		return fmt.Errorf("stream: read topics: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		dir := filepath.Join(b.dir, "topics", name)
		metaBytes, err := os.ReadFile(filepath.Join(dir, "topic.json"))
		if err != nil {
			return fmt.Errorf("stream: read topic meta: %w", err)
		}
		var meta struct {
			Partitions int `json:"partitions"`
		}
		if err := json.Unmarshal(metaBytes, &meta); err != nil {
			return fmt.Errorf("stream: decode topic meta: %w", err)
		}
		if meta.Partitions <= 0 {
			return fmt.Errorf("%w: %s", ErrBadPartitionCount, name)
		}
		t := &topicState{name: name, parts: make([]*partitionState, meta.Partitions)}
		for i := range t.parts {
			p := &partitionState{id: int32(i)}
			if err := b.replaySegment(dir, p); err != nil {
				return err
			}
			if err := b.openSegment(name, p); err != nil {
				return err
			}
			t.parts[i] = p
		}
		b.topics[name] = t
	}
	return b.restoreOffsets()
}

func (b *Broker) replaySegment(dir string, p *partitionState) error {
	path := filepath.Join(dir, strconv.Itoa(int(p.id))+".log")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stream: open segment: %w", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return fmt.Errorf("stream: decode segment record: %w", err)
		}
		p.recs = append(p.recs, r)
		if r.ProducerID != "" && r.Seq > 0 {
			b.dedupe[dedupeKey(r.ProducerID, r.Seq)] = dedupeEntry{Partition: r.Partition, Offset: r.Offset}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("stream: read segment: %w", err)
	}
	return nil
}

func (b *Broker) counter(name, doc string, labels map[string]string) *obs.Counter {
	if b.metrics == nil {
		return &obs.Counter{}
	}
	return b.metrics.Counter(name, doc, labels)
}

func (b *Broker) gauge(name, doc string, labels map[string]string) *obs.Gauge {
	if b.metrics == nil {
		return &obs.Gauge{}
	}
	return b.metrics.Gauge(name, doc, labels)
}

func dedupeKey(producer string, seq int64) string {
	return producer + "|" + strconv.FormatInt(seq, 10)
}
