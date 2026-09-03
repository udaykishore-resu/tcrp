package risk

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/money"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// newTestEngine builds an engine on a manual clock, so every figure — including
// the durations that land in a summary — is reproducible.
func newTestEngine(t *testing.T, opts ...Option) (*Engine, domain.Snapshot) {
	t.Helper()
	snap := domain.SnapshotFromSeed(domain.NewSeed())
	e := New(obs.NewManualClock(domain.SeedTime), nil, obs.NewRegistry(), nil, opts...)
	return e, snap
}

func mustUSD(t *testing.T, s string) money.Amount {
	t.Helper()
	a, err := money.Parse(money.USD, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return a
}

// TestCollateralValueHandComputed pins the arithmetic to a figure worked out by
// hand from the seed, so a change in the rounding, the haircut lookup or the
// advance rate cannot pass silently.
//
//	CP-ALPHA holds, all in USD:
//	  TMMF      40,000,000 x   1.0000 = 40,000,000.00; MMF/AAA  haircut  50bp -> x 0.9950 = 39,800,000.00
//	  UST-10Y      120,000 x  95.2500 = 11,430,000.00; GOVT/AAA haircut 200bp -> x 0.9800 = 11,201,400.00
//	  CORP-JPM      90,000 x  97.8750 =  8,808,750.00; CORP/A   haircut 700bp -> x 0.9300 =  8,192,137.50
//	  market value = 60,238,750.00      collateral value = 59,193,537.50
func TestCollateralValueHandComputed(t *testing.T) {
	e, snap := newTestEngine(t)
	cv, err := e.CollateralValue(context.Background(), snap, "CP-ALPHA")
	if err != nil {
		t.Fatalf("CollateralValue: %v", err)
	}

	if want := mustUSD(t, "60238750.00"); !cv.MarketValue.Equal(want) {
		t.Errorf("market value = %s, want %s", cv.MarketValue, want)
	}
	if want := mustUSD(t, "59193537.50"); !cv.CollateralValue.Equal(want) {
		t.Errorf("collateral value = %s, want %s", cv.CollateralValue, want)
	}
	if !cv.IneligibleValue.IsZero() {
		t.Errorf("ineligible = %s, want zero", cv.IneligibleValue)
	}

	byLine := map[string]domain.CollateralLine{}
	for _, l := range cv.Lines {
		byLine[l.Symbol] = l
	}
	for _, tc := range []struct {
		symbol             string
		market, collateral string
		haircut            money.Ratio
	}{
		{"TMMF", "40000000.00", "39800000.00", money.Bps(50)},
		{"UST-10Y", "11430000.00", "11201400.00", money.Bps(200)},
		{"CORP-JPM", "8808750.00", "8192137.50", money.Bps(700)},
	} {
		l, ok := byLine[tc.symbol]
		if !ok {
			t.Fatalf("no line for %s", tc.symbol)
		}
		if want := mustUSD(t, tc.market); !l.Market.Equal(want) {
			t.Errorf("%s market = %s, want %s", tc.symbol, l.Market, want)
		}
		if want := mustUSD(t, tc.collateral); !l.Collateral.Equal(want) {
			t.Errorf("%s collateral = %s, want %s", tc.symbol, l.Collateral, want)
		}
		if cmpRatio(l.Haircut, tc.haircut) != 0 {
			t.Errorf("%s haircut = %v, want %v", tc.symbol, l.Haircut, tc.haircut)
		}
		if cmpRatio(l.AdvanceRate, tc.haircut.Complement()) != 0 {
			t.Errorf("%s advance = %v, want %v", tc.symbol, l.AdvanceRate, tc.haircut.Complement())
		}
	}
}

// TestCollateralValueTable walks every counterparty in the seed against figures
// derived the same way.
func TestCollateralValueTable(t *testing.T) {
	e, snap := newTestEngine(t)
	for _, tc := range []struct {
		cp                 string
		market, collateral string
	}{
		{"CP-ALPHA", "60238750.00", "59193537.50"},
		// BUND-10Y is EUR: 80,000 x 99.10 = 7,928,000 EUR, x0.98 = 7,769,440 EUR,
		// converted at 1.08 = 8,390,995.20 USD.
		{"CP-BRAVO", "98249740.00", "97259745.20"},
		{"CP-CHARLIE", "43454500.00", "40734575.00"},
		{"CP-DELTA", "11982500.00", "11464600.00"},
		{"CP-ECHO", "36000000.00", "35695000.00"},
		{"CP-FOXTROT", "29967800.00", "27470980.00"},
		{"CP-GOLF", "89002500.00", "88332450.00"},
		{"CP-HOTEL", "0.00", "0.00"},
	} {
		t.Run(tc.cp, func(t *testing.T) {
			cv, err := e.CollateralValue(context.Background(), snap, tc.cp)
			if err != nil {
				t.Fatalf("CollateralValue: %v", err)
			}
			if want := mustUSD(t, tc.market); !cv.MarketValue.Equal(want) {
				t.Errorf("market = %s, want %s", cv.MarketValue, want)
			}
			if want := mustUSD(t, tc.collateral); !cv.CollateralValue.Equal(want) {
				t.Errorf("collateral = %s, want %s", cv.CollateralValue, want)
			}
			if cv.Currency != money.USD {
				t.Errorf("currency = %s, want USD", cv.Currency)
			}
		})
	}
}

func TestCollateralValueUnknownCounterparty(t *testing.T) {
	e, snap := newTestEngine(t)
	if _, err := e.CollateralValue(context.Background(), snap, "CP-NOBODY"); !errors.Is(err, ErrUnknownCounterparty) {
		t.Fatalf("err = %v, want ErrUnknownCounterparty", err)
	}
}

func TestCollateralValueMissingPrice(t *testing.T) {
	e, snap := newTestEngine(t)
	prices := map[string]domain.Price{}
	for k, v := range snap.Prices {
		if k == "UST-10Y" {
			continue
		}
		prices[k] = v
	}
	snap.Prices = prices
	if _, err := e.CollateralValue(context.Background(), snap, "CP-ALPHA"); !errors.Is(err, ErrNoPrice) {
		t.Fatalf("err = %v, want ErrNoPrice", err)
	}
}

// TestMissingFXRateIsAnError is the control that stops a EUR pledge being
// counted as though it were dollars. An engine with no EUR rate must refuse to
// value CP-BRAVO rather than treat 1 EUR as 1 USD.
func TestMissingFXRateIsAnError(t *testing.T) {
	snap := domain.SnapshotFromSeed(domain.NewSeed())
	e := New(obs.NewManualClock(domain.SeedTime), nil, nil, nil, WithFX(money.FXTable{
		Base:  money.USD,
		Rates: map[money.Currency]money.Ratio{money.GBP: {Num: 127, Den: 100}},
	}))

	_, err := e.CollateralValue(context.Background(), snap, "CP-BRAVO")
	if err == nil {
		t.Fatal("valuing a EUR pledge with no EUR rate succeeded; want an error")
	}
	if got := err.Error(); got == "" || !contains(got, "no fx rate for EUR") {
		t.Fatalf("err = %v, want it to name the missing EUR rate", err)
	}

	// A counterparty holding only USD is unaffected: the table is only
	// consulted for the currencies actually held.
	if _, err := e.CollateralValue(context.Background(), snap, "CP-ALPHA"); err != nil {
		t.Fatalf("USD-only counterparty: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestIneligibleCollateral checks that an asset the schedule refuses is held at
// market value but lends nothing, and that an asset the schedule has never
// heard of is treated the same way rather than as unhaircut.
func TestIneligibleCollateral(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*domain.Snapshot)
	}{
		{
			name: "rule marks the class ineligible",
			mutate: func(s *domain.Snapshot) {
				rules := append([]domain.HaircutRule(nil), s.Haircuts.Rules...)
				for i := range rules {
					if rules[i].Class == domain.ClassCorpBond && rules[i].Rating == "A" {
						rules[i].Eligible = false
					}
				}
				s.Haircuts.Rules = rules
			},
		},
		{
			name: "no rule at all for the class",
			mutate: func(s *domain.Snapshot) {
				var rules []domain.HaircutRule
				for _, r := range s.Haircuts.Rules {
					if r.Class == domain.ClassCorpBond {
						continue
					}
					rules = append(rules, r)
				}
				s.Haircuts.Rules = rules
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, snap := newTestEngine(t)
			tc.mutate(&snap)

			cv, err := e.CollateralValue(context.Background(), snap, "CP-ALPHA")
			if err != nil {
				t.Fatalf("CollateralValue: %v", err)
			}
			// Market value is unchanged: we still hold the bond.
			if want := mustUSD(t, "60238750.00"); !cv.MarketValue.Equal(want) {
				t.Errorf("market = %s, want %s", cv.MarketValue, want)
			}
			// CORP-JPM's 8,808,750.00 no longer lends: 59,193,537.50 - 8,192,137.50.
			if want := mustUSD(t, "51001400.00"); !cv.CollateralValue.Equal(want) {
				t.Errorf("collateral = %s, want %s", cv.CollateralValue, want)
			}
			if want := mustUSD(t, "8808750.00"); !cv.IneligibleValue.Equal(want) {
				t.Errorf("ineligible = %s, want %s", cv.IneligibleValue, want)
			}
			for _, l := range cv.Lines {
				if l.Symbol != "CORP-JPM" {
					continue
				}
				if l.Eligible {
					t.Error("CORP-JPM still eligible")
				}
				if !l.Collateral.IsZero() {
					t.Errorf("ineligible line lends %s, want zero", l.Collateral)
				}
				if l.Market.IsZero() {
					t.Error("ineligible line lost its market value")
				}
			}
		})
	}
}

// TestNettingOnOff is the same book read twice: once with an enforceable
// agreement and once without. CP-ALPHA owes 48,000,000 on a repo and is owed
// 6,500,000 on a derivative, both in NS-ALPHA-1.
func TestNettingOnOff(t *testing.T) {
	for _, tc := range []struct {
		name        string
		netting     bool
		wantGross   string
		wantNet     string
		wantEnforce bool
	}{
		{"netting agreement in place", true, "48000000.00", "41500000.00", true},
		{"no netting agreement", false, "48000000.00", "48000000.00", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, snap := newTestEngine(t)
			cps := append([]domain.Counterparty(nil), snap.Counterparties...)
			for i := range cps {
				if cps[i].ID == "CP-ALPHA" {
					cps[i].Netting = tc.netting
				}
			}
			snap.Counterparties = cps

			x, err := e.Exposure(context.Background(), snap, "CP-ALPHA")
			if err != nil {
				t.Fatalf("Exposure: %v", err)
			}
			if want := mustUSD(t, tc.wantGross); !x.GrossExposure.Equal(want) {
				t.Errorf("gross = %s, want %s", x.GrossExposure, want)
			}
			if want := mustUSD(t, tc.wantNet); !x.NetExposure.Equal(want) {
				t.Errorf("net = %s, want %s", x.NetExposure, want)
			}
			if len(x.NettingSets) != 1 {
				t.Fatalf("netting sets = %d, want 1", len(x.NettingSets))
			}
			ns := x.NettingSets[0]
			if ns.ID != "NS-ALPHA-1" || ns.Count != 2 {
				t.Errorf("set = %+v, want NS-ALPHA-1 with 2 obligations", ns)
			}
			if ns.Enforced != tc.wantEnforce {
				t.Errorf("enforced = %v, want %v", ns.Enforced, tc.wantEnforce)
			}
			// The arithmetic net is reported either way, so the value of an
			// agreement is visible even where there is not one.
			if want := mustUSD(t, "41500000.00"); !ns.Net.Equal(want) {
				t.Errorf("set net = %s, want %s", ns.Net, want)
			}
		})
	}
}

// TestExposureTable pins the exposure, the collateral shortfall and the effect
// of the agreement's threshold and minimum transfer amount.
func TestExposureTable(t *testing.T) {
	e, snap := newTestEngine(t)
	for _, tc := range []struct {
		cp                                       string
		gross, net, uncovered, shortfall, excess string
		covered                                  bool
	}{
		{"CP-ALPHA", "48000000.00", "41500000.00", "0.00", "0.00", "17693537.50", true},
		{"CP-BRAVO", "70000000.00", "58000000.00", "0.00", "0.00", "39259745.20", true},
		{"CP-CHARLIE", "36000000.00", "36000000.00", "0.00", "0.00", "4734575.00", true},
		// 45,500,000.00 - 11,464,600.00 = 34,035,400.00 uncovered; less the
		// 500,000.00 threshold, and comfortably over the 250,000.00 MTA.
		{"CP-DELTA", "45500000.00", "45500000.00", "34035400.00", "33535400.00", "0.00", false},
		// Two netting sets, both positive, so netting changes nothing here.
		{"CP-ECHO", "33000000.00", "33000000.00", "0.00", "0.00", "2695000.00", true},
		{"CP-FOXTROT", "28750000.00", "28750000.00", "1279020.00", "779020.00", "0.00", false},
		{"CP-GOLF", "58000000.00", "58000000.00", "0.00", "0.00", "30332450.00", true},
		{"CP-HOTEL", "0.00", "0.00", "0.00", "0.00", "0.00", true},
	} {
		t.Run(tc.cp, func(t *testing.T) {
			x, err := e.Exposure(context.Background(), snap, tc.cp)
			if err != nil {
				t.Fatalf("Exposure: %v", err)
			}
			for _, f := range []struct {
				name string
				got  money.Amount
				want string
			}{
				{"gross", x.GrossExposure, tc.gross},
				{"net", x.NetExposure, tc.net},
				{"uncovered", x.Uncovered, tc.uncovered},
				{"shortfall", x.Shortfall, tc.shortfall},
				{"excess", x.Excess, tc.excess},
			} {
				if want := mustUSD(t, f.want); !f.got.Equal(want) {
					t.Errorf("%s = %s, want %s", f.name, f.got, want)
				}
			}
			if x.Covered != tc.covered {
				t.Errorf("covered = %v, want %v", x.Covered, tc.covered)
			}
		})
	}
}

// TestThresholdAndMinimumTransfer isolates the two operational parameters that
// decide whether an uncovered number is actually callable.
func TestThresholdAndMinimumTransfer(t *testing.T) {
	for _, tc := range []struct {
		name           string
		threshold, mta string
		wantShortfall  string
	}{
		// CP-FOXTROT is uncovered by 1,279,020.00.
		{"default terms", "500000.00", "250000.00", "779020.00"},
		{"no threshold, no MTA", "0.00", "0.00", "1279020.00"},
		{"threshold swallows the call", "2000000.00", "0.00", "0.00"},
		// Uncovered less threshold is 279,020.00, below the 300,000.00 MTA.
		{"MTA suppresses the call", "1000000.00", "300000.00", "0.00"},
		// Exactly at the MTA the call stands: the agreement says "at least".
		{"exactly at the MTA", "1000000.00", "279020.00", "279020.00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, snap := newTestEngine(t, WithTerms(Terms{
				Threshold:       mustUSD(t, tc.threshold),
				MinimumTransfer: mustUSD(t, tc.mta),
			}))
			x, err := e.Exposure(context.Background(), snap, "CP-FOXTROT")
			if err != nil {
				t.Fatalf("Exposure: %v", err)
			}
			if want := mustUSD(t, tc.wantShortfall); !x.Shortfall.Equal(want) {
				t.Errorf("shortfall = %s, want %s", x.Shortfall, want)
			}
		})
	}
}

