// Package ledger contains in-process simulators of the two settlement networks
// the platform settles on: an ERC-3643-style permissioned token on an EVM chain
// and a Daml contract store on a Canton network.
//
// These are simulators, not chains. They model the semantics the rest of the
// platform depends on — permissioned transfer restrictions, role-gated
// administration, block finality and reorganisation on the EVM side, and
// archive-and-recreate contract lifecycles with selective disclosure on the
// Canton side. They do not execute EVM bytecode, they do not implement the
// Daml-LF interpreter, and there is no consensus, no gas, no signing of
// transactions and no participant node. A holder is a string key in a map.
//
// The production artefacts are the Solidity sources under contracts/ and the
// Daml sources under daml/. Those are what would be deployed; the rules in this
// package mirror them, and the tests here assert the same properties the
// Foundry and Daml Script suites assert. Neither of those toolchains is invoked
// by this repository's build.
//
// The simulators exist for two reasons. The first is that the whole platform
// has to run from a clean clone with nothing but a Go toolchain: no Anvil, no
// Canton sandbox, no container runtime. The second is that the cross-ledger
// relayer in internal/bridge has failure modes — a lock orphaned by a reorg
// before finality, a lock orphaned after finality, an event re-observed after a
// chain rewrite — that are the whole reason the relayer is written the way it
// is, and they have to be reproducible on demand rather than waited for.
//
// Nothing here reads the wall clock. Every simulator takes an obs.Clock, block
// times come from it, and identical call sequences produce identical event
// streams, block hashes and transaction hashes.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Errors returned by this package. They are sentinels because a caller that has
// to tell "the compliance module refused this" from "you are not an agent" is
// making two different decisions, and matching on a revert string is not a
// decision anybody should have to defend.
var (
	// ErrReverted is matched by every RevertError. A reverted write left no
	// state change behind, exactly as an EVM revert would.
	ErrReverted = errors.New("ledger: transaction reverted")
	// ErrUnauthorised is returned when the caller lacks the role a write
	// requires.
	ErrUnauthorised = errors.New("ledger: caller lacks the required role")
	// ErrNotVisible is returned by the Canton simulator when a party asks to
	// read a contract it is neither a signatory nor an observer of.
	ErrNotVisible = errors.New("ledger: contract not visible to party")
	// ErrNoSuchContract is returned when a contract id does not exist or has
	// already been archived.
	ErrNoSuchContract = errors.New("ledger: no such active contract")
	// ErrBadReorg is returned when a requested reorganisation is impossible:
	// deeper than the chain, or non-positive.
	ErrBadReorg = errors.New("ledger: reorg depth out of range")
	// ErrUnsupported is returned by an operation a particular ledger does not
	// model. Canton has no block reorganisation, for instance.
	ErrUnsupported = errors.New("ledger: operation not supported by this ledger")
)

// RevertError is the ledger refusing a write on its own rules. It carries the
// reason string the corresponding Solidity require or Daml ensure clause would
// have produced, so a test can assert on which rule fired rather than that
// something failed.
type RevertError struct {
	// Op names the operation that reverted, such as "transfer".
	Op string
	// Reason is the revert string.
	Reason string
}

// Error renders the revert.
func (e *RevertError) Error() string { return fmt.Sprintf("ledger: %s reverted: %s", e.Op, e.Reason) }

// Is reports that every RevertError matches ErrReverted.
func (e *RevertError) Is(target error) bool { return target == ErrReverted }

// Revert reports whether err is a revert and returns its reason.
func Revert(err error) (string, bool) {
	var re *RevertError
	if errors.As(err, &re) {
		return re.Reason, true
	}
	return "", false
}

// Block is one step of a ledger's history. On the EVM simulator it is a block;
// on Canton, which has no blocks, it is the participant's ledger offset and
// Finalised is always true because Canton's commit is deterministically final.
type Block struct {
	Height     uint64    `json:"height"`
	Hash       string    `json:"hash"`
	ParentHash string    `json:"parent_hash"`
	Time       time.Time `json:"time"`
	Finalised  bool      `json:"finalised"`
	// TxCount is the number of transactions the block carries. The EVM
	// simulator produces at most one, which keeps block height and transaction
	// order the same ordering and costs nothing the relayer cares about.
	TxCount int `json:"tx_count"`
}

// Receipt is the outcome of a write. A reverted write returns a receipt with
// Success false and the reason set, alongside a *RevertError, so a caller may
// either inspect the receipt or match the error.
type Receipt struct {
	TxHash       string `json:"tx_hash"`
	Block        uint64 `json:"block"`
	Success      bool   `json:"success"`
	RevertReason string `json:"revert_reason,omitempty"`
}

