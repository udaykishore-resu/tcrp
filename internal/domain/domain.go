// Package domain holds the vocabulary every other package shares: what a
// counterparty, a position, a ledger event and an exposure are.
//
// It has no behaviour beyond validation and rendering, and it depends on
// nothing but the money package. That is deliberate: the lakehouse, the risk
// engine, the ledgers and the agent's tools all speak these types, and if the
// vocabulary depended on any one of them the others would inherit it.
package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/money"
)

// Ledger names a settlement network.
type Ledger string

// Known ledgers. The platform is dual-ledger by construction: the same fund
// share exists as an ERC-3643-style permissioned token on an EVM chain and as a
// Daml contract on a Canton network, and no single ledger is authoritative.
const (
	LedgerEVM    Ledger = "evm"
	LedgerCanton Ledger = "canton"
	LedgerGolden Ledger = "golden" // the institution's off-chain book of record
)

// AssetClass groups instruments for haircut purposes.
type AssetClass string

// Asset classes.
const (
	ClassMMF        AssetClass = "money_market_fund"
	ClassGovBond    AssetClass = "government_bond"
	ClassCorpBond   AssetClass = "corporate_bond"
	ClassEquity     AssetClass = "equity"
	ClassCash       AssetClass = "cash"
	ClassStableCoin AssetClass = "stablecoin"
)

// Counterparty is an entity the desk faces.
type Counterparty struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	LEI          string   `json:"lei"`
	Jurisdiction string   `json:"jurisdiction"`
	Rating       string   `json:"rating"`
	Sector       string   `json:"sector"`
	Netting      bool     `json:"netting_agreement"`
	Whitelisted  bool     `json:"whitelisted"`
	Wallets      []string `json:"wallets,omitempty"`
	Parties      []string `json:"canton_parties,omitempty"`
}

// Instrument is something that can be held as collateral.
type Instrument struct {
	Symbol   string         `json:"symbol"`
	Name     string         `json:"name"`
	Issuer   string         `json:"issuer"`
	Sector   string         `json:"sector"`
	Class    AssetClass     `json:"class"`
	Currency money.Currency `json:"currency"`
	Rating   string         `json:"rating"`
	// Tokenised marks the instruments that exist on a ledger rather than only
	// in the book of record.
	Tokenised bool `json:"tokenised"`
}

// Price is a mark for an instrument at a point in time.
type Price struct {
	Symbol string       `json:"symbol"`
	Px     money.Amount `json:"px"`
	AsOf   time.Time    `json:"as_of"`
	Source string       `json:"source"`
}

// HaircutRule is one row of the eligibility schedule.
type HaircutRule struct {
	Class   AssetClass  `json:"class"`
	Rating  string      `json:"rating"` // empty matches any rating
	Haircut money.Ratio `json:"haircut"`
	// Eligible false means the asset may be held but takes no collateral value.
	Eligible bool `json:"eligible"`
}

// HaircutSchedule resolves an instrument to a haircut.
type HaircutSchedule struct {
	Version string        `json:"version"`
	Rules   []HaircutRule `json:"rules"`
}

// For returns the haircut applying to an instrument. The most specific rule —
// class and rating — wins over a class-only rule; an instrument with no rule at
// all is ineligible rather than unhaircut, because an unknown asset must not
// silently receive full collateral value.
func (s HaircutSchedule) For(i Instrument) (money.Ratio, bool) {
	var (
		best    money.Ratio
		found   bool
		precise bool
	)
	for _, r := range s.Rules {
		if r.Class != i.Class {
			continue
		}
		switch {
		case r.Rating == i.Rating:
			if !r.Eligible {
				return money.Ratio{Num: 1, Den: 1}, false
			}
			return r.Haircut, true
		case r.Rating == "" && !precise:
			if !r.Eligible {
				return money.Ratio{Num: 1, Den: 1}, false
			}
			best, found = r.Haircut, true
		}
	}
	return best, found
}

