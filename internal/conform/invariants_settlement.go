package conform

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/ledger"
)

// bridgeEvents returns a ledger's events of one bridge operation, keyed by the
// nonce carried in the memo. The memo is how the simulators model an indexed
// event parameter; the bridge package writes it and this package reads it, so
// the two agree by construction rather than by convention.
func bridgeEvents(l ledger.Ledger, kind domain.EventKind, op string) map[uint64][]domain.LedgerEvent {
	out := map[uint64][]domain.LedgerEvent{}
	if l == nil {
		return out
	}
	for _, ev := range l.Events(0) {
		if ev.Kind != kind {
			continue
		}
		gotOp, nonce, ok := ledger.ParseBridgeMemo(ev.Memo)
		if !ok || gotOp != op {
			continue
		}
		out[nonce] = append(out[nonce], ev)
	}
	return out
}

// sortedNonces returns a map's nonces in ascending order.
func sortedNonces[V any](m map[uint64]V) []uint64 {
	out := make([]uint64, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ---------- SETTLE-1 ----------

// SettleSupplyConserved is SETTLE-1.
type SettleSupplyConserved struct{}

// ID returns the invariant id.
func (SettleSupplyConserved) ID() string { return "SETTLE-1" }

// Title states the invariant.
func (SettleSupplyConserved) Title() string { return "supply is conserved across the bridge" }

// Domain groups the invariant.
func (SettleSupplyConserved) Domain() string { return DomainSettlement }

// Evaluate runs the bridge's own supply reconciliation and fails on any supply
// difference it raises.
//
// The control is not reimplemented here. The bridge package owns the arithmetic
// and this invariant owns the judgement, which keeps the two from drifting: a
// change to what conservation means is a change in one place, and this stays a
// statement about whether it held.
func (i SettleSupplyConserved) Evaluate(ctx context.Context, ev Evidence) Verdict {
	v := base(i, SeverityCritical, "bridge.Reconcile raises no supply_mismatch break on any symbol")
	rep, ok := ev.SupplyReport()
	if !ok {
		return inconclusive(v, "neither a supply report nor a pair of ledgers and symbols was supplied")
	}

	var (
		refs []Ref
		bad  []string
	)
	for _, b := range rep.Breaks {
		if b.Kind != domain.BreakSupplyDiff {
			continue
		}
		bad = append(bad, fmt.Sprintf("%s: %s expected %d, ledgers say %d",
			b.ID, b.Symbol, b.BookQty, b.LedgerQty))
		refs = append(refs, BreakRef(b))
	}

	observed := plural(len(rep.Symbols), "symbol", "symbols") + " reconciled across " +
		string(rep.SourceLedger) + " and " + string(rep.TargetLedger)
	if len(bad) > 0 {
		return fail(v, observed+", "+plural(len(bad), "break", "breaks"), strings.Join(bad, "; "), refs...)
	}
	return pass(v, observed+", all balanced")
}

// ---------- SETTLE-2 ----------

// SettleHoldersRegistered is SETTLE-2.
type SettleHoldersRegistered struct{}

// ID returns the invariant id.
func (SettleHoldersRegistered) ID() string { return "SETTLE-2" }

// Title states the invariant.
func (SettleHoldersRegistered) Title() string {
	return "no token holder sits outside the identity registry on either ledger"
}

// Domain groups the invariant.
func (SettleHoldersRegistered) Domain() string { return DomainSettlement }

// Evaluate walks the holders of every symbol on both networks.
//
// The bridge vault holds units and is outside the registry by construction, so
// it is exempt by name rather than by a rule about what an unregistered holder
// might legitimately be. An exemption that a reader can enumerate is a control;
// an exemption expressed as a predicate is a hole.
func (i SettleHoldersRegistered) Evaluate(ctx context.Context, ev Evidence) Verdict {
	v := base(i, SeverityCritical,
		"every holder of every symbol on both ledgers is verified in the identity registry")
	views := []LedgerView{ev.Source, ev.Target}
	usable := 0
	for _, view := range views {
		if view.Holders != nil && view.Registered != nil {
			usable++
		}
	}
	if usable == 0 {
		return inconclusive(v, "neither ledger view exposes both a holder list and a registry")
	}
	if len(ev.Symbols) == 0 {
		return inconclusive(v, "no symbols were named, so no holder list could be read")
	}

	symbols := append([]string(nil), ev.Symbols...)
	sort.Strings(symbols)

	var (
		refs    []Ref
		bad     []string
		checked int
		skipped []string
	)
	for _, view := range views {
		name := string(view.Name)
		if view.Ledger != nil {
			name = string(view.Ledger.Name())
		}
		if view.Holders == nil || view.Registered == nil {
			if name != "" {
				skipped = append(skipped, name)
			}
			continue
		}
		for _, sym := range symbols {
			holders := append([]string(nil), view.Holders(sym)...)
			sort.Strings(holders)
			for _, h := range holders {
				if view.exempt(h) {
					continue
				}
				checked++
				if view.Registered(h) {
					continue
				}
				bad = append(bad, fmt.Sprintf("%s: %s holds %s and is not in the registry", name, h, sym))
				refs = append(refs, Ref{Kind: RefLedger, ID: h, Detail: name + " " + sym})
			}
		}
	}

	observed := plural(checked, "holding", "holdings") + " checked"
	if len(bad) > 0 {
		return fail(v, observed+", "+plural(len(bad), "unregistered", "unregistered"), strings.Join(bad, "; "), refs...)
	}
	if len(skipped) > 0 {
		// Half the evidence is a conclusion about half the platform. Say so.
		return inconclusive(v, "no registry evidence for "+strings.Join(skipped, ", ")+
			"; the other network was checked and clean")
	}
	return pass(v, observed+", all registered")
}

// ---------- SETTLE-3 ----------

// SettleMintIsAttested is SETTLE-3.
type SettleMintIsAttested struct{}

// ID returns the invariant id.
func (SettleMintIsAttested) ID() string { return "SETTLE-3" }

// Title states the invariant.
func (SettleMintIsAttested) Title() string {
	return "every bridge mint carries a quorum attestation and a matching source lock"
}

// Domain groups the invariant.
func (SettleMintIsAttested) Domain() string { return DomainSettlement }

// Evaluate starts from what happened on the target ledger, not from what the
// relayer says it did.
//
// The direction matters. Walking the relayer's settlements and confirming each
// one landed proves the relayer is honest about its own work; walking the
// target's mints and demanding an attestation for each proves there is nothing
// on the target that the relayer did not authorise. Only the second one catches
// a mint that came from somewhere else.
func (i SettleMintIsAttested) Evaluate(ctx context.Context, ev Evidence) Verdict {
	v := base(i, SeverityCritical,
		"every bridge mint on the target has a quorum-signed attestation and a lock of the same size on the source")
	if ev.Target.Ledger == nil {
		return inconclusive(v, "no target ledger was supplied, so no mint could be examined")
	}
	if ev.Signers == nil {
		return inconclusive(v, "no signer set was supplied, so no attestation could be verified")
	}
	if ev.Source.Ledger == nil {
		return inconclusive(v, "no source ledger was supplied, so no lock could be matched")
	}

	mints := bridgeEvents(ev.Target.Ledger, domain.EventMint, ledger.BridgeOpMint)
	locks := bridgeEvents(ev.Source.Ledger, domain.EventLock, ledger.BridgeOpLock)
	attested := map[uint64]AttestedMint{}
	for _, m := range ev.Mints {
		attested[m.Attestation.Nonce] = m
	}

	var (
		refs    []Ref
		bad     []string
		checked int
	)
	for _, nonce := range sortedNonces(mints) {
		for _, mint := range mints[nonce] {
			checked++
			am, ok := attested[nonce]
			if !ok {
				bad = append(bad, fmt.Sprintf("nonce %d: %d units of %s minted with no attestation",
					nonce, mint.Quantity, mint.Symbol))
				refs = append(refs, LedgerRef(mint))
				continue
			}
			if _, err := ev.Signers.Verify(am.Attestation, am.Signatures); err != nil {
				bad = append(bad, fmt.Sprintf("nonce %d: %v", nonce, err))
				refs = append(refs, LedgerRef(mint), AttestationRef(am.Attestation))
				continue
			}
			if am.Attestation.Symbol != mint.Symbol || am.Attestation.Quantity != mint.Quantity {
				bad = append(bad, fmt.Sprintf("nonce %d: attestation says %d %s, the mint says %d %s",
					nonce, am.Attestation.Quantity, am.Attestation.Symbol, mint.Quantity, mint.Symbol))
				refs = append(refs, LedgerRef(mint), AttestationRef(am.Attestation))
				continue
			}
			matched := false
			for _, lk := range locks[nonce] {
				if lk.Symbol == mint.Symbol && lk.Quantity == mint.Quantity {
					matched = true
					refs = append(refs, LedgerRef(mint), AttestationRef(am.Attestation), LedgerRef(lk))
					break
				}
			}
			if !matched {
				bad = append(bad, fmt.Sprintf("nonce %d: %d units of %s minted with no matching lock on %s",
					nonce, mint.Quantity, mint.Symbol, ev.Source.Ledger.Name()))
				refs = append(refs, LedgerRef(mint), AttestationRef(am.Attestation))
			}
		}
	}

	observed := plural(checked, "bridge mint", "bridge mints") + " on " + string(ev.Target.Ledger.Name())
	if len(bad) > 0 {
		return fail(v, observed+", "+plural(len(bad), "unbacked", "unbacked"), strings.Join(bad, "; "), refs...)
	}
	return pass(v, observed+", each attested and locked", refs...)
}

// ---------- SETTLE-4 ----------

// SettleNonceNotReplayed is SETTLE-4.
type SettleNonceNotReplayed struct{}

// ID returns the invariant id.
func (SettleNonceNotReplayed) ID() string { return "SETTLE-4" }

// Title states the invariant.
func (SettleNonceNotReplayed) Title() string { return "no bridge nonce is minted twice" }

// Domain groups the invariant.
func (SettleNonceNotReplayed) Domain() string { return DomainSettlement }

// Evaluate counts bridge mints per nonce on both networks.
//
// The relayer's replay protection is keyed on the nonce precisely so that a
// reorganisation re-including a lock mints nothing the second time. This
// invariant checks the outcome of that design against the ledgers rather than
// the state of the map that implements it, because a replay guard that is
// correct in memory and wrong after a restart is the interesting case.
func (i SettleNonceNotReplayed) Evaluate(ctx context.Context, ev Evidence) Verdict {
	v := base(i, SeverityCritical, "each bridge nonce appears at most once as a mint on each ledger")
	views := []LedgerView{ev.Source, ev.Target}
	seen := 0
	for _, view := range views {
		if view.Ledger != nil {
			seen++
		}
	}
	if seen == 0 {
		return inconclusive(v, "no ledger was supplied, so no mint could be counted")
	}

	var (
		refs    []Ref
		bad     []string
		checked int
	)
	for _, view := range views {
		if view.Ledger == nil {
			continue
		}
		mints := bridgeEvents(view.Ledger, domain.EventMint, ledger.BridgeOpMint)
		for _, nonce := range sortedNonces(mints) {
			evs := mints[nonce]
			checked += len(evs)
			if len(evs) < 2 {
				continue
			}
			bad = append(bad, fmt.Sprintf("%s: nonce %d minted %d times", view.Ledger.Name(), nonce, len(evs)))
			for _, e := range evs {
				refs = append(refs, LedgerRef(e))
			}
		}
	}

	observed := plural(checked, "bridge mint", "bridge mints") + " across " +
		plural(seen, "ledger", "ledgers")
	if len(bad) > 0 {
		return fail(v, observed+", "+plural(len(bad), "replayed nonce", "replayed nonces"),
			strings.Join(bad, "; "), refs...)
	}
	return pass(v, observed+", every nonce used once")
}

// ---------- SETTLE-5 ----------

// SettleIssuanceIdentity is SETTLE-5.
type SettleIssuanceIdentity struct{}

// ID returns the invariant id.
func (SettleIssuanceIdentity) ID() string { return "SETTLE-5" }

// Title states the invariant.
func (SettleIssuanceIdentity) Title() string {
	return "circulating supply equals issuance less redemptions"
}

// Domain groups the invariant.
func (SettleIssuanceIdentity) Domain() string { return DomainSettlement }

// Evaluate isolates the issuance half of the supply reconciliation.
//
// SETTLE-1 fails on any supply break at all, which is the right thing for a
// gate and the wrong thing for a person reading the report at speed: it does
// not say whether the bridge lost track of a lock or whether units appeared
// from nowhere. Those are different incidents with different responders, so the
// issuance identity gets its own line and its own verdict.
func (i SettleIssuanceIdentity) Evaluate(ctx context.Context, ev Evidence) Verdict {
	v := base(i, SeverityCritical,
		"circulating(sym) == issued(sym) - redeemed(sym) for every symbol on both ledgers")
	rep, ok := ev.SupplyReport()
	if !ok {
		return inconclusive(v, "neither a supply report nor a pair of ledgers and symbols was supplied")
	}

	var bad []string
	var refs []Ref
	for _, s := range rep.Symbols {
		if s.IssuanceDelta == 0 {
			continue
		}
		bad = append(bad, fmt.Sprintf("%s: %d circulating against %d issued less %d redeemed (delta %d)",
			s.Symbol, s.Circulating, s.Issued, s.Redeemed, s.IssuanceDelta))
	}
	for _, b := range rep.Breaks {
		if strings.HasPrefix(b.ID, "BRK-BRIDGE-ISSUE-") {
			refs = append(refs, BreakRef(b))
		}
	}

	observed := plural(len(rep.Symbols), "symbol", "symbols") + " checked"
	if len(bad) > 0 {
		return fail(v, observed+", "+plural(len(bad), "out of balance", "out of balance"),
			strings.Join(bad, "; "), refs...)
	}
	return pass(v, observed+", issuance identity holds")
}

// ---------- DATA-1 ----------

// DataGoldTracesToBronze is DATA-1.
type DataGoldTracesToBronze struct{}

// ID returns the invariant id.
func (DataGoldTracesToBronze) ID() string { return "DATA-1" }

// Title states the invariant.
func (DataGoldTracesToBronze) Title() string { return "every gold row traces to a bronze event" }

// Domain groups the invariant.
func (DataGoldTracesToBronze) Domain() string { return DomainData }

// Evaluate compares the gold row count to the number of those rows that carry a
// lineage back to a bronze event.
//
// A gold row with no bronze ancestor is a number on a dashboard that nothing
// observed. It is the data-platform equivalent of a mint with no lock, and it
// is worth its own invariant for the same reason: the figure looks entirely
// normal right up to the moment somebody asks where it came from.
func (i DataGoldTracesToBronze) Evaluate(ctx context.Context, ev Evidence) Verdict {
	v := base(i, SeverityHigh, "traced gold rows == gold rows")
	if ev.Lineage == nil {
		return inconclusive(v, "no lineage counter was supplied, so no gold row could be traced")
	}
	gold, traced := ev.Lineage()
	refs := []Ref{{Kind: RefLineage, ID: "gold", Detail: fmt.Sprintf("%d rows, %d traced", gold, traced)}}
	if ev.Quality != nil {
		refs = append(refs, Ref{
			Kind: RefQuality, ID: "snapshot",
			Detail: "quality " + string(ev.Quality.Status) + ", " +
				plural(len(ev.Quality.Checks), "check", "checks"),
		})
	}
	observed := fmt.Sprintf("%d of %d gold rows trace to a bronze event", traced, gold)
	switch {
	case traced > gold:
		return fail(v, observed, "more rows claim a lineage than exist, so the counter itself is wrong", refs...)
	case traced < gold:
		return fail(v, observed,
			fmt.Sprintf("%s have no bronze ancestor", plural(gold-traced, "gold row", "gold rows")), refs...)
	}
	return pass(v, observed, refs...)
}