// Writer is the set of state-changing operations both ledgers implement. Every
// one names the caller, because authority is not ambient: an EVM write is
// checked against the role registry and a Daml exercise against the choice's
// controller, and a simulator that let the process act as anybody would model
// neither.
type Writer interface {
	// Mint issues new units to a holder. Agent-authorised.
	Mint(ctx context.Context, caller, to, symbol string, qty int64, memo string) (Receipt, error)
	// Burn destroys units held by a holder. Agent-authorised.
	Burn(ctx context.Context, caller, from, symbol string, qty int64, memo string) (Receipt, error)
	// Transfer moves units between holders subject to the ledger's transfer
	// restrictions.
	Transfer(ctx context.Context, caller, from, to, symbol string, qty int64, memo string) (Receipt, error)
	// Lock moves units into the bridge vault. Agent-authorised. The vault
	// holding is what the cross-ledger supply control reconciles against.
	Lock(ctx context.Context, caller, holder, symbol string, qty int64, memo string) (Receipt, error)
	// Unlock returns units from the bridge vault to a holder.
	Unlock(ctx context.Context, caller, holder, symbol string, qty int64, memo string) (Receipt, error)
}

// Ledger is the read and write surface the platform programs against. It is
// deliberately narrow: the bridge, the risk recomputer and the lakehouse feed
// all work through it, and neither of them can reach a simulator-only detail
// such as the EVM's reorg control by holding this interface.
type Ledger interface {
	// Name is the network this ledger represents.
	Name() domain.Ledger
	// BalanceOf is the units of symbol held by holder.
	BalanceOf(holder, symbol string) int64
	// TotalSupply is every unit of symbol in existence on this ledger,
	// including units sitting in the bridge vault.
	TotalSupply(symbol string) int64
	// Locked is the units of symbol held by the bridge vault.
	Locked(symbol string) int64
	// Head is the newest block, or the current ledger offset.
	Head() Block
	// Events returns the canonical chain's events with Seq at or above from,
	// in order. After a reorganisation the caller must rescan from zero:
	// sequence numbers survive re-inclusion but orphaned events simply stop
	// being returned, so a high-water cursor cannot see that they went.
	Events(from uint64) []domain.LedgerEvent
	// Subscribe returns a channel receiving events as they are emitted. The
	// channel is buffered and drops rather than blocking the writer, so a slow
	// consumer degrades its own view instead of the ledger's throughput. An
	// event re-emitted after a reorganisation arrives again with the same
	// TxHash and a different Block; ingestion is idempotent on
	// domain.LedgerEvent.ID, so that is safe.
	Subscribe() <-chan domain.LedgerEvent

	Writer
}

// Options configure a simulator. It is one struct for both so the two ledgers
// are constructed the same way; each field says which of them reads it.
type Options struct {
	// Clock is the only source of time. Block times and observation times come
	// from it and nothing in this package calls time.Now.
	Clock obs.Clock
	// Tracer, when set, receives a span per write.
	Tracer *obs.Tracer
	// Metrics, when set, receives transaction, revert and reorg counters.
	Metrics *obs.Registry
	// Confirmations is how many blocks must sit on top of a block before it is
	// final. EVM only; zero defaults to one, so the block at the head is never
	// final and a caller cannot forget to wait. Canton ignores it because its
	// commit is already final.
	Confirmations uint64
	// ObservationLag is added to a block's time to produce an event's
	// ObservedAt. It models the indexer's delay and is fixed rather than random
	// so that a replay is byte-identical.
	ObservationLag time.Duration
	// GenesisBlock is the height of the EVM simulator's genesis block. It
	// defaults to the height the seeded event stream starts from so that
	// simulated and seeded events sit in the same range.
	GenesisBlock uint64
	// Compliance is the EVM token's compliance configuration. The zero value
	// imposes no holder or balance ceilings.
	Compliance ComplianceRules
	// Issuer is the Daml party that signs every asset contract. Canton only.
	Issuer string
	// HolderLabels maps a ledger address or Daml party to the counterparty
	// identifier the rest of the platform uses. Emitted events carry the label
	// where one exists, because the lakehouse reconciles against a book of
	// record that has never heard of a hex address.
	HolderLabels map[string]string
}

func (o Options) resolve() Options {
	if o.Clock == nil {
		o.Clock = obs.RealClock{}
	}
	if o.Confirmations == 0 {
		o.Confirmations = 1
	}
	if o.GenesisBlock == 0 {
		o.GenesisBlock = defaultGenesisBlock
	}
	if o.Issuer == "" {
		o.Issuer = DefaultIssuerParty
	}
	return o
}