func TestExposureAllOrderedAndComplete(t *testing.T) {
	e, snap := newTestEngine(t)
	xs, err := e.ExposureAll(context.Background(), snap)
	if err != nil {
		t.Fatalf("ExposureAll: %v", err)
	}
	if len(xs) != len(snap.Counterparties) {
		t.Fatalf("got %d exposures, want %d", len(xs), len(snap.Counterparties))
	}
	for i := 1; i < len(xs); i++ {
		if xs[i-1].Counterparty >= xs[i].Counterparty {
			t.Fatalf("results not sorted: %s then %s", xs[i-1].Counterparty, xs[i].Counterparty)
		}
	}
	// The pooled path and the single path must agree exactly.
	for _, x := range xs {
		one, err := e.Exposure(context.Background(), snap, x.Counterparty)
		if err != nil {
			t.Fatalf("Exposure(%s): %v", x.Counterparty, err)
		}
		if !one.Uncovered.Equal(x.Uncovered) || !one.NetExposure.Equal(x.NetExposure) {
			t.Errorf("%s: pooled %s/%s != single %s/%s", x.Counterparty,
				x.NetExposure, x.Uncovered, one.NetExposure, one.Uncovered)
		}
	}
}

// TestConcentrationSharesSumToOne checks the shares exactly, not to within a
// tolerance: they are rationals over one denominator, so they either add to one
// or the arithmetic is wrong.
func TestConcentrationSharesSumToOne(t *testing.T) {
	e, snap := newTestEngine(t)
	cs, err := e.Concentration(context.Background(), snap)
	if err != nil {
		t.Fatalf("Concentration: %v", err)
	}
	if len(cs) == 0 {
		t.Fatal("no concentration buckets")
	}

	byDim := map[string][]domain.Concentration{}
	for _, c := range cs {
		byDim[c.Dimension] = append(byDim[c.Dimension], c)
	}
	for _, dim := range []string{DimensionIssuer, DimensionClass, DimensionSector, DimensionLedger} {
		buckets := byDim[dim]
		if len(buckets) == 0 {
			t.Errorf("%s: no buckets", dim)
			continue
		}
		sum := big.NewRat(0, 1)
		total := money.Zero(money.USD)
		for _, c := range buckets {
			if c.Share.Den == 0 {
				t.Fatalf("%s/%s: invalid share %v", dim, c.Bucket, c.Share)
			}
			sum.Add(sum, big.NewRat(c.Share.Num, c.Share.Den))
			total = total.MustAdd(c.Value)
		}
		if sum.Cmp(big.NewRat(1, 1)) != 0 {
			t.Errorf("%s: shares sum to %s, want exactly 1", dim, sum.RatString())
		}
		if want := mustUSD(t, "360150887.70"); !total.Equal(want) {
			t.Errorf("%s: values sum to %s, want %s", dim, total, want)
		}
	}
}

