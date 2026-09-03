package ledger_test

import (
	"context"
	"testing"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/ledger"
)

func newWorld(t *testing.T) *ledger.World {
	t.Helper()
	w, err := ledger.NewWorld(ledger.Options{Clock: testClock(), Confirmations: 3})
	if err != nil {
		t.Fatalf("new world: %v", err)
	}
	return w
}

func TestWorldReproducesSeededBalances(t *testing.T) {
	w := newWorld(t)
	for _, p := range w.Seed.Positions {
		switch p.Ledger {
		case domain.LedgerEVM:
			addr, ok := w.Wallet(p.Counterparty)
			if !ok {
				t.Fatalf("%s has no wallet", p.Counterparty)
			}
			if got := w.EVM.BalanceOf(addr, p.Symbol); got != p.Quantity {
				t.Fatalf("EVM %s/%s = %d, seed says %d", p.Counterparty, p.Symbol, got, p.Quantity)
			}
		case domain.LedgerCanton:
			party, ok := w.Party(p.Counterparty)
			if !ok {
				t.Fatalf("%s has no party", p.Counterparty)
			}
			if got := w.Canton.BalanceOf(party, p.Symbol); got != p.Quantity {
				t.Fatalf("canton %s/%s = %d, seed says %d", p.Counterparty, p.Symbol, got, p.Quantity)
			}
		}
	}
}

func TestWorldLeavesTheUnwhitelistedCounterpartyOutOfTheRegistry(t *testing.T) {
	w := newWorld(t)
	hotel, ok := w.Wallet("CP-HOTEL")
	if !ok {
		t.Fatal("the seed no longer carries CP-HOTEL")
	}
	if w.EVM.Verified(hotel) {
		t.Fatal("CP-HOTEL is in the identity registry; the seed says it is not whitelisted")
	}
	alpha, _ := w.Wallet("CP-ALPHA")
	_, err := w.EVM.Transfer(context.Background(), alpha, alpha, hotel, domain.FundSymbol, 1, "")
	if err == nil {
		t.Fatal("a transfer to the un-whitelisted counterparty succeeded")
	}
	if r, _ := ledger.Revert(err); r != "IdentityRegistry: recipient is not verified" {
		t.Fatalf("reason = %q", r)
	}
}

func TestWorldEventsCarryCounterpartyLabels(t *testing.T) {
	w := newWorld(t)
	evs := w.EVM.Events(0)
	if len(evs) == 0 {
		t.Fatal("no seeded events")
	}
	seen := map[string]bool{}
	for _, e := range evs {
		seen[e.To] = true
		if !e.Finalised {
			t.Fatalf("seeded event %s is not final; the world should start settled", e.ID())
		}
	}
	if !seen["CP-ALPHA"] {
		t.Fatalf("EVM events do not use counterparty labels: %v", seen)
	}
}

func TestWorldSupplyMatchesTheSeed(t *testing.T) {
	w := newWorld(t)
	want := map[string]int64{}
	for _, p := range w.Seed.Positions {
		if p.Ledger == domain.LedgerEVM || p.Ledger == domain.LedgerCanton {
			want[string(p.Ledger)+"|"+p.Symbol] += p.Quantity
		}
	}
	for _, s := range ledger.TokenisedSymbols(w.Seed) {
		if got := w.EVM.TotalSupply(s); got != want[string(domain.LedgerEVM)+"|"+s] {
			t.Fatalf("EVM supply of %s = %d, want %d", s, got, want[string(domain.LedgerEVM)+"|"+s])
		}
		if got := w.Canton.TotalSupply(s); got != want[string(domain.LedgerCanton)+"|"+s] {
			t.Fatalf("canton supply of %s = %d, want %d", s, got, want[string(domain.LedgerCanton)+"|"+s])
		}
	}
}

func TestWorldIsDeterministic(t *testing.T) {
	a, b := newWorld(t), newWorld(t)
	ea, eb := a.EVM.Events(0), b.EVM.Events(0)
	if len(ea) != len(eb) {
		t.Fatalf("event counts differ: %d vs %d", len(ea), len(eb))
	}
	for i := range ea {
		if ea[i] != eb[i] {
			t.Fatalf("EVM event %d differs:\n%+v\n%+v", i, ea[i], eb[i])
		}
	}
	if a.EVM.Head().Hash != b.EVM.Head().Hash {
		t.Fatal("two identically seeded worlds have different head hashes")
	}
	ca, cb := a.Canton.Contracts(), b.Canton.Contracts()
	if len(ca) != len(cb) {
		t.Fatalf("contract counts differ: %d vs %d", len(ca), len(cb))
	}
	for i := range ca {
		if ca[i].ID != cb[i].ID || ca[i].Quantity != cb[i].Quantity {
			t.Fatalf("contract %d differs: %+v vs %+v", i, ca[i], cb[i])
		}
	}
}
