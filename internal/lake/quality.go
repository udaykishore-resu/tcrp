package lake

import (
	"fmt"
	"sort"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
)

// Quality check identifiers. They are constants because a dashboard, a test and
// an incident review all have to name the same control.
const (
	CheckCompleteness = "dq.completeness.ledger_sequence"
	CheckUniqueness   = "dq.uniqueness.event_id"
	CheckTimeliness   = "dq.timeliness.observation_lag"
	CheckAccuracy     = "dq.accuracy.supply_identity"
	CheckReferential  = "dq.referential.known_holder"
	CheckFreshness    = "dq.freshness.prices"
)

// Thresholds parameterise the data-quality controls. The zero value is
// replaced by DefaultThresholds; a caller that wants different limits states
// them rather than editing a constant.
type Thresholds struct {
	// MaxObservationLag is how far behind occurrence an observation may be
	// before the stream is called untimely.
	MaxObservationLag time.Duration
	// MaxPriceAge is how stale a mark may be at the valid time.
	MaxPriceAge time.Duration
}

// DefaultThresholds are the platform's defaults: fifteen minutes of observation
// lag and an hour of price age. Both are well inside a daily risk cut and well
// outside an indexer's ordinary few seconds.
var DefaultThresholds = Thresholds{
	MaxObservationLag: 15 * time.Minute,
	MaxPriceAge:       time.Hour,
}

func (t Thresholds) resolve() Thresholds {
	if t.MaxObservationLag <= 0 {
		t.MaxObservationLag = DefaultThresholds.MaxObservationLag
	}
	if t.MaxPriceAge <= 0 {
		t.MaxPriceAge = DefaultThresholds.MaxPriceAge
	}
	return t
}

// QualityInputs is everything the controls read. It is a value rather than an
// interface so that a check is a pure function of data and can be tested
// without a lake.
type QualityInputs struct {
	ValidAt time.Time
	KnownAt time.Time
	// Events is the stream the point-in-time controls read. They see only what
	// was true by ValidAt and known by KnownAt.
	Events []domain.LedgerEvent
	// Landed is everything the platform has ingested, ignoring both clocks.
	// Completeness and uniqueness read it rather than Events, because whether
	// the stream has a hole in it is a property of the stream and not of a cut:
	// an event the platform has not observed yet is a timeliness finding, and
	// reporting it as a gap would fire on every correctly-handled late arrival.
	// An empty Landed falls back to Events.
	Landed         []domain.LedgerEvent
	Positions      []domain.Position
	Counterparties []domain.Counterparty
	Prices         []domain.Price
}

// RunQualityChecks runs every control and aggregates the verdict. A failing
// critical control makes the whole report fail, which is what a caller checks
// with QualityReport.Usable before acting on a snapshot; a failing
// non-critical control warns, because a stale mark is a reason to look, not a
// reason to stop.
func RunQualityChecks(in QualityInputs, th Thresholds, ranAt time.Time) domain.QualityReport {
	th = th.resolve()
	events := visibleEvents(in.Events, in.ValidAt, in.KnownAt)
	landed := in.Landed
	if len(landed) == 0 {
		landed = events
	}
	checks := []domain.QualityCheck{
		Completeness(landed),
		Uniqueness(landed),
		Timeliness(events, th.MaxObservationLag),
		Accuracy(events, in.Positions),
		ReferentialIntegrity(events, in.Counterparties),
		Freshness(in.Prices, in.ValidAt, th.MaxPriceAge),
	}
	report := domain.QualityReport{Status: domain.QualityPass, Checks: checks, RanAt: ranAt}
	for _, c := range checks {
		switch {
		case c.Status == domain.QualityFail:
			report.Status = domain.QualityFail
		case c.Status == domain.QualityWarn && report.Status == domain.QualityPass:
			report.Status = domain.QualityWarn
		}
	}
	return report
}

// visibleEvents narrows the stream to what was true by validAt and known by
// knownAt. Every control reads the same window, so a report is about one view
// of the world rather than six.
func visibleEvents(events []domain.LedgerEvent, validAt, knownAt time.Time) []domain.LedgerEvent {
	out := make([]domain.LedgerEvent, 0, len(events))
	for _, e := range events {
		if !validAt.IsZero() && e.OccurredAt.After(validAt) {
			continue
		}
		if !knownAt.IsZero() && e.ObservedAt.After(knownAt) {
			continue
		}
		out = append(out, e)
	}
	domain.SortEvents(out)
	return out
}

