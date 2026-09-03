package risk

import (
	"context"
	"testing"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/money"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// TestStressEveryScenario runs every seeded scenario and pins the aggregate.
// The figures are regression pins: they were produced by this engine and the
// arithmetic behind two of them is checked by hand below.
func TestStressEveryScenario(t *testing.T) {
	e, snap := newTestEngine(t)
	base := mustUSD(t, "360150887.70")

	for _, tc := range []struct {
		scenario  string
		stressed  string
		shortfall string
		worst     string
	}{
		{"rate_shock", "344040708.008", "2346799.64", "CP-FOXTROT"},
		{"haircut_widening", "341706098.20", "2097515.00", "CP-FOXTROT"},
		{"counterparty_default", "335134323.305", "2743834.70", "CP-FOXTROT"},
		{"redemption_run", "290375310.875", "14836378.375", "CP-ECHO"},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			sc, ok := domain.NewSeed().Scenario(tc.scenario)
			if !ok {
				t.Fatalf("no scenario %s in the seed", tc.scenario)
			}
			sum, err := e.Stress(context.Background(), snap, sc)
			if err != nil {
				t.Fatalf("Stress: %v", err)
			}
			if !sum.TotalBase.Equal(base) {
				t.Errorf("base = %s, want %s", sum.TotalBase, base)
			}
			if want := mustUSD(t, tc.stressed); !sum.TotalStressed.Equal(want) {
				t.Errorf("stressed = %s, want %s", sum.TotalStressed, want)
			}
			if want := mustUSD(t, tc.shortfall); !sum.TotalShortfall.Equal(want) {
				t.Errorf("new shortfall = %s, want %s", sum.TotalShortfall, want)
			}
			if sum.WorstCP != tc.worst {
				t.Errorf("worst = %s, want %s", sum.WorstCP, tc.worst)
			}
			if sum.Counterparties != len(snap.Counterparties) {
				t.Errorf("counterparties = %d, want %d", sum.Counterparties, len(snap.Counterparties))
			}
			// Every scenario in the seed is adverse: none of them may leave the
			// book better collateralised than it started.
			if !sum.TotalStressed.Less(sum.TotalBase) {
				t.Errorf("stressed %s is not below base %s", sum.TotalStressed, sum.TotalBase)
			}
			if sum.TotalShortfall.IsNegative() {
				t.Errorf("new shortfall %s is negative", sum.TotalShortfall)
			}
		})
	}
}

// TestStressDefaultedCounterpartyIsWorseThanBase is the assertion the scenario
// exists for: assuming a counterparty has failed must not produce an outcome
// milder than the unstressed book.
func TestStressDefaultedCounterpartyIsWorseThanBase(t *testing.T) {
	e, snap := newTestEngine(t)
	ctx := context.Background()

	sc, ok := domain.NewSeed().Scenario("counterparty_default")
	if !ok {
		t.Fatal("no counterparty_default scenario")
	}
	sum, err := e.Stress(ctx, snap, sc)
	if err != nil {
		t.Fatalf("Stress: %v", err)
	}

	baseUncovered := money.Zero(money.USD)
	xs, err := e.ExposureAll(ctx, snap)
	if err != nil {
		t.Fatalf("ExposureAll: %v", err)
	}
	for _, x := range xs {
		baseUncovered = baseUncovered.MustAdd(x.Uncovered)
	}
	stressedUncovered := money.Zero(money.USD)
	for _, r := range sum.Results {
		stressedUncovered = stressedUncovered.MustAdd(r.StressedUncov)
		if r.StressedValue.Greater(r.BaseCollateral) {
			t.Errorf("%s: stressed collateral %s exceeds base %s",
				r.Counterparty, r.StressedValue, r.BaseCollateral)
		}
		if r.Delta.Greater(money.Zero(money.USD)) {
			t.Errorf("%s: delta %s is positive under an adverse scenario", r.Counterparty, r.Delta)
		}
	}
	if !stressedUncovered.Greater(baseUncovered) {
		t.Fatalf("stressed uncovered %s is not worse than base %s", stressedUncovered, baseUncovered)
	}
	if sum.TotalShortfall.IsZero() {
		t.Fatal("a counterparty default produced no new shortfall")
	}
}

