package risk

import (
	"context"
	"strings"
	"testing"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/money"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// TestFormatCollateral pins the rendered block for the hand-computed
// counterparty, so a change in the layout is a change somebody chose.
func TestFormatCollateral(t *testing.T) {
	e, snap := newTestEngine(t)
	cv, err := e.CollateralValue(context.Background(), snap, "CP-ALPHA")
	if err != nil {
		t.Fatalf("CollateralValue: %v", err)
	}
	got := FormatCollateral(cv)
	want := `Collateral  CP-ALPHA  as of 2026-09-03T16:00:00Z  schedule haircuts-2026.09
  marks as of 2026-09-03T15:55:00Z
  SYMBOL    CLASS              LEDGER    QUANTITY      PRICE           MARKET  HAIRCUT  ADVANCE       COLLATERAL
  CORP-JPM  corporate_bond     golden      90,000  97.88 USD   8808750.00 USD    7.00%   93.00%   8192137.50 USD
  TMMF      money_market_fund  evm     40,000,000   1.00 USD  40000000.00 USD    0.50%   99.50%  39800000.00 USD
  UST-10Y   government_bond    golden     120,000  95.25 USD  11430000.00 USD    2.00%   98.00%  11201400.00 USD
  Market value      60238750.00 USD
  Collateral value  59193537.50 USD
`
	if got != want {
		t.Errorf("FormatCollateral:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestFormatExposure(t *testing.T) {
	e, snap := newTestEngine(t)
	x, err := e.Exposure(context.Background(), snap, "CP-DELTA")
	if err != nil {
		t.Fatalf("Exposure: %v", err)
	}
	got := FormatExposure(x)

	for _, want := range []string{
		"Exposure  CP-DELTA  as of 2026-09-03T16:00:00Z",
		"Gross exposure    45500000.00 USD",
		"Net exposure      45500000.00 USD",
		"Collateral        11464600.00 USD",
		"Uncovered         34035400.00 USD",
		"Threshold           500000.00 USD",
		"Minimum transfer    250000.00 USD",
		"MARGIN CALL       33535400.00 USD",
		"NS-DELTA-1",
		"enforceable",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Excess") {
		t.Errorf("an uncovered counterparty should not show an excess:\n%s", got)
	}

	// A covered counterparty shows the excess and says there is no call.
	covered, err := e.Exposure(context.Background(), snap, "CP-ALPHA")
	if err != nil {
		t.Fatalf("Exposure: %v", err)
	}
	out := FormatExposure(covered)
	for _, want := range []string{"Excess", "Margin call       none"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "MARGIN CALL") {
		t.Errorf("a covered counterparty should not show a call:\n%s", out)
	}
}

func TestFormatExposureNettingNotEnforceable(t *testing.T) {
	e, snap := newTestEngine(t)
	cps := append([]domain.Counterparty(nil), snap.Counterparties...)
	for i := range cps {
		if cps[i].ID == "CP-ALPHA" {
			cps[i].Netting = false
		}
	}
	snap.Counterparties = cps

	x, err := e.Exposure(context.Background(), snap, "CP-ALPHA")
	if err != nil {
		t.Fatalf("Exposure: %v", err)
	}
	got := FormatExposure(x)
	if !strings.Contains(got, "not enforceable") {
		t.Errorf("an unenforceable netting set is not marked:\n%s", got)
	}
	// The set's arithmetic net is still shown, so the value of an agreement is
	// legible even where there is not one.
	if !strings.Contains(got, "41500000.00 USD") {
		t.Errorf("the arithmetic net is missing:\n%s", got)
	}
}

func TestFormatStress(t *testing.T) {
	e, snap := newTestEngine(t)
	sc, ok := domain.NewSeed().Scenario("counterparty_default")
	if !ok {
		t.Fatal("no counterparty_default scenario")
	}
	sum, err := e.Stress(context.Background(), snap, sc)
	if err != nil {
		t.Fatalf("Stress: %v", err)
	}
	got := FormatStress(sum)

	for _, want := range []string{
		"Stress  counterparty_default  as of 2026-09-03T16:00:00Z  8 counterparties",
		"Base collateral      360150887.70 USD",
		// 335,134,323.305 rounds half-even to .30, not .31.
		"Stressed collateral  335134323.30 USD",
		"Worst counterparty   CP-FOXTROT",
		"(-6.95%)",
		"COUNTERPARTY",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Worst first: the counterparty with the largest new shortfall leads the
	// table, so the reader does not have to scan for it.
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	var header int
	for i, l := range lines {
		if strings.Contains(l, "COUNTERPARTY") {
			header = i
		}
	}
	if header == 0 || header+1 >= len(lines) {
		t.Fatalf("no table rows:\n%s", got)
	}
	if !strings.Contains(lines[header+1], sum.WorstCP) {
		t.Errorf("first row is %q, want the worst counterparty %s", lines[header+1], sum.WorstCP)
	}
}

func TestFormatBreaches(t *testing.T) {
	if got := FormatBreaches(nil); got != "Breaches  none: every limit observed\n" {
		t.Errorf("empty render = %q", got)
	}

	e, snap := newTestEngine(t)
	bs, err := e.CheckLimits(context.Background(), snap)
	if err != nil {
		t.Fatalf("CheckLimits: %v", err)
	}
	got := FormatBreaches(bs)
	for _, want := range []string{
		"Breaches  2  (critical 2, breach 0, warning 0)",
		"BRC-LIM-CP-EXP-CP-DELTA",
		"34035400.00 USD",
		"20000000.00 USD",
		"170.2%",
		"BRC-LIM-ISSUER-CONC-SELF",
		// A ratio limit renders as percentages on both sides.
		"68.40%",
		"35.00%",
		"195.4%",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatCollateralEmptyPledge(t *testing.T) {
	e, snap := newTestEngine(t)
	cv, err := e.CollateralValue(context.Background(), snap, "CP-HOTEL")
	if err != nil {
		t.Fatalf("CollateralValue: %v", err)
	}
	got := FormatCollateral(cv)
	if !strings.Contains(got, "no pledged positions") {
		t.Errorf("an empty pledge should say so:\n%s", got)
	}
}

func TestFormatCollateralIneligibleLine(t *testing.T) {
	e, snap := newTestEngine(t)
	rules := append([]domain.HaircutRule(nil), snap.Haircuts.Rules...)
	for i := range rules {
		if rules[i].Class == domain.ClassCorpBond && rules[i].Rating == "A" {
			rules[i].Eligible = false
		}
	}
	snap.Haircuts.Rules = rules

	cv, err := e.CollateralValue(context.Background(), snap, "CP-ALPHA")
	if err != nil {
		t.Fatalf("CollateralValue: %v", err)
	}
	got := FormatCollateral(cv)
	for _, want := range []string{"ineligible", "Ineligible         8808750.00 USD"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// TestFormatIsDeterministic renders the same inputs repeatedly and compares.
// The renderers are the platform's most-copied output; if one of them reordered
// a table between calls, a diff of two reports would be unreadable.
func TestFormatIsDeterministic(t *testing.T) {
	e, snap := newTestEngine(t)
	ctx := context.Background()

	cv, err := e.CollateralValue(ctx, snap, "CP-GOLF")
	if err != nil {
		t.Fatal(err)
	}
	x, err := e.Exposure(ctx, snap, "CP-GOLF")
	if err != nil {
		t.Fatal(err)
	}
	bs, err := e.CheckLimits(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := domain.NewSeed().Scenario("rate_shock")
	sum, err := e.Stress(ctx, snap, sc)
	if err != nil {
		t.Fatal(err)
	}

	for name, render := range map[string]func() string{
		"collateral": func() string { return FormatCollateral(cv) },
		"exposure":   func() string { return FormatExposure(x) },
		"breaches":   func() string { return FormatBreaches(bs) },
		"stress":     func() string { return FormatStress(sum) },
	} {
		first := render()
		for i := 0; i < 5; i++ {
			if got := render(); got != first {
				t.Errorf("%s render %d differs", name, i+1)
				break
			}
		}
	}
}

func TestFormatHelpers(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0", "0"}, {"7", "7"}, {"999", "999"}, {"1000", "1,000"},
		{"-1234567", "-1,234,567"}, {"1234567890", "1,234,567,890"},
	} {
		var v int64
		if _, err := fmtSscan(tc.in, &v); err != nil {
			t.Fatal(err)
		}
		if got := group(v); got != tc.want {
			t.Errorf("group(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
	if got := pct(money.Bps(250)); got != "2.50%" {
		t.Errorf("pct(250bp) = %s, want 2.50%%", got)
	}
	if got := pct(money.Ratio{}); got != "-" {
		t.Errorf("pct(invalid) = %s, want -", got)
	}
	if got := relative(money.FromInt(money.USD, -5), money.FromInt(money.USD, 100)); got != "-5.00%" {
		t.Errorf("relative = %s, want -5.00%%", got)
	}
	if got := relative(money.FromInt(money.USD, 5), money.Zero(money.USD)); got != "-" {
		t.Errorf("relative over a zero base = %s, want -", got)
	}
	if got := stamp(domain.SeedTime); got != "2026-09-03T16:00:00Z" {
		t.Errorf("stamp = %s", got)
	}
	if got := utilisation(domain.Breach{}); got != "-" {
		t.Errorf("utilisation of an empty breach = %s, want -", got)
	}
}

func TestSpansAreEmitted(t *testing.T) {
	exp := obs.NewMemoryExporter(0)
	clock := obs.NewManualClock(domain.SeedTime)
	tracer := obs.NewTracer("risk-test", clock, exp).Deterministic(true)
	snap := domain.SnapshotFromSeed(domain.NewSeed())
	e := New(clock, tracer, obs.NewRegistry(), nil)

	ctx := context.Background()
	if _, err := e.CollateralValue(ctx, snap, "CP-ALPHA"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Exposure(ctx, snap, "CP-ALPHA"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CheckLimits(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Stress(ctx, snap, domain.StressScenario{ID: "flat"}); err != nil {
		t.Fatal(err)
	}

	names := map[string]int{}
	for _, s := range exp.Spans() {
		names[s.Name]++
	}
	for _, want := range []string{
		"risk.collateral_value", "risk.exposure", "risk.exposure_all",
		"risk.concentration", "risk.check_limits", "risk.stress",
	} {
		if names[want] == 0 {
			t.Errorf("no %s span emitted; got %v", want, names)
		}
	}
}

// fmtSscan is a tiny wrapper so the helper table above reads as data.
func fmtSscan(s string, v *int64) (int, error) {
	var neg bool
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	var out int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errBadDigit
		}
		out = out*10 + int64(r-'0')
	}
	if neg {
		out = -out
	}
	*v = out
	return 1, nil
}

var errBadDigit = errString("bad digit")

type errString string

func (e errString) Error() string { return string(e) }
