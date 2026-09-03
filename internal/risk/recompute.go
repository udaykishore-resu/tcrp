package risk

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Source is where ledger events arrive from. It is an interface with one method
// so that the recomputer can be driven by an indexer, a replay file or a test
// without any of them knowing about the others.
type Source interface {
	// Events returns the channel of observations. Closing it shuts the
	// recomputer down as cleanly as cancelling its context does.
	Events() <-chan domain.LedgerEvent
}

// ChannelSource adapts a plain channel to a Source.
type ChannelSource struct{ ch <-chan domain.LedgerEvent }

// NewChannelSource wraps ch as a Source.
func NewChannelSource(ch <-chan domain.LedgerEvent) *ChannelSource {
	return &ChannelSource{ch: ch}
}

// Events returns the wrapped channel.
func (s *ChannelSource) Events() <-chan domain.LedgerEvent { return s.ch }

// SnapshotProvider returns the curated view the engine calculates from. The
// recomputer never builds a snapshot itself: deciding what was true and when it
// was known is the lakehouse's job, and threading both clocks through the
// signature keeps a restatement and a live recompute on the same code path.
type SnapshotProvider func(ctx context.Context, validAt, knownAt time.Time) (domain.Snapshot, error)

// Result is one recompute cycle's output.
type Result struct {
	// Seq counts cycles, starting at one.
	Seq uint64 `json:"seq"`
	// AsOf is the snapshot's business time.
	AsOf time.Time `json:"as_of"`
	// TriggeredBy lists the natural keys of the events that caused the cycle,
	// in arrival order, so a figure can be traced back to the transactions that
	// moved it.
	TriggeredBy []string `json:"triggered_by,omitempty"`
	// Counterparties are the ones actually recalculated, sorted.
	Counterparties []string `json:"counterparties,omitempty"`
	// Partial is true when only the affected counterparties were recomputed.
	Partial bool `json:"partial"`
	// Exposures are the recomputed exposures, in counterparty order.
	Exposures []domain.Exposure `json:"exposures,omitempty"`
	// Breaches is the full breach list from the cycle; NewBreaches is the
	// subset the register had not already raised.
	Breaches    []domain.Breach `json:"breaches,omitempty"`
	NewBreaches []domain.Breach `json:"new_breaches,omitempty"`
	// Lag is the delay from the newest event's observation to the end of the
	// cycle. It is the number the recompute SLO is written against.
	Lag time.Duration `json:"lag"`
	// Err carries a cycle that failed. A failed cycle is published rather than
	// swallowed, because a subscriber that stops receiving results and a
	// subscriber receiving errors need to behave differently.
	Err error `json:"-"`
}

// Recomputer turns a stream of ledger events into recalculated exposure.
//
// It debounces: a burst of events from one block produces one recompute, not
// one per log entry, because the engine's cost is per snapshot and the desk
// only ever sees the last answer. Where the events name their counterparties it
// recomputes only those, and falls back to the whole book when they do not —
// a partial recompute that guessed wrong is worse than a full one that was slow.
type Recomputer struct {
	engine   *Engine
	source   Source
	provider SnapshotProvider
	register *BreachRegister
	clock    obs.Clock
	tracer   *obs.Tracer
	metrics  *Metrics

	debounce time.Duration
	maxDelay time.Duration
	buffer   int

	mu     sync.Mutex
	subs   []chan Result
	closed bool
	seq    uint64
}

// RecomputeOption configures a Recomputer.
type RecomputeOption func(*Recomputer)

// WithDebounce sets the quiet period a burst must fall silent for before the
// cycle runs. Zero recomputes on every drain of the channel.
func WithDebounce(d time.Duration) RecomputeOption {
	return func(r *Recomputer) { r.debounce = d }
}

// WithMaxDelay caps how long a continuous stream of events can defer a
// recompute. Without it a busy ledger would debounce forever.
func WithMaxDelay(d time.Duration) RecomputeOption {
	return func(r *Recomputer) { r.maxDelay = d }
}

// WithBreachRegister wires the cycle into a breach register, so a recompute
// both measures the limits and files what it found.
func WithBreachRegister(reg *BreachRegister) RecomputeOption {
	return func(r *Recomputer) { r.register = reg }
}

// WithSubscriberBuffer sets the buffer depth of each Subscribe channel.
func WithSubscriberBuffer(n int) RecomputeOption {
	return func(r *Recomputer) {
		if n > 0 {
			r.buffer = n
		}
	}
}

