package domain

import (
	"time"
)

// Snapshot is the curated, point-in-time view of the world that the risk engine
// calculates from. It is the contract between the lakehouse and the engine: the
// lakehouse decides what was true and when it was known, and the engine decides
// what that means for exposure. Neither imports the other.
type Snapshot struct {
	// ValidAt is the business time the snapshot describes.
	ValidAt time.Time `json:"valid_at"`
	// KnownAt is the transaction time: what the platform had learned by then.
	// A snapshot with ValidAt in the past and KnownAt now is a restatement; the
	// same ValidAt with KnownAt also in the past is what the desk actually saw.
	KnownAt time.Time `json:"known_at"`

	Counterparties []Counterparty   `json:"counterparties"`
	Instruments    []Instrument     `json:"instruments"`
	Prices         map[string]Price `json:"prices"`
	Positions      []Position       `json:"positions"`
	Obligations    []Obligation     `json:"obligations"`
	Haircuts       HaircutSchedule  `json:"haircuts"`
	Limits         []Limit          `json:"limits"`

	// Lineage names the gold tables and snapshot ids this view was built from,
	// so a figure on a dashboard can be walked back to the rows behind it.
	Lineage []string `json:"lineage,omitempty"`
	// Quality carries the data-quality verdict. A snapshot that failed a
	// critical control is still returned, but marked, so a caller can refuse to
	// act on it rather than never seeing it.
	Quality QualityReport `json:"quality"`
}

// QualityStatus is the outcome of the data quality controls.
type QualityStatus string

// Quality statuses.
const (
	QualityPass QualityStatus = "pass"
	QualityWarn QualityStatus = "warn"
	QualityFail QualityStatus = "fail"
)

// QualityCheck is one control's result.
type QualityCheck struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Status   QualityStatus `json:"status"`
	Observed string        `json:"observed,omitempty"`
	Expected string        `json:"expected,omitempty"`
	Detail   string        `json:"detail,omitempty"`
	Critical bool          `json:"critical"`
}

// QualityReport aggregates the controls run against a dataset.
type QualityReport struct {
	Status QualityStatus  `json:"status"`
	Checks []QualityCheck `json:"checks,omitempty"`
	RanAt  time.Time      `json:"ran_at,omitzero"`
}

// Usable reports whether the snapshot may drive a decision that changes state.
func (q QualityReport) Usable() bool { return q.Status != QualityFail }

// Counterparty returns a counterparty by id.
func (s Snapshot) Counterparty(id string) (Counterparty, bool) {
	for _, c := range s.Counterparties {
		if c.ID == id {
			return c, true
		}
	}
	return Counterparty{}, false
}

// Instrument returns an instrument by symbol.
func (s Snapshot) Instrument(sym string) (Instrument, bool) {
	for _, i := range s.Instruments {
		if i.Symbol == sym {
			return i, true
		}
	}
	return Instrument{}, false
}

// PositionsOf returns the positions of one counterparty.
func (s Snapshot) PositionsOf(cp string) []Position {
	var out []Position
	for _, p := range s.Positions {
		if p.Counterparty == cp {
			out = append(out, p)
		}
	}
	SortPositions(out)
	return out
}

// ObligationsOf returns the obligations of one counterparty.
func (s Snapshot) ObligationsOf(cp string) []Obligation {
	var out []Obligation
	for _, o := range s.Obligations {
		if o.Counterparty == cp {
			out = append(out, o)
		}
	}
	return out
}

// SnapshotFromSeed builds a snapshot directly from the seeded dataset, so the
// risk engine can be exercised without the lakehouse running.
func SnapshotFromSeed(s *Seed) Snapshot {
	prices := make(map[string]Price, len(s.Prices))
	for _, p := range s.Prices {
		prices[p.Symbol] = p
	}
	return Snapshot{
		ValidAt: s.AsOf, KnownAt: s.AsOf,
		Counterparties: s.Counterparties, Instruments: s.Instruments, Prices: prices,
		Positions: s.Positions, Obligations: s.Obligations, Haircuts: s.Haircuts,
		Limits: s.Limits, Lineage: []string{"seed:domain.NewSeed"},
		Quality: QualityReport{Status: QualityPass, RanAt: s.AsOf},
	}
}
