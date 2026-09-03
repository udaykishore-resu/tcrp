package risk

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

func seedProvider() SnapshotProvider {
	snap := domain.SnapshotFromSeed(domain.NewSeed())
	return func(context.Context, time.Time, time.Time) (domain.Snapshot, error) {
		return snap, nil
	}
}

func transfer(seq uint64, from, to string) domain.LedgerEvent {
	return domain.LedgerEvent{
		Seq: seq, Ledger: domain.LedgerEVM, Kind: domain.EventTransfer,
		Symbol: domain.FundSymbol, From: from, To: to, Quantity: 1_000,
		TxHash: "0xdeadbeef", LogIndex: int(seq),
		OccurredAt: domain.SeedTime, ObservedAt: domain.SeedTime,
	}
}

// awaitResult reads one result or fails the test, so a hang is a failure with a
// message rather than a package timeout.
func awaitResult(t *testing.T, ch <-chan Result) Result {
	t.Helper()
	select {
	case res, ok := <-ch:
		if !ok {
			t.Fatal("result channel closed before a result arrived")
		}
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a recompute result")
		return Result{}
	}
}

func TestRecomputerPartialRecompute(t *testing.T) {
	e := New(obs.NewManualClock(domain.SeedTime), nil, obs.NewRegistry(), nil)
	events := make(chan domain.LedgerEvent, 8)
	rc := NewRecomputer(e, NewChannelSource(events), seedProvider(),
		WithDebounce(time.Millisecond), WithMaxDelay(50*time.Millisecond))
	results := rc.Subscribe()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rc.Run(ctx) }()

	events <- transfer(1, "CP-ALPHA", "CP-DELTA")
	events <- transfer(2, "CP-DELTA", "CP-FOXTROT")

	res := awaitResult(t, results)
	if res.Err != nil {
		t.Fatalf("cycle error: %v", res.Err)
	}
	if !res.Partial {
		t.Errorf("a recompute of three named counterparties was not marked partial")
	}
	want := []string{"CP-ALPHA", "CP-DELTA", "CP-FOXTROT"}
	if len(res.Counterparties) != len(want) {
		t.Fatalf("recomputed %v, want %v", res.Counterparties, want)
	}
	for i := range want {
		if res.Counterparties[i] != want[i] {
			t.Fatalf("recomputed %v, want %v (sorted)", res.Counterparties, want)
		}
	}
	if len(res.Exposures) != len(want) {
		t.Errorf("exposures = %d, want %d", len(res.Exposures), len(want))
	}
	// Limits are always measured over the whole book.
	if len(res.Breaches) != 2 {
		t.Errorf("breaches = %d, want 2", len(res.Breaches))
	}
	if len(res.TriggeredBy) != 2 {
		t.Errorf("triggered by %v, want two event ids", res.TriggeredBy)
	}
	if res.Seq != 1 {
		t.Errorf("seq = %d, want 1", res.Seq)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
}

func TestRecomputerFullRecomputeOnUnknownCounterparty(t *testing.T) {
	e := New(obs.NewManualClock(domain.SeedTime), nil, nil, nil)
	events := make(chan domain.LedgerEvent, 4)
	rc := NewRecomputer(e, NewChannelSource(events), seedProvider(), WithDebounce(time.Millisecond))
	results := rc.Subscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rc.Run(ctx) }()

	for _, ev := range []domain.LedgerEvent{
		// A bridge lock names nobody the snapshot knows.
		{Seq: 1, Kind: domain.EventLock, Symbol: domain.FundSymbol, ObservedAt: domain.SeedTime},
		// A transfer to a counterparty outside the book.
		transfer(2, "CP-ALPHA", "CP-STRANGER"),
	} {
		res := func() Result {
			events <- ev
			return awaitResult(t, results)
		}()
		if res.Err != nil {
			t.Fatalf("cycle error: %v", res.Err)
		}
		if res.Partial {
			t.Errorf("event %d produced a partial recompute; want a full one", ev.Seq)
		}
		if len(res.Counterparties) != 8 {
			t.Errorf("event %d recomputed %d counterparties, want all 8", ev.Seq, len(res.Counterparties))
		}
	}
}

func TestRecomputerDebouncesABurst(t *testing.T) {
	e := New(obs.NewManualClock(domain.SeedTime), nil, nil, nil)
	events := make(chan domain.LedgerEvent, 64)
	rc := NewRecomputer(e, NewChannelSource(events), seedProvider(),
		WithDebounce(40*time.Millisecond), WithMaxDelay(2*time.Second))
	results := rc.Subscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rc.Run(ctx) }()

	for i := uint64(1); i <= 25; i++ {
		events <- transfer(i, "CP-ALPHA", "CP-BRAVO")
	}
	res := awaitResult(t, results)
	if len(res.TriggeredBy) != 25 {
		t.Errorf("cycle covered %d events, want all 25 collapsed into one", len(res.TriggeredBy))
	}
	select {
	case extra := <-results:
		t.Errorf("a second cycle ran for the same burst: %+v", extra.TriggeredBy)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestRecomputerPublishesProviderErrors(t *testing.T) {
	boom := errors.New("lakehouse unavailable")
	e := New(obs.NewManualClock(domain.SeedTime), nil, nil, nil)
	events := make(chan domain.LedgerEvent, 1)
	rc := NewRecomputer(e, NewChannelSource(events),
		func(context.Context, time.Time, time.Time) (domain.Snapshot, error) {
			return domain.Snapshot{}, boom
		}, WithDebounce(time.Millisecond))
	results := rc.Subscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rc.Run(ctx) }()

	events <- transfer(1, "CP-ALPHA", "CP-BRAVO")
	res := awaitResult(t, results)
	if !errors.Is(res.Err, boom) {
		t.Fatalf("err = %v, want the provider's error", res.Err)
	}
}