func TestConcentrationKnownBuckets(t *testing.T) {
	e, snap := newTestEngine(t)
	cs, err := e.Concentration(context.Background(), snap)
	if err != nil {
		t.Fatalf("Concentration: %v", err)
	}
	got := map[string]domain.Concentration{}
	for _, c := range cs {
		got[c.Dimension+"/"+c.Bucket] = c
	}
	for _, tc := range []struct {
		key      string
		value    string
		breached bool
	}{
		// Every fund share and every cash balance is issued by the institution.
		{DimensionIssuer + "/SELF", "246337500.00", true},
		{DimensionIssuer + "/US-TREASURY", "54997600.00", false},
		{DimensionIssuer + "/DE-BUND", "8390995.20", false},
		{DimensionClass + "/equity", "22648080.00", false},
		{DimensionLedger + "/evm", "86937500.00", false},
		{DimensionSector + "/sovereign", "63388595.20", false},
	} {
		c, ok := got[tc.key]
		if !ok {
			t.Errorf("%s: missing", tc.key)
			continue
		}
		if want := mustUSD(t, tc.value); !c.Value.Equal(want) {
			t.Errorf("%s value = %s, want %s", tc.key, c.Value, want)
		}
		if c.Breached != tc.breached {
			t.Errorf("%s breached = %v, want %v", tc.key, c.Breached, tc.breached)
		}
	}
}

