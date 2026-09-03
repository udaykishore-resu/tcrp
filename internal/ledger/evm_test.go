package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/ledger"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

const (
	alice = "0xa1f0000000000000000000000000000000000001"
	bob   = "0xa1f0000000000000000000000000000000000002"
	carol = "0xa1f0000000000000000000000000000000000003"
	mallo = "0xa1f00000000000000000000000000000000000ff" // never registered
	admin = "0x00000000000000000000000000000000000a9e07"
	sym   = domain.FundSymbol
)

func testClock() *obs.ManualClock { return obs.NewManualClock(domain.SeedTime) }

func newEVM(t *testing.T, rules ledger.ComplianceRules, confs uint64) *ledger.EVM {
	t.Helper()
	e := ledger.NewEVM(admin, ledger.Options{
		Clock: testClock(), Confirmations: confs, Compliance: rules,
		ObservationLag: 3 * time.Second,
	})
	for _, a := range []struct{ addr, country string }{{alice, "US"}, {bob, "GB"}, {carol, "LU"}} {
		if err := e.Register(admin, ledger.Identity{
			Address: a.addr, Country: a.country,
			Claims: []string{ledger.ClaimKYC, ledger.ClaimAccredited},
		}); err != nil {
			t.Fatalf("register %s: %v", a.addr, err)
		}
	}
	return e
}

func mustMint(t *testing.T, e *ledger.EVM, to string, qty int64) ledger.Receipt {
	t.Helper()
	rc, err := e.Mint(context.Background(), admin, to, sym, qty, "primary issuance")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return rc
}

func TestTransferToUnregisteredHolderReverts(t *testing.T) {
	e := newEVM(t, ledger.ComplianceRules{}, 2)
	mustMint(t, e, alice, 1_000)

	rc, err := e.Transfer(context.Background(), alice, alice, mallo, sym, 100, "")
	if err == nil {
		t.Fatal("transfer to an unregistered address succeeded; the identity registry is not enforced")
	}
	if !errors.Is(err, ledger.ErrReverted) {
		t.Fatalf("want a revert, got %v", err)
	}
	reason, ok := ledger.Revert(err)
	if !ok || reason != "IdentityRegistry: recipient is not verified" {
		t.Fatalf("revert reason = %q", reason)
	}
	if rc.Success {
		t.Fatal("receipt reports success on a reverted transfer")
	}
	if got := e.BalanceOf(alice, sym); got != 1_000 {
		t.Fatalf("sender balance moved on a reverted transfer: %d", got)
	}
	if got := e.BalanceOf(mallo, sym); got != 0 {
		t.Fatalf("unregistered address received %d", got)
	}
	if got := e.TotalSupply(sym); got != 1_000 {
		t.Fatalf("supply = %d, want 1000", got)
	}
}

func TestMintBurnSupply(t *testing.T) {
	e := newEVM(t, ledger.ComplianceRules{}, 1)
	mustMint(t, e, alice, 5_000)
	mustMint(t, e, bob, 2_500)
	if got := e.TotalSupply(sym); got != 7_500 {
		t.Fatalf("supply = %d", got)
	}
	if _, err := e.Burn(context.Background(), admin, alice, sym, 1_500, "redemption"); err != nil {
		t.Fatalf("burn: %v", err)
	}
	if got := e.TotalSupply(sym); got != 6_000 {
		t.Fatalf("supply after burn = %d", got)
	}
	if got := e.BalanceOf(alice, sym); got != 3_500 {
		t.Fatalf("alice = %d", got)
	}
	var sum int64
	for _, h := range e.Holders(sym) {
		sum += e.BalanceOf(h, sym)
	}
	if sum != e.TotalSupply(sym) {
		t.Fatalf("sum of balances %d != supply %d", sum, e.TotalSupply(sym))
	}
}

