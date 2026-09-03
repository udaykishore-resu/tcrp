package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Roles on the EVM token. They mirror the AccessControl roles declared in
// contracts/src/PermissionedToken.sol, and the split is the one ERC-3643
// deployments actually use: the owner administers the contract, the agent runs
// the day-to-day issuance and recovery operations, and the compliance officer
// changes the rules the agent has to work inside. Nobody holds all three in a
// production deployment, which is the only reason to separate them at all.
const (
	// RoleOwner grants and revokes roles, pauses the token and points it at a
	// different compliance module.
	RoleOwner = "OWNER_ROLE"
	// RoleAgent mints, burns, freezes, forces transfers for recovery and
	// operates the bridge vault.
	RoleAgent = "AGENT_ROLE"
	// RoleCompliance writes the identity registry and the compliance
	// parameters.
	RoleCompliance = "COMPLIANCE_ROLE"
)

// Identity is a registered investor as the identity registry sees it: an
// address, the jurisdiction its claims were issued in, and the claim topics an
// issuer has attested. It is deliberately not a person — the registry holds
// attestations about an address, which is the whole point of putting KYC behind
// an on-chain claim rather than a name.
type Identity struct {
	Address string   `json:"address"`
	Country string   `json:"country"`
	Claims  []string `json:"claims,omitempty"`
	// Counterparty is the platform's identifier for the legal entity behind the
	// address, when one is known. It is not part of ERC-3643; it is what the
	// indexer would resolve so that a reconciliation against the book of record
	// has something to join on.
	Counterparty string `json:"counterparty,omitempty"`
}

// HasClaim reports whether the identity carries a claim topic.
func (i Identity) HasClaim(c string) bool {
	for _, have := range i.Claims {
		if have == c {
			return true
		}
	}
	return false
}

// ComplianceRules are the transfer restrictions the compliance module enforces.
// The zero value imposes no ceilings, which is the right default for a
// simulator: a restriction that is on unless you turn it off is a restriction
// nobody remembers is there.
type ComplianceRules struct {
	// MaxHolders caps the number of addresses with a non-zero balance. Zero
	// means unlimited. Funds carry this because exceeding a holder count turns
	// a private placement into a public offering.
	MaxHolders int
	// MaxPerInvestor caps any single address's balance. Zero means unlimited.
	MaxPerInvestor int64
	// AllowedCountries restricts the registry countries that may receive.
	// Empty means every registered country.
	AllowedCountries []string
	// RequiredClaims are the claim topics a recipient must carry.
	RequiredClaims []string
}

func (c ComplianceRules) countryAllowed(country string) bool {
	if len(c.AllowedCountries) == 0 {
		return true
	}
	for _, a := range c.AllowedCountries {
		if a == country {
			return true
		}
	}
	return false
}

// evmTx is a committed transaction. It carries everything needed to re-execute
// it, because a reorganisation re-includes transactions rather than inventing
// new ones: the hash, the sequence number and the observation time survive
// re-inclusion, and only the block changes.
type evmTx struct {
	hash       string
	seq        uint64
	op         string
	kind       domain.EventKind
	caller     string
	from, to   string
	symbol     string
	qty        int64
	memo       string
	observedAt time.Time
	prevFrozen bool
	blockIdx   int
}

type evmBlock struct {
	blk Block
	tx  int // index into EVM.txs, or -1 for an empty block
}

// EVM is an ERC-3643-style permissioned token on a simulated chain.
//
// It mirrors contracts/src/PermissionedToken.sol: every transfer passes the
// identity registry and the compliance module, administration is role-gated,
// the token can be paused, an agent can freeze an account or force a transfer
// to recover from a lost key, and a bridge vault holds units that have been
// locked for a cross-ledger transfer.
//
// Two things are deliberately outside the block model. Identity registry
// entries, role grants and compliance parameters are configuration: they apply
// immediately and a reorganisation does not rewind them. On a real chain they
// are transactions and a deep enough reorganisation would take them with it;
// modelling that would add a failure mode the relayer does not have, since the
// relayer never reads the registry. The other is gas, ordering and mempool
// behaviour, none of which exist here: one transaction per block, in call
// order.
type EVM struct {
	mu      sync.Mutex
	opts    Options
	metrics *Metrics

	registry   map[string]Identity
	roles      map[string]map[string]bool
	exempt     map[string]bool
	compliance ComplianceRules
	paused     bool

	balances map[string]map[string]int64 // symbol -> holder -> units
	supply   map[string]int64
	frozen   map[string]bool

	blocks []evmBlock
	txs    []evmTx
	seq    uint64
	nonce  uint64

	subs []chan domain.LedgerEvent
}

