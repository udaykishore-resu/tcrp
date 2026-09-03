package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/ledger"
)

const (
	issuer  = ledger.DefaultIssuerParty
	partyA  = "Alpha::1220"
	partyB  = "Bravo::1220"
	partyC  = "Charlie::1220"
	auditor = "Auditor::1220" // a stakeholder in nothing
)

func newCanton(t *testing.T) *ledger.Canton {
	t.Helper()
	return ledger.NewCanton(ledger.Options{Clock: testClock()})
}

func issue(t *testing.T, c *ledger.Canton, to string, qty int64) {
	t.Helper()
	if _, err := c.Mint(context.Background(), issuer, to, sym, qty, "primary issuance"); err != nil {
		t.Fatalf("issue to %s: %v", to, err)
	}
}

func TestSelectiveDisclosure(t *testing.T) {
	c := newCanton(t)
	issue(t, c, partyA, 1_000)
	issue(t, c, partyB, 2_000)

	seenBy := func(p string) []ledger.Contract { return c.ContractsVisibleTo(p) }

	a := seenBy(partyA)
	if len(a) != 1 {
		t.Fatalf("CP-A sees %d contracts, want only its own holding", len(a))
	}
	if a[0].Owner != partyA {
		t.Fatalf("CP-A sees a contract owned by %s", a[0].Owner)
	}
	b := seenBy(partyB)
	if len(b) != 1 || b[0].Owner != partyB {
		t.Fatalf("CP-B sees %+v", b)
	}
	// The issuer signs every holding, so it sees both. That is not a
	// privilege bolted on; it follows from being a signatory.
	if got := len(seenBy(issuer)); got != 2 {
		t.Fatalf("issuer sees %d contracts, want 2", got)
	}
	if got := len(seenBy(auditor)); got != 0 {
		t.Fatalf("a party that is a stakeholder in nothing sees %d contracts", got)
	}

	// The store itself knows both, which is what makes the projection a real
	// check rather than an artefact of an empty store.
	if got := len(c.Contracts()); got != 2 {
		t.Fatalf("store holds %d contracts", got)
	}

	// A direct fetch is refused rather than answered emptily.
	bHolding := b[0].ID
	if _, err := c.FetchAs(partyA, bHolding); !errors.Is(err, ledger.ErrNotVisible) {
		t.Fatalf("CP-A fetched CP-B's holding: %v", err)
	}
	if _, err := c.FetchAs(issuer, bHolding); err != nil {
		t.Fatalf("issuer cannot fetch a contract it signed: %v", err)
	}
	if _, err := c.LookupHolding(partyA, partyB, sym); !errors.Is(err, ledger.ErrNotVisible) {
		t.Fatalf("a key lookup leaked CP-B's holding to CP-A: %v", err)
	}
	if _, err := c.LookupHolding(partyB, partyB, sym); err != nil {
		t.Fatalf("CP-B cannot look up its own holding: %v", err)
	}
}

func TestTransactionStreamIsProjected(t *testing.T) {
	c := newCanton(t)
	issue(t, c, partyA, 1_000)
	issue(t, c, partyB, 2_000)

	if got := len(c.Transactions()); got != 2 {
		t.Fatalf("participant sees %d transactions", got)
	}
	if got := len(c.TransactionsVisibleTo(partyA)); got != 1 {
		t.Fatalf("CP-A's stream carries %d transactions, want 1", got)
	}
	if got := len(c.TransactionsVisibleTo(issuer)); got != 2 {
		t.Fatalf("issuer's stream carries %d transactions, want 2", got)
	}
	if got := len(c.TransactionsVisibleTo(auditor)); got != 0 {
		t.Fatalf("an unrelated party's stream carries %d transactions", got)
	}
}