// Position is a holding, recorded bitemporally.
//
// ValidFrom/ValidTo is when the holding was true on the ledger or in the book.
// TxFrom/TxTo is when the platform learned it. Keeping both is what lets the
// desk answer "what did we believe our exposure was at 16:00 yesterday" and
// "what was it actually" with the same query and two different clocks — which
// is the question every collateral dispute turns into.
type Position struct {
	ID           string    `json:"id"`
	Counterparty string    `json:"counterparty"`
	Symbol       string    `json:"symbol"`
	Ledger       Ledger    `json:"ledger"`
	Quantity     int64     `json:"quantity"`
	Pledged      bool      `json:"pledged"`
	ValidFrom    time.Time `json:"valid_from"`
	ValidTo      time.Time `json:"valid_to,omitzero"`
	TxFrom       time.Time `json:"tx_from"`
	TxTo         time.Time `json:"tx_to,omitzero"`
	Source       string    `json:"source"`
	BlockHeight  uint64    `json:"block_height,omitempty"`
	TxHash       string    `json:"tx_hash,omitempty"`
	Corrected    bool      `json:"corrected,omitempty"`
}

// Open reports whether the row is the current version on both axes.
func (p Position) Open() bool { return p.ValidTo.IsZero() && p.TxTo.IsZero() }

// ValidAt reports whether the row describes the world at valid time v as known
// at transaction time t.
func (p Position) ValidAt(v, t time.Time) bool {
	if !p.ValidFrom.IsZero() && v.Before(p.ValidFrom) {
		return false
	}
	if !p.ValidTo.IsZero() && !v.Before(p.ValidTo) {
		return false
	}
	if !p.TxFrom.IsZero() && t.Before(p.TxFrom) {
		return false
	}
	if !p.TxTo.IsZero() && !t.Before(p.TxTo) {
		return false
	}
	return true
}

// Key identifies the economic slot a position occupies.
func (p Position) Key() string {
	return strings.Join([]string{p.Counterparty, p.Symbol, string(p.Ledger)}, "|")
}

// ObligationKind classifies what is owed.
type ObligationKind string

// Obligation kinds.
const (
	ObligationRepo       ObligationKind = "repo"
	ObligationDerivative ObligationKind = "derivative_mtm"
	ObligationLoan       ObligationKind = "loan"
	ObligationFee        ObligationKind = "fee"
)

// Obligation is an amount owed by a counterparty to the institution
// (positive) or by the institution to the counterparty (negative).
type Obligation struct {
	ID           string         `json:"id"`
	Counterparty string         `json:"counterparty"`
	Kind         ObligationKind `json:"kind"`
	Amount       money.Amount   `json:"amount"`
	NettingSet   string         `json:"netting_set"`
	AsOf         time.Time      `json:"as_of"`
	DueAt        time.Time      `json:"due_at,omitzero"`
}

// EventKind classifies a ledger event.
type EventKind string

// Ledger event kinds.
const (
	EventTransfer EventKind = "transfer"
	EventMint     EventKind = "mint"
	EventBurn     EventKind = "burn"
	EventLock     EventKind = "lock"
	EventUnlock   EventKind = "unlock"
	EventPledge   EventKind = "pledge"
	EventRelease  EventKind = "release"
	EventFreeze   EventKind = "freeze"
)

// LedgerEvent is an observation from a settlement network. OccurredAt is the
// block or transaction time; ObservedAt is when the platform saw it. The gap
// between them is the lag every reconciliation control has to tolerate.
type LedgerEvent struct {
	Seq        uint64    `json:"seq"`
	Ledger     Ledger    `json:"ledger"`
	Kind       EventKind `json:"kind"`
	Symbol     string    `json:"symbol"`
	From       string    `json:"from,omitempty"`
	To         string    `json:"to,omitempty"`
	Quantity   int64     `json:"quantity"`
	Block      uint64    `json:"block"`
	TxHash     string    `json:"tx_hash"`
	LogIndex   int       `json:"log_index"`
	OccurredAt time.Time `json:"occurred_at"`
	ObservedAt time.Time `json:"observed_at"`
	Finalised  bool      `json:"finalised"`
	Memo       string    `json:"memo,omitempty"`
}

