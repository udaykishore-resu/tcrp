package lake_test

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/lake"
	"github.com/udaykishore-resu/tcrp/internal/money"
)

func ev(seq uint64, ledger domain.Ledger, kind domain.EventKind, from, to string, qty int64, block uint64, lag time.Duration) domain.LedgerEvent {
	return domain.LedgerEvent{
		Seq: seq, Ledger: ledger, Kind: kind, Symbol: "TMMF",
		From: from, To: to, Quantity: qty, Block: block,
		TxHash: "0x" + string(rune('a'+seq)), LogIndex: 0,
		OccurredAt: origin, ObservedAt: origin.Add(lag), Finalised: true,
	}
}

func TestCompletenessDetectsSequenceGapsAndBlockRegressions(t *testing.T) {
	clean := []domain.LedgerEvent{
		ev(1, domain.LedgerEVM, domain.EventMint, "", "CP-A", 100, 10, time.Second),
		ev(2, domain.LedgerEVM, domain.EventMint, "", "CP-B", 100, 11, time.Second),
		ev(3, domain.LedgerCanton, domain.EventMint, "", "CP-C", 100, 12, time.Second),
	}
	if got := lake.Completeness(clean); got.Status != domain.QualityPass {
		t.Fatalf("clean stream reported %s: %s", got.Status, got.Detail)
	}

	gapped := []domain.LedgerEvent{clean[0], clean[2]}
	got := lake.Completeness(gapped)
	if got.Status != domain.QualityFail {
		t.Fatalf("a stream missing sequence 2 reported %s", got.Status)
	}

	regressed := []domain.LedgerEvent{
		clean[0],
		ev(2, domain.LedgerEVM, domain.EventMint, "", "CP-B", 100, 9, time.Second),
		clean[2],
	}
	if got := lake.Completeness(regressed); got.Status != domain.QualityFail {
		t.Fatalf("a stream whose block height regressed reported %s", got.Status)
	}
}

func TestUniquenessDetectsARepeatedEvent(t *testing.T) {
	e := ev(1, domain.LedgerEVM, domain.EventMint, "", "CP-A", 100, 10, time.Second)
	if got := lake.Uniqueness([]domain.LedgerEvent{e}); got.Status != domain.QualityPass {
		t.Fatalf("one event reported %s", got.Status)
	}
	got := lake.Uniqueness([]domain.LedgerEvent{e, e})
	if got.Status != domain.QualityFail || !got.Critical {
		t.Fatalf("a duplicated event reported %s (critical=%v)", got.Status, got.Critical)
	}
}

func TestTimelinessWarnsOnObservationLag(t *testing.T) {
	prompt := ev(1, domain.LedgerEVM, domain.EventMint, "", "CP-A", 100, 10, 5*time.Second)
	if got := lake.Timeliness([]domain.LedgerEvent{prompt}, time.Minute); got.Status != domain.QualityPass {
		t.Fatalf("a five second lag reported %s", got.Status)
	}
	late := ev(2, domain.LedgerEVM, domain.EventMint, "", "CP-A", 100, 11, 2*time.Hour)
	got := lake.Timeliness([]domain.LedgerEvent{prompt, late}, time.Minute)
	if got.Status != domain.QualityWarn {
		t.Fatalf("a two hour lag reported %s", got.Status)
	}
	if got.Critical {
		t.Fatal("timeliness is critical; a late arrival the bitemporal store handles must not block a snapshot")
	}
}

func TestAccuracyChecksTheSupplyIdentity(t *testing.T) {
	events := []domain.LedgerEvent{
		ev(1, domain.LedgerEVM, domain.EventMint, "", "CP-A", 1_000, 10, time.Second),
		ev(2, domain.LedgerEVM, domain.EventTransfer, "CP-A", "CP-B", 400, 11, time.Second),
		ev(3, domain.LedgerEVM, domain.EventLock, "CP-B", "", 100, 12, time.Second),
	}
	balanced := []domain.Position{
		{Counterparty: "CP-A", Symbol: "TMMF", Ledger: domain.LedgerEVM, Quantity: 600},
		{Counterparty: "CP-B", Symbol: "TMMF", Ledger: domain.LedgerEVM, Quantity: 300},
	}
	if got := lake.Accuracy(events, balanced); got.Status != domain.QualityPass {
		t.Fatalf("a balanced book reported %s: %s", got.Status, got.Detail)
	}
	unbalanced := append([]domain.Position(nil), balanced...)
	unbalanced[0].Quantity = 601
	got := lake.Accuracy(events, unbalanced)
	if got.Status != domain.QualityFail || !got.Critical {
		t.Fatalf("an unbalanced book reported %s (critical=%v)", got.Status, got.Critical)
	}
}