// NewEVM builds the EVM simulator. The deployer address is granted every role,
// exactly as a constructor would, and the bridge vault is registered as an
// exempt holder so that locking units does not itself trip a holder ceiling.
func NewEVM(deployer string, opts Options) *EVM {
	opts = opts.resolve()
	e := &EVM{
		opts:       opts,
		metrics:    NewMetrics(opts.Metrics, domain.LedgerEVM),
		registry:   map[string]Identity{},
		roles:      map[string]map[string]bool{},
		exempt:     map[string]bool{VaultAddress: true},
		compliance: opts.Compliance,
		balances:   map[string]map[string]int64{},
		supply:     map[string]int64{},
		frozen:     map[string]bool{},
	}
	for _, r := range []string{RoleOwner, RoleAgent, RoleCompliance} {
		e.roles[r] = map[string]bool{deployer: true}
	}
	genesisTime := opts.Clock.Now()
	e.blocks = []evmBlock{{
		blk: Block{
			Height: opts.GenesisBlock,
			Hash:   blockHash("", opts.GenesisBlock, ""),
			Time:   genesisTime,
			// The genesis block is final by definition; nothing can be
			// reorganised out from under it.
			Finalised: true,
		},
		tx: -1,
	}}
	e.metrics.Head.Set(float64(opts.GenesisBlock))
	e.metrics.Finalised.Set(float64(opts.GenesisBlock))
	return e
}

// Name reports the network.
func (e *EVM) Name() domain.Ledger { return domain.LedgerEVM }

// Confirmations is how many blocks must sit on top of a block before it is
// final.
func (e *EVM) Confirmations() uint64 { return e.opts.Confirmations }

// --- administration -------------------------------------------------------

// GrantRole gives addr a role. Owner-authorised.
func (e *EVM) GrantRole(caller, role, addr string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.hasRole(caller, RoleOwner) {
		return fmt.Errorf("%w: %s is not %s", ErrUnauthorised, caller, RoleOwner)
	}
	if e.roles[role] == nil {
		e.roles[role] = map[string]bool{}
	}
	e.roles[role][addr] = true
	return nil
}

// RevokeRole removes a role from addr. Owner-authorised.
func (e *EVM) RevokeRole(caller, role, addr string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.hasRole(caller, RoleOwner) {
		return fmt.Errorf("%w: %s is not %s", ErrUnauthorised, caller, RoleOwner)
	}
	delete(e.roles[role], addr)
	return nil
}

// HasRole reports whether addr holds role.
func (e *EVM) HasRole(addr, role string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hasRole(addr, role)
}

func (e *EVM) hasRole(addr, role string) bool { return e.roles[role][addr] }

// Register writes an identity into the registry. Compliance-authorised.
func (e *EVM) Register(caller string, id Identity) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.hasRole(caller, RoleCompliance) {
		return fmt.Errorf("%w: %s is not %s", ErrUnauthorised, caller, RoleCompliance)
	}
	if id.Address == "" {
		return fmt.Errorf("%w: identity has no address", ErrReverted)
	}
	e.registry[id.Address] = id
	return nil
}

// Deregister removes an identity. Compliance-authorised. It does not move the
// address's balance: a deregistered holder keeps what it holds and simply
// cannot receive more, which is what a revoked claim means. Getting the balance
// back is what ForcedTransfer is for.
func (e *EVM) Deregister(caller, addr string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.hasRole(caller, RoleCompliance) {
		return fmt.Errorf("%w: %s is not %s", ErrUnauthorised, caller, RoleCompliance)
	}
	delete(e.registry, addr)
	return nil
}

