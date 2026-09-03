package bridge_test

import (
	"context"
	"testing"

	"github.com/udaykishore-resu/tcrp/internal/bridge"
	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/ledger"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

func TestSeededWorldBalances(t *testing.T) {
	h := newHarness(t, 1, 2)
	rep := bridge.Reconcile(h.world.EVM, h.world.Canton, ledger.TokenisedSymbols(h.world.Seed), domain.SeedTime)
	if !rep.Balanced {
		t.Fatalf("a freshly seeded world does not balance: %+v", rep.Breaks)
	}
	if len(rep.Symbols) != len(ledger.TokenisedSymbols(h.world.Seed)) {
		t.Fatalf("report covers %d symbols", len(rep.Symbols))
	}
	s, ok := rep.Symbol(sym)
	if !ok {
		t.Fatal("no row for the fund share")
	}
	if s.LockedOnSource != 0 || s.BridgedToTarget != 0 {
		t.Fatalf("nothing has been bridged yet: %+v", s)
	}
	if s.Circulating != s.Issued-s.Redeemed {
		t.Fatalf("circulating %d != issued %d less redeemed %d", s.Circulating, s.Issued, s.Redeemed)
	}
	if s.Circulating != s.SourceSupply+s.TargetSupply {
		t.Fatalf("circulating %d != the two supplies %d + %d", s.Circulating, s.SourceSupply, s.TargetSupply)
	}
}

func TestReconcileIsPureAndDeterministic(t *testing.T) {
	h := newHarness(t, 1, 2)
	h.lock("CP-ALPHA", 1_000_000)
	h.world.EVM.Mine(1)
	h.poll()

	syms := ledger.TokenisedSymbols(h.world.Seed)
	headBefore := h.world.EVM.Head()
	offsetBefore := h.world.Canton.Head()

	a := bridge.Reconcile(h.world.EVM, h.world.Canton, syms, domain.SeedTime)
	b := bridge.Reconcile(h.world.Canton, h.world.EVM, syms, domain.SeedTime)
	c := bridge.Reconcile(h.world.EVM, h.world.Canton, syms, domain.SeedTime)

	if len(a.Symbols) != len(c.Symbols) {
		t.Fatal("two identical reconciliations differ in shape")
	}
	for i := range a.Symbols {
		if a.Symbols[i] != c.Symbols[i] {
			t.Fatalf("row %d differs between two identical calls:\n%+v\n%+v", i, a.Symbols[i], c.Symbols[i])
		}
	}
	// Swapping the arguments swaps the direction, not the answer: the reverse
	// view must still balance.
	if !a.Balanced || !b.Balanced {
		t.Fatalf("balanced: forward %v, reverse %v", a.Balanced, b.Balanced)
	}
	if h.world.EVM.Head() != headBefore || h.world.Canton.Head() != offsetBefore {
		t.Fatal("reconciliation moved a ledger; it must be side-effect free")
	}
	// Rows come back in symbol order regardless of the order they were asked
	// for, because a report that changes shape with its argument order cannot
	// be diffed between runs.
	shuffled := append([]string(nil), syms...)
	for i, j := 0, len(shuffled)-1; i < j; i, j = i+1, j-1 {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	d := bridge.Reconcile(h.world.EVM, h.world.Canton, shuffled, domain.SeedTime)
	for i := range a.Symbols {
		if a.Symbols[i].Symbol != d.Symbols[i].Symbol {
			t.Fatalf("row order depends on the argument order: %s vs %s", a.Symbols[i].Symbol, d.Symbols[i].Symbol)
		}
	}
}

func TestUnbackedMintOnTheTargetIsABreak(t *testing.T) {
	h := newHarness(t, 1, 2)
	// A mint carrying a bridge memo that no lock on the source authorised.
	// This is what a compromised relayer key produces, and the control is the
	// thing that catches it without trusting the relayer's own records.
	if _, err := h.world.Canton.Mint(context.Background(), h.world.Issuer, h.party("CP-BRAVO"), sym,
		1_234_000, ledger.BridgeMemo(ledger.BridgeOpMint, 99)); err != nil {
		t.Fatalf("mint: %v", err)
	}
	rep := bridge.Reconcile(h.world.EVM, h.world.Canton, []string{sym}, domain.SeedTime)
	if rep.Balanced {
		t.Fatal("an unbacked bridge mint reported balanced")
	}
	s, _ := rep.Symbol(sym)
	if s.LockDelta != 1_234_000 {
		t.Fatalf("lock delta = %d", s.LockDelta)
	}
	// The same event breaks the issuance identity too: units are circulating
	// that no primary issuance created. Two controls catching one fault from
	// two directions is the point of having both.
	if s.IssuanceDelta != 1_234_000 {
		t.Fatalf("issuance delta = %d", s.IssuanceDelta)
	}
	if len(rep.Breaks) != 2 {
		t.Fatalf("breaks = %d: %+v", len(rep.Breaks), rep.Breaks)
	}
	for _, b := range rep.Breaks {
		if b.Kind != domain.BreakSupplyDiff {
			t.Fatalf("break kind = %s", b.Kind)
		}
		if b.Status != domain.BreakOpen || b.Severity != "critical" {
			t.Fatalf("break = %+v", b)
		}
		if b.Symbol != sym || b.Delta == 0 {
			t.Fatalf("break = %+v", b)
		}
		if !b.Material(1) {
			t.Fatalf("a 1,234,000-share break is not material: %+v", b)
		}
		if len(b.Notes) == 0 {
			t.Fatal("break carries no explanation")
		}
	}
	// Deterministic identifiers: the same fault reconciles to the same break.
	again := bridge.Reconcile(h.world.EVM, h.world.Canton, []string{sym}, domain.SeedTime)
	for i := range rep.Breaks {
		if rep.Breaks[i].ID != again.Breaks[i].ID {
			t.Fatalf("break ids are not stable: %s vs %s", rep.Breaks[i].ID, again.Breaks[i].ID)
		}
	}
}

func TestLockWithoutMintIsABreak(t *testing.T) {
	h := newHarness(t, 4, 2)
	h.lock("CP-ALPHA", 2_500_000)
	// No poll: the lock is on the chain and nothing has been minted against
	// it, which is what an outage of the relayer looks like to the control.
	rep := bridge.Reconcile(h.world.EVM, h.world.Canton, []string{sym}, domain.SeedTime)
	if rep.Balanced {
		t.Fatal("a lock with no matching mint reported balanced")
	}
	s, _ := rep.Symbol(sym)
	if s.LockDelta != -2_500_000 {
		t.Fatalf("lock delta = %d, want the shortfall on the target", s.LockDelta)
	}
	if s.LockedOnSource != 2_500_000 || s.BridgedToTarget != 0 {
		t.Fatalf("row = %+v", s)
	}
}

func TestRedemptionKeepsTheIssuanceIdentity(t *testing.T) {
	h := newHarness(t, 1, 2)
	before := bridge.Reconcile(h.world.EVM, h.world.Canton, []string{sym}, domain.SeedTime)
	b, _ := before.Symbol(sym)

	// A primary redemption: the issuer retires units on the EVM side.
	if _, err := h.world.EVM.Burn(context.Background(), h.world.Operator, h.wallet("CP-ALPHA"), sym,
		1_000_000, "redemption R-1"); err != nil {
		t.Fatalf("burn: %v", err)
	}
	after := bridge.Reconcile(h.world.EVM, h.world.Canton, []string{sym}, domain.SeedTime)
	a, _ := after.Symbol(sym)
	if !after.Balanced {
		t.Fatalf("a primary redemption unbalanced the control: %+v", after.Breaks)
	}
	if a.Redeemed-b.Redeemed != 1_000_000 {
		t.Fatalf("redeemed moved by %d", a.Redeemed-b.Redeemed)
	}
	if b.Circulating-a.Circulating != 1_000_000 {
		t.Fatalf("circulating moved by %d", b.Circulating-a.Circulating)
	}
}

func TestRecordSupplyWritesGauges(t *testing.T) {
	h := newHarness(t, 1, 2)
	reg := obs.NewRegistry()
	rep := bridge.Reconcile(h.world.EVM, h.world.Canton, []string{sym}, domain.SeedTime)
	bridge.RecordSupply(reg, rep)
	if got := reg.Gauge(bridge.MetricSupplyDelta, "", map[string]string{"symbol": sym}).Value(); got != 0 {
		t.Fatalf("supply delta gauge = %v on a balanced world", got)
	}
	bridge.RecordSupply(nil, rep) // a nil registry is tolerated
}
