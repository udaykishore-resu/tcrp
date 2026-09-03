package bridge

import (
	"context"
	"fmt"

	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/ledger"
)

// Redeemer is the burn-and-unlock direction: it watches the target ledger for
// bridge burns, waits for finality, gathers a quorum of attestations, and
// unlocks the same quantity from the source ledger's vault.
//
// It is the same pipeline as the mint direction with the ledgers exchanged, and
// that is deliberate rather than convenient. The failure modes are symmetric —
// a burn can be orphaned before or after finality exactly as a lock can — and
// writing the return path as a second, subtly different loop is how bridges end
// up with replay protection on one side only.
//
// Nonces are namespaced by direction: the burn that redeems lock nonce 4 gets
// its own nonce from the burn sequence, so the two directions cannot collide
// and neither can replay the other's settlements.
//
// One thing this direction cannot be tested against in this repository:
// Canton's commit is deterministically final, so the simulator cannot produce
// a reorganisation and there is no way to orphan a burn. The orphan handling is
// not untested, because it is the same code the mint direction runs and the
// relayer's tests drive it against the EVM simulator, which can. But if the
// target were ever a probabilistic chain, that path would want its own test
// rather than an inherited one.
type Redeemer struct {
	p       *pipeline
	resolve TargetAddress
}

// NewRedeemer builds the burn-and-unlock redeemer. It takes the same Config as
// the relayer, with Source and Target meaning the same ledgers: the redeemer
// simply reads the target and writes the source.
func NewRedeemer(cfg Config) (*Redeemer, error) {
	p, err := newPipeline(cfg, DirectionRedeem)
	if err != nil {
		return nil, err
	}
	p.from, p.to = cfg.Target, cfg.Source
	p.triggerKind, p.triggerOp = domain.EventBurn, ledger.BridgeOpBurn
	p.responseKind, p.responseOp = domain.EventUnlock, ledger.BridgeOpUnlock
	// The audit vocabulary has no unlock action. Burning is audited as a token
	// burn; the unlock is audited as the ledger transfer it is, with the
	// direction and the nonce in the details so that the two halves of a
	// redemption can be joined.
	p.auditTrigger = audit.ActionTokensBurned
	p.auditResponse = audit.ActionTransferExecuted
	p.auditToken = ""
	r := &Redeemer{p: p}
	p.submit = func(ctx context.Context, a Attestation) (ledger.Receipt, error) {
		return p.to.Unlock(ctx, p.cfg.SourceOperator, r.sourceAddress(a.Holder), a.Symbol, a.Quantity,
			ledger.BridgeMemo(ledger.BridgeOpUnlock, a.Nonce))
	}
	return r, nil
}

// SetSourceAddress installs the translation from a counterparty identifier to
// the source ledger's address or party.
func (r *Redeemer) SetSourceAddress(f TargetAddress) { r.resolve = f }

func (r *Redeemer) sourceAddress(holder string) string {
	if r.resolve == nil {
		return holder
	}
	return r.resolve(holder)
}

// Burn initiates a redemption: it allocates the next burn nonce and burns the
// units on the target ledger. The unlock happens on a later poll, once the burn
// is final and a quorum has attested to it.
//
// holderAddress is the holder's address or party on the target ledger, which is
// where the units to be burned actually are. As with the relayer's Lock, the
// redeemer's lock is held across the ledger write so that a nonce cannot be
// allocated twice.
func (r *Redeemer) Burn(ctx context.Context, holderAddress, symbol string, qty int64) (uint64, ledger.Receipt, error) {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	if !r.p.symbols[symbol] {
		return 0, ledger.Receipt{}, fmt.Errorf("bridge: %s is not relayed", symbol)
	}
	nonce := r.p.nextNonce()
	rc, err := r.p.from.Burn(ctx, r.p.cfg.TargetOperator, holderAddress, symbol, qty,
		ledger.BridgeMemo(ledger.BridgeOpBurn, nonce))
	return nonce, rc, err
}

// Poll runs one redemption cycle.
func (r *Redeemer) Poll(ctx context.Context) (PollResult, error) { return r.p.Poll(ctx) }

// Alerts returns every alert raised so far.
func (r *Redeemer) Alerts() []Alert { return r.p.Alerts() }

// Settlements returns the completed unlocks, ordered by nonce.
func (r *Redeemer) Settlements() []Settlement { return r.p.Settlements() }

// Pending returns the nonces waiting on finality, a quorum or a retry.
func (r *Redeemer) Pending() []uint64 { return r.p.Pending() }
