package api

import (
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/money"
)

// The wire shapes below exist because money.Amount marshals as exact minor
// units and money.Ratio as a fraction, which is right for a machine and
// unreadable in a browser. Every figure therefore travels twice: the exact
// value a client should compute with, and the rendered string the platform
// itself would print. The dashboard shows the second and never re-formats the
// first, so a number on the screen is the number the risk engine produced.

// Amount is a monetary figure on the wire.
type Amount struct {
	// Currency is the ISO code.
	Currency string `json:"currency"`
	// Units is the exact value in 1e-6 of Currency.
	Units int64 `json:"units"`
	// Value is Units expressed in whole currency units, for charting only.
	Value float64 `json:"value"`
	// Display is the platform's own rendering, rounded half-even to two places.
	Display string `json:"display"`
}

func amt(a money.Amount) Amount {
	return Amount{
		Currency: string(a.Ccy),
		Units:    a.Units,
		Value:    float64(a.Units) / 1e6,
		Display:  a.Display(),
	}
}

// Ratio is a fraction on the wire.
type Ratio struct {
	// Num and Den are the exact fraction.
	Num int64 `json:"num"`
	Den int64 `json:"den"`
	// Percent is the fraction as a percentage, for charting only.
	Percent float64 `json:"percent"`
	// Display is the percentage rendered to two places, or "-" when undefined.
	Display string `json:"display"`
}

func rat(r money.Ratio) Ratio {
	out := Ratio{Num: r.Num, Den: r.Den, Display: "-"}
	if r.Den != 0 {
		out.Percent = r.Float() * 100
		out.Display = formatPercent(out.Percent)
	}
	return out
}

func formatPercent(p float64) string {
	return trimFloat(p) + "%"
}

