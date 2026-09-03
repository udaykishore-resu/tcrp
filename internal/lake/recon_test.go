package lake_test

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/lake"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// afterTheCut is a transaction time late enough to include the seed's
// late-arriving event, which is the only way all three planted anomalies are
// visible at once.
var afterTheCut = domain.SeedTime.Add(60 * time.Minute)

func TestReconciliationFindsExactlyTheThreePlantedAnomalies(t *testing.T) {
	ctx := context.Background()
	lk, seed := newSeededLake(t)

	res, err := lk.Reconcile(ctx, domain.SeedTime, afterTheCut)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Breaks) != 3 {
		for _, b := range res.Breaks {
			t.Logf("break %s %s cp=%s ledger=%s delta=%d", b.ID, b.Kind, b.Counterparty, b.Ledger, b.Delta)
		}
		t.Fatalf("found %d breaks, want exactly the 3 planted anomalies", len(res.Breaks))
	}
	if res.Compared != len(seed.Events) {
		t.Fatalf("compared %d events, want all %d", res.Compared, len(seed.Events))
	}
	if res.Matched != res.Compared-3 {
		t.Fatalf("matched %d of %d compared, want %d", res.Matched, res.Compared, res.Compared-3)
	}

	byKind := map[domain.BreakKind]domain.Break{}
	for _, b := range res.Breaks {
		if _, dupe := byKind[b.Kind]; dupe {
			t.Fatalf("two breaks of kind %s", b.Kind)
		}
		byKind[b.Kind] = b
	}

	// Anomaly 1: an on-chain transfer the book of record never received.
	missing, ok := byKind[domain.BreakMissingInBook]
	if !ok {
		t.Fatal("no missing_on_book break for the settlement instruction the book never got")
	}
	if missing.Counterparty != "CP-DELTA" || missing.Ledger != domain.LedgerEVM {
		t.Fatalf("missing_in_book break is against %s on %s", missing.Counterparty, missing.Ledger)
	}
	if missing.LedgerQty != 1_750_000 || missing.BookQty != 0 || missing.Delta != 1_750_000 {
		t.Fatalf("missing_in_book quantities: ledger %d book %d delta %d",
			missing.LedgerQty, missing.BookQty, missing.Delta)
	}
	if missing.Severity != "critical" {
		t.Fatalf("a 1.75m share difference is %s, want critical", missing.Severity)
	}

	// Anomaly 2: a partial fill the book recorded at a different size.
	mismatch, ok := byKind[domain.BreakQuantityDiff]
	if !ok {
		t.Fatal("no quantity_mismatch break for the partial fill")
	}
	if mismatch.Ledger != domain.LedgerCanton || mismatch.Counterparty != "CP-BRAVO" {
		t.Fatalf("quantity_mismatch break is against %s on %s", mismatch.Counterparty, mismatch.Ledger)
	}
	if mismatch.LedgerQty != 900_000 || mismatch.BookQty != 1_000_000 || mismatch.Delta != -100_000 {
		t.Fatalf("quantity_mismatch quantities: ledger %d book %d delta %d",
			mismatch.LedgerQty, mismatch.BookQty, mismatch.Delta)
	}
	if mismatch.Severity != "high" {
		t.Fatalf("a 100k share difference is %s, want high", mismatch.Severity)
	}

	// Anomaly 3: the event observed after the risk cut.
	stale, ok := byKind[domain.BreakStale]
	if !ok {
		t.Fatal("no stale_observation break for the late-arriving transfer")
	}
	late := lateEvent(t, seed)
	if stale.Ledger != late.Ledger || stale.Symbol != late.Symbol {
		t.Fatalf("stale break is on %s/%s, want %s/%s", stale.Ledger, stale.Symbol, late.Ledger, late.Symbol)
	}
	if stale.Counterparty != late.To {
		t.Fatalf("stale break is against %s, want the receiving side %s", stale.Counterparty, late.To)
	}

	for _, b := range res.Breaks {
		if b.Status != domain.BreakOpen {
			t.Fatalf("break %s came out %s rather than open", b.ID, b.Status)
		}
		if b.DetectedAt.IsZero() || !b.AsOf.Equal(domain.SeedTime) {
			t.Fatalf("break %s has detected=%s as-of=%s", b.ID, b.DetectedAt, b.AsOf)
		}
	}
}