// NewRecomputer builds a recomputer. It does not start anything; Run does.
func NewRecomputer(engine *Engine, src Source, provider SnapshotProvider, opts ...RecomputeOption) *Recomputer {
	r := &Recomputer{
		engine: engine, source: src, provider: provider,
		clock: engine.Clock(), tracer: engine.tracer, metrics: engine.Metrics(),
		debounce: 50 * time.Millisecond, maxDelay: 500 * time.Millisecond, buffer: 8,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Subscribe returns a channel of results. The channel is buffered and a slow
// subscriber is dropped from rather than allowed to stall the pipeline; every
// channel is closed when Run returns, so a range over it terminates.
func (r *Recomputer) Subscribe() <-chan Result {
	c := make(chan Result, r.buffer)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		close(c)
		return c
	}
	r.subs = append(r.subs, c)
	return c
}

// Run consumes events until ctx is cancelled or the source's channel closes,
// then closes every subscriber channel and returns. It owns no goroutines of
// its own beyond the fan-out inside the engine, so there is nothing left
// running when it returns.
func (r *Recomputer) Run(ctx context.Context) error {
	defer r.shutdown()

	events := r.source.Events()
	var (
		pending  []domain.LedgerEvent
		quiet    *time.Timer
		deadline *time.Timer
		quietC   <-chan time.Time
		lateC    <-chan time.Time
	)
	stop := func() {
		if quiet != nil {
			quiet.Stop()
			quiet = nil
			quietC = nil
		}
		if deadline != nil {
			deadline.Stop()
			deadline = nil
			lateC = nil
		}
	}
	defer stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case ev, ok := <-events:
			if !ok {
				if len(pending) > 0 {
					r.cycle(ctx, pending)
				}
				return nil
			}
			pending = append(pending, ev)
			if quiet == nil {
				quiet = time.NewTimer(r.debounce)
				deadline = time.NewTimer(r.maxDelay)
				quietC, lateC = quiet.C, deadline.C
			} else {
				if !quiet.Stop() {
					select {
					case <-quiet.C:
					default:
					}
				}
				quiet.Reset(r.debounce)
			}

		case <-quietC:
			batch := pending
			pending = nil
			stop()
			r.cycle(ctx, batch)

		case <-lateC:
			batch := pending
			pending = nil
			stop()
			r.cycle(ctx, batch)
		}
	}
}

// cycle runs one recompute over a debounced batch and publishes the result.
func (r *Recomputer) cycle(ctx context.Context, batch []domain.LedgerEvent) {
	if len(batch) == 0 || ctx.Err() != nil {
		return
	}
	var span *obs.Span
	if r.tracer != nil {
		ctx, span = r.tracer.Start(ctx, "risk.recompute", obs.KindConsumer)
		defer span.End()
	}

	r.mu.Lock()
	r.seq++
	seq := r.seq
	r.mu.Unlock()

	triggered := make([]string, 0, len(batch))
	affected := map[string]bool{}
	unresolved := false
	newest := time.Time{}
	for _, ev := range batch {
		triggered = append(triggered, ev.ID())
		if ev.ObservedAt.After(newest) {
			newest = ev.ObservedAt
		}
		from, to := ev.From, ev.To
		if from == "" && to == "" {
			unresolved = true
			continue
		}
		if from != "" {
			affected[from] = true
		}
		if to != "" {
			affected[to] = true
		}
	}

	res := Result{Seq: seq, TriggeredBy: triggered}
	span.SetAttrs(map[string]any{
		"tcrp.risk.recompute.seq":    seq,
		"tcrp.risk.recompute.events": len(batch),
	})

	now := r.clock.Now()
	snap, err := r.provider(ctx, now, now)
	if err != nil {
		res.Err = err
		span.RecordError(err)
		r.publish(res)
		return
	}
	res.AsOf = snap.ValidAt

	// An event naming a counterparty the snapshot does not know is not a reason
	// to recompute nothing; it is a reason to recompute everything.
	targets := make([]string, 0, len(affected))
	for cp := range affected {
		if _, ok := snap.Counterparty(cp); ok {
			targets = append(targets, cp)
		} else {
			unresolved = true
		}
	}
	sort.Strings(targets)
	if unresolved || len(targets) == 0 {
		targets = counterpartyIDs(snap)
	} else {
		res.Partial = true
	}
	res.Counterparties = targets

	exposures := make([]domain.Exposure, len(targets))
	errs := make([]error, len(targets))
	r.engine.fanOut(ctx, len(targets), func(i int) {
		exposures[i], errs[i] = r.engine.exposure(snap, targets[i], false)
	})
	if err := firstError(errs); err != nil {
		res.Err = err
		span.RecordError(err)
		r.publish(res)
		return
	}
	res.Exposures = exposures

	// Limits are always evaluated over the whole book: a concentration share is
	// a property of the portfolio, and recomputing it from one counterparty's
	// slice would be arithmetic that happens to be wrong.
	breaches, err := r.engine.CheckLimits(ctx, snap)
	if err != nil {
		res.Err = err
		span.RecordError(err)
		r.publish(res)
		return
	}
	res.Breaches = breaches
	if r.register != nil {
		fresh, err := r.register.Record(ctx, breaches)
		res.NewBreaches = fresh
		if err != nil {
			res.Err = err
			span.RecordError(err)
		}
	}

	if !newest.IsZero() {
		res.Lag = r.clock.Now().Sub(newest)
		if res.Lag < 0 {
			res.Lag = 0
		}
	}
	r.metrics.ObserveRecompute(res.Lag)
	span.SetAttrs(map[string]any{
		"tcrp.risk.recompute.partial":        res.Partial,
		"tcrp.risk.recompute.counterparties": len(targets),
		"tcrp.risk.recompute.breaches":       len(breaches),
		"tcrp.risk.recompute.lag_ms":         float64(res.Lag.Nanoseconds()) / 1e6,
	})
	r.publish(res)
}

// publish delivers a result to every subscriber without ever blocking. A
// subscriber that cannot keep up misses a cycle; it does not stall the desk's
// recompute loop.
func (r *Recomputer) publish(res Result) {
	r.mu.Lock()
	subs := append([]chan Result(nil), r.subs...)
	r.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- res:
		default:
		}
	}
}

// shutdown closes every subscriber channel exactly once.
func (r *Recomputer) shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	for _, c := range r.subs {
		close(c)
	}
	r.subs = nil
}
