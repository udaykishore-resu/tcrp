package lake_test

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/lake"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// lateEvent returns the seed's deliberately late-arriving transfer: it occurred
// ninety minutes before the risk cut and was observed thirty-five minutes after
// it.
func lateEvent(t *testing.T, seed *domain.Seed) domain.LedgerEvent {
	t.Helper()
	for _, e := range seed.Events {
		if e.Memo == "late-arriving; observed after the risk cut" {
			return e
		}
	}
	t.Fatal("the seed no longer contains the late-arriving event")
	return domain.LedgerEvent{}
}

func newSeededLake(t *testing.T) (*lake.Lake, *domain.Seed) {
	t.Helper()
	seed := domain.NewSeed()
	clock := obs.NewManualClock(domain.SeedTime.Add(2 * time.Hour))
	lk, err := lake.NewFromSeed(context.Background(), t.TempDir(), clock, seed)
	if err != nil {
		t.Fatalf("NewFromSeed: %v", err)
	}
	return lk, seed
}

func TestAsOfDiffersEitherSideOfTheLateArrival(t *testing.T) {
	lk, seed := newSeededLake(t)
	late := lateEvent(t, seed)

	if !late.OccurredAt.Before(domain.SeedTime) {
		t.Fatalf("the late event occurred at %s, expected before the cut", late.OccurredAt)
	}
	if !late.ObservedAt.After(domain.SeedTime) {
		t.Fatalf("the late event was observed at %s, expected after the cut", late.ObservedAt)
	}

	beforeCut := lk.Silver.AsOf(domain.SeedTime, domain.SeedTime)
	afterCut := lk.Silver.AsOf(domain.SeedTime, late.ObservedAt.Add(time.Minute))

	fromBefore := quantityOf(t, beforeCut, late.From, late.Symbol, late.Ledger)
	fromAfter := quantityOf(t, afterCut, late.From, late.Symbol, late.Ledger)
	if fromBefore-fromAfter != late.Quantity {
		t.Fatalf("%s held %d before the correction and %d after; the difference should be the late transfer of %d",
			late.From, fromBefore, fromAfter, late.Quantity)
	}
	toBefore := quantityOf(t, beforeCut, late.To, late.Symbol, late.Ledger)
	toAfter := quantityOf(t, afterCut, late.To, late.Symbol, late.Ledger)
	if toAfter-toBefore != late.Quantity {
		t.Fatalf("%s held %d before and %d after; the difference should be %d",
			late.To, toBefore, toAfter, late.Quantity)
	}

	// The two views differ only on the slots the late event touched.
	touched := map[string]bool{
		lake.PositionKey(late.From, late.Symbol, late.Ledger): true,
		lake.PositionKey(late.To, late.Symbol, late.Ledger):   true,
	}
	byKeyBefore := map[string]int64{}
	for _, p := range beforeCut {
		byKeyBefore[p.Key()] = p.Quantity
	}
	for _, p := range afterCut {
		if touched[p.Key()] {
			continue
		}
		if byKeyBefore[p.Key()] != p.Quantity {
			t.Fatalf("%s changed from %d to %d, but the late event does not touch it",
				p.Key(), byKeyBefore[p.Key()], p.Quantity)
		}
	}
}

func TestCorrectionSupersedesWithoutErasing(t *testing.T) {
	lk, seed := newSeededLake(t)
	late := lateEvent(t, seed)
	key := lake.PositionKey(late.From, late.Symbol, late.Ledger)

	history := lk.Silver.History(key)
	if len(history) < 3 {
		t.Fatalf("history for %s has %d rows, expected several versions", key, len(history))
	}

	var corrections, closedAtObservation int
	for _, p := range history {
		if p.Corrected {
			corrections++
			if !p.TxFrom.Equal(late.ObservedAt.UTC()) {
				t.Fatalf("correction %s has tx_from %s, want the late event's observation time %s",
					p.ID, p.TxFrom, late.ObservedAt)
			}
			if !p.ValidFrom.Equal(late.OccurredAt.UTC()) {
				t.Fatalf("correction %s is valid from %s, want the late event's occurrence time %s",
					p.ID, p.ValidFrom, late.OccurredAt)
			}
		}
		if p.TxTo.Equal(late.ObservedAt.UTC()) {
			closedAtObservation++
		}
	}
	if corrections != 1 {
		t.Fatalf("%d corrected rows on %s, want exactly 1", corrections, key)
	}
	if closedAtObservation != 1 {
		t.Fatalf("%d rows closed on the transaction axis at the observation, want 1", closedAtObservation)
	}

	// The superseded belief is still readable at the transaction time it was
	// held, which is the whole point of keeping it.
	superseded := lk.Silver.AsOf(domain.SeedTime, domain.SeedTime)
	if quantityOf(t, superseded, late.From, late.Symbol, late.Ledger) == 0 {
		t.Fatal("the earlier belief is no longer queryable")
	}
	last := lk.Silver.AsOf(domain.SeedTime, domain.SeedTime.Add(2*time.Hour))
	for _, p := range last {
		if p.Key() == key && !p.Corrected {
			t.Fatalf("the current version of %s is not flagged as a correction", key)
		}
	}
}

