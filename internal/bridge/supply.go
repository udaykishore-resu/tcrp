package bridge

import (
	"fmt"
	"sort"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/ledger"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// SymbolSupply is one symbol's cross-network supply position.
//
// Every field is a count of whole shares. There is no money here: a share is a
// share on both networks and converting it to a value would put a price between
// the control and the thing it is controlling.
type SymbolSupply struct {
	Symbol string `json:"symbol"`
	// SourceSupply and TargetSupply are each ledger's total supply, including
	// units sitting in its bridge vault.
	SourceSupply int64 `json:"source_supply"`
	TargetSupply int64 `json:"target_supply"`
	// LockedOnSource and LockedOnTarget are each vault's balance.
	LockedOnSource int64 `json:"locked_on_source"`
	LockedOnTarget int64 `json:"locked_on_target"`
	// BridgedToTarget is the net quantity minted on the target against a
	// source lock: bridge mints less bridge burns. It is what LockedOnSource
	// has to equal.
	BridgedToTarget int64 `json:"bridged_to_target"`
	// BridgedToSource is the same figure for the reverse direction.
	BridgedToSource int64 `json:"bridged_to_source"`
	// Issued is every unit ever created by a primary issuance, on either
	// network. Bridge mints are excluded: bridging moves units, it does not
	// issue them.
	Issued int64 `json:"issued"`
	// Redeemed is every unit ever destroyed by a redemption, on either
	// network, again excluding bridge burns.
	Redeemed int64 `json:"redeemed"`
	// Circulating is what holders actually hold: each network's supply less
	// what its own vault has locked away.
	Circulating int64 `json:"circulating"`
	// LockDelta is BridgedToTarget less LockedOnSource. Non-zero means units
	// exist on the target that nothing on the source is backing, or units are
	// locked on the source that never arrived on the target.
	LockDelta int64 `json:"lock_delta"`
	// ReverseLockDelta is the same figure for the reverse direction.
	ReverseLockDelta int64 `json:"reverse_lock_delta"`
	// IssuanceDelta is Circulating less (Issued − Redeemed). Non-zero means
	// units came into or went out of existence outside the issuance process.
	IssuanceDelta int64 `json:"issuance_delta"`
	// Balanced is true when all three deltas are zero.
	Balanced bool `json:"balanced"`
}

// SupplyReport is the outcome of one cross-network supply reconciliation.
type SupplyReport struct {
	AsOf         time.Time     `json:"as_of"`
	SourceLedger domain.Ledger `json:"source_ledger"`
	TargetLedger domain.Ledger `json:"target_ledger"`
	Symbols      []SymbolSupply
	// Balanced is true when every symbol balances.
	Balanced bool `json:"balanced"`
	// Breaks are the differences, worst first. Empty when Balanced.
	Breaks []domain.Break `json:"breaks,omitempty"`
}

// Symbol returns one symbol's position.
func (r SupplyReport) Symbol(sym string) (SymbolSupply, bool) {
	for _, s := range r.Symbols {
		if s.Symbol == sym {
			return s, true
		}
	}
	return SymbolSupply{}, false
}

// Reconcile proves the two supply invariants a lock-and-mint bridge must
// satisfy, for every symbol, across both networks:
//
//	locked_on_source(sym) == bridged_to_target(sym)
//	circulating(sym)      == issued(sym) − redeemed(sym)
//
// The first says every unit minted on the target is backed by a unit that
// cannot move on the source. The second says the bridge did not quietly create
// or destroy any: a unit only comes into existence through a primary issuance
// and only leaves through a redemption, and bridging is neither.
//
// It is a pure function. It reads both ledgers, writes nothing, keeps no state
// between calls, and takes the as-of instant as an argument rather than a
// clock, so the same pair of ledgers always produces the same report — which is
// what lets a conformance invariant call it and lets a break it raises be
// reproduced three months later. Use RecordSupply if the figures should also
// reach a metrics registry.
//
// What it cannot prove: that the units on the target are held by the right
// parties. Supply is a conservation check, not a reconciliation of holdings —
// that is the lakehouse's job, against the book of record.
func Reconcile(source, target ledger.Ledger, symbols []string, asOf time.Time) SupplyReport {
	rep := SupplyReport{
		AsOf: asOf.UTC(), SourceLedger: source.Name(), TargetLedger: target.Name(),
		Balanced: true,
	}
	syms := append([]string(nil), symbols...)
	sort.Strings(syms)

	srcFlows := bridgeFlows(source)
	tgtFlows := bridgeFlows(target)

	for _, sym := range syms {
		s := SymbolSupply{
			Symbol:         sym,
			SourceSupply:   source.TotalSupply(sym),
			TargetSupply:   target.TotalSupply(sym),
			LockedOnSource: source.Locked(sym),
			LockedOnTarget: target.Locked(sym),
		}
		src, tgt := srcFlows[sym], tgtFlows[sym]
		s.BridgedToTarget = tgt.bridgeMinted - tgt.bridgeBurned
		s.BridgedToSource = src.bridgeMinted - src.bridgeBurned
		s.Issued = src.issued + tgt.issued
		s.Redeemed = src.redeemed + tgt.redeemed
		s.Circulating = (s.SourceSupply - s.LockedOnSource) + (s.TargetSupply - s.LockedOnTarget)
		s.LockDelta = s.BridgedToTarget - s.LockedOnSource
		s.ReverseLockDelta = s.BridgedToSource - s.LockedOnTarget
		s.IssuanceDelta = s.Circulating - (s.Issued - s.Redeemed)
		s.Balanced = s.LockDelta == 0 && s.ReverseLockDelta == 0 && s.IssuanceDelta == 0
		if !s.Balanced {
			rep.Balanced = false
		}
		rep.Symbols = append(rep.Symbols, s)

		if s.LockDelta != 0 {
			rep.Breaks = append(rep.Breaks, supplyBreak(
				"BRK-BRIDGE-LOCK-"+sym, sym, target.Name(), asOf,
				s.LockedOnSource, s.BridgedToTarget,
				fmt.Sprintf("%d units minted on %s against %d locked on %s",
					s.BridgedToTarget, target.Name(), s.LockedOnSource, source.Name())))
		}
		if s.ReverseLockDelta != 0 {
			rep.Breaks = append(rep.Breaks, supplyBreak(
				"BRK-BRIDGE-LOCK-REVERSE-"+sym, sym, source.Name(), asOf,
				s.LockedOnTarget, s.BridgedToSource,
				fmt.Sprintf("%d units minted on %s against %d locked on %s",
					s.BridgedToSource, source.Name(), s.LockedOnTarget, target.Name())))
		}
		if s.IssuanceDelta != 0 {
			rep.Breaks = append(rep.Breaks, supplyBreak(
				"BRK-BRIDGE-ISSUE-"+sym, sym, "", asOf,
				s.Issued-s.Redeemed, s.Circulating,
				fmt.Sprintf("%d units circulating across %s and %s against %d issued less %d redeemed",
					s.Circulating, source.Name(), target.Name(), s.Issued, s.Redeemed)))
		}
	}
	domain.SortBreaks(rep.Breaks)
	return rep
}

func supplyBreak(id, symbol string, l domain.Ledger, asOf time.Time, book, onLedger int64, note string) domain.Break {
	return domain.Break{
		ID: id, Kind: domain.BreakSupplyDiff, Symbol: symbol, Ledger: l,
		BookQty: book, LedgerQty: onLedger, Delta: onLedger - book,
		Status: domain.BreakOpen, DetectedAt: asOf.UTC(), AsOf: asOf.UTC(),
		// A supply difference is never anything but critical. It says the
		// platform's two networks disagree about how much of the fund exists,
		// and there is no size of that disagreement worth triaging as routine.
		Severity: "critical",
		Notes:    []string{note},
	}
}

// flows is one ledger's issuance and bridge activity for a symbol.
type flows struct {
	issued       int64
	redeemed     int64
	bridgeMinted int64
	bridgeBurned int64
}

// bridgeFlows reads a ledger's whole event history and splits mints and burns
// into primary issuance and bridge activity, keyed by symbol.
//
// The split is by memo, which is how these simulators model an indexed event
// parameter. A mint with no bridge memo is a primary issuance by definition:
// there is nothing else it could be, since every bridge mint this package
// writes carries one.
func bridgeFlows(l ledger.Ledger) map[string]flows {
	out := map[string]flows{}
	for _, ev := range l.Events(0) {
		op, _, isBridge := ledger.ParseBridgeMemo(ev.Memo)
		f := out[ev.Symbol]
		switch ev.Kind {
		case domain.EventMint:
			if isBridge && op == ledger.BridgeOpMint {
				f.bridgeMinted += ev.Quantity
			} else {
				f.issued += ev.Quantity
			}
		case domain.EventBurn:
			if isBridge && op == ledger.BridgeOpBurn {
				f.bridgeBurned += ev.Quantity
			} else {
				f.redeemed += ev.Quantity
			}
		default:
			continue
		}
		out[ev.Symbol] = f
	}
	return out
}

// RecordSupply writes a report's figures to a metrics registry. It is separate
// from Reconcile so that the reconciliation stays a pure function: a control
// that a conformance test calls must not have an observable side effect on the
// process that runs it.
func RecordSupply(r *obs.Registry, rep SupplyReport) {
	if r == nil {
		return
	}
	for _, s := range rep.Symbols {
		lb := map[string]string{"symbol": s.Symbol}
		r.Gauge(MetricSupplyDelta, "bridge lock-versus-mint difference in shares", lb).Set(float64(s.LockDelta))
		r.Gauge(MetricSupplyBreaks, "open cross-network supply breaks", lb).Set(float64(breaksFor(rep, s.Symbol)))
	}
}

func breaksFor(rep SupplyReport, sym string) int {
	n := 0
	for _, b := range rep.Breaks {
		if b.Symbol == sym {
			n++
		}
	}
	return n
}