func TestCheckLimitsSeed(t *testing.T) {
	e, snap := newTestEngine(t)
	bs, err := e.CheckLimits(context.Background(), snap)
	if err != nil {
		t.Fatalf("CheckLimits: %v", err)
	}
	want := map[string]domain.BreachSeverity{
		// 34,035,400.00 uncovered against a 20,000,000.00 limit: 170%.
		"BRC-LIM-CP-EXP-CP-DELTA": domain.BreachCritical,
		// 68.40% of collateral issued by SELF against a 35% limit: 195%.
		"BRC-LIM-ISSUER-CONC-SELF": domain.BreachCritical,
	}
	if len(bs) != len(want) {
		t.Fatalf("got %d breaches, want %d: %+v", len(bs), len(want), bs)
	}
	for _, b := range bs {
		sev, ok := want[b.ID]
		if !ok {
			t.Errorf("unexpected breach %s", b.ID)
			continue
		}
		if b.Severity != sev {
			t.Errorf("%s severity = %s, want %s", b.ID, b.Severity, sev)
		}
		if b.Status != domain.BreachOpen {
			t.Errorf("%s status = %s, want open", b.ID, b.Status)
		}
		if !b.DetectedAt.Equal(domain.SeedTime) {
			t.Errorf("%s detected at %s, want the engine clock %s", b.ID, b.DetectedAt, domain.SeedTime)
		}
	}
	// Worst first.
	for i := 1; i < len(bs); i++ {
		if breachRank[bs[i-1].Severity] < breachRank[bs[i].Severity] {
			t.Errorf("breaches not worst-first at %d", i)
		}
	}
}

