package lake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/obs"
	"github.com/udaykishore-resu/tcrp/internal/stream"
)

// EventTopic is the topic ledger events arrive on.
const EventTopic = "ledger.events"

// DefaultBatchSize is how many records one pipeline cycle polls.
const DefaultBatchSize = 256

// ErrNoConsumer is returned when a pipeline is built without one.
var ErrNoConsumer = errors.New("lake: pipeline needs a consumer")

// PipelineConfig wires a stream consumer to the lake's layers.
type PipelineConfig struct {
	// Consumer supplies the records. It is required.
	Consumer *stream.Consumer
	// Bronze and Silver are required; Gold and Recon are optional.
	Bronze *Bronze
	Silver *Silver
	Gold   *GoldStore
	Recon  *Reconciler
	// Book is the golden source the reconciler compares against. Without it
	// the reconciliation step is skipped.
	Book Book
	// BatchSize is the maximum number of records one cycle polls.
	BatchSize int
	// IdleWait is how long Run pauses after a cycle that found nothing.
	IdleWait time.Duration
}

// Pipeline moves records from the stream into bronze, silver and gold.
//
// The order matters and is the whole of the delivery guarantee. Records are
// polled, landed in bronze, folded into silver, and only then is the offset
// committed. A crash between the write and the commit replays the batch, and
// bronze's natural key and silver's applied-event set make the replay a no-op.
// At-least-once delivery plus idempotent application is what "exactly once"
// actually means in a system that can be restarted.
type Pipeline struct {
	opts     Options
	consumer *stream.Consumer
	bronze   *Bronze
	silver   *Silver
	gold     *GoldStore
	recon    *Reconciler
	book     Book
	batch    int
	idle     time.Duration
}

// Progress reports one cycle's work.
type Progress struct {
	// Polled is the number of records taken off the topic.
	Polled int
	// Landed is the number of events new to bronze.
	Landed int
	// Duplicates is the number bronze suppressed on the natural key.
	Duplicates int
	// Rows is the number of bitemporal versions silver wrote.
	Rows int
	// Corrections is the number of those that superseded a published belief.
	Corrections int
	// BreaksOpen is the number of breaks awaiting investigation after the
	// cycle, or -1 when the pipeline has no book to reconcile against.
	BreaksOpen int
	// Lag is the number of records still between the consumer and the end of
	// its partitions.
	Lag int64
}

// NewPipeline wires a pipeline.
func NewPipeline(cfg PipelineConfig, opts Options) (*Pipeline, error) {
	if cfg.Consumer == nil {
		return nil, ErrNoConsumer
	}
	if cfg.Bronze == nil || cfg.Silver == nil {
		return nil, errors.New("lake: pipeline needs a bronze and a silver store")
	}
	opts = opts.resolve()
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.IdleWait <= 0 {
		cfg.IdleWait = 5 * time.Millisecond
	}
	return &Pipeline{
		opts: opts, consumer: cfg.Consumer, bronze: cfg.Bronze, silver: cfg.Silver,
		gold: cfg.Gold, recon: cfg.Recon, book: cfg.Book,
		batch: cfg.BatchSize, idle: cfg.IdleWait,
	}, nil
}

// Consumer exposes the group member the pipeline reads from, so a caller can
// inspect its assignment or rewind it.
func (p *Pipeline) Consumer() *stream.Consumer { return p.consumer }