func TestTransferArchivesAndRecreates(t *testing.T) {
	ctx := context.Background()
	c := newCanton(t)
	issue(t, c, partyA, 1_000)
	before := c.ContractsVisibleTo(partyA)[0].ID

	if _, err := c.Transfer(ctx, partyA, partyA, partyB, sym, 400, "settlement"); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if _, err := c.FetchAs(issuer, before); !errors.Is(err, ledger.ErrNoSuchContract) {
		t.Fatalf("the source holding was not archived: %v", err)
	}
	if got := c.BalanceOf(partyA, sym); got != 600 {
		t.Fatalf("sender = %d", got)
	}
	if got := c.BalanceOf(partyB, sym); got != 400 {
		t.Fatalf("recipient = %d", got)
	}
	if got := c.TotalSupply(sym); got != 1_000 {
		t.Fatalf("supply moved on a transfer: %d", got)
	}

	// The tree records one exercise with an archive and two creates under it.
	txs := c.Transactions()
	last := txs[len(txs)-1]
	if len(last.Roots) != 1 || last.Roots[0].Kind != ledger.NodeExercise || last.Roots[0].Choice != "Transfer" {
		t.Fatalf("root = %+v", last.Roots)
	}
	var archives, creates int
	for _, n := range last.Roots[0].Children {
		switch n.Kind {
		case ledger.NodeArchive:
			archives++
		case ledger.NodeCreate:
			creates++
		}
	}
	if archives != 1 || creates != 2 {
		t.Fatalf("tree has %d archives and %d creates, want 1 and 2 (remainder plus recipient)", archives, creates)
	}
}

func TestTransferRequiresTheOwner(t *testing.T) {
	c := newCanton(t)
	issue(t, c, partyA, 100)
	_, err := c.Transfer(context.Background(), partyB, partyA, partyB, sym, 10, "")
	if err == nil {
		t.Fatal("a non-owner moved another party's holding")
	}
	if r, _ := ledger.Revert(err); r != "FundShare: only the owner may transfer" {
		t.Fatalf("reason = %q", r)
	}
}