func TestRecomputerFilesBreaches(t *testing.T) {
	clock := obs.NewManualClock(domain.SeedTime)
	e := New(clock, nil, obs.NewRegistry(), nil)
	reg := NewBreachRegister(clock, nil, e.Metrics())

	events := make(chan domain.LedgerEvent, 4)
	rc := NewRecomputer(e, NewChannelSource(events), seedProvider(),
		WithDebounce(time.Millisecond), WithBreachRegister(reg))
	results := rc.Subscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rc.Run(ctx) }()

	events <- transfer(1, "CP-ALPHA", "CP-BRAVO")
	first := awaitResult(t, results)
	if len(first.NewBreaches) != 2 {
		t.Fatalf("first cycle raised %d breaches, want 2", len(first.NewBreaches))
	}

	events <- transfer(2, "CP-ALPHA", "CP-BRAVO")
	second := awaitResult(t, results)
	if len(second.NewBreaches) != 0 {
		t.Errorf("second cycle re-raised %d breaches", len(second.NewBreaches))
	}
	if len(second.Breaches) != 2 {
		t.Errorf("second cycle measured %d breaches, want 2", len(second.Breaches))
	}
	if got := reg.Len(); got != 2 {
		t.Errorf("register holds %d, want 2", got)
	}
}

func TestRecomputerLagMetric(t *testing.T) {
	clock := obs.NewManualClock(domain.SeedTime)
	e := New(clock, nil, obs.NewRegistry(), nil)
	events := make(chan domain.LedgerEvent, 1)
	rc := NewRecomputer(e, NewChannelSource(events), seedProvider(), WithDebounce(time.Millisecond))
	results := rc.Subscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rc.Run(ctx) }()

	// The event was observed a minute before the engine's clock reads.
	ev := transfer(1, "CP-ALPHA", "CP-BRAVO")
	ev.ObservedAt = domain.SeedTime.Add(-time.Minute)
	events <- ev

	res := awaitResult(t, results)
	if res.Lag != time.Minute {
		t.Errorf("lag = %s, want 1m", res.Lag)
	}
	if got := e.Metrics().Recomputes.Value(); got != 1 {
		t.Errorf("recompute counter = %v, want 1", got)
	}
	if got := e.Metrics().RecomputeLag.Count(); got != 1 {
		t.Errorf("lag observations = %d, want 1", got)
	}
}

// TestRecomputerShutsDownOnCancel is the leak test. It counts goroutines before
// and after, and it also checks that subscriber channels are closed rather than
// left dangling, because a subscriber blocked forever on a channel nobody will
// ever write to is a leak that a goroutine count would not catch.
func TestRecomputerShutsDownOnCancel(t *testing.T) {
	settle(t)
	before := runtime.NumGoroutine()

	e := New(obs.NewManualClock(domain.SeedTime), nil, nil, nil)
	events := make(chan domain.LedgerEvent, 32)
	rc := NewRecomputer(e, NewChannelSource(events), seedProvider(),
		WithDebounce(time.Millisecond), WithSubscriberBuffer(1))

	subs := []<-chan Result{rc.Subscribe(), rc.Subscribe(), rc.Subscribe()}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rc.Run(ctx) }()

	for i := uint64(1); i <= 5; i++ {
		events <- transfer(i, "CP-ALPHA", "CP-BRAVO")
	}
	// One subscriber drains; the other two deliberately do not, to prove a slow
	// subscriber neither blocks the loop nor survives shutdown.
	awaitResult(t, subs[0])

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	for i, ch := range subs {
		deadline := time.After(2 * time.Second)
		for open := true; open; {
			select {
			case _, ok := <-ch:
				open = ok
			case <-deadline:
				t.Fatalf("subscriber %d channel was never closed", i)
			}
		}
	}
	// Subscribing after shutdown returns a channel that is already closed,
	// rather than one that will never carry anything.
	late := rc.Subscribe()
	select {
	case _, ok := <-late:
		if ok {
			t.Error("a post-shutdown subscription received a result")
		}
	case <-time.After(time.Second):
		t.Error("a post-shutdown subscription channel was not closed")
	}

	settle(t)
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines: %d before, %d after", before, after)
	}
}

// TestRecomputerStopsWhenSourceCloses covers the other shutdown path: the
// indexer goes away rather than the context being cancelled.
func TestRecomputerStopsWhenSourceCloses(t *testing.T) {
	settle(t)
	before := runtime.NumGoroutine()

	e := New(obs.NewManualClock(domain.SeedTime), nil, nil, nil)
	events := make(chan domain.LedgerEvent, 4)
	rc := NewRecomputer(e, NewChannelSource(events), seedProvider(), WithDebounce(10*time.Millisecond))
	results := rc.Subscribe()

	done := make(chan error, 1)
	go func() { done <- rc.Run(context.Background()) }()

	events <- transfer(1, "CP-ALPHA", "CP-BRAVO")
	close(events)

	// The pending batch is still flushed on the way out: a shutdown must not
	// silently discard work already accepted.
	res := awaitResult(t, results)
	if res.Err != nil || len(res.TriggeredBy) != 1 {
		t.Errorf("final cycle = %+v", res)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil on a closed source", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when the source closed")
	}
	for range results {
	}

	settle(t)
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines: %d before, %d after", before, after)
	}
}

// settle waits for previously-started goroutines to finish so a count is not
// racing the runtime.
func settle(t *testing.T) {
	t.Helper()
	base := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n <= base {
			base = n
			if i > 3 {
				return
			}
			continue
		}
		base = n
	}
}