// RunOnce polls one batch and drives it through the layers. It returns a zero
// Progress when there was nothing to read.
func (p *Pipeline) RunOnce(ctx context.Context) (Progress, error) {
	ctx, sp := p.opts.start(ctx, "lake.pipeline.cycle", obs.KindConsumer)
	if sp != nil {
		defer sp.End()
	}
	var out Progress
	out.BreaksOpen = -1

	records, err := p.consumer.Poll(ctx, p.batch)
	if err != nil {
		if sp != nil {
			sp.RecordError(err)
		}
		return Progress{}, err
	}
	out.Polled = len(records)
	if len(records) == 0 {
		out.Lag = p.consumer.Lag()
		p.opts.gauge("tcrp_lake_pipeline_lag", "Records between the pipeline and the end of its partitions.", nil).
			Set(float64(out.Lag))
		return out, nil
	}

	events := make([]domain.LedgerEvent, 0, len(records))
	for _, r := range records {
		var ev domain.LedgerEvent
		if err := json.Unmarshal(r.Value, &ev); err != nil {
			if sp != nil {
				sp.RecordError(err)
			}
			return Progress{}, fmt.Errorf("lake: decode record at %s/%d offset %d: %w",
				r.Topic, r.Partition, r.Offset, err)
		}
		events = append(events, ev)
	}

	ingested, err := p.bronze.Append(ctx, events)
	if err != nil {
		if sp != nil {
			sp.RecordError(err)
		}
		return Progress{}, err
	}
	out.Landed, out.Duplicates = ingested.Accepted, ingested.Duplicates

	applied, err := p.silver.Apply(ctx, events)
	if err != nil {
		if sp != nil {
			sp.RecordError(err)
		}
		return Progress{}, err
	}
	out.Rows, out.Corrections = applied.Rows, applied.Corrections

	// Commit last. Everything above is idempotent, so a crash here costs a
	// replay rather than a lost record.
	if err := p.consumer.Commit(ctx); err != nil {
		if sp != nil {
			sp.RecordError(err)
		}
		return Progress{}, err
	}

	if p.recon != nil && len(p.book.Instructions) > 0 {
		now := p.opts.Clock.Now()
		res, err := p.recon.Reconcile(ctx, ReconInput{
			ValidAt: now, KnownAt: now,
			Events:         p.bronze.EventsKnownAt(now),
			Ledger:         p.silver.AsOf(now, now),
			Book:           p.book,
			Counterparties: p.referenceCounterparties(),
		})
		if err != nil {
			if sp != nil {
				sp.RecordError(err)
			}
			return Progress{}, err
		}
		out.BreaksOpen = len(res.Open())
	}

	out.Lag = p.consumer.Lag()
	p.opts.counter("tcrp_lake_pipeline_records_total", "Records processed by the ingest pipeline.", nil).
		Add(float64(out.Polled))
	p.opts.gauge("tcrp_lake_pipeline_lag", "Records between the pipeline and the end of its partitions.", nil).
		Set(float64(out.Lag))
	if sp != nil {
		sp.SetAttrs(map[string]any{
			"lake.pipeline.polled":      out.Polled,
			"lake.pipeline.landed":      out.Landed,
			"lake.pipeline.duplicates":  out.Duplicates,
			"lake.pipeline.rows":        out.Rows,
			"lake.pipeline.corrections": out.Corrections,
			"lake.pipeline.lag":         out.Lag,
		})
	}
	return out, nil
}

// Run cycles until ctx is cancelled, pausing for IdleWait after a cycle that
// found nothing. A cancelled context is how the loop ends and is not an error.
func (p *Pipeline) Run(ctx context.Context) error {
	timer := time.NewTimer(p.idle)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		progress, err := p.RunOnce(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		if progress.Polled > 0 {
			continue
		}
		timer.Reset(p.idle)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

// Snapshot builds a curated snapshot, when the pipeline was given a gold store.
func (p *Pipeline) Snapshot(ctx context.Context, validAt, knownAt time.Time) (domain.Snapshot, error) {
	if p.gold == nil {
		return domain.Snapshot{}, errors.New("lake: pipeline has no gold store")
	}
	return p.gold.Snapshot(ctx, validAt, knownAt)
}

func (p *Pipeline) referenceCounterparties() []domain.Counterparty {
	if p.gold == nil {
		return nil
	}
	return p.gold.Reference().Counterparties
}

// PublishEvents writes ledger events onto a topic, keyed by ledger and symbol
// so that every event touching one instrument on one chain lands in the same
// partition and is therefore delivered in order.
func PublishEvents(ctx context.Context, producer *stream.Producer, topic string, events []domain.LedgerEvent) (int, error) {
	ordered := append([]domain.LedgerEvent(nil), events...)
	domain.SortEvents(ordered)
	for i, ev := range ordered {
		body, err := json.Marshal(ev)
		if err != nil {
			return i, fmt.Errorf("lake: encode event %s: %w", ev.ID(), err)
		}
		if _, _, err := producer.Produce(ctx, topic, stream.Record{
			Key:       []byte(string(ev.Ledger) + "|" + ev.Symbol),
			Value:     body,
			Headers:   map[string]string{"event_id": ev.ID(), "kind": string(ev.Kind)},
			Timestamp: ev.ObservedAt.UTC(),
		}); err != nil {
			return i, err
		}
	}
	return len(ordered), nil
}