// Completeness checks that the ingest stream has no hole in it.
//
// It checks two things. Per ledger, block heights must not regress or repeat
// when the events are read in sequence order, which is how a missed reorg or a
// replayed range shows up. Globally, the sequence numbers must be contiguous,
// which is how a dropped batch shows up. Per-ledger block contiguity would be
// the stronger control against a real indexer; the platform's own sequence is
// a single cursor shared by both ledgers, so per-ledger heights are sparse by
// construction and a contiguity test on them would fire on clean data.
func Completeness(events []domain.LedgerEvent) domain.QualityCheck {
	c := domain.QualityCheck{ID: CheckCompleteness, Name: "Ledger sequence completeness", Critical: true}
	if len(events) == 0 {
		c.Status = domain.QualityPass
		c.Observed = "no events"
		return c
	}
	byLedger := map[domain.Ledger][]domain.LedgerEvent{}
	for _, e := range events {
		byLedger[e.Ledger] = append(byLedger[e.Ledger], e)
	}
	ledgers := make([]domain.Ledger, 0, len(byLedger))
	for l := range byLedger {
		ledgers = append(ledgers, l)
	}
	sort.Slice(ledgers, func(i, j int) bool { return ledgers[i] < ledgers[j] })

	var problems []string
	for _, l := range ledgers {
		es := byLedger[l]
		sort.Slice(es, func(i, j int) bool { return es[i].Seq < es[j].Seq })
		for i := 1; i < len(es); i++ {
			if es[i].Block <= es[i-1].Block {
				problems = append(problems, fmt.Sprintf("%s block %d follows %d at seq %d",
					l, es[i].Block, es[i-1].Block, es[i].Seq))
			}
		}
	}
	seqs := make([]uint64, 0, len(events))
	for _, e := range events {
		seqs = append(seqs, e.Seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	gaps := 0
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			gaps++
		}
	}
	if gaps > 0 {
		problems = append(problems, fmt.Sprintf("%d gap(s) in the global sequence %d..%d", gaps, seqs[0], seqs[len(seqs)-1]))
	}
	c.Expected = "monotonic block heights per ledger and a contiguous global sequence"
	if len(problems) == 0 {
		c.Status = domain.QualityPass
		c.Observed = fmt.Sprintf("%d events, sequence %d..%d", len(events), seqs[0], seqs[len(seqs)-1])
		return c
	}
	c.Status = domain.QualityFail
	c.Observed = fmt.Sprintf("%d problem(s)", len(problems))
	c.Detail = joinLimited(problems, 5)
	return c
}

// Uniqueness checks that no event arrived twice under the same natural key. A
// duplicate here would double-count a transfer, so it is critical.
func Uniqueness(events []domain.LedgerEvent) domain.QualityCheck {
	c := domain.QualityCheck{ID: CheckUniqueness, Name: "Event identity uniqueness", Critical: true}
	seen := map[string]int{}
	for _, e := range events {
		seen[e.ID()]++
	}
	var dupes []string
	for id, n := range seen {
		if n > 1 {
			dupes = append(dupes, fmt.Sprintf("%s seen %d times", id, n))
		}
	}
	sort.Strings(dupes)
	c.Expected = "one row per ledger:tx_hash:log_index"
	c.Observed = fmt.Sprintf("%d distinct ids over %d events", len(seen), len(events))
	if len(dupes) == 0 {
		c.Status = domain.QualityPass
		return c
	}
	c.Status = domain.QualityFail
	c.Detail = joinLimited(dupes, 5)
	return c
}

// Timeliness checks the worst observation lag in the window. It warns rather
// than fails: a late observation is correctly handled by the bitemporal store,
// so it is a reason to look at the indexer, not a reason to refuse the
// snapshot.
func Timeliness(events []domain.LedgerEvent, maxLag time.Duration) domain.QualityCheck {
	c := domain.QualityCheck{ID: CheckTimeliness, Name: "Observation lag", Critical: false}
	c.Expected = fmt.Sprintf("every event observed within %s of occurring", maxLag)
	if len(events) == 0 {
		c.Status = domain.QualityPass
		c.Observed = "no events"
		return c
	}
	var worst time.Duration
	worstID := ""
	for _, e := range events {
		lag := e.ObservedAt.Sub(e.OccurredAt)
		if lag > worst {
			worst, worstID = lag, e.ID()
		}
	}
	c.Observed = fmt.Sprintf("worst lag %s", worst)
	if worst <= maxLag {
		c.Status = domain.QualityPass
		return c
	}
	c.Status = domain.QualityWarn
	c.Detail = fmt.Sprintf("%s was observed %s after it occurred", worstID, worst)
	return c
}