func TestMintRequiresAgentRole(t *testing.T) {
	e := newEVM(t, ledger.ComplianceRules{}, 1)
	_, err := e.Mint(context.Background(), alice, alice, sym, 1, "")
	if err == nil {
		t.Fatal("a non-agent minted")
	}
	if reason, _ := ledger.Revert(err); reason != "AccessControl: caller is missing AGENT_ROLE" {
		t.Fatalf("reason = %q", reason)
	}
	if err := e.GrantRole(alice, ledger.RoleAgent, alice); !errors.Is(err, ledger.ErrUnauthorised) {
		t.Fatalf("a non-owner granted a role: %v", err)
	}
	if err := e.GrantRole(admin, ledger.RoleAgent, alice); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := e.Mint(context.Background(), alice, alice, sym, 1, ""); err != nil {
		t.Fatalf("mint after grant: %v", err)
	}
}

func TestComplianceCeilings(t *testing.T) {
	e := newEVM(t, ledger.ComplianceRules{MaxHolders: 2, MaxPerInvestor: 1_000}, 1)
	mustMint(t, e, alice, 1_000)
	mustMint(t, e, bob, 500)

	if _, err := e.Mint(context.Background(), admin, carol, sym, 1, ""); err == nil {
		t.Fatal("third holder admitted under a two-holder cap")
	} else if r, _ := ledger.Revert(err); r != "Compliance: max holders exceeded" {
		t.Fatalf("reason = %q", r)
	}
	if _, err := e.Mint(context.Background(), admin, bob, sym, 501, ""); err == nil {
		t.Fatal("per-investor cap not enforced")
	} else if r, _ := ledger.Revert(err); r != "Compliance: max balance per investor exceeded" {
		t.Fatalf("reason = %q", r)
	}
	// Exactly at the ceiling is allowed; one over is not.
	if _, err := e.Mint(context.Background(), admin, bob, sym, 500, ""); err != nil {
		t.Fatalf("mint to the ceiling: %v", err)
	}
}

func TestCountryAndClaimRestrictions(t *testing.T) {
	e := newEVM(t, ledger.ComplianceRules{AllowedCountries: []string{"US", "GB"}}, 1)
	mustMint(t, e, alice, 1_000)
	if _, err := e.Transfer(context.Background(), alice, alice, carol, sym, 10, ""); err == nil {
		t.Fatal("transfer into a disallowed country succeeded")
	} else if r, _ := ledger.Revert(err); r != "Compliance: recipient country is not allowed" {
		t.Fatalf("reason = %q", r)
	}

	if err := e.SetCompliance(admin, ledger.ComplianceRules{RequiredClaims: []string{"professional"}}); err != nil {
		t.Fatalf("set compliance: %v", err)
	}
	if _, err := e.Transfer(context.Background(), alice, alice, bob, sym, 10, ""); err == nil {
		t.Fatal("transfer without the required claim succeeded")
	} else if r, _ := ledger.Revert(err); r != "Compliance: recipient is missing claim professional" {
		t.Fatalf("reason = %q", r)
	}
}