func TestReconciliationIsCleanBeforeTheLateEventArrives(t *testing.T) {
	ctx := context.Background()
	lk, _ := newSeededLake(t)

	// At the cut itself the late transfer has not been observed, so only the
	// two book divergences are visible. That the third break appears later is
	// the bitemporal point restated as an operational one.
	res, err := lk.Reconcile(ctx, domain.SeedTime, domain.SeedTime)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Breaks) != 2 {
		for _, b := range res.Breaks {
			t.Logf("break %s %s cp=%s", b.ID, b.Kind, b.Counterparty)
		}
		t.Fatalf("found %d breaks at the cut, want 2", len(res.Breaks))
	}
	for _, b := range res.Breaks {
		if b.Kind == domain.BreakStale {
			t.Fatal("the stale break appeared before the late event was observed")
		}
	}
}

func TestReconciliationIsCleanWhenTheBookAgrees(t *testing.T) {
	ctx := context.Background()
	seed := domain.NewSeed()
	clock := obs.NewManualClock(domain.SeedTime.Add(2 * time.Hour))
	lk, err := lake.NewFromSeed(ctx, t.TempDir(), clock, seed)
	if err != nil {
		t.Fatalf("NewFromSeed: %v", err)
	}
	// Replay the same events into a book with no divergences and a policy that
	// tolerates the late arrival: there is then nothing to find, which is how a
	// control proves it is not simply always firing.
	lk.Book = lake.BookFromEvents(seed.Events, nil)
	lk.Recon = lake.NewReconciler(lk.Options, lake.ReconPolicy{StaleAfter: 24 * time.Hour})

	res, err := lk.Reconcile(ctx, domain.SeedTime, afterTheCut)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Breaks) != 0 {
		for _, b := range res.Breaks {
			t.Logf("false positive: %s %s cp=%s delta=%d", b.ID, b.Kind, b.Counterparty, b.Delta)
		}
		t.Fatalf("%d breaks against an agreeing book", len(res.Breaks))
	}
	if res.Matched != res.Compared {
		t.Fatalf("matched %d of %d", res.Matched, res.Compared)
	}
}