// Accuracy checks the supply identity per ledger and symbol: everything minted,
// less everything burned, less everything still locked, equals everything held.
// A transfer moves quantity between holders and cannot change the total, so a
// difference here means the position store and the event stream disagree, which
// is the one failure that makes every downstream number wrong.
//
// The identity says nothing about the sign of any one holding. A stream that
// transfers from a party the ledger never issued to leaves that party negative
// and the identity still balances; that is a separate control and this is not
// it.
func Accuracy(events []domain.LedgerEvent, positions []domain.Position) domain.QualityCheck {
	c := domain.QualityCheck{ID: CheckAccuracy, Name: "Supply identity", Critical: true}
	c.Expected = "minted - burned - locked + unlocked == total held, per ledger and symbol"

	type slot struct {
		ledger domain.Ledger
		symbol string
	}
	supply := map[slot]int64{}
	held := map[slot]int64{}
	for _, e := range events {
		s := slot{e.Ledger, e.Symbol}
		switch e.Kind {
		case domain.EventMint:
			supply[s] += e.Quantity
		case domain.EventBurn:
			supply[s] -= e.Quantity
		case domain.EventLock:
			supply[s] -= e.Quantity
		case domain.EventUnlock:
			supply[s] += e.Quantity
		}
	}
	for _, p := range positions {
		s := slot{p.Ledger, p.Symbol}
		if _, tracked := supply[s]; !tracked {
			// A holding the event stream never issued — a book-of-record row
			// rather than a ledger one. The identity says nothing about it.
			continue
		}
		held[s] += p.Quantity
	}
	slots := make([]slot, 0, len(supply))
	for s := range supply {
		slots = append(slots, s)
	}
	sort.Slice(slots, func(i, j int) bool {
		if slots[i].ledger != slots[j].ledger {
			return slots[i].ledger < slots[j].ledger
		}
		return slots[i].symbol < slots[j].symbol
	})
	var problems []string
	for _, s := range slots {
		if supply[s] != held[s] {
			problems = append(problems, fmt.Sprintf("%s/%s: supply %d, held %d, delta %d",
				s.ledger, s.symbol, supply[s], held[s], held[s]-supply[s]))
		}
	}
	c.Observed = fmt.Sprintf("%d ledger/symbol pair(s) checked", len(slots))
	if len(problems) == 0 {
		c.Status = domain.QualityPass
		return c
	}
	c.Status = domain.QualityFail
	c.Detail = joinLimited(problems, 5)
	return c
}

// ReferentialIntegrity checks that every party moving tokens is a counterparty
// the platform knows. An unknown holder means either the registry is behind or
// a transfer reached an address nobody onboarded, and both are critical: an
// exposure to an entity with no rating, no netting agreement and no
// jurisdiction cannot be measured at all.
func ReferentialIntegrity(events []domain.LedgerEvent, cps []domain.Counterparty) domain.QualityCheck {
	c := domain.QualityCheck{ID: CheckReferential, Name: "Known holders", Critical: true}
	c.Expected = "every from/to party resolves in the counterparty registry"
	known := make(map[string]bool, len(cps))
	for _, cp := range cps {
		known[cp.ID] = true
	}
	unknown := map[string]bool{}
	for _, e := range events {
		for _, party := range []string{e.From, e.To} {
			if party != "" && !known[party] {
				unknown[party] = true
			}
		}
	}
	names := make([]string, 0, len(unknown))
	for p := range unknown {
		names = append(names, p)
	}
	sort.Strings(names)
	c.Observed = fmt.Sprintf("%d unknown holder(s)", len(names))
	if len(names) == 0 {
		c.Status = domain.QualityPass
		return c
	}
	c.Status = domain.QualityFail
	c.Detail = joinLimited(names, 5)
	return c
}

// Freshness checks that every mark is recent enough at the valid time to be
// worth marking against. It warns rather than fails, because the correct
// response to a stale mark is to widen the haircut or call the pricing desk,
// not to stop reporting.
func Freshness(prices []domain.Price, validAt time.Time, maxAge time.Duration) domain.QualityCheck {
	c := domain.QualityCheck{ID: CheckFreshness, Name: "Price freshness", Critical: false}
	c.Expected = fmt.Sprintf("every mark no older than %s at the valid time", maxAge)
	if len(prices) == 0 {
		c.Status = domain.QualityFail
		c.Observed = "no marks"
		c.Critical = true
		return c
	}
	var stale []string
	var worst time.Duration
	for _, p := range prices {
		age := validAt.Sub(p.AsOf)
		if age > worst {
			worst = age
		}
		if age > maxAge {
			stale = append(stale, fmt.Sprintf("%s is %s old", p.Symbol, age))
		}
	}
	sort.Strings(stale)
	c.Observed = fmt.Sprintf("oldest mark %s", worst)
	if len(stale) == 0 {
		c.Status = domain.QualityPass
		return c
	}
	c.Status = domain.QualityWarn
	c.Detail = joinLimited(stale, 5)
	return c
}

func joinLimited(items []string, limit int) string {
	if len(items) <= limit {
		return joinComma(items)
	}
	return fmt.Sprintf("%s (+%d more)", joinComma(items[:limit]), len(items)-limit)
}

func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}