func (o Options) label(addr string) string {
	if l, ok := o.HolderLabels[addr]; ok && l != "" {
		return l
	}
	return addr
}

// defaultGenesisBlock is the height the seeded event stream starts from.
const defaultGenesisBlock = 21_400_000

// VaultAddress is the EVM address of the bridge vault: the account tokens move
// into when they are locked for a cross-ledger transfer. It is a plain holder
// on the token, which is what makes the supply control checkable — the locked
// balance is a balance, not a number the bridge keeps to itself.
const VaultAddress = "0x00000000000000000000000000000000000bd1d6"

// VaultParty is the Canton equivalent of VaultAddress.
const VaultParty = "BridgeVault::1220"

// DefaultIssuerParty is the Daml party that signs every asset contract.
const DefaultIssuerParty = "Issuer::1220"

// Bridge memo operations. A lock event has to carry the bridge nonce that keys
// replay protection, and it has to survive a chain reorganisation re-including
// the transaction. Encoding it in the event memo is how these simulators model
// an indexed event parameter; a real deployment reads it from the log topic.
const (
	BridgeOpLock   = "lock"
	BridgeOpUnlock = "unlock"
	BridgeOpMint   = "mint"
	BridgeOpBurn   = "burn"
)

const bridgeMemoPrefix = "bridge:"

// BridgeMemo renders the memo a bridge operation writes onto its event.
func BridgeMemo(op string, nonce uint64) string {
	return bridgeMemoPrefix + op + ":" + strconv.FormatUint(nonce, 10)
}

// ParseBridgeMemo reads a memo written by BridgeMemo. It reports false for any
// other memo, which is how the supply control tells a bridge mint from a
// primary issuance.
func ParseBridgeMemo(memo string) (op string, nonce uint64, ok bool) {
	rest, found := strings.CutPrefix(memo, bridgeMemoPrefix)
	if !found {
		return "", 0, false
	}
	op, num, found := strings.Cut(rest, ":")
	if !found {
		return "", 0, false
	}
	switch op {
	case BridgeOpLock, BridgeOpUnlock, BridgeOpMint, BridgeOpBurn:
	default:
		return "", 0, false
	}
	n, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return op, n, true
}

// Metric names. They are constants because a dashboard, an alert rule and a
// test all have to agree on the string.
const (
	MetricTransactions = "tcrp_ledger_transactions_total"
	MetricReverts      = "tcrp_ledger_reverts_total"
	MetricReorgs       = "tcrp_ledger_reorgs_total"
	MetricOrphaned     = "tcrp_ledger_orphaned_transactions_total"
	MetricHead         = "tcrp_ledger_head_block"
	MetricFinalised    = "tcrp_ledger_finalised_block"
)

// Metrics is a simulator's instrumentation. A nil registry is replaced by a
// private one, so instrumentation is never a reason for a caller to have to
// build observability plumbing it does not otherwise need.
type Metrics struct {
	// Transactions counts committed writes.
	Transactions *obs.Counter
	// Reverts counts writes refused by a ledger rule.
	Reverts *obs.Counter
	// Reorgs counts chain reorganisations.
	Reorgs *obs.Counter
	// Orphaned counts transactions a reorganisation removed from the canonical
	// chain.
	Orphaned *obs.Counter
	// Head is the newest block height.
	Head *obs.Gauge
	// Finalised is the newest final block height.
	Finalised *obs.Gauge
}

// NewMetrics registers a simulator's metrics on r, labelled by ledger.
func NewMetrics(r *obs.Registry, l domain.Ledger) *Metrics {
	if r == nil {
		r = obs.NewRegistry()
	}
	lb := map[string]string{"ledger": string(l)}
	return &Metrics{
		Transactions: r.Counter(MetricTransactions, "ledger writes committed", lb),
		Reverts:      r.Counter(MetricReverts, "ledger writes refused by a ledger rule", lb),
		Reorgs:       r.Counter(MetricReorgs, "chain reorganisations", lb),
		Orphaned:     r.Counter(MetricOrphaned, "transactions removed from the canonical chain", lb),
		Head:         r.Gauge(MetricHead, "newest block height", lb),
		Finalised:    r.Gauge(MetricFinalised, "newest final block height", lb),
	}
}

// fanout delivers an event to subscribers without blocking on any of them.
func fanout(subs []chan domain.LedgerEvent, e domain.LedgerEvent) {
	for _, c := range subs {
		select {
		case c <- e:
		default:
		}
	}
}

// subscribeBuffer is the depth of a subscription channel. It is generous
// because a subscriber that falls this far behind has a problem a bigger buffer
// would only hide.
const subscribeBuffer = 256
