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

// Template names the Daml template a contract instantiates. They mirror
// daml/Collateral/Fund.daml.
type Template string

// Templates.
const (
	// TemplateFundShare is a holding: signatory the issuer, observer the
	// holder. The holder is not a signatory, which is why a holder cannot
	// unilaterally create one — every new holding is created under the
	// issuer's authority, delegated through a choice on an existing contract.
	TemplateFundShare Template = "FundShare"
	// TemplateTransferProposal is the first half of the propose-accept pair.
	TemplateTransferProposal Template = "TransferProposal"
	// TemplatePledge is a holding encumbered in favour of a pledgee, who is
	// added as an observer so that the collateral taker can see the collateral
	// without being able to move it.
	TemplatePledge Template = "Pledge"
)

// ContractID identifies a contract on the participant.
type ContractID string

// TransactionID identifies a committed transaction.
type TransactionID string

// Contract is one active or archived contract in the store.
//
// Signatories and Observers are the whole disclosure model: a party sees a
// contract if and only if it appears in one of them. That is not an access
// control list bolted on top of a shared database — it is the reason the same
// asset on Canton and on a public chain are different products, and it is what
// ContractsVisibleTo enforces.
type Contract struct {
	ID          ContractID `json:"id"`
	Template    Template   `json:"template"`
	Signatories []string   `json:"signatories"`
	Observers   []string   `json:"observers,omitempty"`
	// Key is the contract key. FundShare and Pledge are keyed by issuer,
	// symbol and owner, so a party holds at most one of each per symbol and
	// "archive and recreate" is the only way a balance changes.
	Key string `json:"key,omitempty"`
	// Maintainer is the party that maintains the key. It is always the issuer.
	Maintainer string `json:"maintainer,omitempty"`

	Issuer   string `json:"issuer"`
	Owner    string `json:"owner"`
	Symbol   string `json:"symbol"`
	Quantity int64  `json:"quantity"`
	// Recipient is set on a TransferProposal.
	Recipient string `json:"recipient,omitempty"`
	// Pledgee is set on a Pledge.
	Pledgee string `json:"pledgee,omitempty"`

	CreatedAt  time.Time     `json:"created_at"`
	CreatedBy  TransactionID `json:"created_by"`
	Archived   bool          `json:"archived"`
	ArchivedAt time.Time     `json:"archived_at,omitzero"`
	ArchivedBy TransactionID `json:"archived_by,omitempty"`
}

// VisibleTo reports whether a party may read the contract.
func (c Contract) VisibleTo(party string) bool {
	for _, p := range c.Signatories {
		if p == party {
			return true
		}
	}
	for _, p := range c.Observers {
		if p == party {
			return true
		}
	}
	return false
}

// NodeKind classifies a node of a transaction tree.
type NodeKind string

// Node kinds.
const (
	NodeCreate   NodeKind = "create"
	NodeExercise NodeKind = "exercise"
	NodeArchive  NodeKind = "archive"
	NodeFetch    NodeKind = "fetch"
)

// Node is one action in a transaction tree. An exercise node has children:
// everything the choice body did under the exercising party's authority.
type Node struct {
	Kind     NodeKind   `json:"kind"`
	Contract ContractID `json:"contract"`
	Template Template   `json:"template"`
	Choice   string     `json:"choice,omitempty"`
	Actor    string     `json:"actor,omitempty"`
	Children []Node     `json:"children,omitempty"`
}

// Transaction is one atomic commit. Daml's unit of atomicity is the
// transaction, not the contract: a transfer that archives one holding and
// creates two either happens entirely or not at all, and the tree is what
// records that it was one act rather than three.
type Transaction struct {
	ID          TransactionID `json:"id"`
	Offset      uint64        `json:"offset"`
	Command     string        `json:"command"`
	Actor       string        `json:"actor"`
	EffectiveAt time.Time     `json:"effective_at"`
	Roots       []Node        `json:"roots"`
}

