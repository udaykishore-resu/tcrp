package lake_test

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/lake"
	"github.com/udaykishore-resu/tcrp/internal/obs"
	"github.com/udaykishore-resu/tcrp/internal/stream"
)

type rig struct {
	broker *stream.Broker
	lake   *lake.Lake
	pipe   *lake.Pipeline
	seed   *domain.Seed
	clock  *obs.ManualClock
	reg    *obs.Registry
}

func newRig(t *testing.T, lakeDir, brokerDir string, batch int) *rig {
	t.Helper()
	ctx := context.Background()
	seed := domain.NewSeed()
	clock := obs.NewManualClock(domain.SeedTime.Add(2 * time.Hour))
	reg := obs.NewRegistry()
	tracer := obs.NewTracer("lake-test", clock).Deterministic(true)
	opts := lake.Options{Clock: clock, Tracer: tracer, Metrics: reg}

	broker, err := stream.NewBroker(stream.Config{Dir: brokerDir, Clock: clock, Metrics: reg})
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	if err := broker.CreateTopic(lake.EventTopic, 4); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	bronze, err := lake.NewBronze(ctx, lakeDir, opts)
	if err != nil {
		t.Fatalf("NewBronze: %v", err)
	}
	silver, err := lake.NewSilver(ctx, lakeDir, opts)
	if err != nil {
		t.Fatalf("NewSilver: %v", err)
	}
	gold, err := lake.NewGold(ctx, lakeDir, bronze, silver, lake.ReferenceFromSeed(seed), opts)
	if err != nil {
		t.Fatalf("NewGold: %v", err)
	}
	consumer, err := broker.Join("lake-ingest", []string{lake.EventTopic})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	recon := lake.NewReconciler(opts, lake.ReconPolicy{})
	book := lake.BookFromEvents(seed.Events, lake.SeedDivergences(seed))
	pipe, err := lake.NewPipeline(lake.PipelineConfig{
		Consumer: consumer, Bronze: bronze, Silver: silver, Gold: gold,
		Recon: recon, Book: book, BatchSize: batch,
	}, opts)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return &rig{
		broker: broker, clock: clock, seed: seed, pipe: pipe, reg: reg,
		lake: &lake.Lake{
			Dir: lakeDir, Options: opts, Bronze: bronze, Silver: silver,
			Gold: gold, Recon: recon, Book: book, Seed: seed,
		},
	}
}

func TestPipelineDrivesStreamThroughEveryLayer(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir(), t.TempDir(), 5)

	producer := r.broker.Producer("indexer")
	n, err := lake.PublishEvents(ctx, producer, lake.EventTopic, r.seed.Events)
	if err != nil {
		t.Fatalf("PublishEvents: %v", err)
	}
	if n != len(r.seed.Events) {
		t.Fatalf("published %d of %d events", n, len(r.seed.Events))
	}

	landed, cycles := 0, 0
	for {
		progress, err := r.pipe.RunOnce(ctx)
		if err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if progress.Polled == 0 {
			break
		}
		cycles++
		landed += progress.Landed
		if cycles > 100 {
			t.Fatal("pipeline did not drain")
		}
	}
	if landed != len(r.seed.Events) {
		t.Fatalf("landed %d events, want %d", landed, len(r.seed.Events))
	}
	if cycles < 2 {
		t.Fatalf("drained in %d cycles; the batch size should have forced several", cycles)
	}
	if r.lake.Bronze.Count() != len(r.seed.Events) {
		t.Fatalf("bronze holds %d events", r.lake.Bronze.Count())
	}

	// The end state matches a lake built from the seed in one shot, which is
	// what makes the streaming path and the batch path interchangeable.
	oneShot, _ := newSeededLake(t)
	want := oneShot.Silver.AsOf(domain.SeedTime, afterTheCut)
	got := r.lake.Silver.AsOf(domain.SeedTime, afterTheCut)
	if len(got) != len(want) {
		t.Fatalf("streamed lake has %d positions, one-shot has %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Key() != want[i].Key() || got[i].Quantity != want[i].Quantity {
			t.Fatalf("%s streamed to %d, one-shot %s is %d",
				got[i].Key(), got[i].Quantity, want[i].Key(), want[i].Quantity)
		}
	}

	res, err := r.lake.Reconcile(ctx, domain.SeedTime, afterTheCut)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Breaks) != 3 {
		t.Fatalf("the streamed lake found %d breaks, want 3", len(res.Breaks))
	}
	if got := r.reg.Counter("tcrp_lake_bronze_events_total", "", nil).Value(); got != float64(len(r.seed.Events)) {
		t.Fatalf("bronze counter reads %v", got)
	}
	if got := r.reg.Gauge("tcrp_lake_pipeline_lag", "", nil).Value(); got != 0 {
		t.Fatalf("pipeline lag gauge reads %v after draining", got)
	}
}

func TestPipelineReplayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir(), t.TempDir(), 1000)

	if _, err := lake.PublishEvents(ctx, r.broker.Producer("indexer"), lake.EventTopic, r.seed.Events); err != nil {
		t.Fatalf("PublishEvents: %v", err)
	}
	first, err := r.pipe.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if first.Landed != len(r.seed.Events) {
		t.Fatalf("first cycle landed %d events", first.Landed)
	}
	rowsAfterFirst := len(r.lake.Silver.Rows())

	// Simulate a crash between the downstream write and the offset commit: the
	// consumer is rewound and the whole batch is delivered again.
	for _, tp := range r.pipe.Consumer().Assignment() {
		if err := r.pipe.Consumer().Seek(tp, 0); err != nil {
			t.Fatalf("Seek: %v", err)
		}
	}
	replay, err := r.pipe.RunOnce(ctx)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Polled != len(r.seed.Events) {
		t.Fatalf("replay polled %d records, want the whole batch", replay.Polled)
	}
	if replay.Landed != 0 || replay.Duplicates != len(r.seed.Events) {
		t.Fatalf("replay landed %d events and suppressed %d", replay.Landed, replay.Duplicates)
	}
	if got := len(r.lake.Silver.Rows()); got != rowsAfterFirst {
		t.Fatalf("replay changed the silver row count from %d to %d", rowsAfterFirst, got)
	}
}

func TestPipelineRunStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := newRig(t, t.TempDir(), t.TempDir(), 8)
	if _, err := lake.PublishEvents(ctx, r.broker.Producer("indexer"), lake.EventTopic, r.seed.Events); err != nil {
		t.Fatalf("PublishEvents: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- r.pipe.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for r.lake.Bronze.Count() < len(r.seed.Events) {
		select {
		case <-deadline:
			cancel()
			t.Fatalf("pipeline landed only %d of %d events", r.lake.Bronze.Count(), len(r.seed.Events))
		case err := <-done:
			t.Fatalf("Run returned early: %v", err)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
}
