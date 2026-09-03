// Package lake is the platform's lakehouse: bronze landing, a bitemporal
// silver position store, gold snapshots, data-quality controls and
// reconciliation against the book of record.
//
// The metadata model is Iceberg's and is implemented rather than mimed.
// Tables carry schemas with stable field ids and partition specs with identity
// and date transforms; every write commits a snapshot with a parent, a manifest
// list and manifests whose entries carry row counts and per-column lower and
// upper bounds; scans use those bounds to skip files and report how many they
// skipped; snapshots can be read by id or by wall-clock time, expired with
// their unreferenced files, and small files compacted into large ones.
//
// The data file format is not Iceberg's. Files are gzipped NDJSON behind the
// FileFormat interface, because the platform depends on nothing outside the
// standard library and a Parquet writer is not a weekend. That is a real
// limitation and it costs the things a columnar format buys: no column pruning,
// no page statistics, no dictionary or run-length encoding, and a scan that
// decodes every column of every row it opens. Parquet is the production choice
// and FileFormat is the seam it goes through; everything above it — the
// manifests, the bounds, the snapshots, the bitemporal history — is unaffected
// by the swap.
//
// The other honest scoping is volume. Silver rewrites its whole row set on each
// commit and the gold join happens in one process's memory. At the seeded
// dataset's size that is free and it keeps the code readable. The Spark job
// under spark/ is the same model expressed for the volumes where it is not.
package lake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Options configure every component of the lake. The zero value is a lake on
// the real clock, writing gzipped NDJSON, emitting no telemetry.
type Options struct {
	// Clock is the only source of time. Nothing in this package calls
	// time.Now, which is what lets the demo and the tests replay.
	Clock obs.Clock
	// Tracer, when set, receives a span per ingest, per apply and per
	// reconciliation.
	Tracer *obs.Tracer
	// Metrics, when set, receives row and break counters and lag gauges.
	Metrics *obs.Registry
	// Format is the data file format. Nil means gzipped NDJSON.
	Format FileFormat
}

func (o Options) resolve() Options {
	if o.Clock == nil {
		o.Clock = obs.RealClock{}
	}
	if o.Format == nil {
		o.Format = NDJSONGzip{}
	}
	return o
}

func (o Options) tableOptions() TableOptions {
	return TableOptions{Clock: o.Clock, Format: o.Format}
}

// start begins a span when a tracer is configured and returns a no-op
// otherwise, so call sites do not have to test for nil.
func (o Options) start(ctx context.Context, name string, kind obs.SpanKind) (context.Context, *obs.Span) {
	if o.Tracer == nil {
		return ctx, nil
	}
	return o.Tracer.Start(ctx, name, kind)
}

func (o Options) counter(name, doc string, labels map[string]string) *obs.Counter {
	if o.Metrics == nil {
		return &obs.Counter{}
	}
	return o.Metrics.Counter(name, doc, labels)
}

func (o Options) gauge(name, doc string, labels map[string]string) *obs.Gauge {
	if o.Metrics == nil {
		return &obs.Gauge{}
	}
	return o.Metrics.Gauge(name, doc, labels)
}

// Lake is a whole populated lakehouse: the three layers, the reconciler and the
// book of record they compare against. It exists so that the demo, the risk
// engine's tests and the agent's tools all reach for one constructor instead of
// each wiring five components in a slightly different order.
type Lake struct {
	// Dir is the root the tables live under.
	Dir string
	// Options are the resolved options every layer shares.
	Options Options
	Bronze  *Bronze
	Silver  *Silver
	Gold    *GoldStore
	Recon   *Reconciler
	// Book is the golden source reconciliation compares the ledgers against.
	Book Book
	// Seed is the dataset the lake was built from.
	Seed *domain.Seed
}

// NewFromSeed builds a complete populated lake from the seeded dataset in one
// call: every event landed in bronze, folded into silver's bitemporal history,
// and a book of record replayed alongside with the seed's planted divergences
// so the reconciliation controls have something to find.
func NewFromSeed(ctx context.Context, dir string, clock obs.Clock, seed *domain.Seed) (*Lake, error) {
	return NewFromSeedWith(ctx, dir, seed, Options{Clock: clock})
}

// NewFromSeedWith is NewFromSeed with telemetry and a file format attached.
func NewFromSeedWith(ctx context.Context, dir string, seed *domain.Seed, opts Options) (*Lake, error) {
	if seed == nil {
		return nil, errors.New("lake: nil seed")
	}
	opts = opts.resolve()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("lake: create root: %w", err)
	}
	bronze, err := NewBronze(ctx, dir, opts)
	if err != nil {
		return nil, err
	}
	silver, err := NewSilver(ctx, dir, opts)
	if err != nil {
		return nil, err
	}
	gold, err := NewGold(ctx, dir, bronze, silver, ReferenceFromSeed(seed), opts)
	if err != nil {
		return nil, err
	}
	if _, err := bronze.Append(ctx, seed.Events); err != nil {
		return nil, err
	}
	if _, err := silver.Apply(ctx, seed.Events); err != nil {
		return nil, err
	}
	return &Lake{
		Dir:     dir,
		Options: opts,
		Bronze:  bronze,
		Silver:  silver,
		Gold:    gold,
		Recon:   NewReconciler(opts, ReconPolicy{}),
		Book:    BookFromEvents(seed.Events, SeedDivergences(seed)),
		Seed:    seed,
	}, nil
}

// Snapshot builds the curated view for a (validAt, knownAt) pair.
func (l *Lake) Snapshot(ctx context.Context, validAt, knownAt time.Time) (domain.Snapshot, error) {
	return l.Gold.Snapshot(ctx, validAt, knownAt)
}

// Reconcile compares the ledger view against the book of record at a
// (validAt, knownAt) pair and records what it finds in the break register.
func (l *Lake) Reconcile(ctx context.Context, validAt, knownAt time.Time) (ReconResult, error) {
	return l.Recon.Reconcile(ctx, ReconInput{
		ValidAt:        validAt,
		KnownAt:        knownAt,
		Events:         l.Bronze.EventsKnownAt(knownAt),
		Ledger:         l.Silver.AsOf(validAt, knownAt),
		Book:           l.Book,
		Counterparties: l.Seed.Counterparties,
	})
}

// Tables returns the lake's tables in layer order, for a caller that wants to
// report snapshot counts or run maintenance across all of them.
func (l *Lake) Tables() []*Table {
	return []*Table{l.Bronze.Table(), l.Silver.Table(), l.Gold.Table()}
}