// Canton is a Daml-style contract store on a simulated Canton participant.
//
// It mirrors daml/Collateral/Fund.daml: contracts have signatories and
// observers, a transfer archives the source holding and creates new ones rather
// than mutating a balance, contracts carry keys with maintainers, and the
// propose-accept pattern gives the recipient a say in what it receives. Reads
// are projected: ContractsVisibleTo shows a party only what it is a stakeholder
// in.
//
// What it is not: there is no Daml-LF interpreter, no ledger API, no domain
// sequencer, no sub-transaction privacy across participants and no
// authorisation checking beyond the controller checks written here. Choices are
// Go methods, and their ensure clauses are if statements. The Daml sources are
// the artefact that would be uploaded to a participant; this is what lets the
// rest of the platform run without one.
//
// Canton commits are deterministically final once the sequencer has ordered
// them, so there is no reorganisation here and Head is always final. That
// asymmetry with the EVM side is real and it is the reason the bridge treats
// its two ledgers differently.
type Canton struct {
	mu      sync.Mutex
	opts    Options
	metrics *Metrics

	contracts map[ContractID]*Contract
	order     []ContractID
	byKey     map[string]ContractID
	txs       []Transaction
	events    []domain.LedgerEvent

	seq     uint64
	cidSeq  uint64
	txSeq   uint64
	lastTx  TransactionID
	subs    []chan domain.LedgerEvent
	issuer  string
	genesis time.Time
}

// NewCanton builds the Canton simulator.
func NewCanton(opts Options) *Canton {
	opts = opts.resolve()
	return &Canton{
		opts:      opts,
		metrics:   NewMetrics(opts.Metrics, domain.LedgerCanton),
		contracts: map[ContractID]*Contract{},
		byKey:     map[string]ContractID{},
		issuer:    opts.Issuer,
		genesis:   opts.Clock.Now(),
	}
}

// Name reports the network.
func (c *Canton) Name() domain.Ledger { return domain.LedgerCanton }

// Issuer is the party that signs every asset contract.
func (c *Canton) Issuer() string { return c.issuer }

// --- reads ----------------------------------------------------------------

// BalanceOf returns the units of symbol owned by a party, counting pledged
// holdings: a pledge encumbers a holding, it does not transfer it.
func (c *Canton) BalanceOf(holder, symbol string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var total int64
	for _, id := range c.order {
		k := c.contracts[id]
		if k.Archived || k.Symbol != symbol || k.Owner != holder {
			continue
		}
		if k.Template == TemplateFundShare || k.Template == TemplatePledge {
			total += k.Quantity
		}
	}
	return total
}

// TotalSupply returns every unit of symbol held on this participant.
func (c *Canton) TotalSupply(symbol string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var total int64
	for _, id := range c.order {
		k := c.contracts[id]
		if k.Archived || k.Symbol != symbol {
			continue
		}
		if k.Template == TemplateFundShare || k.Template == TemplatePledge {
			total += k.Quantity
		}
	}
	return total
}

// Locked returns the units of symbol held by the bridge vault party.
func (c *Canton) Locked(symbol string) int64 { return c.BalanceOf(VaultParty, symbol) }

// Head returns the current ledger offset as a Block. Canton has no blocks; the
// offset is the transaction count and it is always final.
func (c *Canton) Head() Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := Block{Height: c.txSeq, Hash: string(c.lastTx), Time: c.genesis, Finalised: true}
	if n := len(c.txs); n > 0 {
		b.Time = c.txs[n-1].EffectiveAt
		b.TxCount = 1
		if n > 1 {
			b.ParentHash = string(c.txs[n-2].ID)
		}
	}
	return b
}

// Events returns events with Seq at or above from.
func (c *Canton) Events(from uint64) []domain.LedgerEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]domain.LedgerEvent, 0, len(c.events))
	for _, e := range c.events {
		if e.Seq >= from {
			out = append(out, e)
		}
	}
	return out
}

// Subscribe returns a channel receiving events as they are emitted.
func (c *Canton) Subscribe() <-chan domain.LedgerEvent {
	ch := make(chan domain.LedgerEvent, subscribeBuffer)
	c.mu.Lock()
	c.subs = append(c.subs, ch)
	c.mu.Unlock()
	return ch
}

// Contracts returns every contract, active and archived, in creation order.
// It is the participant operator's view and no party has it.
func (c *Canton) Contracts() []Contract {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Contract, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, *c.contracts[id])
	}
	return out
}

// ContractsVisibleTo returns the active contracts a party is a stakeholder in,
// in creation order. This is the selective disclosure boundary: a party that is
// neither a signatory nor an observer of a contract does not learn that it
// exists, and there is no query that gets around it.
func (c *Canton) ContractsVisibleTo(party string) []Contract {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Contract, 0, 8)
	for _, id := range c.order {
		k := c.contracts[id]
		if k.Archived || !k.VisibleTo(party) {
			continue
		}
		out = append(out, *k)
	}
	return out
}