// Verified reports whether an address is in the identity registry.
func (e *EVM) Verified(addr string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.registry[addr]
	return ok
}

// Identity returns a registered identity.
func (e *EVM) Identity(addr string) (Identity, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.registry[addr]
	return id, ok
}

// SetCompliance replaces the compliance parameters. Compliance-authorised. It
// is not retroactive: an address already over a newly lowered ceiling keeps its
// balance and simply cannot receive more.
func (e *EVM) SetCompliance(caller string, rules ComplianceRules) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.hasRole(caller, RoleCompliance) {
		return fmt.Errorf("%w: %s is not %s", ErrUnauthorised, caller, RoleCompliance)
	}
	e.compliance = rules
	return nil
}

// Compliance returns the current compliance parameters.
func (e *EVM) Compliance() ComplianceRules {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.compliance
}

// Pause halts every transfer, mint and burn. Owner-authorised.
func (e *EVM) Pause(caller string) error { return e.setPaused(caller, true) }

// Unpause resumes the token. Owner-authorised.
func (e *EVM) Unpause(caller string) error { return e.setPaused(caller, false) }

func (e *EVM) setPaused(caller string, v bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.hasRole(caller, RoleOwner) {
		return fmt.Errorf("%w: %s is not %s", ErrUnauthorised, caller, RoleOwner)
	}
	e.paused = v
	return nil
}

// Paused reports whether the token is halted.
func (e *EVM) Paused() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.paused
}

// --- reads ----------------------------------------------------------------

// BalanceOf returns the units of symbol held by holder.
func (e *EVM) BalanceOf(holder, symbol string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.balances[symbol][holder]
}

// TotalSupply returns every unit of symbol in existence, vault included.
func (e *EVM) TotalSupply(symbol string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.supply[symbol]
}

// Locked returns the units of symbol held by the bridge vault.
func (e *EVM) Locked(symbol string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.balances[symbol][VaultAddress]
}

// Holders returns the addresses with a non-zero balance of symbol, sorted.
func (e *EVM) Holders(symbol string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.balances[symbol]))
	for a, v := range e.balances[symbol] {
		if v > 0 {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// Frozen reports whether an account is frozen.
func (e *EVM) Frozen(addr string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.frozen[addr]
}

// Head returns the newest block.
func (e *EVM) Head() Block {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.headLocked()
}

func (e *EVM) headLocked() Block {
	b := e.blocks[len(e.blocks)-1].blk
	b.Finalised = e.finalLocked(b.Height)
	return b
}

// FinalisedHeight is the newest height that can no longer be reorganised out.
func (e *EVM) FinalisedHeight() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	head := e.blocks[len(e.blocks)-1].blk.Height
	if head < e.opts.GenesisBlock+e.opts.Confirmations {
		return e.opts.GenesisBlock
	}
	return head - e.opts.Confirmations
}

func (e *EVM) finalLocked(h uint64) bool {
	if h <= e.opts.GenesisBlock {
		return true
	}
	head := e.blocks[len(e.blocks)-1].blk.Height
	return head >= h+e.opts.Confirmations
}

// Events returns canonical-chain events with Seq at or above from.
func (e *EVM) Events(from uint64) []domain.LedgerEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]domain.LedgerEvent, 0, len(e.txs))
	for _, t := range e.txs {
		if t.seq < from {
			continue
		}
		out = append(out, e.eventForLocked(t))
	}
	return out
}

// Subscribe returns a channel receiving events as they are emitted.
func (e *EVM) Subscribe() <-chan domain.LedgerEvent {
	c := make(chan domain.LedgerEvent, subscribeBuffer)
	e.mu.Lock()
	e.subs = append(e.subs, c)
	e.mu.Unlock()
	return c
}