// trimFloat renders a float to two decimal places without importing a
// formatter for one call site.
func trimFloat(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	hundredths := int64(v*100 + 0.5)
	whole, frac := hundredths/100, hundredths%100
	s := itoa(whole) + "." + pad2(frac)
	if neg {
		return "-" + s
	}
	return s
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func pad2(v int64) string {
	if v < 10 {
		return "0" + itoa(v)
	}
	return itoa(v)
}

// ExposureView is one counterparty's exposure as the API reports it.
type ExposureView struct {
	Counterparty string    `json:"counterparty"`
	Name         string    `json:"name,omitempty"`
	AsOf         time.Time `json:"as_of"`
	Gross        Amount    `json:"gross_exposure"`
	Net          Amount    `json:"net_exposure"`
	Collateral   Amount    `json:"collateral_value"`
	Uncovered    Amount    `json:"uncovered_exposure"`
	Shortfall    Amount    `json:"margin_shortfall"`
	Excess       Amount    `json:"margin_excess"`
	Threshold    Amount    `json:"threshold"`
	MinTransfer  Amount    `json:"minimum_transfer"`
	Covered      bool      `json:"covered"`
	NettingSets  []NetView `json:"netting_sets,omitempty"`
}

// NetView is one netting set inside an exposure.
type NetView struct {
	ID       string `json:"id"`
	Gross    Amount `json:"gross"`
	Net      Amount `json:"net"`
	Count    int    `json:"count"`
	Enforced bool   `json:"enforceable"`
}

func exposureView(x domain.Exposure, name string) ExposureView {
	out := ExposureView{
		Counterparty: x.Counterparty, Name: name, AsOf: x.AsOf,
		Gross: amt(x.GrossExposure), Net: amt(x.NetExposure),
		Collateral: amt(x.Collateral), Uncovered: amt(x.Uncovered),
		Shortfall: amt(x.Shortfall), Excess: amt(x.Excess),
		Threshold: amt(x.Threshold), MinTransfer: amt(x.MinimumTransfer),
		Covered: x.Covered,
	}
	for _, ns := range x.NettingSets {
		out.NettingSets = append(out.NettingSets, NetView{
			ID: ns.ID, Gross: amt(ns.Gross), Net: amt(ns.Net),
			Count: ns.Count, Enforced: ns.Enforced,
		})
	}
	return out
}

// CollateralLineView is one instrument's contribution to a pledge.
type CollateralLineView struct {
	Symbol      string `json:"symbol"`
	Class       string `json:"class"`
	Ledger      string `json:"ledger"`
	Quantity    int64  `json:"quantity"`
	Price       Amount `json:"price"`
	Market      Amount `json:"market_value"`
	Haircut     Ratio  `json:"haircut"`
	AdvanceRate Ratio  `json:"advance_rate"`
	Collateral  Amount `json:"collateral_value"`
	Eligible    bool   `json:"eligible"`
}

// CollateralView is a counterparty's pledge, line by line.
type CollateralView struct {
	Counterparty   string               `json:"counterparty"`
	AsOf           time.Time            `json:"as_of"`
	PriceAsOf      time.Time            `json:"price_as_of,omitzero"`
	HaircutVersion string               `json:"haircut_version"`
	Lines          []CollateralLineView `json:"lines"`
	MarketValue    Amount               `json:"market_value"`
	Collateral     Amount               `json:"collateral_value"`
	Ineligible     Amount               `json:"ineligible_value"`
}

func collateralView(cv domain.CollateralValue) CollateralView {
	out := CollateralView{
		Counterparty: cv.Counterparty, AsOf: cv.AsOf, PriceAsOf: cv.PriceAsOf,
		HaircutVersion: cv.HaircutVersion, Lines: []CollateralLineView{},
		MarketValue: amt(cv.MarketValue), Collateral: amt(cv.CollateralValue),
		Ineligible: amt(cv.IneligibleValue),
	}
	for _, l := range cv.Lines {
		out.Lines = append(out.Lines, CollateralLineView{
			Symbol: l.Symbol, Class: string(l.Class), Ledger: string(l.Ledger),
			Quantity: l.Quantity, Price: amt(l.Price), Market: amt(l.Market),
			Haircut: rat(l.Haircut), AdvanceRate: rat(l.AdvanceRate),
			Collateral: amt(l.Collateral), Eligible: l.Eligible,
		})
	}
	return out
}

// ConcentrationView is one slice of the portfolio measured against its limit.
type ConcentrationView struct {
	Dimension string `json:"dimension"`
	Bucket    string `json:"bucket"`
	Value     Amount `json:"value"`
	Share     Ratio  `json:"share"`
	Limit     Ratio  `json:"limit"`
	Breached  bool   `json:"breached"`
}

func concentrationView(c domain.Concentration) ConcentrationView {
	return ConcentrationView{
		Dimension: c.Dimension, Bucket: c.Bucket, Value: amt(c.Value),
		Share: rat(c.Share), Limit: rat(c.LimitPc), Breached: c.Breached,
	}
}

// BreachView is one limit exceeded.
type BreachView struct {
	ID          string    `json:"id"`
	LimitID     string    `json:"limit_id"`
	Kind        string    `json:"kind"`
	Scope       string    `json:"scope"`
	Severity    string    `json:"severity"`
	Status      string    `json:"status"`
	Observed    Amount    `json:"observed"`
	Limit       Amount    `json:"limit"`
	ObservedPct Ratio     `json:"observed_pct"`
	LimitPct    Ratio     `json:"limit_pct"`
	Utilisation Ratio     `json:"utilisation"`
	DetectedAt  time.Time `json:"detected_at"`
	AsOf        time.Time `json:"as_of"`
	TraceID     string    `json:"trace_id,omitempty"`
	Notes       []string  `json:"notes,omitempty"`
}

func breachView(b domain.Breach) BreachView {
	status := string(b.Status)
	if status == "" {
		status = string(domain.BreachOpen)
	}
	out := BreachView{
		ID: b.ID, LimitID: b.LimitID, Kind: string(b.Kind), Scope: b.Scope,
		Severity: string(b.Severity), Status: status,
		Observed: amt(b.Observed), Limit: amt(b.Limit),
		ObservedPct: rat(b.ObservedPc), LimitPct: rat(b.LimitPc),
		DetectedAt: b.DetectedAt, AsOf: b.AsOf, TraceID: b.TraceID, Notes: b.Notes,
	}
	// Utilisation is observed over limit, in whichever of the two forms the
	// limit was written. It is derived here rather than in the risk engine
	// because it is a presentation figure and nothing decides on it.
	switch {
	case b.ObservedPc.Den != 0 && b.LimitPc.Den != 0 && b.LimitPc.Num != 0:
		out.Utilisation = rat(money.Ratio{
			Num: b.ObservedPc.Num * b.LimitPc.Den,
			Den: b.ObservedPc.Den * b.LimitPc.Num,
		})
	case b.Limit.Units != 0:
		out.Utilisation = rat(money.Ratio{Num: b.Observed.Units, Den: b.Limit.Units})
	}
	return out
}

// BreakView is one reconciliation break.
type BreakView struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Counterparty string    `json:"counterparty,omitempty"`
	Symbol       string    `json:"symbol,omitempty"`
	Ledger       string    `json:"ledger,omitempty"`
	BookQty      int64     `json:"book_qty"`
	LedgerQty    int64     `json:"ledger_qty"`
	Delta        int64     `json:"delta"`
	Status       string    `json:"status"`
	Severity     string    `json:"severity"`
	DetectedAt   time.Time `json:"detected_at"`
	AsOf         time.Time `json:"as_of"`
	AgeMinutes   float64   `json:"age_minutes"`
	Assignee     string    `json:"assignee,omitempty"`
	Resolution   string    `json:"resolution,omitempty"`
	ResolvedAt   time.Time `json:"resolved_at,omitzero"`
	Notes        []string  `json:"notes,omitempty"`
}