func TestReferentialIntegrityChecksHolders(t *testing.T) {
	known := []domain.Counterparty{{ID: "CP-A"}, {ID: "CP-B"}}
	events := []domain.LedgerEvent{ev(1, domain.LedgerEVM, domain.EventTransfer, "CP-A", "CP-B", 10, 10, time.Second)}
	if got := lake.ReferentialIntegrity(events, known); got.Status != domain.QualityPass {
		t.Fatalf("known holders reported %s", got.Status)
	}
	events = append(events, ev(2, domain.LedgerEVM, domain.EventTransfer, "CP-A", "CP-GHOST", 10, 11, time.Second))
	got := lake.ReferentialIntegrity(events, known)
	if got.Status != domain.QualityFail || !got.Critical {
		t.Fatalf("an unknown holder reported %s (critical=%v)", got.Status, got.Critical)
	}
}

func TestFreshnessChecksMarkAge(t *testing.T) {
	fresh := []domain.Price{{Symbol: "TMMF", Px: money.MustParse(money.USD, "1.00"), AsOf: origin.Add(-time.Minute)}}
	if got := lake.Freshness(fresh, origin, time.Hour); got.Status != domain.QualityPass {
		t.Fatalf("a one minute old mark reported %s", got.Status)
	}
	stale := []domain.Price{{Symbol: "TMMF", Px: money.MustParse(money.USD, "1.00"), AsOf: origin.Add(-4 * time.Hour)}}
	if got := lake.Freshness(stale, origin, time.Hour); got.Status != domain.QualityWarn {
		t.Fatalf("a four hour old mark reported %s", got.Status)
	}
	if got := lake.Freshness(nil, origin, time.Hour); got.Status != domain.QualityFail || !got.Critical {
		t.Fatal("no marks at all must be a critical failure")
	}
}

func TestSeededSnapshotPassesEveryControlBeforeTheLateArrival(t *testing.T) {
	ctx := context.Background()
	lk, _ := newSeededLake(t)

	atCut, err := lk.Snapshot(ctx, domain.SeedTime, domain.SeedTime)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if atCut.Quality.Status != domain.QualityPass {
		for _, c := range atCut.Quality.Checks {
			t.Logf("%s %s %s", c.ID, c.Status, c.Detail)
		}
		t.Fatalf("the snapshot at the cut is %s, want pass", atCut.Quality.Status)
	}
	if len(atCut.Quality.Checks) != 6 {
		t.Fatalf("%d controls ran, want 6", len(atCut.Quality.Checks))
	}

	// Once the late arrival is known, timeliness warns but nothing critical
	// fails, so the snapshot stays usable.
	later, err := lk.Snapshot(ctx, domain.SeedTime, afterTheCut)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if later.Quality.Status != domain.QualityWarn {
		t.Fatalf("the snapshot after the late arrival is %s, want warn", later.Quality.Status)
	}
	if !later.Quality.Usable() {
		t.Fatal("a timeliness warning made the snapshot unusable")
	}
	for _, c := range later.Quality.Checks {
		if c.ID == lake.CheckTimeliness && c.Status != domain.QualityWarn {
			t.Fatalf("timeliness reported %s after the late arrival", c.Status)
		}
	}
}

func TestGoldSnapshotCarriesLineageAndReferenceData(t *testing.T) {
	ctx := context.Background()
	lk, seed := newSeededLake(t)

	snap, err := lk.Snapshot(ctx, domain.SeedTime, afterTheCut)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Lineage) != 4 {
		t.Fatalf("lineage has %d entries: %v", len(snap.Lineage), snap.Lineage)
	}
	for _, want := range []string{lake.BronzeTable, lake.SilverTable, lake.GoldTable} {
		found := false
		for _, l := range snap.Lineage {
			if len(l) > len(want) && l[5:5+len(want)] == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("lineage %v does not name %s", snap.Lineage, want)
		}
	}
	if len(snap.Counterparties) != len(seed.Counterparties) {
		t.Fatalf("snapshot carries %d counterparties, want %d", len(snap.Counterparties), len(seed.Counterparties))
	}
	if len(snap.Obligations) != len(seed.Obligations) || len(snap.Limits) != len(seed.Limits) {
		t.Fatalf("snapshot carries %d obligations and %d limits", len(snap.Obligations), len(snap.Limits))
	}
	if snap.Haircuts.Version != seed.Haircuts.Version {
		t.Fatalf("snapshot haircut schedule is %q", snap.Haircuts.Version)
	}
	if px, ok := snap.Prices[domain.FundSymbol]; !ok || px.Px.Ccy != money.USD {
		t.Fatalf("snapshot has no usable mark for %s", domain.FundSymbol)
	}
	for _, p := range snap.Positions {
		if p.Quantity == 0 {
			t.Fatalf("a netted-out slot %s reached the curated snapshot", p.Key())
		}
	}
	// The gold table materialised the same rows.
	res, err := lk.Gold.Table().Scan(ctx)
	if err != nil {
		t.Fatalf("gold scan: %v", err)
	}
	if int64(len(res.Rows)) < int64(len(snap.Positions)) {
		t.Fatalf("gold table holds %d rows, snapshot has %d positions", len(res.Rows), len(snap.Positions))
	}
}