func TestProposeAccept(t *testing.T) {
	ctx := context.Background()
	c := newCanton(t)
	issue(t, c, partyA, 1_000)

	pid, _, err := c.ProposeTransfer(ctx, partyA, partyB, sym, 300, "")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	// The units leave the sender immediately, so they cannot be proposed twice.
	if got := c.BalanceOf(partyA, sym); got != 700 {
		t.Fatalf("sender = %d while a proposal is outstanding", got)
	}
	if got := c.BalanceOf(partyB, sym); got != 0 {
		t.Fatalf("recipient credited before accepting: %d", got)
	}
	// The recipient can see what it is being offered; a third party cannot.
	if _, err := c.FetchAs(partyB, pid); err != nil {
		t.Fatalf("recipient cannot see the proposal: %v", err)
	}
	if _, err := c.FetchAs(partyC, pid); !errors.Is(err, ledger.ErrNotVisible) {
		t.Fatalf("an unrelated party saw the proposal: %v", err)
	}
	// Only the recipient may accept.
	if _, err := c.AcceptTransfer(ctx, partyC, pid, ""); err == nil {
		t.Fatal("a third party accepted somebody else's proposal")
	}
	if _, err := c.AcceptTransfer(ctx, partyB, pid, "accepted"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if got := c.BalanceOf(partyB, sym); got != 300 {
		t.Fatalf("recipient = %d after accepting", got)
	}
	if _, err := c.AcceptTransfer(ctx, partyB, pid, ""); err == nil {
		t.Fatal("an archived proposal was accepted a second time")
	}
	if got := c.TotalSupply(sym); got != 1_000 {
		t.Fatalf("supply = %d through a propose-accept round trip", got)
	}
}

func TestRejectReturnsTheUnits(t *testing.T) {
	ctx := context.Background()
	c := newCanton(t)
	issue(t, c, partyA, 500)
	pid, _, err := c.ProposeTransfer(ctx, partyA, partyB, sym, 200, "")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if _, err := c.RejectTransfer(ctx, partyB, pid, "declined"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got := c.BalanceOf(partyA, sym); got != 500 {
		t.Fatalf("sender = %d after a rejection", got)
	}
	if got := c.BalanceOf(partyB, sym); got != 0 {
		t.Fatalf("recipient = %d after rejecting", got)
	}
}

func TestPledgeMakesThePledgeeAnObserver(t *testing.T) {
	ctx := context.Background()
	c := newCanton(t)
	issue(t, c, partyA, 1_000)

	pid, _, err := c.Pledge(ctx, partyA, partyB, sym, 600, "repo NS-1")
	if err != nil {
		t.Fatalf("pledge: %v", err)
	}
	k, err := c.FetchAs(partyB, pid)
	if err != nil {
		t.Fatalf("the pledgee cannot see the collateral it holds: %v", err)
	}
	if k.Template != ledger.TemplatePledge || k.Pledgee != partyB || k.Owner != partyA {
		t.Fatalf("pledge = %+v", k)
	}
	if _, err := c.FetchAs(partyC, pid); !errors.Is(err, ledger.ErrNotVisible) {
		t.Fatalf("an unrelated party saw the pledge: %v", err)
	}
	// A pledge encumbers; it does not transfer. The owner's balance is intact.
	if got := c.BalanceOf(partyA, sym); got != 1_000 {
		t.Fatalf("pledging moved the owner's balance to %d", got)
	}
	if got := c.BalanceOf(partyB, sym); got != 0 {
		t.Fatalf("the pledgee acquired a balance of %d", got)
	}
	// Only the pledgee releases.
	if _, err := c.Release(ctx, partyA, pid, ""); err == nil {
		t.Fatal("the pledgor released its own collateral")
	}
	if _, err := c.Release(ctx, partyB, pid, "closed"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := c.BalanceOf(partyA, sym); got != 1_000 {
		t.Fatalf("owner = %d after release", got)
	}
	if got := len(c.ContractsVisibleTo(partyB)); got != 0 {
		t.Fatalf("the pledgee still sees %d contracts after releasing", got)
	}
}

func TestOnlyTheIssuerIssuesAndRedeems(t *testing.T) {
	ctx := context.Background()
	c := newCanton(t)
	if _, err := c.Mint(ctx, partyA, partyA, sym, 100, ""); err == nil {
		t.Fatal("a holder issued shares to itself")
	}
	issue(t, c, partyA, 100)
	if _, err := c.Burn(ctx, partyA, partyA, sym, 50, ""); err == nil {
		t.Fatal("a holder redeemed its own shares")
	}
	if _, err := c.Burn(ctx, issuer, partyA, sym, 50, "redemption"); err != nil {
		t.Fatalf("burn: %v", err)
	}
	if got := c.TotalSupply(sym); got != 50 {
		t.Fatalf("supply = %d", got)
	}
	if _, err := c.Burn(ctx, issuer, partyA, sym, 999, ""); err == nil {
		t.Fatal("redeemed more than the holder held")
	}
}

func TestCantonEventsAreFinalOnCommit(t *testing.T) {
	c := newCanton(t)
	ch := c.Subscribe()
	issue(t, c, partyA, 100)
	evs := c.Events(0)
	if len(evs) != 1 {
		t.Fatalf("events = %d", len(evs))
	}
	e := evs[0]
	if e.Ledger != domain.LedgerCanton || e.Kind != domain.EventMint || !e.Finalised {
		t.Fatalf("event = %+v", e)
	}
	if e.To != partyA || e.OccurredAt.IsZero() || e.ObservedAt.IsZero() {
		t.Fatalf("event = %+v", e)
	}
	if !c.Head().Finalised || c.Head().Height != 1 {
		t.Fatalf("head = %+v", c.Head())
	}
	select {
	case got := <-ch:
		if got.ID() != e.ID() {
			t.Fatalf("subscriber got %s, table has %s", got.ID(), e.ID())
		}
	default:
		t.Fatal("no event delivered to the subscriber")
	}
}

func TestCantonLockAndUnlock(t *testing.T) {
	ctx := context.Background()
	c := newCanton(t)
	issue(t, c, partyA, 1_000)
	if _, err := c.Lock(ctx, partyA, partyA, sym, 100, ""); err == nil {
		t.Fatal("a holder operated the bridge vault")
	}
	if _, err := c.Lock(ctx, issuer, partyA, sym, 400, ledger.BridgeMemo(ledger.BridgeOpLock, 3)); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if got := c.Locked(sym); got != 400 {
		t.Fatalf("locked = %d", got)
	}
	if got := c.TotalSupply(sym); got != 1_000 {
		t.Fatalf("locking changed supply: %d", got)
	}
	if _, err := c.Unlock(ctx, issuer, partyA, sym, 400, ledger.BridgeMemo(ledger.BridgeOpUnlock, 3)); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if got := c.Locked(sym); got != 0 || c.BalanceOf(partyA, sym) != 1_000 {
		t.Fatalf("locked = %d, holder = %d", got, c.BalanceOf(partyA, sym))
	}
}

var _ ledger.Ledger = (*ledger.Canton)(nil)