func TestUnknownHolderIsDetected(t *testing.T) {
	ctx := context.Background()
	lk, seed := newSeededLake(t)

	rogue := domain.LedgerEvent{
		Seq: 9_000, Ledger: domain.LedgerEVM, Kind: domain.EventTransfer, Symbol: domain.FundSymbol,
		From: "CP-ALPHA", To: "CP-NOBODY", Quantity: 5_000,
		Block: 21_500_000, TxHash: "0xrogue", LogIndex: 0,
		OccurredAt: domain.SeedTime.Add(-time.Hour), ObservedAt: domain.SeedTime.Add(-time.Hour), Finalised: true,
	}
	events := append(append([]domain.LedgerEvent(nil), seed.Events...), rogue)
	res, err := lk.Recon.Reconcile(ctx, lake.ReconInput{
		ValidAt: domain.SeedTime, KnownAt: afterTheCut,
		Events:         events,
		Ledger:         lk.Silver.AsOf(domain.SeedTime, afterTheCut),
		Book:           lake.BookFromEvents(events, lake.SeedDivergences(seed)),
		Counterparties: seed.Counterparties,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	found := false
	for _, b := range res.Breaks {
		if b.Kind == domain.BreakUnknownHolder {
			found = true
			if b.Counterparty != "CP-NOBODY" {
				t.Fatalf("unknown_holder break names %s", b.Counterparty)
			}
			if b.Severity != "critical" {
				t.Fatalf("unknown_holder break is %s, want critical", b.Severity)
			}
		}
	}
	if !found {
		t.Fatal("a transfer to an unregistered holder produced no unknown_holder break")
	}
}

func TestImmaterialDifferenceIsSuppressedRatherThanOpened(t *testing.T) {
	ctx := context.Background()
	clock := obs.NewManualClock(origin)
	rec := lake.NewReconciler(lake.Options{Clock: clock}, lake.ReconPolicy{MaterialityQty: 10_000})

	ev := domain.LedgerEvent{
		Seq: 1, Ledger: domain.LedgerEVM, Kind: domain.EventTransfer, Symbol: "TMMF",
		From: "CP-A", To: "CP-B", Quantity: 1_000,
		Block: 1, TxHash: "0x1", OccurredAt: origin, ObservedAt: origin, Finalised: true,
	}
	book := lake.BookFromEvents([]domain.LedgerEvent{ev}, []lake.Divergence{
		{EventID: ev.ID(), Quantity: 999, Note: "rounding"},
	})
	res, err := rec.Reconcile(ctx, lake.ReconInput{
		ValidAt: origin, KnownAt: origin,
		Events: []domain.LedgerEvent{ev}, Book: book,
		Counterparties: []domain.Counterparty{{ID: "CP-A"}, {ID: "CP-B"}},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Suppressed != 1 {
		t.Fatalf("suppressed %d, want 1", res.Suppressed)
	}
	if len(res.Open()) != 0 {
		t.Fatalf("%d breaks opened below the materiality threshold", len(res.Open()))
	}
	if res.Breaks[0].Severity != "low" || res.Breaks[0].Resolution == "" {
		t.Fatalf("suppressed break is %s with resolution %q", res.Breaks[0].Severity, res.Breaks[0].Resolution)
	}
}

func TestInvestigationWorkflow(t *testing.T) {
	ctx := context.Background()
	seed := domain.NewSeed()
	clock := obs.NewManualClock(domain.SeedTime.Add(2 * time.Hour))
	lk, err := lake.NewFromSeed(ctx, t.TempDir(), clock, seed)
	if err != nil {
		t.Fatalf("NewFromSeed: %v", err)
	}
	res, err := lk.Reconcile(ctx, domain.SeedTime, afterTheCut)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	reg := lk.Recon.Register()
	id := res.Breaks[0].ID

	acked, err := reg.Acknowledge(id, "ops.jhalvorsen", "chasing the settlement agent")
	if err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if acked.Status != domain.BreakAcknowledged || acked.Assignee != "ops.jhalvorsen" {
		t.Fatalf("acknowledged break is %s assigned to %q", acked.Status, acked.Assignee)
	}

	// Re-running detection must not undo the acknowledgement.
	if _, err := lk.Reconcile(ctx, domain.SeedTime, afterTheCut); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	again, ok := reg.Get(id)
	if !ok || again.Status != domain.BreakAcknowledged {
		t.Fatalf("re-detection reset the break to %s", again.Status)
	}

	clock.Advance(90 * time.Minute)
	aged, _ := reg.Get(id)
	if aged.AgeMinutes < 90 {
		t.Fatalf("break age is %.0f minutes after 90 minutes passed", aged.AgeMinutes)
	}
	buckets := reg.Ageing()
	total := 0
	for _, b := range buckets {
		total += b.Count
	}
	if total != len(reg.Unresolved()) {
		t.Fatalf("ageing buckets hold %d breaks, register holds %d unresolved", total, len(reg.Unresolved()))
	}

	resolved, err := reg.Resolve(id, "ops.jhalvorsen", "book updated from the agent's confirmation")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Status != domain.BreakResolved || resolved.ResolvedAt.IsZero() {
		t.Fatalf("resolved break is %s at %s", resolved.Status, resolved.ResolvedAt)
	}
	if len(resolved.Notes) < 2 {
		t.Fatalf("resolved break carries %d notes, expected the whole trail", len(resolved.Notes))
	}
	if _, err := reg.Acknowledge(id, "someone", "reopening"); err == nil {
		t.Fatal("a resolved break was acknowledged again")
	}
	if _, err := reg.Resolve("BRK-nope", "x", "y"); err == nil {
		t.Fatal("resolving an unknown break succeeded")
	}

	other := res.Breaks[1].ID
	suppressed, err := reg.Suppress(other, "risk.dsmith", "known intraday timing difference")
	if err != nil {
		t.Fatalf("Suppress: %v", err)
	}
	if suppressed.Status != domain.BreakSuppressed || suppressed.Resolution == "" {
		t.Fatalf("suppressed break is %s with resolution %q", suppressed.Status, suppressed.Resolution)
	}
}

func TestSeedDivergencesAreTheTwoBookAnomalies(t *testing.T) {
	seed := domain.NewSeed()
	divs := lake.SeedDivergences(seed)
	if len(divs) != 2 {
		t.Fatalf("%d divergences, want 2", len(divs))
	}
	var missing, resized int
	for _, d := range divs {
		switch {
		case d.Missing:
			missing++
		case d.Quantity == 1_000_000:
			resized++
		default:
			t.Fatalf("unexpected divergence %+v", d)
		}
	}
	if missing != 1 || resized != 1 {
		t.Fatalf("divergences: %d missing, %d resized", missing, resized)
	}
}
