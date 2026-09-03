package ledger_test

import (
	"context"
	"sync"
	"testing"

	"github.com/udaykishore-resu/tcrp/internal/ledger"
)

// The simulators are shared between the relayer, the risk recomputer and the
// lakehouse feed, all of which run concurrently. These tests exist to be run
// under -race; without it they assert only that nothing deadlocks.

func TestEVMConcurrentWritesConserveSupply(t *testing.T) {
	ctx := context.Background()
	e := newEVM(t, ledger.ComplianceRules{}, 2)
	mustMint(t, e, alice, 10_000)
	mustMint(t, e, bob, 10_000)

	sub := e.Subscribe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range sub {
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				from, to := alice, bob
				if (i+j)%2 == 0 {
					from, to = bob, alice
				}
				// Reverts are expected and are not the point; the point is that
				// concurrent writes leave a consistent ledger.
				_, _ = e.Transfer(ctx, from, from, to, sym, 1, "")
				_ = e.BalanceOf(alice, sym)
				_ = e.Events(0)
				_ = e.Head()
			}
		}(i)
	}
	wg.Wait()

	var sum int64
	for _, h := range e.Holders(sym) {
		sum += e.BalanceOf(h, sym)
	}
	if sum != 20_000 || e.TotalSupply(sym) != 20_000 {
		t.Fatalf("balances %d, supply %d, want 20000 of each", sum, e.TotalSupply(sym))
	}
	if got := len(e.Events(0)); got == 0 {
		t.Fatal("no events recorded")
	}
	// Sequence numbers are unique and in order on the canonical chain.
	seen := map[uint64]bool{}
	var last uint64
	for _, ev := range e.Events(0) {
		if seen[ev.Seq] {
			t.Fatalf("duplicate sequence number %d", ev.Seq)
		}
		if ev.Seq < last {
			t.Fatalf("sequence went backwards: %d after %d", ev.Seq, last)
		}
		seen[ev.Seq], last = true, ev.Seq
	}
}

func TestCantonConcurrentReadsAndWrites(t *testing.T) {
	ctx := context.Background()
	c := newCanton(t)
	issue(t, c, partyA, 10_000)
	issue(t, c, partyB, 10_000)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				from, to := partyA, partyB
				if (i+j)%2 == 0 {
					from, to = partyB, partyA
				}
				_, _ = c.Transfer(ctx, from, from, to, sym, 1, "")
			}
		}(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = c.ContractsVisibleTo(partyA)
				_ = c.TransactionsVisibleTo(issuer)
				_ = c.BalanceOf(partyB, sym)
				_ = c.Events(0)
			}
		}()
	}
	wg.Wait()

	if got := c.TotalSupply(sym); got != 20_000 {
		t.Fatalf("supply = %d after concurrent transfers", got)
	}
	if got := c.BalanceOf(partyA, sym) + c.BalanceOf(partyB, sym); got != 20_000 {
		t.Fatalf("balances sum to %d", got)
	}
	// Every party's projection is still a subset of the participant's view.
	all := len(c.Contracts())
	if got := len(c.ContractsVisibleTo(partyA)); got > all {
		t.Fatalf("a party sees %d contracts of %d in the store", got, all)
	}
}

func TestSlowSubscriberDoesNotBlockTheLedger(t *testing.T) {
	ctx := context.Background()
	e := newEVM(t, ledger.ComplianceRules{}, 1)
	_ = e.Subscribe() // never drained
	mustMint(t, e, alice, 5_000)
	for i := 0; i < 400; i++ {
		if _, err := e.Transfer(ctx, alice, alice, bob, sym, 1, ""); err != nil {
			t.Fatalf("transfer %d: %v", i, err)
		}
	}
	if got := len(e.Events(0)); got != 401 {
		t.Fatalf("events = %d", got)
	}
}