func TestFreezeAndPause(t *testing.T) {
	ctx := context.Background()
	e := newEVM(t, ledger.ComplianceRules{}, 1)
	mustMint(t, e, alice, 1_000)
	mustMint(t, e, bob, 1_000)

	if _, err := e.Freeze(ctx, admin, alice, sym, "sanctions screen hit"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if !e.Frozen(alice) {
		t.Fatal("freeze did not take")
	}
	if _, err := e.Transfer(ctx, alice, alice, bob, sym, 1, ""); err == nil {
		t.Fatal("frozen account transferred")
	} else if r, _ := ledger.Revert(err); r != "Token: sender account is frozen" {
		t.Fatalf("reason = %q", r)
	}
	if _, err := e.Transfer(ctx, bob, bob, alice, sym, 1, ""); err == nil {
		t.Fatal("transfer to a frozen account succeeded")
	}
	if _, err := e.Unfreeze(ctx, admin, alice, sym, "cleared"); err != nil {
		t.Fatalf("unfreeze: %v", err)
	}
	if _, err := e.Transfer(ctx, alice, alice, bob, sym, 1, ""); err != nil {
		t.Fatalf("transfer after unfreeze: %v", err)
	}

	if err := e.Pause(admin); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := e.Transfer(ctx, alice, alice, bob, sym, 1, ""); err == nil {
		t.Fatal("transfer while paused")
	} else if r, _ := ledger.Revert(err); r != "Pausable: token is paused" {
		t.Fatalf("reason = %q", r)
	}
	// Recovery has to work while the token is paused, because a pause is often
	// exactly why recovery is being run.
	if _, err := e.ForcedTransfer(ctx, admin, alice, bob, sym, 50, "key recovery"); err != nil {
		t.Fatalf("forced transfer while paused: %v", err)
	}
	if err := e.Unpause(admin); err != nil {
		t.Fatalf("unpause: %v", err)
	}
}

func TestForcedTransferStillRequiresVerifiedRecipient(t *testing.T) {
	ctx := context.Background()
	e := newEVM(t, ledger.ComplianceRules{}, 1)
	mustMint(t, e, alice, 100)
	if _, err := e.ForcedTransfer(ctx, admin, alice, mallo, sym, 10, "recovery"); err == nil {
		t.Fatal("recovery moved tokens to an unregistered address")
	} else if r, _ := ledger.Revert(err); r != "IdentityRegistry: recipient is not verified" {
		t.Fatalf("reason = %q", r)
	}
}

func TestFinalityFollowsConfirmations(t *testing.T) {
	e := newEVM(t, ledger.ComplianceRules{}, 3)
	rc := mustMint(t, e, alice, 100)

	evs := e.Events(0)
	if len(evs) != 1 {
		t.Fatalf("events = %d", len(evs))
	}
	if evs[0].Finalised {
		t.Fatal("a block at the head reports final under a 3-confirmation policy")
	}
	if evs[0].Block != rc.Block {
		t.Fatalf("event block %d != receipt block %d", evs[0].Block, rc.Block)
	}
	e.Mine(2)
	if e.Events(0)[0].Finalised {
		t.Fatal("final after two confirmations under a three-confirmation policy")
	}
	e.Mine(1)
	if !e.Events(0)[0].Finalised {
		t.Fatal("not final after three confirmations")
	}
	if got := e.FinalisedHeight(); got != rc.Block {
		t.Fatalf("finalised height = %d, want %d", got, rc.Block)
	}
}

func TestEventShapeMatchesLakehouseExpectations(t *testing.T) {
	e := newEVM(t, ledger.ComplianceRules{}, 1)
	mustMint(t, e, alice, 100)
	ev := e.Events(0)[0]
	if ev.Ledger != domain.LedgerEVM || ev.Kind != domain.EventMint || ev.Symbol != sym {
		t.Fatalf("event = %+v", ev)
	}
	if ev.To != alice || ev.From != "" {
		t.Fatalf("mint event has from=%q to=%q", ev.From, ev.To)
	}
	if ev.OccurredAt.IsZero() || ev.ObservedAt.IsZero() {
		t.Fatal("occurred_at or observed_at unset")
	}
	if !ev.ObservedAt.After(ev.OccurredAt) {
		t.Fatalf("observed_at %v is not after occurred_at %v", ev.ObservedAt, ev.OccurredAt)
	}
	if ev.TxHash == "" || ev.ID() == "" {
		t.Fatal("event has no natural key")
	}
}

func TestHolderLabelsResolveToCounterparties(t *testing.T) {
	e := ledger.NewEVM(admin, ledger.Options{
		Clock: testClock(), Confirmations: 1,
		HolderLabels: map[string]string{alice: "CP-ALPHA"},
	})
	if err := e.Register(admin, ledger.Identity{Address: alice, Country: "US"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	mustMint(t, e, alice, 10)
	if got := e.Events(0)[0].To; got != "CP-ALPHA" {
		t.Fatalf("event To = %q, want the counterparty label", got)
	}
}

func TestSubscribeReceivesEvents(t *testing.T) {
	e := newEVM(t, ledger.ComplianceRules{}, 1)
	ch := e.Subscribe()
	mustMint(t, e, alice, 10)
	select {
	case ev := <-ch:
		if ev.Kind != domain.EventMint {
			t.Fatalf("kind = %s", ev.Kind)
		}
	default:
		t.Fatal("no event delivered to the subscriber")
	}
}

func TestReorgRewindsUnfinalisedAndReapplies(t *testing.T) {
	ctx := context.Background()
	e := newEVM(t, ledger.ComplianceRules{}, 3)
	mustMint(t, e, alice, 1_000)
	e.Mine(5) // put the mint beyond reorg range

	rc1, err := e.Transfer(ctx, alice, alice, bob, sym, 100, "drop me")
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	rc2, err := e.Transfer(ctx, alice, alice, carol, sym, 200, "keep me")
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	before := e.Head()
	seqBefore := seqOf(t, e, rc2.TxHash)

	res, err := e.Reorg(2, rc1.TxHash)
	if err != nil {
		t.Fatalf("reorg: %v", err)
	}
	if res.ViolatedFinality {
		t.Fatal("a two-block rewind under a three-confirmation policy reported a finality violation")
	}
	if len(res.Orphaned) != 1 || res.Orphaned[0].TxHash != rc1.TxHash {
		t.Fatalf("orphaned = %+v", res.Orphaned)
	}
	if len(res.Reapplied) != 1 || res.Reapplied[0].TxHash != rc2.TxHash {
		t.Fatalf("reapplied = %+v", res.Reapplied)
	}
	if res.Reapplied[0].Block == rc2.Block {
		t.Fatalf("a re-included transaction kept its old height %d", rc2.Block)
	}
	if res.To.Height != before.Height-1 {
		t.Fatalf("head = %d, want one block shorter than %d", res.To.Height, before.Height)
	}
	if res.Reapplied[0].Seq != seqBefore {
		t.Fatalf("re-included seq = %d, want the original %d", res.Reapplied[0].Seq, seqBefore)
	}
	if got := e.BalanceOf(bob, sym); got != 0 {
		t.Fatalf("dropped transfer left %d with bob", got)
	}
	if got := e.BalanceOf(carol, sym); got != 200 {
		t.Fatalf("retained transfer lost: carol = %d", got)
	}
	if got := e.BalanceOf(alice, sym); got != 800 {
		t.Fatalf("alice = %d, want 800", got)
	}
	// The canonical chain no longer mentions the dropped transaction.
	for _, ev := range e.Events(0) {
		if ev.TxHash == rc1.TxHash {
			t.Fatal("an orphaned transaction is still on the canonical chain")
		}
	}
	var sum int64
	for _, h := range e.Holders(sym) {
		sum += e.BalanceOf(h, sym)
	}
	if sum != e.TotalSupply(sym) {
		t.Fatalf("balances %d != supply %d after reorg", sum, e.TotalSupply(sym))
	}
}

// seqOf finds the sequence number the canonical chain carries for a
// transaction hash.
func seqOf(t *testing.T, e *ledger.EVM, hash string) uint64 {
	t.Helper()
	for _, ev := range e.Events(0) {
		if ev.TxHash == hash {
			return ev.Seq
		}
	}
	t.Fatalf("transaction %s is not on the canonical chain", hash)
	return 0
}

func TestReorgPastFinalityIsReported(t *testing.T) {
	ctx := context.Background()
	e := newEVM(t, ledger.ComplianceRules{}, 1)
	mustMint(t, e, alice, 1_000)
	rc, err := e.Transfer(ctx, alice, alice, bob, sym, 100, "")
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	e.Mine(4)
	if !e.Events(0)[1].Finalised {
		t.Fatal("transfer is not final; the test cannot exercise a finality violation")
	}
	res, err := e.Reorg(5, rc.TxHash)
	if err != nil {
		t.Fatalf("reorg: %v", err)
	}
	if !res.ViolatedFinality {
		t.Fatal("a rewind past the confirmation depth did not report a finality violation")
	}
	if got := e.BalanceOf(bob, sym); got != 0 {
		t.Fatalf("bob still holds %d after the transfer was orphaned", got)
	}
}

func TestReorgDepthOutOfRange(t *testing.T) {
	e := newEVM(t, ledger.ComplianceRules{}, 1)
	if _, err := e.Reorg(0); !errors.Is(err, ledger.ErrBadReorg) {
		t.Fatalf("depth 0: %v", err)
	}
	if _, err := e.Reorg(99); !errors.Is(err, ledger.ErrBadReorg) {
		t.Fatalf("depth beyond the chain: %v", err)
	}
}

func TestReorgOrphansDependentTransactions(t *testing.T) {
	ctx := context.Background()
	e := newEVM(t, ledger.ComplianceRules{}, 5)
	mustMint(t, e, alice, 1_000)
	rc1, err := e.Transfer(ctx, alice, alice, bob, sym, 500, "funds bob")
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if _, err := e.Transfer(ctx, bob, bob, carol, sym, 500, "spends what bob was funded with"); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	res, err := e.Reorg(2, rc1.TxHash)
	if err != nil {
		t.Fatalf("reorg: %v", err)
	}
	if len(res.Orphaned) != 2 {
		t.Fatalf("orphaned = %d, want both the dropped transfer and the one that depended on it", len(res.Orphaned))
	}
	if got := e.BalanceOf(carol, sym); got != 0 {
		t.Fatalf("carol = %d after the funding transaction was orphaned", got)
	}
	if got := e.BalanceOf(alice, sym); got != 1_000 {
		t.Fatalf("alice = %d, want the full opening balance back", got)
	}
}

func TestDeterministicReplay(t *testing.T) {
	run := func() []domain.LedgerEvent {
		e := newEVM(t, ledger.ComplianceRules{}, 2)
		mustMint(t, e, alice, 1_000)
		if _, err := e.Transfer(context.Background(), alice, alice, bob, sym, 250, "a"); err != nil {
			t.Fatalf("transfer: %v", err)
		}
		if _, err := e.Lock(context.Background(), admin, bob, sym, 100, ledger.BridgeMemo(ledger.BridgeOpLock, 1)); err != nil {
			t.Fatalf("lock: %v", err)
		}
		e.Mine(3)
		return e.Events(0)
	}
	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatalf("event counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("event %d differs:\n%+v\n%+v", i, a[i], b[i])
		}
	}
}

func TestLockMovesUnitsIntoTheVault(t *testing.T) {
	ctx := context.Background()
	e := newEVM(t, ledger.ComplianceRules{MaxHolders: 2}, 1)
	mustMint(t, e, alice, 1_000)
	// The vault is exempt from the holder ceiling; if it were not, locking
	// would fail here and the bridge would be unusable on a capped fund.
	if _, err := e.Lock(ctx, admin, alice, sym, 400, ledger.BridgeMemo(ledger.BridgeOpLock, 7)); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if got := e.Locked(sym); got != 400 {
		t.Fatalf("locked = %d", got)
	}
	if got := e.BalanceOf(alice, sym); got != 600 {
		t.Fatalf("alice = %d", got)
	}
	if got := e.TotalSupply(sym); got != 1_000 {
		t.Fatalf("locking changed supply: %d", got)
	}
	ev := e.Events(0)[1]
	if ev.Kind != domain.EventLock || ev.From != alice || ev.To != "" {
		t.Fatalf("lock event = %+v", ev)
	}
	op, nonce, ok := ledger.ParseBridgeMemo(ev.Memo)
	if !ok || op != ledger.BridgeOpLock || nonce != 7 {
		t.Fatalf("memo %q parsed as op=%q nonce=%d ok=%v", ev.Memo, op, nonce, ok)
	}
	if _, err := e.Unlock(ctx, admin, alice, sym, 400, ledger.BridgeMemo(ledger.BridgeOpUnlock, 7)); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if got := e.Locked(sym); got != 0 {
		t.Fatalf("locked after unlock = %d", got)
	}
}

func TestParseBridgeMemoRejectsAnythingElse(t *testing.T) {
	for _, m := range []string{"", "primary issuance", "bridge:lock", "bridge:teleport:1", "bridge:lock:x"} {
		if _, _, ok := ledger.ParseBridgeMemo(m); ok {
			t.Fatalf("memo %q parsed as a bridge memo", m)
		}
	}
}

var _ ledger.Ledger = (*ledger.EVM)(nil)