func TestOutOfOrderArrivalRewritesTheValidTimeline(t *testing.T) {
	ctx := context.Background()
	clock := obs.NewManualClock(origin)
	silver, err := lake.NewSilver(ctx, t.TempDir(), lake.Options{Clock: clock})
	if err != nil {
		t.Fatalf("NewSilver: %v", err)
	}
	mk := func(seq uint64, kind domain.EventKind, from, to string, qty int64, occurred, observed time.Duration) domain.LedgerEvent {
		return domain.LedgerEvent{
			Seq: seq, Ledger: domain.LedgerEVM, Kind: kind, Symbol: "TMMF",
			From: from, To: to, Quantity: qty,
			Block: 100 + seq, TxHash: "0xdead" + string(rune('a'+seq)), LogIndex: 0,
			OccurredAt: origin.Add(occurred), ObservedAt: origin.Add(observed), Finalised: true,
		}
	}
	events := []domain.LedgerEvent{
		mk(1, domain.EventMint, "", "CP-A", 1_000, 0, time.Minute),
		mk(2, domain.EventTransfer, "CP-A", "CP-B", 100, 2*time.Hour, 2*time.Hour+time.Minute),
		// Occurred between the two above, observed after both: a genuine
		// out-of-order arrival.
		mk(3, domain.EventTransfer, "CP-A", "CP-C", 250, time.Hour, 3*time.Hour),
	}
	if _, err := silver.Apply(ctx, events); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	key := lake.PositionKey("CP-A", "TMMF", domain.LedgerEVM)
	// Before the third event was observed, CP-A held 1000 until t+2h and 900
	// afterwards.
	before := origin.Add(2*time.Hour + 30*time.Minute)
	if got := quantityOf(t, silver.AsOf(origin.Add(90*time.Minute), before), "CP-A", "TMMF", domain.LedgerEVM); got != 1_000 {
		t.Fatalf("at t+90m as known at t+2h30m, CP-A held %d, want 1000", got)
	}
	// Once the out-of-order transfer is known, the same valid time reads 750.
	after := origin.Add(4 * time.Hour)
	if got := quantityOf(t, silver.AsOf(origin.Add(90*time.Minute), after), "CP-A", "TMMF", domain.LedgerEVM); got != 750 {
		t.Fatalf("at t+90m as known at t+4h, CP-A held %d, want 750", got)
	}
	// And so does everything after it: the correction propagated forward.
	if got := quantityOf(t, silver.AsOf(origin.Add(3*time.Hour), after), "CP-A", "TMMF", domain.LedgerEVM); got != 650 {
		t.Fatalf("at t+3h as known at t+4h, CP-A held %d, want 650", got)
	}

	corrections := 0
	for _, p := range silver.History(key) {
		if p.Corrected {
			corrections++
		}
	}
	if corrections == 0 {
		t.Fatal("the out-of-order arrival produced no correction rows")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	seed := domain.NewSeed()
	clock := obs.NewManualClock(domain.SeedTime)
	silver, err := lake.NewSilver(ctx, t.TempDir(), lake.Options{Clock: clock})
	if err != nil {
		t.Fatalf("NewSilver: %v", err)
	}
	first, err := silver.Apply(ctx, seed.Events)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rowsAfterFirst := len(silver.Rows())
	second, err := silver.Apply(ctx, seed.Events)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if second.Events != 0 || second.Rows != 0 {
		t.Fatalf("replay applied %d events and wrote %d rows", second.Events, second.Rows)
	}
	if second.Duplicates != first.Events {
		t.Fatalf("replay reported %d duplicates, want %d", second.Duplicates, first.Events)
	}
	if len(silver.Rows()) != rowsAfterFirst {
		t.Fatalf("replay changed the row count from %d to %d", rowsAfterFirst, len(silver.Rows()))
	}
}

func TestSilverReopensFromItsTable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seed := domain.NewSeed()
	clock := obs.NewManualClock(domain.SeedTime)
	silver, err := lake.NewSilver(ctx, dir, lake.Options{Clock: clock})
	if err != nil {
		t.Fatalf("NewSilver: %v", err)
	}
	if _, err := silver.Apply(ctx, seed.Events); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := silver.AsOf(domain.SeedTime, domain.SeedTime.Add(2*time.Hour))

	reopened, err := lake.NewSilver(ctx, dir, lake.Options{Clock: clock})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := reopened.AsOf(domain.SeedTime, domain.SeedTime.Add(2*time.Hour))
	if len(got) != len(want) {
		t.Fatalf("reopened store has %d positions, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Key() != want[i].Key() || got[i].Quantity != want[i].Quantity {
			t.Fatalf("position %d differs: %s=%d against %s=%d",
				i, got[i].Key(), got[i].Quantity, want[i].Key(), want[i].Quantity)
		}
	}
}

func quantityOf(t *testing.T, ps []domain.Position, cp, symbol string, ledger domain.Ledger) int64 {
	t.Helper()
	key := lake.PositionKey(cp, symbol, ledger)
	for _, p := range ps {
		if p.Key() == key {
			return p.Quantity
		}
	}
	return 0
}