func (e *EVM) eventForLocked(t evmTx) domain.LedgerEvent {
	b := e.blocks[t.blockIdx].blk
	ev := domain.LedgerEvent{
		Seq:        t.seq,
		Ledger:     domain.LedgerEVM,
		Kind:       t.kind,
		Symbol:     t.symbol,
		Quantity:   t.qty,
		Block:      b.Height,
		TxHash:     t.hash,
		LogIndex:   0,
		OccurredAt: b.Time,
		ObservedAt: t.observedAt,
		Finalised:  e.finalLocked(b.Height),
		Memo:       t.memo,
	}
	if t.from != "" {
		ev.From = e.opts.label(t.from)
	}
	if t.to != "" {
		ev.To = e.opts.label(t.to)
	}
	return ev
}

// --- writes ---------------------------------------------------------------

// Mint issues units to a holder. Agent-authorised.
func (e *EVM) Mint(ctx context.Context, caller, to, symbol string, qty int64, memo string) (Receipt, error) {
	return e.submit(ctx, evmTx{op: "mint", kind: domain.EventMint, caller: caller, to: to, symbol: symbol, qty: qty, memo: memo})
}

// Burn destroys units held by a holder. Agent-authorised. It works on a frozen
// account, because burning is how a redemption completes when the holder's key
// is gone.
func (e *EVM) Burn(ctx context.Context, caller, from, symbol string, qty int64, memo string) (Receipt, error) {
	return e.submit(ctx, evmTx{op: "burn", kind: domain.EventBurn, caller: caller, from: from, symbol: symbol, qty: qty, memo: memo})
}

// Transfer moves units between holders subject to every transfer restriction.
// The caller must be the sender: there is no allowance model here, because the
// platform never delegates spending authority to a contract.
func (e *EVM) Transfer(ctx context.Context, caller, from, to, symbol string, qty int64, memo string) (Receipt, error) {
	return e.submit(ctx, evmTx{op: "transfer", kind: domain.EventTransfer, caller: caller, from: from, to: to, symbol: symbol, qty: qty, memo: memo})
}

// ForcedTransfer moves units without the sender's consent. Agent-authorised.
// It is the recovery path: a lost key, a court order, a mistaken settlement.
// It bypasses the pause, the freeze and the compliance ceilings, because every
// one of those is a reason the transfer could not happen the ordinary way, but
// it still refuses an unverified recipient — recovery must not be a hole in the
// permissioning.
func (e *EVM) ForcedTransfer(ctx context.Context, caller, from, to, symbol string, qty int64, memo string) (Receipt, error) {
	return e.submit(ctx, evmTx{op: "forced", kind: domain.EventTransfer, caller: caller, from: from, to: to, symbol: symbol, qty: qty, memo: memo})
}

// Freeze stops an account transacting. Agent-authorised.
func (e *EVM) Freeze(ctx context.Context, caller, addr, symbol, memo string) (Receipt, error) {
	return e.submit(ctx, evmTx{op: "freeze", kind: domain.EventFreeze, caller: caller, to: addr, symbol: symbol, memo: memo})
}

// Unfreeze restores an account. Agent-authorised.
func (e *EVM) Unfreeze(ctx context.Context, caller, addr, symbol, memo string) (Receipt, error) {
	return e.submit(ctx, evmTx{op: "unfreeze", kind: domain.EventFreeze, caller: caller, to: addr, symbol: symbol, memo: memo})
}

// Lock moves units into the bridge vault. Agent-authorised.
func (e *EVM) Lock(ctx context.Context, caller, holder, symbol string, qty int64, memo string) (Receipt, error) {
	return e.submit(ctx, evmTx{op: "lock", kind: domain.EventLock, caller: caller, from: holder, symbol: symbol, qty: qty, memo: memo})
}

// Unlock returns units from the bridge vault. Agent-authorised.
func (e *EVM) Unlock(ctx context.Context, caller, holder, symbol string, qty int64, memo string) (Receipt, error) {
	return e.submit(ctx, evmTx{op: "unlock", kind: domain.EventUnlock, caller: caller, to: holder, symbol: symbol, memo: memo, qty: qty})
}

