package stream

import (
	"context"
	"sync"
)

// Producer is an idempotent producer. Every record it sends carries its
// identity and a monotonic sequence number, and the broker refuses a sequence
// it has already accepted. A caller that retries after an ambiguous failure
// therefore gets the original partition and offset back rather than a second
// copy of the record — which is the whole of the "effectively once" story on
// the write side. The read side completes it by committing offsets only after
// the downstream write has landed.
type Producer struct {
	b   *Broker
	id  string
	mu  sync.Mutex
	seq int64
}

// Producer returns a producer with the given identity. Two producers sharing an
// identity share a sequence space and will suppress each other's records, so
// the identity must be unique per writer.
func (b *Broker) Producer(id string) *Producer {
	return &Producer{b: b, id: id}
}

// ID returns the producer's identity.
func (p *Producer) ID() string { return p.id }

// Produce sends a record and returns where it landed.
//
// A record with Seq zero is allocated the next sequence number. A record
// carrying a Seq the producer has used before is a retry: the broker returns
// the original coordinates and appends nothing. That is how a caller replays a
// send whose outcome it never learned.
func (p *Producer) Produce(ctx context.Context, topic string, r Record) (int32, int64, error) {
	p.mu.Lock()
	r.ProducerID = p.id
	if r.Seq <= 0 {
		p.seq++
		r.Seq = p.seq
	} else if r.Seq > p.seq {
		p.seq = r.Seq
	}
	p.mu.Unlock()
	return p.b.Produce(ctx, topic, r)
}

// Sequence returns the highest sequence number this producer has allocated.
func (p *Producer) Sequence() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seq
}