// FetchAs reads one contract as a party. A party that cannot see the contract
// gets ErrNotVisible rather than a redacted contract, because "you may not know
// whether this exists" and "this exists but you may not read it" are different
// statements and only the first one is true here.
func (c *Canton) FetchAs(party string, id ContractID) (Contract, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, ok := c.contracts[id]
	if !ok || k.Archived {
		return Contract{}, fmt.Errorf("%w: %s", ErrNoSuchContract, id)
	}
	if !k.VisibleTo(party) {
		return Contract{}, fmt.Errorf("%w: %s cannot see %s", ErrNotVisible, party, id)
	}
	return *k, nil
}

// LookupHolding resolves a party's FundShare holding by contract key, as a
// reading party. Daml requires a key lookup to be made by a stakeholder, so a
// party that cannot see the holding gets ErrNotVisible rather than the key's
// existence.
func (c *Canton) LookupHolding(reader, owner, symbol string) (Contract, error) {
	c.mu.Lock()
	id, ok := c.byKey[holdingKey(c.issuer, symbol, owner, TemplateFundShare)]
	c.mu.Unlock()
	if !ok {
		return Contract{}, fmt.Errorf("%w: holding of %s by %s", ErrNoSuchContract, symbol, owner)
	}
	return c.FetchAs(reader, id)
}

// Transactions returns the committed transaction trees in order.
func (c *Canton) Transactions() []Transaction {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Transaction, len(c.txs))
	copy(out, c.txs)
	return out
}

// TransactionsVisibleTo projects the transaction stream onto one party: each
// transaction keeps only the nodes touching a contract that party is a
// stakeholder in, and a transaction with no such node is not returned at all.
// This is what a party's ledger API stream actually looks like.
func (c *Canton) TransactionsVisibleTo(party string) []Transaction {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Transaction, 0, len(c.txs))
	for _, tx := range c.txs {
		roots := c.projectNodes(tx.Roots, party)
		if len(roots) == 0 {
			continue
		}
		p := tx
		p.Roots = roots
		out = append(out, p)
	}
	return out
}

