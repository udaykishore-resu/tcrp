package ledger

import (
	"context"
	"fmt"
	"sort"

	"github.com/udaykishore-resu/tcrp/internal/domain"
)

// OperatorAddress is the EVM address the platform's own agent transacts from.
// It is granted every role at deployment, which is right for a simulator and
// wrong for a deployment: production splits owner, agent and compliance across
// separate keys held by separate people, and contracts/README.md says so.
const OperatorAddress = "0x00000000000000000000000000000000000a9e07"

// World is the pair of ledgers built from the seeded dataset, together with the
// mappings the rest of the platform needs to talk to them: a counterparty is a
// hex address on one network and a Daml party on the other, and something has
// to hold that translation.
type World struct {
	// EVM is the permissioned-token network.
	EVM *EVM
	// Canton is the Daml contract-store network.
	Canton *Canton
	// Seed is the dataset the world was built from.
	Seed *domain.Seed
	// Operator is the EVM address holding the agent role.
	Operator string
	// Issuer is the Daml party signing every asset contract.
	Issuer string

	wallets map[string]string // counterparty -> EVM address
	parties map[string]string // counterparty -> Daml party
}

// Wallet returns a counterparty's EVM address.
func (w *World) Wallet(cp string) (string, bool) { a, ok := w.wallets[cp]; return a, ok }

// Party returns a counterparty's Daml party.
func (w *World) Party(cp string) (string, bool) { p, ok := w.parties[cp]; return p, ok }

// NewWorld builds both simulators pre-populated from the seeded dataset, so a
// test, the demo and an eval scenario all start from the same portfolio and the
// same on-ledger balances.
//
// It reproduces the seed's opening positions rather than replaying the seed's
// event stream. The seed's events include three deliberate anomalies that exist
// to give the lakehouse's reconciliation controls something to find; replaying
// them here would move balances the risk engine has already been told about.
// What this function guarantees is that for every seeded position on a ledger,
// that ledger's BalanceOf agrees with domain.Position.Quantity.
//
// CP-HOTEL is deliberately left out of the identity registry, exactly as the
// seed leaves it un-whitelisted. Every transfer to it must revert, and
// TestTransferToUnregisteredHolderReverts asserts that it does.
func NewWorld(opts Options) (*World, error) {
	opts = opts.resolve()
	seed := domain.NewSeed()

	wallets := map[string]string{}
	parties := map[string]string{}
	labels := map[string]string{}
	for _, cp := range seed.Counterparties {
		if len(cp.Wallets) > 0 {
			wallets[cp.ID] = cp.Wallets[0]
			labels[cp.Wallets[0]] = cp.ID
		}
		if len(cp.Parties) > 0 {
			parties[cp.ID] = cp.Parties[0]
			labels[cp.Parties[0]] = cp.ID
		}
	}
	labels[OperatorAddress] = "TCRP-OPERATOR"
	labels[VaultAddress] = "TCRP-BRIDGE-VAULT"
	labels[VaultParty] = "TCRP-BRIDGE-VAULT"
	labels[opts.Issuer] = "SELF"
	for k, v := range opts.HolderLabels {
		labels[k] = v
	}
	opts.HolderLabels = labels

	w := &World{
		EVM: NewEVM(OperatorAddress, opts), Canton: NewCanton(opts), Seed: seed,
		Operator: OperatorAddress, Issuer: opts.Issuer,
		wallets: wallets, parties: parties,
	}

	// The identity registry. A counterparty the seed does not whitelist is not
	// registered, which is the only thing standing between it and the token.
	for _, cp := range seed.Counterparties {
		addr, ok := wallets[cp.ID]
		if !ok || !cp.Whitelisted {
			continue
		}
		id := Identity{
			Address: addr, Country: cp.Jurisdiction,
			Claims:       []string{ClaimKYC, ClaimAccredited},
			Counterparty: cp.ID,
		}
		if err := w.EVM.Register(OperatorAddress, id); err != nil {
			return nil, fmt.Errorf("ledger: registering %s: %w", cp.ID, err)
		}
	}

	ctx := context.Background()

	// Opening balances. Positions are sorted so that the transaction order, and
	// therefore every block hash and transaction hash, is identical on every
	// run regardless of how the seed happened to order its slice.
	positions := append([]domain.Position(nil), seed.Positions...)
	domain.SortPositions(positions)

	for _, p := range positions {
		switch p.Ledger {
		case domain.LedgerEVM:
			addr, ok := wallets[p.Counterparty]
			if !ok {
				return nil, fmt.Errorf("ledger: %s has no wallet for position %s", p.Counterparty, p.ID)
			}
			if _, err := w.EVM.Mint(ctx, OperatorAddress, addr, p.Symbol, p.Quantity, "primary issuance "+p.ID); err != nil {
				return nil, fmt.Errorf("ledger: seeding %s: %w", p.ID, err)
			}
		case domain.LedgerCanton:
			party, ok := parties[p.Counterparty]
			if !ok {
				return nil, fmt.Errorf("ledger: %s has no party for position %s", p.Counterparty, p.ID)
			}
			if _, err := w.Canton.Mint(ctx, opts.Issuer, party, p.Symbol, p.Quantity, "primary issuance "+p.ID); err != nil {
				return nil, fmt.Errorf("ledger: seeding %s: %w", p.ID, err)
			}
		}
	}

	// Enough blocks on top that every opening balance is final. A relayer that
	// starts against this world should not have to wait out the seeding.
	w.EVM.Mine(int(opts.Confirmations))
	return w, nil
}

// Claim topics the registry attests. They are the two an ERC-3643 deployment
// almost always starts with: the holder has been identified, and the holder is
// permitted to hold this class of security.
const (
	ClaimKYC        = "kyc"
	ClaimAccredited = "accredited"
)

// TokenisedSymbols returns the seeded symbols that exist on a ledger, sorted.
// It is what the supply reconciliation iterates, so that adding a tokenised
// instrument to the seed extends the control without anybody editing the
// control.
func TokenisedSymbols(seed *domain.Seed) []string {
	out := make([]string, 0, 4)
	for _, i := range seed.Instruments {
		if i.Tokenised {
			out = append(out, i.Symbol)
		}
	}
	sort.Strings(out)
	return out
}