func breakView(b domain.Break) BreakView {
	return BreakView{
		ID: b.ID, Kind: string(b.Kind), Counterparty: b.Counterparty, Symbol: b.Symbol,
		Ledger: string(b.Ledger), BookQty: b.BookQty, LedgerQty: b.LedgerQty, Delta: b.Delta,
		Status: string(b.Status), Severity: b.Severity, DetectedAt: b.DetectedAt,
		AsOf: b.AsOf, AgeMinutes: b.AgeMinutes, Assignee: b.Assignee,
		Resolution: b.Resolution, ResolvedAt: b.ResolvedAt, Notes: b.Notes,
	}
}

// StressResultView is one counterparty's outcome under a scenario.
type StressResultView struct {
	Counterparty   string `json:"counterparty"`
	BaseCollateral Amount `json:"base_collateral"`
	Stressed       Amount `json:"stressed_collateral"`
	BaseUncovered  Amount `json:"base_uncovered"`
	StressedUncov  Amount `json:"stressed_uncovered"`
	Delta          Amount `json:"delta"`
	NewShortfall   Amount `json:"new_shortfall"`
	BreachesLimit  bool   `json:"breaches_limit"`
}

// StressView is one scenario run.
type StressView struct {
	Scenario       string             `json:"scenario"`
	Name           string             `json:"name,omitempty"`
	Description    string             `json:"description,omitempty"`
	AsOf           time.Time          `json:"as_of"`
	Counterparties int                `json:"counterparties"`
	DurationMS     float64            `json:"duration_ms"`
	TotalBase      Amount             `json:"total_base_collateral"`
	TotalStressed  Amount             `json:"total_stressed_collateral"`
	TotalShortfall Amount             `json:"total_new_shortfall"`
	Change         Amount             `json:"change"`
	WorstCP        string             `json:"worst_counterparty,omitempty"`
	Results        []StressResultView `json:"results"`
}

func stressView(s domain.StressSummary, sc domain.StressScenario) StressView {
	change := s.TotalStressed
	if d, err := s.TotalStressed.Sub(s.TotalBase); err == nil {
		change = d
	}
	out := StressView{
		Scenario: s.Scenario, Name: sc.Name, Description: sc.Description,
		AsOf: s.AsOf, Counterparties: s.Counterparties, DurationMS: s.DurationMS,
		TotalBase: amt(s.TotalBase), TotalStressed: amt(s.TotalStressed),
		TotalShortfall: amt(s.TotalShortfall), Change: amt(change),
		WorstCP: s.WorstCP, Results: []StressResultView{},
	}
	for _, r := range s.Results {
		out.Results = append(out.Results, StressResultView{
			Counterparty: r.Counterparty, BaseCollateral: amt(r.BaseCollateral),
			Stressed: amt(r.StressedValue), BaseUncovered: amt(r.BaseUncovered),
			StressedUncov: amt(r.StressedUncov), Delta: amt(r.Delta),
			NewShortfall: amt(r.NewShortfall), BreachesLimit: r.BreachesLimit,
		})
	}
	return out
}