// TestStressDefaultSuppressesNetting isolates the modelling decision: in the
// default scenario close-out netting is assumed to be challenged, so the
// defaulted counterparty's exposure reverts to gross. CP-ALPHA owes 48,000,000
// and is owed 6,500,000, so the difference must be exactly 6,500,000.
func TestStressDefaultSuppressesNetting(t *testing.T) {
	e, snap := newTestEngine(t)
	ctx := context.Background()

	// A shock deep enough that CP-ALPHA is uncovered either way, so the
	// netting difference is visible rather than absorbed by excess collateral.
	shock := domain.StressScenario{ID: "halve", PriceShock: money.Percent(-50)}
	withDefault := shock
	withDefault.ID = "halve_with_default"
	withDefault.DefaultedCounterparties = []string{"CP-ALPHA"}

	plain, err := e.Stress(ctx, snap, shock)
	if err != nil {
		t.Fatalf("Stress: %v", err)
	}
	defaulted, err := e.Stress(ctx, snap, withDefault)
	if err != nil {
		t.Fatalf("Stress: %v", err)
	}

	find := func(s domain.StressSummary, cp string) domain.StressResult {
		t.Helper()
		for _, r := range s.Results {
			if r.Counterparty == cp {
				return r
			}
		}
		t.Fatalf("no result for %s", cp)
		return domain.StressResult{}
	}
	a, b := find(plain, "CP-ALPHA"), find(defaulted, "CP-ALPHA")

	if !a.StressedValue.Equal(b.StressedValue) {
		t.Errorf("collateral differs (%s vs %s); a default must not change the pledge's value",
			a.StressedValue, b.StressedValue)
	}
	diff, err := b.StressedUncov.Sub(a.StressedUncov)
	if err != nil {
		t.Fatal(err)
	}
	if want := mustUSD(t, "6500000.00"); !diff.Equal(want) {
		t.Errorf("netting suppression is worth %s, want %s", diff, want)
	}

	// Nobody else is affected by CP-ALPHA's default.
	for _, cp := range []string{"CP-BRAVO", "CP-DELTA", "CP-GOLF"} {
		if !find(plain, cp).StressedUncov.Equal(find(defaulted, cp).StressedUncov) {
			t.Errorf("%s moved when CP-ALPHA defaulted", cp)
		}
	}
}

// TestStressRedemptionHandComputed checks the redemption and haircut add-on
// arithmetic on one counterparty:
//
//	CP-GOLF under redemption_run (MMF -150bp, haircut add-on +300bp, 25% redeemed)
//	  TMMF     54,000,000 x 0.75 = 40,500,000 shares @ 0.985 = 39,892,500.00
//	           haircut 50bp + 300bp = 350bp -> x 0.965 = 38,496,262.50
//	  CASH-USD 15,000,000 @ 1.0000 = 15,000,000.00
//	           haircut 0bp + 300bp = 300bp -> x 0.97   = 14,550,000.00
//	  UST-10Y     210,000 @ 95.25 = 20,002,500.00
//	           haircut 200bp + 300bp = 500bp -> x 0.95 = 19,002,375.00
//	  stressed collateral = 72,048,637.50
func TestStressRedemptionHandComputed(t *testing.T) {
	e, snap := newTestEngine(t)
	sc, ok := domain.NewSeed().Scenario("redemption_run")
	if !ok {
		t.Fatal("no redemption_run scenario")
	}
	sum, err := e.Stress(context.Background(), snap, sc)
	if err != nil {
		t.Fatalf("Stress: %v", err)
	}
	for _, r := range sum.Results {
		if r.Counterparty != "CP-GOLF" {
			continue
		}
		if want := mustUSD(t, "72048637.50"); !r.StressedValue.Equal(want) {
			t.Fatalf("CP-GOLF stressed collateral = %s, want %s", r.StressedValue, want)
		}
		return
	}
	t.Fatal("no result for CP-GOLF")
}

// TestStressAllMatchesIndividualRuns checks the concurrent multi-scenario path
// against the single-scenario one and pins the output order to the input order.
func TestStressAllMatchesIndividualRuns(t *testing.T) {
	e, snap := newTestEngine(t)
	ctx := context.Background()
	scenarios := domain.NewSeed().Scenarios

	all, err := e.StressAll(ctx, snap, scenarios)
	if err != nil {
		t.Fatalf("StressAll: %v", err)
	}
	if len(all) != len(scenarios) {
		t.Fatalf("got %d summaries, want %d", len(all), len(scenarios))
	}
	for i, sc := range scenarios {
		if all[i].Scenario != sc.ID {
			t.Fatalf("summary %d is %s, want %s (input order not preserved)", i, all[i].Scenario, sc.ID)
		}
		one, err := e.Stress(ctx, snap, sc)
		if err != nil {
			t.Fatalf("Stress(%s): %v", sc.ID, err)
		}
		if !one.TotalStressed.Equal(all[i].TotalStressed) || !one.TotalShortfall.Equal(all[i].TotalShortfall) {
			t.Errorf("%s: concurrent run differs from single run", sc.ID)
		}
		if one.WorstCP != all[i].WorstCP {
			t.Errorf("%s: worst counterparty %s vs %s", sc.ID, all[i].WorstCP, one.WorstCP)
		}
	}
}

func TestStressRejectsUnnamedScenario(t *testing.T) {
	e, snap := newTestEngine(t)
	if _, err := e.Stress(context.Background(), snap, domain.StressScenario{}); err != ErrNoScenario {
		t.Fatalf("err = %v, want ErrNoScenario", err)
	}
}

func TestStressMetricsPerScenario(t *testing.T) {
	reg := obs.NewRegistry()
	snap := domain.SnapshotFromSeed(domain.NewSeed())
	e := New(obs.NewManualClock(domain.SeedTime), nil, reg, nil)

	if _, err := e.StressAll(context.Background(), snap, domain.NewSeed().Scenarios); err != nil {
		t.Fatalf("StressAll: %v", err)
	}
	for _, id := range []string{"rate_shock", "haircut_widening", "counterparty_default", "redemption_run"} {
		if got := e.Metrics().StressLatency(id).Count(); got != 1 {
			t.Errorf("%s: %d latency observations, want 1", id, got)
		}
	}
	if !contains(reg.Render(), MetricStressSeconds) {
		t.Errorf("registry render missing %s", MetricStressSeconds)
	}
}
