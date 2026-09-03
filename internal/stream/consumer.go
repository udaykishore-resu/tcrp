package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/udaykishore-resu/tcrp/internal/obs"
)

type groupState struct {
	name      string
	members   []*Consumer
	committed map[TopicPartition]int64
	nextID    int
}

// Consumer is one member of a consumer group. It owns the partitions the group
// assigned to it and nothing else; polling a partition another member owns is
// not possible, which is the property that lets a pipeline scale out without
// processing a record twice.
type Consumer struct {
	b      *Broker
	group  *groupState
	id     string
	topics []string
	// pos is the next offset to read per assigned partition. It is the
	// consumer's own progress; committed is the group's durable record of it.
	pos    map[TopicPartition]int64
	assign []TopicPartition
	closed bool
}

// Join adds a member to a consumer group subscribed to the named topics and
// rebalances the group. The returned Consumer starts from the group's committed
// offset for each partition it is assigned, so a process restarted against a
// broker with a directory resumes where it left off.
func (b *Broker) Join(group string, topics []string) (*Consumer, error) {
	if len(topics) == 0 {
		return nil, ErrNoTopics
	}
	if strings.ContainsAny(group, `/\`) || group == "" || strings.Contains(group, "..") {
		return nil, fmt.Errorf("stream: group name %q is not usable as a file name", group)
	}
	subs := append([]string(nil), topics...)
	sort.Strings(subs)

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}
	for _, t := range subs {
		if _, ok := b.topics[t]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrUnknownTopic, t)
		}
	}
	g, ok := b.groups[group]
	if !ok {
		g = &groupState{name: group, committed: map[TopicPartition]int64{}}
		b.groups[group] = g
	}
	g.nextID++
	c := &Consumer{
		b:      b,
		group:  g,
		id:     fmt.Sprintf("%s-member-%d", group, g.nextID),
		topics: subs,
		pos:    map[TopicPartition]int64{},
	}
	g.members = append(g.members, c)
	b.rebalance(g)
	return c, nil
}

// ID returns the member id the group assigned. Member ids are allocated from a
// counter rather than randomly so that an assignment is reproducible.
func (c *Consumer) ID() string { return c.id }

// Group returns the consumer group name.
func (c *Consumer) Group() string { return c.group.name }

// Assignment returns the partitions this member currently owns, sorted.
func (c *Consumer) Assignment() []TopicPartition {
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	return append([]TopicPartition(nil), c.assign...)
}

// Position returns the next offset this member will read on tp.
func (c *Consumer) Position(tp TopicPartition) int64 {
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	return c.pos[tp]
}

// Committed returns the group's committed offset for tp, which is where a
// replacement member would start.
func (c *Consumer) Committed(tp TopicPartition) int64 {
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	return c.group.committed[tp]
}

// Poll returns up to max records from the assigned partitions and advances the
// member's position past them. It does not block: an empty slice means there is
// nothing to read right now, not that the topic ended. Records are drained
// partition by partition in sorted order, so a poll is reproducible.
func (c *Consumer) Poll(ctx context.Context, max int) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if max <= 0 {
		return nil, nil
	}
	var sp *obs.Span
	if c.b.tracer != nil {
		_, sp = c.b.tracer.Start(ctx, "stream.poll", obs.KindConsumer)
		defer sp.End()
		sp.SetAttrs(map[string]any{
			"messaging.system":                "kafka",
			"messaging.operation.name":        "receive",
			"messaging.consumer.group.name":   c.group.name,
			"messaging.kafka.consumer.member": c.id,
		})
	}

	c.b.mu.Lock()
	if c.b.closed || c.closed {
		c.b.mu.Unlock()
		return nil, ErrClosed
	}
	out := make([]Record, 0, max)
	for _, tp := range c.assign {
		if len(out) >= max {
			break
		}
		t, ok := c.b.topics[tp.Topic]
		if !ok {
			continue
		}
		p := t.parts[tp.Partition]
		for c.pos[tp] < int64(len(p.recs)) && len(out) < max {
			out = append(out, p.recs[c.pos[tp]])
			c.pos[tp]++
		}
	}
	c.updateLagLocked()
	topics := append([]string(nil), c.topics...)
	c.b.mu.Unlock()

	if n := len(out); n > 0 {
		for _, t := range topics {
			consumed := 0
			for _, r := range out {
				if r.Topic == t {
					consumed++
				}
			}
			if consumed == 0 {
				continue
			}
			c.b.counter("tcrp_stream_records_consumed_total", "Records handed to consumers.",
				map[string]string{"topic": t, "group": c.group.name}).Add(float64(consumed))
		}
	}
	if sp != nil {
		sp.SetAttr("messaging.batch.message_count", int64(len(out)))
	}
	return out, nil
}

// Commit records the member's position as the group's committed offset and,
// when the broker has a directory, writes it to disk. Committing after the
// downstream write is what makes a restart replay rather than lose records.
func (c *Consumer) Commit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	if c.b.closed || c.closed {
		return ErrClosed
	}
	for _, tp := range c.assign {
		c.group.committed[tp] = c.pos[tp]
	}
	return c.b.persistOffsets(c.group)
}

// Seek moves the member's position on an owned partition, which is how a
// replay of an already-consumed range is requested.
func (c *Consumer) Seek(tp TopicPartition, offset int64) error {
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	for _, a := range c.assign {
		if a == tp {
			if offset < 0 {
				offset = 0
			}
			c.pos[tp] = offset
			return nil
		}
	}
	return fmt.Errorf("stream: %s is not assigned to %s", tp, c.id)
}

// Lag returns the number of records between the member's position and the end
// of each assigned partition.
func (c *Consumer) Lag() int64 {
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	return c.lagLocked()
}

// Close removes the member from its group and rebalances the remaining
// members. It does not commit; an uncommitted position is lost, which is the
// same bargain a real consumer makes.
func (c *Consumer) Close() error {
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	g := c.group
	kept := g.members[:0]
	for _, m := range g.members {
		if m != c {
			kept = append(kept, m)
		}
	}
	g.members = kept
	c.assign = nil
	c.b.rebalance(g)
	return nil
}

func (c *Consumer) lagLocked() int64 {
	var lag int64
	for _, tp := range c.assign {
		t, ok := c.b.topics[tp.Topic]
		if !ok {
			continue
		}
		lag += int64(len(t.parts[tp.Partition].recs)) - c.pos[tp]
	}
	return lag
}

func (c *Consumer) updateLagLocked() {
	byTopic := map[string]int64{}
	for _, tp := range c.assign {
		t, ok := c.b.topics[tp.Topic]
		if !ok {
			continue
		}
		byTopic[tp.Topic] += int64(len(t.parts[tp.Partition].recs)) - c.pos[tp]
	}
	names := make([]string, 0, len(byTopic))
	for n := range byTopic {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c.b.gauge("tcrp_stream_consumer_lag", "Records between a group's position and the end of a topic.",
			map[string]string{"topic": n, "group": c.group.name}).Set(float64(byTopic[n]))
	}
}

// rebalance recomputes the group's assignment. The strategy is Kafka's range
// assignor: for each topic, the members subscribed to it are sorted by member
// id and take contiguous runs of partitions, the earlier members taking the
// remainder. Sorting rather than hashing is what makes an assignment
// reproducible, and reproducibility is what lets a test assert on it.
//
// The caller holds b.mu.
func (b *Broker) rebalance(g *groupState) {
	sort.Slice(g.members, func(i, j int) bool { return g.members[i].id < g.members[j].id })
	next := make(map[*Consumer][]TopicPartition, len(g.members))
	topics := map[string]bool{}
	for _, m := range g.members {
		for _, t := range m.topics {
			topics[t] = true
		}
	}
	names := make([]string, 0, len(topics))
	for t := range topics {
		names = append(names, t)
	}
	sort.Strings(names)

	for _, name := range names {
		t, ok := b.topics[name]
		if !ok {
			continue
		}
		var subs []*Consumer
		for _, m := range g.members {
			for _, s := range m.topics {
				if s == name {
					subs = append(subs, m)
					break
				}
			}
		}
		if len(subs) == 0 {
			continue
		}
		n, k := len(t.parts), len(subs)
		per, extra := n/k, n%k
		start := 0
		for i, m := range subs {
			count := per
			if i < extra {
				count++
			}
			for p := start; p < start+count; p++ {
				next[m] = append(next[m], TopicPartition{Topic: name, Partition: int32(p)})
			}
			start += count
		}
	}

	for _, m := range g.members {
		assigned := next[m]
		sort.Slice(assigned, func(i, j int) bool { return assigned[i].Less(assigned[j]) })
		// A member that keeps a partition keeps its position; a member that is
		// handed a new one starts from the group's committed offset, which is
		// exactly what a real rebalance does and why an uncommitted batch is
		// reprocessed after one.
		pos := make(map[TopicPartition]int64, len(assigned))
		for _, tp := range assigned {
			if had, ok := m.pos[tp]; ok && contains(m.assign, tp) {
				pos[tp] = had
				continue
			}
			pos[tp] = g.committed[tp]
		}
		m.assign, m.pos = assigned, pos
	}
}

func contains(tps []TopicPartition, tp TopicPartition) bool {
	for _, t := range tps {
		if t == tp {
			return true
		}
	}
	return false
}

// persistOffsets writes the group's committed offsets. The caller holds b.mu.
func (b *Broker) persistOffsets(g *groupState) error {
	if b.dir == "" {
		return nil
	}
	flat := make(map[string]int64, len(g.committed))
	for tp, off := range g.committed {
		flat[tp.String()] = off
	}
	body, err := json.MarshalIndent(flat, "", "  ")
	if err != nil {
		return fmt.Errorf("stream: encode offsets: %w", err)
	}
	path := filepath.Join(b.dir, "groups", g.name+".json")
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("stream: write offsets: %w", err)
	}
	return nil
}

func (b *Broker) restoreOffsets() error {
	entries, err := os.ReadDir(filepath.Join(b.dir, "groups"))
	if err != nil {
		return fmt.Errorf("stream: read groups: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(b.dir, "groups", name))
		if err != nil {
			return fmt.Errorf("stream: read offsets: %w", err)
		}
		var flat map[string]int64
		if err := json.Unmarshal(body, &flat); err != nil {
			return fmt.Errorf("stream: decode offsets: %w", err)
		}
		g := &groupState{
			name:      strings.TrimSuffix(name, ".json"),
			committed: map[TopicPartition]int64{},
		}
		keys := make([]string, 0, len(flat))
		for k := range flat {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			topic, part, ok := strings.Cut(k, "/")
			if !ok {
				continue
			}
			id, err := strconv.Atoi(part)
			if err != nil {
				continue
			}
			g.committed[TopicPartition{Topic: topic, Partition: int32(id)}] = flat[k]
		}
		b.groups[g.name] = g
	}
	return nil
}