// TestBreachSeverityBoundaries sits the utilisation exactly on each boundary.
//
// CP-FOXTROT is uncovered by 1,279,020.00, which divides exactly by both the
// 80% warning level (1,598,775.00) and the 1.5x critical level (852,680.00), so
// the boundaries are reachable without rounding. Each case is then repeated one
// micro-unit — 1e-6 — the other side of the line. The contract is that a limit
// exactly reached is breached and a warning level exactly reached warns.
func TestBreachSeverityBoundaries(t *testing.T) {
	const uncoveredUnits int64 = 1_279_020_000_000 // 1,279,020.00 in 1e-6 units

	usd := func(units int64) money.Amount { return money.FromUnits(money.USD, units) }

	for _, tc := range []struct {
		name    string
		limit   money.Amount
		warnAt  money.Ratio
		want    domain.BreachSeverity
		wantAny bool
	}{
		{"far inside the limit", usd(uncoveredUnits * 4), money.Percent(80), "", false},
		{"one micro-unit below the warning level", usd(1_598_775_000_001), money.Percent(80), "", false},
		{"exactly at the warning level", usd(1_598_775_000_000), money.Percent(80), domain.BreachWarn, true},
		{"inside the warning band", usd(1_400_000_000_000), money.Percent(80), domain.BreachWarn, true},
		{"one micro-unit above the limit", usd(uncoveredUnits + 1), money.Percent(80), domain.BreachWarn, true},
		{"exactly at the limit", usd(uncoveredUnits), money.Percent(80), domain.BreachHard, true},
		{"one micro-unit below the limit", usd(uncoveredUnits - 1), money.Percent(80), domain.BreachHard, true},
		{"one micro-unit short of critical", usd(852_680_000_001), money.Percent(80), domain.BreachHard, true},
		{"exactly at critical", usd(852_680_000_000), money.Percent(80), domain.BreachCritical, true},
		{"far past critical", usd(uncoveredUnits / 2), money.Percent(80), domain.BreachCritical, true},
		{"no warning level configured", usd(uncoveredUnits * 2), money.Ratio{}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, snap := newTestEngine(t)
			x, err := e.Exposure(context.Background(), snap, "CP-FOXTROT")
			if err != nil {
				t.Fatalf("Exposure: %v", err)
			}
			if x.Uncovered.Units != uncoveredUnits {
				t.Fatalf("fixture drifted: uncovered = %s", x.Uncovered)
			}

			snap.Limits = []domain.Limit{{
				ID: "LIM-T", Kind: domain.LimitCounterparty, Scope: "CP-FOXTROT",
				Amount: tc.limit, WarnAt: tc.warnAt,
				Owner: "risk-manager", Effective: domain.SeedTime.Add(-24 * time.Hour),
			}}
			bs, err := e.CheckLimits(context.Background(), snap)
			if err != nil {
				t.Fatalf("CheckLimits: %v", err)
			}
			if !tc.wantAny {
				if len(bs) != 0 {
					t.Fatalf("got %d breaches, want none: %+v", len(bs), bs)
				}
				return
			}
			if len(bs) != 1 {
				t.Fatalf("got %d breaches, want 1: %+v", len(bs), bs)
			}
			if bs[0].Severity != tc.want {
				t.Errorf("severity = %s, want %s (limit %s, observed %s)",
					bs[0].Severity, tc.want, tc.limit, x.Uncovered)
			}
		})
	}
}