func (c *Canton) projectNodes(ns []Node, party string) []Node {
	out := make([]Node, 0, len(ns))
	for _, n := range ns {
		kids := c.projectNodes(n.Children, party)
		k, ok := c.contracts[n.Contract]
		visible := ok && k.VisibleTo(party)
		if !visible && len(kids) == 0 {
			continue
		}
		p := n
		p.Children = kids
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// --- writes ---------------------------------------------------------------

// commit opens a transaction, runs body, and commits it atomically. body
// appends nodes and events; if it returns an error nothing is written, which is
// what makes a Daml transaction all-or-nothing.
type txBuilder struct {
	c       *Canton
	command string
	actor   string
	at      time.Time
	roots   []Node
	events  []domain.LedgerEvent
	id      TransactionID
}

func (c *Canton) begin(command, actor string) *txBuilder {
	return &txBuilder{c: c, command: command, actor: actor, at: c.opts.Clock.Now(), id: c.nextTxID(command, actor)}
}

func (c *Canton) nextTxID(command, actor string) TransactionID {
	sum := sha256.Sum256([]byte(strconv.FormatUint(c.txSeq+1, 10) + "|" + command + "|" + actor))
	return TransactionID("tx-" + hex.EncodeToString(sum[:8]))
}

func (b *txBuilder) create(k Contract) ContractID {
	b.c.cidSeq++
	sum := sha256.Sum256([]byte(string(b.id) + "|" + strconv.FormatUint(b.c.cidSeq, 10)))
	id := ContractID("00" + hex.EncodeToString(sum[:16]))
	k.ID = id
	k.CreatedAt = b.at
	k.CreatedBy = b.id
	sort.Strings(k.Signatories)
	sort.Strings(k.Observers)
	kk := k
	b.c.contracts[id] = &kk
	b.c.order = append(b.c.order, id)
	if k.Key != "" {
		b.c.byKey[k.Key] = id
	}
	return id
}

func (b *txBuilder) archive(id ContractID) {
	k := b.c.contracts[id]
	k.Archived = true
	k.ArchivedAt = b.at
	k.ArchivedBy = b.id
	if k.Key != "" && b.c.byKey[k.Key] == id {
		delete(b.c.byKey, k.Key)
	}
}

func (b *txBuilder) emit(kind domain.EventKind, symbol, from, to string, qty int64, memo string) {
	b.c.seq++
	b.events = append(b.events, domain.LedgerEvent{
		Seq: b.c.seq, Ledger: domain.LedgerCanton, Kind: kind, Symbol: symbol,
		From: b.c.opts.label(from), To: b.c.opts.label(to), Quantity: qty,
		Block: b.c.txSeq + 1, TxHash: string(b.id), LogIndex: len(b.events),
		OccurredAt: b.at, ObservedAt: b.at.Add(b.c.opts.ObservationLag),
		// A Canton transaction is final when it is committed. There is no
		// probabilistic settlement here to wait out.
		Finalised: true,
		Memo:      memo,
	})
}

func (b *txBuilder) commit() Receipt {
	b.c.txSeq++
	b.c.lastTx = b.id
	b.c.txs = append(b.c.txs, Transaction{
		ID: b.id, Offset: b.c.txSeq, Command: b.command, Actor: b.actor,
		EffectiveAt: b.at, Roots: b.roots,
	})
	b.c.events = append(b.c.events, b.events...)
	for _, e := range b.events {
		fanout(b.c.subs, e)
	}
	b.c.metrics.Transactions.Inc()
	b.c.metrics.Head.Set(float64(b.c.txSeq))
	b.c.metrics.Finalised.Set(float64(b.c.txSeq))
	return Receipt{TxHash: string(b.id), Block: b.c.txSeq, Success: true}
}

func holdingKey(issuer, symbol, owner string, tpl Template) string {
	return string(tpl) + "|" + issuer + "|" + symbol + "|" + owner
}

// activeHolding returns a party's FundShare contract for a symbol.
func (c *Canton) activeHolding(owner, symbol string) (ContractID, *Contract, bool) {
	id, ok := c.byKey[holdingKey(c.issuer, symbol, owner, TemplateFundShare)]
	if !ok {
		return "", nil, false
	}
	k := c.contracts[id]
	if k == nil || k.Archived {
		return "", nil, false
	}
	return id, k, true
}

func (c *Canton) newHolding(b *txBuilder, owner, symbol string, qty int64) (Node, ContractID) {
	id := b.create(Contract{
		Template: TemplateFundShare, Signatories: []string{c.issuer}, Observers: []string{owner},
		Key: holdingKey(c.issuer, symbol, owner, TemplateFundShare), Maintainer: c.issuer,
		Issuer: c.issuer, Owner: owner, Symbol: symbol, Quantity: qty,
	})
	return Node{Kind: NodeCreate, Contract: id, Template: TemplateFundShare}, id
}

// credit adds qty to a party's holding by archiving what it has and creating a
// replacement. That is the Daml idiom and it is not an implementation detail:
// there is no cell to increment, so every balance change leaves an archive and
// a create in the transaction tree.
func (c *Canton) credit(b *txBuilder, owner, symbol string, qty int64) []Node {
	var nodes []Node
	total := qty
	if id, k, ok := c.activeHolding(owner, symbol); ok {
		total += k.Quantity
		b.archive(id)
		nodes = append(nodes, Node{Kind: NodeArchive, Contract: id, Template: TemplateFundShare})
	}
	n, _ := c.newHolding(b, owner, symbol, total)
	return append(nodes, n)
}

// debit removes qty from a party's holding, archiving it and recreating the
// remainder. The caller must have checked the balance.
func (c *Canton) debit(b *txBuilder, owner, symbol string, qty int64) []Node {
	id, k, ok := c.activeHolding(owner, symbol)
	if !ok {
		return nil
	}
	rest := k.Quantity - qty
	b.archive(id)
	nodes := []Node{{Kind: NodeArchive, Contract: id, Template: TemplateFundShare}}
	if rest > 0 {
		n, _ := c.newHolding(b, owner, symbol, rest)
		nodes = append(nodes, n)
	}
	return nodes
}

func (c *Canton) revert(op, reason string) (Receipt, error) {
	c.metrics.Reverts.Inc()
	return Receipt{Success: false, RevertReason: reason}, &RevertError{Op: op, Reason: reason}
}

func (c *Canton) span(ctx context.Context, op string, symbol string, qty int64) *obs.Span {
	if c.opts.Tracer == nil {
		return nil
	}
	_, sp := c.opts.Tracer.Start(ctx, "ledger.canton."+op, obs.KindClient)
	sp.SetAttrs(map[string]any{
		"tcrp.ledger":        string(domain.LedgerCanton),
		"tcrp.ledger.op":     op,
		"tcrp.ledger.symbol": symbol,
		"tcrp.ledger.qty":    qty,
	})
	return sp
}

// Mint issues units to a party, creating or growing its holding. Only the
// issuer may exercise it, because the issuer is the sole signatory of every
// holding and nothing else can create one.
func (c *Canton) Mint(ctx context.Context, caller, to, symbol string, qty int64, memo string) (Receipt, error) {
	sp := c.span(ctx, "mint", symbol, qty)
	defer sp.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	if caller != c.issuer {
		return c.revert("mint", "FundShare: only the issuer may issue")
	}
	if qty <= 0 {
		return c.revert("mint", "FundShare: quantity must be positive")
	}
	b := c.begin("Issue", caller)
	b.roots = append(b.roots, Node{Kind: NodeExercise, Choice: "Issue", Actor: caller, Template: TemplateFundShare,
		Children: c.credit(b, to, symbol, qty)})
	b.emit(domain.EventMint, symbol, "", to, qty, memo)
	return b.commit(), nil
}

// Burn destroys units held by a party. Issuer-authorised, because redemption
// retires the issuer's own obligation.
func (c *Canton) Burn(ctx context.Context, caller, from, symbol string, qty int64, memo string) (Receipt, error) {
	sp := c.span(ctx, "burn", symbol, qty)
	defer sp.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	if caller != c.issuer {
		return c.revert("burn", "FundShare: only the issuer may redeem")
	}
	if qty <= 0 {
		return c.revert("burn", "FundShare: quantity must be positive")
	}
	if _, k, ok := c.activeHolding(from, symbol); !ok || k.Quantity < qty {
		return c.revert("burn", "FundShare: insufficient holding")
	}
	b := c.begin("Redeem", caller)
	b.roots = append(b.roots, Node{Kind: NodeExercise, Choice: "Redeem", Actor: caller, Template: TemplateFundShare,
		Children: c.debit(b, from, symbol, qty)})
	b.emit(domain.EventBurn, symbol, from, "", qty, memo)
	return b.commit(), nil
}

// Transfer moves units in one step, exercising the Transfer choice on the
// sender's holding. The controller is the owner; the issuer's authority to
// create the new holdings comes from the choice being on a contract the issuer
// signed, which is exactly how delegation works in Daml.
//
// It is the direct path. Where the recipient has to consent — which is the
// normal case for a security — use ProposeTransfer and Accept.
func (c *Canton) Transfer(ctx context.Context, caller, from, to, symbol string, qty int64, memo string) (Receipt, error) {
	sp := c.span(ctx, "transfer", symbol, qty)
	defer sp.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	if caller != from {
		return c.revert("transfer", "FundShare: only the owner may transfer")
	}
	if qty <= 0 {
		return c.revert("transfer", "FundShare: quantity must be positive")
	}
	id, k, ok := c.activeHolding(from, symbol)
	if !ok || k.Quantity < qty {
		return c.revert("transfer", "FundShare: insufficient holding")
	}
	b := c.begin("Transfer", caller)
	kids := c.debit(b, from, symbol, qty)
	kids = append(kids, c.credit(b, to, symbol, qty)...)
	b.roots = append(b.roots, Node{Kind: NodeExercise, Contract: id, Template: TemplateFundShare,
		Choice: "Transfer", Actor: caller, Children: kids})
	b.emit(domain.EventTransfer, symbol, from, to, qty, memo)
	return b.commit(), nil
}

// ProposeTransfer creates a TransferProposal: the first half of Daml's
// propose-accept pattern. The sender's units are debited into the proposal, so
// they cannot be spent twice while the recipient decides, and the recipient is
// an observer so it can see what it is being offered.
//
// This is the idiom that distinguishes a Daml asset from an ERC-20. On an
// ERC-20 the sender can push tokens at any address and the recipient finds out
// afterwards. Here nothing lands in a party's portfolio without that party
// having exercised a choice.
func (c *Canton) ProposeTransfer(ctx context.Context, caller, to, symbol string, qty int64, memo string) (ContractID, Receipt, error) {
	sp := c.span(ctx, "propose_transfer", symbol, qty)
	defer sp.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	if qty <= 0 {
		rc, err := c.revert("propose_transfer", "TransferProposal: quantity must be positive")
		return "", rc, err
	}
	if to == caller {
		rc, err := c.revert("propose_transfer", "TransferProposal: recipient must differ from owner")
		return "", rc, err
	}
	id, k, ok := c.activeHolding(caller, symbol)
	if !ok || k.Quantity < qty {
		rc, err := c.revert("propose_transfer", "FundShare: insufficient holding")
		return "", rc, err
	}
	b := c.begin("ProposeTransfer", caller)
	kids := c.debit(b, caller, symbol, qty)
	pid := b.create(Contract{
		Template: TemplateTransferProposal, Signatories: []string{c.issuer},
		Observers: []string{caller, to}, Maintainer: c.issuer,
		Issuer: c.issuer, Owner: caller, Recipient: to, Symbol: symbol, Quantity: qty,
	})
	kids = append(kids, Node{Kind: NodeCreate, Contract: pid, Template: TemplateTransferProposal})
	b.roots = append(b.roots, Node{Kind: NodeExercise, Contract: id, Template: TemplateFundShare,
		Choice: "ProposeTransfer", Actor: caller, Children: kids})
	return pid, b.commit(), nil
}

// AcceptTransfer completes a proposal. The controller is the recipient.
func (c *Canton) AcceptTransfer(ctx context.Context, caller string, pid ContractID, memo string) (Receipt, error) {
	sp := c.span(ctx, "accept_transfer", "", 0)
	defer sp.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.contracts[pid]
	if !ok || p.Archived || p.Template != TemplateTransferProposal {
		return c.revert("accept_transfer", "TransferProposal: no such proposal")
	}
	if caller != p.Recipient {
		return c.revert("accept_transfer", "TransferProposal: only the recipient may accept")
	}
	b := c.begin("Accept", caller)
	b.archive(pid)
	kids := []Node{{Kind: NodeArchive, Contract: pid, Template: TemplateTransferProposal}}
	kids = append(kids, c.credit(b, p.Recipient, p.Symbol, p.Quantity)...)
	b.roots = append(b.roots, Node{Kind: NodeExercise, Contract: pid, Template: TemplateTransferProposal,
		Choice: "Accept", Actor: caller, Children: kids})
	b.emit(domain.EventTransfer, p.Symbol, p.Owner, p.Recipient, p.Quantity, memo)
	return b.commit(), nil
}

// RejectTransfer archives a proposal and returns the units to the sender.
// Either the recipient or the sender may exercise it: the recipient declines,
// the sender withdraws.
func (c *Canton) RejectTransfer(ctx context.Context, caller string, pid ContractID, memo string) (Receipt, error) {
	sp := c.span(ctx, "reject_transfer", "", 0)
	defer sp.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.contracts[pid]
	if !ok || p.Archived || p.Template != TemplateTransferProposal {
		return c.revert("reject_transfer", "TransferProposal: no such proposal")
	}
	if caller != p.Recipient && caller != p.Owner {
		return c.revert("reject_transfer", "TransferProposal: only the recipient or the owner may reject")
	}
	b := c.begin("Reject", caller)
	b.archive(pid)
	kids := []Node{{Kind: NodeArchive, Contract: pid, Template: TemplateTransferProposal}}
	kids = append(kids, c.credit(b, p.Owner, p.Symbol, p.Quantity)...)
	b.roots = append(b.roots, Node{Kind: NodeExercise, Contract: pid, Template: TemplateTransferProposal,
		Choice: "Reject", Actor: caller, Children: kids})
	return b.commit(), nil
}

// Pledge encumbers part of a holding in favour of a pledgee, who becomes an
// observer of the resulting contract. The collateral taker can therefore see
// the collateral it holds — which is the whole point — without acquiring any
// authority to move it.
func (c *Canton) Pledge(ctx context.Context, caller, pledgee, symbol string, qty int64, memo string) (ContractID, Receipt, error) {
	sp := c.span(ctx, "pledge", symbol, qty)
	defer sp.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	if qty <= 0 {
		rc, err := c.revert("pledge", "Pledge: quantity must be positive")
		return "", rc, err
	}
	id, k, ok := c.activeHolding(caller, symbol)
	if !ok || k.Quantity < qty {
		rc, err := c.revert("pledge", "FundShare: insufficient holding")
		return "", rc, err
	}
	b := c.begin("Pledge", caller)
	kids := c.debit(b, caller, symbol, qty)
	pid := b.create(Contract{
		Template: TemplatePledge, Signatories: []string{c.issuer}, Observers: []string{caller, pledgee},
		Key: holdingKey(c.issuer, symbol, caller+"@"+pledgee, TemplatePledge), Maintainer: c.issuer,
		Issuer: c.issuer, Owner: caller, Pledgee: pledgee, Symbol: symbol, Quantity: qty,
	})
	kids = append(kids, Node{Kind: NodeCreate, Contract: pid, Template: TemplatePledge})
	b.roots = append(b.roots, Node{Kind: NodeExercise, Contract: id, Template: TemplateFundShare,
		Choice: "Pledge", Actor: caller, Children: kids})
	b.emit(domain.EventPledge, symbol, caller, pledgee, qty, memo)
	return pid, b.commit(), nil
}

// Release returns a pledged holding to its owner. The controller is the
// pledgee: collateral is released by the taker, not taken back by the giver.
func (c *Canton) Release(ctx context.Context, caller string, pid ContractID, memo string) (Receipt, error) {
	sp := c.span(ctx, "release", "", 0)
	defer sp.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.contracts[pid]
	if !ok || p.Archived || p.Template != TemplatePledge {
		return c.revert("release", "Pledge: no such pledge")
	}
	if caller != p.Pledgee {
		return c.revert("release", "Pledge: only the pledgee may release")
	}
	b := c.begin("Release", caller)
	b.archive(pid)
	kids := []Node{{Kind: NodeArchive, Contract: pid, Template: TemplatePledge}}
	kids = append(kids, c.credit(b, p.Owner, p.Symbol, p.Quantity)...)
	b.roots = append(b.roots, Node{Kind: NodeExercise, Contract: pid, Template: TemplatePledge,
		Choice: "Release", Actor: caller, Children: kids})
	b.emit(domain.EventRelease, p.Symbol, p.Pledgee, p.Owner, p.Quantity, memo)
	return b.commit(), nil
}

// Lock moves units into the bridge vault party. Issuer-authorised.
func (c *Canton) Lock(ctx context.Context, caller, holder, symbol string, qty int64, memo string) (Receipt, error) {
	sp := c.span(ctx, "lock", symbol, qty)
	defer sp.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	if caller != c.issuer {
		return c.revert("lock", "BridgeVault: only the issuer may lock")
	}
	if _, k, ok := c.activeHolding(holder, symbol); !ok || k.Quantity < qty || qty <= 0 {
		return c.revert("lock", "FundShare: insufficient holding")
	}
	b := c.begin("Lock", caller)
	kids := c.debit(b, holder, symbol, qty)
	kids = append(kids, c.credit(b, VaultParty, symbol, qty)...)
	b.roots = append(b.roots, Node{Kind: NodeExercise, Choice: "Lock", Actor: caller, Template: TemplateFundShare, Children: kids})
	b.emit(domain.EventLock, symbol, holder, "", qty, memo)
	return b.commit(), nil
}

// Unlock returns units from the bridge vault party. Issuer-authorised.
func (c *Canton) Unlock(ctx context.Context, caller, holder, symbol string, qty int64, memo string) (Receipt, error) {
	sp := c.span(ctx, "unlock", symbol, qty)
	defer sp.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	if caller != c.issuer {
		return c.revert("unlock", "BridgeVault: only the issuer may unlock")
	}
	if _, k, ok := c.activeHolding(VaultParty, symbol); !ok || k.Quantity < qty || qty <= 0 {
		return c.revert("unlock", "BridgeVault: insufficient locked balance")
	}
	b := c.begin("Unlock", caller)
	kids := c.debit(b, VaultParty, symbol, qty)
	kids = append(kids, c.credit(b, holder, symbol, qty)...)
	b.roots = append(b.roots, Node{Kind: NodeExercise, Choice: "Unlock", Actor: caller, Template: TemplateFundShare, Children: kids})
	b.emit(domain.EventUnlock, symbol, "", holder, qty, memo)
	return b.commit(), nil
}
