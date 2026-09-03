package bridge_test

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/bridge"
	"github.com/udaykishore-resu/tcrp/internal/domain"
)

// redeemerFor builds a burn-and-unlock redeemer over the same world as the
// harness's relayer, so a round trip can be driven end to end.
func (h *harness) redeemerFor(t *testing.T, quorum int) *bridge.Redeemer {
	t.Helper()
	set, err := bridge.SignerSetFrom(quorum, h.signers...)
	if err != nil {
		t.Fatalf("signer set: %v", err)
	}
	r, err := bridge.NewRedeemer(bridge.Config{
		Source: h.world.EVM, Target: h.world.Canton, Symbols: []string{sym},
		Signers: h.signers, SignerSet: set,
		SourceOperator: h.world.Operator, TargetOperator: h.world.Issuer,
		Clock: h.clock, Metrics: h.registry, Audit: h.log,
		Actor: "bridge-redeemer", Tenant: "platform",
		Retry: bridge.RetryPolicy{Attempts: 3, Base: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("redeemer: %v", err)
	}
	r.SetSourceAddress(func(cp string) string { return h.wallet(cp) })
	return r
}

func TestBurnAndUnlockRoundTrip(t *testing.T) {
	h := newHarness(t, 1, 2)
	red := h.redeemerFor(t, 2)
	ctx := context.Background()
	party, wallet := h.party("CP-ALPHA"), h.wallet("CP-ALPHA")

	evmBefore := h.world.EVM.BalanceOf(wallet, sym)
	cantonBefore := h.world.Canton.BalanceOf(party, sym)

	// Outbound: lock on the EVM side, mint on Canton.
	h.lock("CP-ALPHA", 6_000_000)
	h.world.EVM.Mine(1)
	if len(h.poll().Settled) != 1 {
		t.Fatal("outbound leg did not settle")
	}
	if got := h.world.EVM.BalanceOf(wallet, sym); got != evmBefore-6_000_000 {
		t.Fatalf("source balance = %d", got)
	}
	if got := h.world.Canton.BalanceOf(party, sym); got != cantonBefore+6_000_000 {
		t.Fatalf("target balance = %d", got)
	}

	// Return: burn on Canton, unlock on the EVM side.
	nonce, _, err := red.Burn(ctx, party, sym, 6_000_000)
	if err != nil {
		t.Fatalf("burn: %v", err)
	}
	if nonce != 1 {
		t.Fatalf("first burn nonce = %d", nonce)
	}
	res, err := red.Poll(ctx)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(res.Settled) != 1 {
		t.Fatalf("return leg did not settle: %+v", res)
	}
	s := res.Settled[0]
	if s.Direction != bridge.DirectionRedeem || s.Attestation.SourceLedger != domain.LedgerCanton {
		t.Fatalf("settlement = %+v", s)
	}
	if s.Attestation.TargetLedger != domain.LedgerEVM || s.Attestation.Holder != "CP-ALPHA" {
		t.Fatalf("attestation = %+v", s.Attestation)
	}

	if got := h.world.EVM.BalanceOf(wallet, sym); got != evmBefore {
		t.Fatalf("source balance after the round trip = %d, want the original %d", got, evmBefore)
	}
	if got := h.world.Canton.BalanceOf(party, sym); got != cantonBefore {
		t.Fatalf("target balance after the round trip = %d, want the original %d", got, cantonBefore)
	}
	if got := h.world.EVM.Locked(sym); got != 0 {
		t.Fatalf("the vault still holds %d after the round trip", got)
	}
	rep := bridge.Reconcile(h.world.EVM, h.world.Canton, []string{sym}, domain.SeedTime)
	if !rep.Balanced {
		t.Fatalf("supply does not balance after a round trip: %+v", rep.Breaks)
	}
	if r, _ := rep.Symbol(sym); r.BridgedToTarget != 0 {
		t.Fatalf("bridged outstanding = %d after redeeming everything", r.BridgedToTarget)
	}
}

func TestRedeemIsReplaySafe(t *testing.T) {
	h := newHarness(t, 1, 2)
	red := h.redeemerFor(t, 2)
	ctx := context.Background()
	party, wallet := h.party("CP-BRAVO"), h.wallet("CP-BRAVO")

	// CP-BRAVO holds on Canton and nothing on the EVM side, so give the vault
	// something to unlock from: lock CP-ALPHA's units, then redeem CP-BRAVO's.
	h.lock("CP-ALPHA", 5_000_000)
	h.world.EVM.Mine(1)
	h.poll()

	evmBefore := h.world.EVM.BalanceOf(wallet, sym)
	if _, _, err := red.Burn(ctx, party, sym, 5_000_000); err != nil {
		t.Fatalf("burn: %v", err)
	}
	if len(mustPoll(t, red).Settled) != 1 {
		t.Fatal("did not settle")
	}
	after := h.world.EVM.BalanceOf(wallet, sym)
	if after != evmBefore+5_000_000 {
		t.Fatalf("unlocked to %d, want %d", after, evmBefore+5_000_000)
	}
	// Every subsequent poll is a no-op, including one that re-reads the same
	// burn event.
	for i := 0; i < 3; i++ {
		if res := mustPoll(t, red); len(res.Settled) != 0 {
			t.Fatalf("poll %d settled again: %+v", i, res)
		}
	}
	if got := h.world.EVM.BalanceOf(wallet, sym); got != after {
		t.Fatalf("balance drifted to %d over repeated polls", got)
	}
	if got := h.world.EVM.Locked(sym); got != 0 {
		t.Fatalf("vault = %d", got)
	}
	if len(red.Alerts()) != 0 {
		t.Fatalf("alerts = %+v", red.Alerts())
	}
}

func TestRedeemHoldsWithoutAQuorum(t *testing.T) {
	h := newHarness(t, 1, 2)
	red := h.redeemerFor(t, 3) // 3 of 3
	ctx := context.Background()
	party := h.party("CP-ALPHA")

	h.lock("CP-ALPHA", 1_000_000)
	h.world.EVM.Mine(1)
	h.poll()

	h.signers[0].SetAvailable(false)
	if _, _, err := red.Burn(ctx, party, sym, 1_000_000); err != nil {
		t.Fatalf("burn: %v", err)
	}
	res := mustPoll(t, red)
	if len(res.Settled) != 0 || res.Waiting != 1 {
		t.Fatalf("settled without a quorum: %+v", res)
	}
	if got := h.world.EVM.Locked(sym); got != 1_000_000 {
		t.Fatalf("units were unlocked without a quorum: vault = %d", got)
	}
	alerts := red.Alerts()
	if len(alerts) != 1 || alerts[0].Kind != bridge.AlertQuorumUnavailable {
		t.Fatalf("alerts = %+v", alerts)
	}
	// The burn has already happened, so until the quorum returns the supply
	// control shows the shortfall. That is correct: units really are missing
	// from the target with nothing released on the source.
	rep := bridge.Reconcile(h.world.EVM, h.world.Canton, []string{sym}, domain.SeedTime)
	if rep.Balanced {
		t.Fatal("a burn awaiting its unlock reported balanced")
	}

	h.signers[0].SetAvailable(true)
	if len(mustPoll(t, red).Settled) != 1 {
		t.Fatal("did not settle once the signer returned")
	}
	if !bridge.Reconcile(h.world.EVM, h.world.Canton, []string{sym}, domain.SeedTime).Balanced {
		t.Fatal("supply still unbalanced after the unlock")
	}
}

func TestRedeemAuditsBothHalves(t *testing.T) {
	h := newHarness(t, 1, 2)
	red := h.redeemerFor(t, 2)
	ctx := context.Background()

	h.lock("CP-ALPHA", 2_000_000)
	h.world.EVM.Mine(1)
	h.poll()
	if _, _, err := red.Burn(ctx, h.party("CP-ALPHA"), sym, 2_000_000); err != nil {
		t.Fatalf("burn: %v", err)
	}
	mustPoll(t, red)

	counts := map[audit.Action]int{}
	for _, r := range h.log.Records() {
		counts[r.Action]++
	}
	if counts[audit.ActionTokensBurned] != 1 {
		t.Fatalf("token burn audited %d times", counts[audit.ActionTokensBurned])
	}
	if counts[audit.ActionTransferExecuted] != 1 {
		t.Fatalf("unlock audited %d times", counts[audit.ActionTransferExecuted])
	}
	if err := h.log.Verify(); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	for _, r := range h.log.Records() {
		if r.Action != audit.ActionTransferExecuted {
			continue
		}
		if r.Details["direction"] != string(bridge.DirectionRedeem) {
			t.Fatalf("unlock record does not name its direction: %+v", r.Details)
		}
	}
}

func TestRedeemerRejectsAnUnrelayedSymbol(t *testing.T) {
	h := newHarness(t, 1, 2)
	red := h.redeemerFor(t, 2)
	if _, _, err := red.Burn(context.Background(), h.party("CP-ECHO"), "USDC", 1); err == nil {
		t.Fatal("a symbol the redeemer does not relay was burned")
	}
}

func mustPoll(t *testing.T, r *bridge.Redeemer) bridge.PollResult {
	t.Helper()
	res, err := r.Poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	return res
}