// TestAggregateUncoveredLimit exercises the one limit kind the seed does not
// breach, by tightening it until it does.
func TestAggregateUncoveredLimit(t *testing.T) {
	// 34,035,400.00 (Delta) + 1,279,020.00 (Foxtrot) = 35,314,420.00.
	const aggregate = "35314420.00"

	e, snap := newTestEngine(t)
	xs, err := e.ExposureAll(context.Background(), snap)
	if err != nil {
		t.Fatalf("ExposureAll: %v", err)
	}
	total := money.Zero(money.USD)
	for _, x := range xs {
		total = total.MustAdd(x.Uncovered)
	}
	if want := mustUSD(t, aggregate); !total.Equal(want) {
		t.Fatalf("aggregate uncovered = %s, want %s", total, want)
	}

	snap.Limits = []domain.Limit{{
		ID: "LIM-AGG", Kind: domain.LimitUncoveredAggr, Scope: "*",
		Amount: mustUSD(t, "30000000.00"), WarnAt: money.Percent(80),
		Owner: "treasurer", Effective: domain.SeedTime.Add(-24 * time.Hour),
	}}
	bs, err := e.CheckLimits(context.Background(), snap)
	if err != nil {
		t.Fatalf("CheckLimits: %v", err)
	}
	if len(bs) != 1 {
		t.Fatalf("got %d breaches, want 1: %+v", len(bs), bs)
	}
	if bs[0].ID != "BRC-LIM-AGG-*" || bs[0].Severity != domain.BreachHard {
		t.Errorf("breach = %s/%s, want BRC-LIM-AGG-*/breach", bs[0].ID, bs[0].Severity)
	}
	if !bs[0].Observed.Equal(total) {
		t.Errorf("observed = %s, want %s", bs[0].Observed, total)
	}
}