// Mine produces n empty blocks. Real chains produce blocks whether or not
// anybody transacted, and a relayer waiting for confirmations depends on it.
func (e *EVM) Mine(n int) Block {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := 0; i < n; i++ {
		e.appendBlockLocked(-1)
	}
	return e.headLocked()
}

func (e *EVM) submit(ctx context.Context, t evmTx) (Receipt, error) {
	var span *obs.Span
	if e.opts.Tracer != nil {
		_, span = e.opts.Tracer.Start(ctx, "ledger.evm."+t.op, obs.KindClient)
		span.SetAttrs(map[string]any{
			"tcrp.ledger":        string(domain.LedgerEVM),
			"tcrp.ledger.op":     t.op,
			"tcrp.ledger.symbol": t.symbol,
			"tcrp.ledger.qty":    t.qty,
		})
		defer span.End()
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.nonce++
	t.hash = txHash(e.nonce, t)
	e.seq++
	t.seq = e.seq
	rc, err := e.runLocked(t, false)
	if err != nil {
		e.metrics.Reverts.Inc()
		span.RecordError(err)
		return rc, err
	}
	e.metrics.Transactions.Inc()
	span.SetAttr("tcrp.ledger.tx_hash", rc.TxHash).SetAttr("tcrp.ledger.block", rc.Block)
	return rc, nil
}

// runLocked validates and applies one transaction, appending a block for it.
// reuse says the transaction is being re-included after a reorganisation, in
// which case its hash, sequence number and observation time are already set.
func (e *EVM) runLocked(t evmTx, reuse bool) (Receipt, error) {
	if reason := e.checkLocked(t); reason != "" {
		return Receipt{TxHash: t.hash, Success: false, RevertReason: reason},
			&RevertError{Op: t.op, Reason: reason}
	}
	idx := len(e.txs)
	blk := e.appendBlockLocked(idx)
	if !reuse {
		t.observedAt = blk.Time.Add(e.opts.ObservationLag)
	}
	t.blockIdx = len(e.blocks) - 1
	t.prevFrozen = e.frozen[t.to]
	e.applyLocked(t, +1)
	e.txs = append(e.txs, t)

	ev := e.eventForLocked(t)
	fanout(e.subs, ev)
	return Receipt{TxHash: t.hash, Block: blk.Height, Success: true}, nil
}

// checkLocked returns the revert reason, or empty if the transaction is legal.
// The order matters: it is the order the Solidity contract checks in, so a test
// that asserts on a reason string asserts on the same rule on both sides.
func (e *EVM) checkLocked(t evmTx) string {
	forced := t.op == "forced"

	switch t.op {
	case "mint", "burn", "freeze", "unfreeze", "forced", "lock", "unlock":
		if !e.hasRole(t.caller, RoleAgent) {
			return "AccessControl: caller is missing AGENT_ROLE"
		}
	case "transfer":
		if t.caller != t.from {
			return "Token: caller is not the sender"
		}
	}
	if e.paused && !forced {
		return "Pausable: token is paused"
	}
	switch t.op {
	case "freeze", "unfreeze":
		if _, ok := e.registry[t.to]; !ok {
			return "IdentityRegistry: address is not verified"
		}
		return ""
	}
	if t.qty <= 0 {
		return "Token: amount must be positive"
	}

	// Sender-side checks. Mint and unlock have no ordinary sender; unlock's
	// source is the vault, which is exempt.
	from := t.from
	if t.op == "unlock" {
		from = VaultAddress
	}
	if from != "" {
		if e.frozen[from] && !forced && t.op != "burn" {
			return "Token: sender account is frozen"
		}
		if e.balances[t.symbol][from] < t.qty {
			return "Token: insufficient balance"
		}
	}

	// Recipient-side checks. Burn and lock have no ordinary recipient; lock's
	// destination is the vault.
	to := t.to
	if t.op == "lock" {
		to = VaultAddress
	}
	if to == "" {
		return ""
	}
	if !e.exempt[to] {
		id, ok := e.registry[to]
		if !ok {
			return "IdentityRegistry: recipient is not verified"
		}
		if !e.compliance.countryAllowed(id.Country) {
			return "Compliance: recipient country is not allowed"
		}
		for _, c := range e.compliance.RequiredClaims {
			if !id.HasClaim(c) {
				return "Compliance: recipient is missing claim " + c
			}
		}
	}
	if e.frozen[to] && !forced {
		return "Token: recipient account is frozen"
	}
	if !e.exempt[to] && !forced {
		if m := e.compliance.MaxPerInvestor; m > 0 && e.balances[t.symbol][to]+t.qty > m {
			return "Compliance: max balance per investor exceeded"
		}
		if m := e.compliance.MaxHolders; m > 0 && e.balances[t.symbol][to] == 0 {
			n := 0
			for _, v := range e.balances[t.symbol] {
				if v > 0 {
					n++
				}
			}
			if n+1 > m {
				return "Compliance: max holders exceeded"
			}
		}
	}
	return ""
}

// applyLocked moves balances. dir is +1 to apply and -1 to undo, which is what
// a reorganisation needs: the effects of these operations are exact integer
// deltas, so unwinding them is exact too.
func (e *EVM) applyLocked(t evmTx, dir int64) {
	credit := func(addr string, q int64) {
		if e.balances[t.symbol] == nil {
			e.balances[t.symbol] = map[string]int64{}
		}
		e.balances[t.symbol][addr] += q
		if e.balances[t.symbol][addr] == 0 {
			delete(e.balances[t.symbol], addr)
		}
	}
	q := t.qty * dir
	switch t.op {
	case "mint":
		credit(t.to, q)
		e.supply[t.symbol] += q
	case "burn":
		credit(t.from, -q)
		e.supply[t.symbol] -= q
	case "transfer", "forced":
		credit(t.from, -q)
		credit(t.to, q)
	case "lock":
		credit(t.from, -q)
		credit(VaultAddress, q)
	case "unlock":
		credit(VaultAddress, -q)
		credit(t.to, q)
	case "freeze":
		if dir > 0 {
			e.frozen[t.to] = true
		} else {
			e.frozen[t.to] = t.prevFrozen
		}
	case "unfreeze":
		if dir > 0 {
			delete(e.frozen, t.to)
		} else if t.prevFrozen {
			e.frozen[t.to] = true
		}
	}
}

func (e *EVM) appendBlockLocked(txIdx int) Block {
	parent := e.blocks[len(e.blocks)-1].blk
	h := parent.Height + 1
	txc := 0
	tag := ""
	if txIdx >= 0 {
		txc = 1
		tag = strconv.Itoa(txIdx)
	}
	b := Block{
		Height:     h,
		Hash:       blockHash(parent.Hash, h, tag),
		ParentHash: parent.Hash,
		Time:       e.opts.Clock.Now(),
		TxCount:    txc,
	}
	e.blocks = append(e.blocks, evmBlock{blk: b, tx: txIdx})
	e.metrics.Head.Set(float64(h))
	if h >= e.opts.GenesisBlock+e.opts.Confirmations {
		e.metrics.Finalised.Set(float64(h - e.opts.Confirmations))
	}
	return b
}

// --- reorganisation -------------------------------------------------------

// ReorgResult describes what a reorganisation did.
type ReorgResult struct {
	// From is the head before the rewind.
	From Block
	// To is the head after the rewind and re-application.
	To Block
	// Orphaned are the events of transactions that left the canonical chain,
	// either because the caller dropped them or because re-execution reverted
	// them. They are reported at the block they used to occupy.
	Orphaned []domain.LedgerEvent
	// Reapplied are the events of transactions re-included on the new chain,
	// at their new heights.
	Reapplied []domain.LedgerEvent
	// ViolatedFinality is true when the rewind removed a block the ledger had
	// already reported as final. It is the disaster case: everything
	// downstream that acted on finality — the bridge above all — acted on a
	// promise the chain has now broken, and it needs to know.
	ViolatedFinality bool
}

// Reorg rewinds the last depth blocks and rebuilds the chain from the
// transactions they carried, dropping any whose hash appears in dropped.
//
// A retained transaction is re-executed at a new height, keeping its hash,
// sequence number and observation time, because a chain that reorganises
// re-includes the same signed transactions rather than inventing new ones. A
// retained transaction whose re-execution now reverts — because a transaction
// it depended on was dropped — is orphaned too, which is what happens on a real
// chain and is worth having rather than papering over.
//
// The depth may exceed the confirmation threshold. That is not a mistake to be
// refused: a reorganisation deeper than the finality assumption is precisely
// the event the bridge's alerting exists for, and there has to be a way to
// produce it.
func (e *EVM) Reorg(depth int, dropped ...string) (ReorgResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if depth <= 0 || depth >= len(e.blocks) {
		return ReorgResult{}, fmt.Errorf("%w: depth %d on a chain of %d blocks", ErrBadReorg, depth, len(e.blocks))
	}
	drop := make(map[string]bool, len(dropped))
	for _, h := range dropped {
		drop[h] = true
	}
	res := ReorgResult{From: e.headLocked()}

	cut := len(e.blocks) - depth
	var rewound []evmTx
	for i := len(e.blocks) - 1; i >= cut; i-- {
		if e.blocks[i].blk.Finalised || e.finalLocked(e.blocks[i].blk.Height) {
			res.ViolatedFinality = true
		}
		if ti := e.blocks[i].tx; ti >= 0 {
			t := e.txs[ti]
			e.applyLocked(t, -1)
			rewound = append(rewound, t)
		}
	}
	// rewound is newest-first; re-inclusion happens in original order.
	for i, j := 0, len(rewound)-1; i < j; i, j = i+1, j-1 {
		rewound[i], rewound[j] = rewound[j], rewound[i]
	}
	keptTxs := len(e.txs) - len(rewound)
	e.blocks = e.blocks[:cut]
	e.txs = e.txs[:keptTxs]

	for _, t := range rewound {
		if drop[t.hash] {
			res.Orphaned = append(res.Orphaned, e.orphanEvent(t))
			e.metrics.Orphaned.Inc()
			continue
		}
		if _, err := e.runLocked(t, true); err != nil {
			res.Orphaned = append(res.Orphaned, e.orphanEvent(t))
			e.metrics.Orphaned.Inc()
			continue
		}
		res.Reapplied = append(res.Reapplied, e.eventForLocked(e.txs[len(e.txs)-1]))
	}
	res.To = e.headLocked()
	e.metrics.Reorgs.Inc()
	return res, nil
}

// orphanEvent renders an event for a transaction that is no longer on the
// canonical chain. Block and Finalised describe where it used to be, which is
// what a consumer needs in order to recognise what it has to undo.
func (e *EVM) orphanEvent(t evmTx) domain.LedgerEvent {
	ev := domain.LedgerEvent{
		Seq: t.seq, Ledger: domain.LedgerEVM, Kind: t.kind, Symbol: t.symbol,
		Quantity: t.qty, TxHash: t.hash, ObservedAt: t.observedAt, Memo: t.memo,
	}
	if t.from != "" {
		ev.From = e.opts.label(t.from)
	}
	if t.to != "" {
		ev.To = e.opts.label(t.to)
	}
	return ev
}

func blockHash(parent string, height uint64, tag string) string {
	sum := sha256.Sum256([]byte(parent + "|" + strconv.FormatUint(height, 10) + "|" + tag))
	return "0x" + hex.EncodeToString(sum[:])
}

func txHash(nonce uint64, t evmTx) string {
	sum := sha256.Sum256([]byte(strconv.FormatUint(nonce, 10) + "|" + t.op + "|" + t.caller +
		"|" + t.from + "|" + t.to + "|" + t.symbol + "|" + strconv.FormatInt(t.qty, 10) + "|" + t.memo))
	return "0x" + hex.EncodeToString(sum[:])
}