// ID returns the event's natural key, which is what makes ingestion idempotent.
func (e LedgerEvent) ID() string {
	return fmt.Sprintf("%s:%s:%d", e.Ledger, e.TxHash, e.LogIndex)
}

// BreakKind classifies a reconciliation difference.
type BreakKind string

// Break kinds.
const (
	BreakMissingOnChain BreakKind = "missing_on_chain"
	BreakMissingInBook  BreakKind = "missing_in_book"
	BreakQuantityDiff   BreakKind = "quantity_mismatch"
	BreakStale          BreakKind = "stale_observation"
	BreakSupplyDiff     BreakKind = "supply_mismatch"
	BreakUnknownHolder  BreakKind = "unknown_holder"
)

// BreakStatus is where a break sits in the investigation workflow.
type BreakStatus string

// Break statuses.
const (
	BreakOpen         BreakStatus = "open"
	BreakAcknowledged BreakStatus = "acknowledged"
	BreakResolved     BreakStatus = "resolved"
	BreakSuppressed   BreakStatus = "suppressed"
)

// Break is a difference between two sources that both claim to be true.
type Break struct {
	ID           string      `json:"id"`
	Kind         BreakKind   `json:"kind"`
	Counterparty string      `json:"counterparty,omitempty"`
	Symbol       string      `json:"symbol,omitempty"`
	Ledger       Ledger      `json:"ledger,omitempty"`
	BookQty      int64       `json:"book_qty"`
	LedgerQty    int64       `json:"ledger_qty"`
	Delta        int64       `json:"delta"`
	Status       BreakStatus `json:"status"`
	DetectedAt   time.Time   `json:"detected_at"`
	AsOf         time.Time   `json:"as_of"`
	AgeMinutes   float64     `json:"age_minutes"`
	Assignee     string      `json:"assignee,omitempty"`
	Notes        []string    `json:"notes,omitempty"`
	ResolvedAt   time.Time   `json:"resolved_at,omitzero"`
	Resolution   string      `json:"resolution,omitempty"`
	Severity     string      `json:"severity"`
}

// Material reports whether the break is worth a human's attention. A one-share
// difference on a billion-share position is noise; the threshold is explicit so
// that "we suppressed it" is a decision on the record rather than a filter
// somebody added to a dashboard.
func (b Break) Material(absThreshold int64) bool {
	d := b.Delta
	if d < 0 {
		d = -d
	}
	return d >= absThreshold
}

// Sort helpers used across packages so orderings are stable everywhere.

// SortPositions orders positions deterministically.
func SortPositions(ps []Position) {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].Counterparty != ps[j].Counterparty {
			return ps[i].Counterparty < ps[j].Counterparty
		}
		if ps[i].Symbol != ps[j].Symbol {
			return ps[i].Symbol < ps[j].Symbol
		}
		if ps[i].Ledger != ps[j].Ledger {
			return ps[i].Ledger < ps[j].Ledger
		}
		return ps[i].TxFrom.Before(ps[j].TxFrom)
	})
}

// SortEvents orders events by sequence.
func SortEvents(es []LedgerEvent) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].OccurredAt.Equal(es[j].OccurredAt) {
			return es[i].Seq < es[j].Seq
		}
		return es[i].OccurredAt.Before(es[j].OccurredAt)
	})
}

// SortBreaks orders breaks worst-first.
func SortBreaks(bs []Break) {
	rank := map[string]int{"critical": 3, "high": 2, "medium": 1, "low": 0}
	sort.Slice(bs, func(i, j int) bool {
		if rank[bs[i].Severity] != rank[bs[j].Severity] {
			return rank[bs[i].Severity] > rank[bs[j].Severity]
		}
		return bs[i].DetectedAt.Before(bs[j].DetectedAt)
	})
}