// TestLimitNotYetEffective checks a limit dated after the snapshot is ignored
// rather than applied retrospectively.
func TestLimitNotYetEffective(t *testing.T) {
	e, snap := newTestEngine(t)
	snap.Limits = []domain.Limit{{
		ID: "LIM-FUTURE", Kind: domain.LimitCounterparty, Scope: "*",
		Amount: mustUSD(t, "1.00"), WarnAt: money.Percent(50),
		Effective: domain.SeedTime.Add(24 * time.Hour),
	}}
	bs, err := e.CheckLimits(context.Background(), snap)
	if err != nil {
		t.Fatalf("CheckLimits: %v", err)
	}
	if len(bs) != 0 {
		t.Fatalf("a limit effective tomorrow produced %d breaches today", len(bs))
	}
}

// TestDeterminism runs the whole engine twice over the same snapshot and
// compares the JSON byte for byte. Anything that leaked a map iteration or a
// wall clock into a result shows up here.
func TestDeterminism(t *testing.T) {
	run := func() []byte {
		e, snap := newTestEngine(t)
		ctx := context.Background()
		xs, err := e.ExposureAll(ctx, snap)
		if err != nil {
			t.Fatalf("ExposureAll: %v", err)
		}
		cs, err := e.Concentration(ctx, snap)
		if err != nil {
			t.Fatalf("Concentration: %v", err)
		}
		bs, err := e.CheckLimits(ctx, snap)
		if err != nil {
			t.Fatalf("CheckLimits: %v", err)
		}
		ss, err := e.StressAll(ctx, snap, domain.NewSeed().Scenarios)
		if err != nil {
			t.Fatalf("StressAll: %v", err)
		}
		var cvs []domain.CollateralValue
		for _, cp := range counterpartyIDs(snap) {
			cv, err := e.CollateralValue(ctx, snap, cp)
			if err != nil {
				t.Fatalf("CollateralValue: %v", err)
			}
			cvs = append(cvs, cv)
		}
		b, err := json.Marshal(map[string]any{
			"collateral": cvs, "exposures": xs, "concentration": cs,
			"breaches": bs, "stress": ss,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}
	first := run()
	for i := 0; i < 3; i++ {
		if got := run(); string(got) != string(first) {
			t.Fatalf("run %d differs from run 0 (%d vs %d bytes)", i+1, len(got), len(first))
		}
	}
}

// TestDeterminismUnderVaryingWorkerCounts checks that the fan-out is not what
// makes the answer stable: one worker and sixteen must produce the same bytes.
func TestDeterminismUnderVaryingWorkerCounts(t *testing.T) {
	snap := domain.SnapshotFromSeed(domain.NewSeed())
	var first []byte
	for _, workers := range []int{1, 2, 3, 8, 16} {
		e := New(obs.NewManualClock(domain.SeedTime), nil, nil, nil, WithWorkers(workers))
		xs, err := e.ExposureAll(context.Background(), snap)
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		b, err := json.Marshal(xs)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = b
			continue
		}
		if string(b) != string(first) {
			t.Fatalf("workers=%d produced different results", workers)
		}
	}
}

func TestWidenHaircut(t *testing.T) {
	for _, tc := range []struct {
		name     string
		h, addOn money.Ratio
		wantNum  int64
		wantDen  int64
	}{
		{"no add-on", money.Bps(50), money.Ratio{}, 1, 200},
		{"50bp plus 100bp", money.Bps(50), money.Bps(100), 3, 200},
		{"200bp plus 500bp", money.Bps(200), money.Bps(500), 7, 100},
		{"clamped at one", money.Percent(95), money.Percent(20), 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := reduce(widen(tc.h, tc.addOn))
			if got.Num != tc.wantNum || got.Den != tc.wantDen {
				t.Errorf("widen = %d/%d, want %d/%d", got.Num, got.Den, tc.wantNum, tc.wantDen)
			}
		})
	}
}

func TestMetricsRecorded(t *testing.T) {
	reg := obs.NewRegistry()
	snap := domain.SnapshotFromSeed(domain.NewSeed())
	e := New(obs.NewManualClock(domain.SeedTime), nil, reg, nil)

	if _, err := e.CollateralValue(context.Background(), snap, "CP-ALPHA"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CollateralValue(context.Background(), snap, "CP-NOBODY"); err == nil {
		t.Fatal("want an error for an unknown counterparty")
	}
	if got := e.Metrics().Calculations.Value(); got != 2 {
		t.Errorf("calculations = %v, want 2", got)
	}
	if got := e.Metrics().Errors.Value(); got != 1 {
		t.Errorf("errors = %v, want 1", got)
	}
	if got := e.Metrics().CalculationLatency("collateral_value").Count(); got != 2 {
		t.Errorf("latency observations = %d, want 2", got)
	}
	out := reg.Render()
	for _, want := range []string{MetricCalculations, MetricCalcErrors, MetricCalcSeconds} {
		if !contains(out, want) {
			t.Errorf("registry render missing %s", want)
		}
	}
}
