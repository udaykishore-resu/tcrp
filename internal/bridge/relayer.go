package bridge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/ledger"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Direction names which way a flow moves value.
type Direction string

// Directions.
const (
	// DirectionMint is lock on the source, mint on the target.
	DirectionMint Direction = "lock_and_mint"
	// DirectionRedeem is burn on the target, unlock on the source.
	DirectionRedeem Direction = "burn_and_unlock"
)

// AlertKind classifies a condition the relayer cannot resolve by itself.
type AlertKind string

// Alert kinds.
const (
	// AlertFinalityViolation says a settled flow's source transaction has left
	// the canonical chain after the relayer treated it as final. Units exist
	// on the target with nothing behind them. Nothing is minted or burned in
	// response; a human unwinds it.
	AlertFinalityViolation AlertKind = "finality_violation"
	// AlertAbandoned says a pending flow's source transaction was orphaned
	// before it was final. Nothing was minted, so nothing has to be undone —
	// but the initiator is waiting for units that are never coming.
	AlertAbandoned AlertKind = "attestation_abandoned"
	// AlertQuorumUnavailable says too few signers were reachable to authorise
	// a settlement. The flow stays pending and is retried on the next poll.
	AlertQuorumUnavailable AlertKind = "quorum_unavailable"
	// AlertSubmissionFailed says every retry of a target submission failed.
	AlertSubmissionFailed AlertKind = "submission_failed"
)

// Alert is a condition an operator has to see. It is a value rather than a log
// line because the conformance suite asserts on it and a dashboard renders it.
type Alert struct {
	Kind      AlertKind `json:"kind"`
	Direction Direction `json:"direction"`
	// Nonce, Symbol, Holder and Quantity identify the flow the alert is about,
	// so that an operator does not have to join back to a settlement record to
	// know what is at stake.
	Nonce        uint64        `json:"nonce"`
	Symbol       string        `json:"symbol"`
	Holder       string        `json:"holder"`
	Quantity     int64         `json:"quantity"`
	SourceTxHash string        `json:"source_tx_hash,omitempty"`
	SourceLedger domain.Ledger `json:"source_ledger"`
	// Detail is a sentence naming what happened and what it means, written for
	// somebody reading it at three in the morning.
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
	// Severity is "critical" where units are unbacked and "high" where a flow
	// is stuck. Nothing here is lower: a bridge that raises routine alerts
	// trains its operators to ignore them.
	Severity string `json:"severity"`
}

// Settlement is a completed flow: the attestation that authorised it, the
// signatures that carried it, and where it landed.
type Settlement struct {
	Nonce        uint64      `json:"nonce"`
	Direction    Direction   `json:"direction"`
	Attestation  Attestation `json:"attestation"`
	Signatures   []Signature `json:"signatures"`
	TargetTxHash string      `json:"target_tx_hash"`
	TargetBlock  uint64      `json:"target_block"`
	SettledAt    time.Time   `json:"settled_at"`
	// Orphaned is set when the source transaction later left the canonical
	// chain. The settlement is not deleted: the units on the target are real
	// and the record of how they got there has to survive the discovery that
	// it should not have happened.
	Orphaned bool `json:"orphaned,omitempty"`
}

// PollResult is what one poll did.
type PollResult struct {
	// Observed is the number of trigger events on the canonical chain.
	Observed int
	// Waiting is the number of flows held back for confirmations or for a
	// quorum.
	Waiting int
	// Settled are the flows completed by this poll.
	Settled []Settlement
	// Abandoned are the nonces whose source transaction was orphaned before
	// finality.
	Abandoned []uint64
	// Alerts are the alerts raised by this poll.
	Alerts []Alert
}

// RetryPolicy governs target submission retries.
type RetryPolicy struct {
	// Attempts is the total number of tries, including the first. Zero means
	// three.
	Attempts int
	// Base is the first backoff interval. Zero means 100ms.
	Base time.Duration
	// Max caps the backoff. Zero means five seconds.
	Max time.Duration
	// Wait is how the relayer sleeps between attempts. A nil Wait does not
	// sleep at all, which is what a test wants: the backoff schedule is
	// deterministic and asserting on it does not require waiting it out.
	// Production sets this to time.Sleep.
	Wait func(time.Duration)
}

func (p RetryPolicy) resolve() RetryPolicy {
	if p.Attempts <= 0 {
		p.Attempts = 3
	}
	if p.Base <= 0 {
		p.Base = 100 * time.Millisecond
	}
	if p.Max <= 0 {
		p.Max = 5 * time.Second
	}
	return p
}

func (p RetryPolicy) wait(d time.Duration) {
	if p.Wait != nil {
		p.Wait(d)
	}
}

// Config configures a relayer or a redeemer.
type Config struct {
	// Source is the ledger units are locked on and unlocked from.
	Source ledger.Ledger
	// Target is the ledger units are minted on and burned from.
	Target ledger.Ledger
	// Symbols the flow relays. A symbol not listed is ignored, so one bridge
	// deployment does not have to relay every instrument on the chain.
	Symbols []string
	// Signers are the attesters this relayer can reach.
	Signers []*Signer
	// SignerSet is the membership and quorum verification runs against. It is
	// separate from Signers because the set is the protocol's and the reachable
	// signers are this process's.
	SignerSet *SignerSet
	// SourceOperator is the caller used for writes on the source ledger.
	SourceOperator string
	// TargetOperator is the caller used for writes on the target ledger.
	TargetOperator string
	// Clock is the only source of time.
	Clock obs.Clock
	// Tracer, when set, receives a span per poll and per settlement.
	Tracer *obs.Tracer
	// Metrics, when set, receives the bridge's counters and histograms.
	Metrics *obs.Registry
	// Audit, when set, receives a record per observed lock and per settlement.
	// A nil log is supported: the bridge is usable in a test that is not about
	// the audit trail.
	Audit *audit.Log
	// Actor and Tenant identify the relayer in audit records.
	Actor  string
	Tenant string
	// Retry governs target submission retries.
	Retry RetryPolicy
}

func (c Config) validate() error {
	switch {
	case c.Source == nil || c.Target == nil:
		return errors.New("bridge: both ledgers are required")
	case c.Source.Name() == c.Target.Name():
		return errors.New("bridge: source and target are the same ledger")
	case c.SignerSet == nil:
		return errors.New("bridge: a signer set is required")
	case len(c.Symbols) == 0:
		return errors.New("bridge: at least one symbol is required")
	}
	return nil
}

func (c Config) resolve() Config {
	if c.Clock == nil {
		c.Clock = obs.RealClock{}
	}
	if c.Actor == "" {
		c.Actor = "bridge-relayer"
	}
	if c.Tenant == "" {
		c.Tenant = "platform"
	}
	c.Retry = c.Retry.resolve()
	c.Symbols = append([]string(nil), c.Symbols...)
	sort.Strings(c.Symbols)
	return c
}

// pipeline is the machinery both directions share: watch a ledger for trigger
// events, wait for finality, gather a quorum, write the response on the other
// ledger, and never write it twice.
//
// It rescans the watched ledger's whole event history on every poll rather than
// following a cursor. That is O(events) per poll and it is the right trade at
// this size, because a cursor cannot see an event disappear — and an event
// disappearing is the case the whole design is about. A production relayer
// keeps a cursor and subscribes to reorg notifications; the loss is efficiency,
// not correctness, and the comment is here so nobody has to reverse-engineer
// the choice.
type pipeline struct {
	mu  sync.Mutex
	cfg Config
	dir Direction

	// from is the ledger carrying the trigger; to is the ledger the response
	// is written on.
	from, to     ledger.Ledger
	triggerKind  domain.EventKind
	triggerOp    string
	responseOp   string
	responseKind domain.EventKind
	submit       func(ctx context.Context, a Attestation) (ledger.Receipt, error)

	auditTrigger  audit.Action
	auditResponse audit.Action
	auditToken    audit.Action

	metrics *Metrics
	symbols map[string]bool

	settled   map[uint64]Settlement
	pending   map[uint64]Attestation
	firstSeen map[uint64]bool
	alerted   map[uint64]map[AlertKind]bool
	alerts    []Alert
}

func newPipeline(cfg Config, dir Direction) (*pipeline, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg = cfg.resolve()
	p := &pipeline{
		cfg: cfg, dir: dir,
		metrics:   NewMetrics(cfg.Metrics, dir),
		symbols:   map[string]bool{},
		settled:   map[uint64]Settlement{},
		pending:   map[uint64]Attestation{},
		firstSeen: map[uint64]bool{},
		alerted:   map[uint64]map[AlertKind]bool{},
	}
	for _, s := range cfg.Symbols {
		p.symbols[s] = true
	}
	return p, nil
}

// Poll runs one cycle. It is safe to call repeatedly and does nothing on a
// cycle in which nothing changed.
func (p *pipeline) Poll(ctx context.Context) (PollResult, error) {
	var span *obs.Span
	if p.cfg.Tracer != nil {
		ctx, span = p.cfg.Tracer.Start(ctx, "bridge.poll", obs.KindInternal)
		span.SetAttrs(map[string]any{
			"tcrp.bridge.direction": string(p.dir),
			"tcrp.bridge.from":      string(p.from.Name()),
			"tcrp.bridge.to":        string(p.to.Name()),
		})
		defer span.End()
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	live := p.liveTriggers()
	res := PollResult{Observed: len(live)}

	// A settled flow whose source transaction is gone is the disaster case.
	// Report it once, mark the settlement orphaned, and do nothing else: there
	// is no automatic unwind that is safe without knowing where the minted
	// units went after they were minted.
	for _, nonce := range sortedNonces(p.settled) {
		if _, ok := live[nonce]; ok {
			continue
		}
		s := p.settled[nonce]
		if s.Orphaned {
			continue
		}
		s.Orphaned = true
		p.settled[nonce] = s
		p.raise(&res, Alert{
			Kind: AlertFinalityViolation, Nonce: nonce, Symbol: s.Attestation.Symbol,
			Holder: s.Attestation.Holder, Quantity: s.Attestation.Quantity,
			SourceTxHash: s.Attestation.SourceTxHash, SourceLedger: p.from.Name(),
			Severity: "critical",
			Detail: fmt.Sprintf("source transaction %s left the canonical chain after settling on %s at block %d; %d units on %s are unbacked",
				s.Attestation.SourceTxHash, p.to.Name(), s.TargetBlock, s.Attestation.Quantity, p.to.Name()),
		})
	}

	// A pending flow whose source transaction is gone was orphaned before it
	// was final. Nothing was written on the other ledger, so abandoning it is
	// the whole remedy.
	for _, nonce := range sortedNonces(p.pending) {
		if _, ok := live[nonce]; ok {
			continue
		}
		a := p.pending[nonce]
		delete(p.pending, nonce)
		res.Abandoned = append(res.Abandoned, nonce)
		p.metrics.Abandoned.Inc()
		p.raise(&res, Alert{
			Kind: AlertAbandoned, Nonce: nonce, Symbol: a.Symbol, Holder: a.Holder,
			Quantity: a.Quantity, SourceTxHash: a.SourceTxHash, SourceLedger: p.from.Name(),
			Severity: "high",
			Detail: fmt.Sprintf("source transaction %s was orphaned before finality; nothing was written on %s",
				a.SourceTxHash, p.to.Name()),
		})
	}

	for _, nonce := range sortedEventNonces(live) {
		ev := live[nonce]
		if !p.firstSeen[nonce] {
			p.firstSeen[nonce] = true
			p.metrics.Observed.Inc()
			p.record(p.auditTrigger, nonce, audit.OutcomeOK, map[string]any{
				"ledger": string(p.from.Name()), "symbol": ev.Symbol, "quantity": ev.Quantity,
				"holder": triggerHolder(ev), "tx_hash": ev.TxHash, "block": ev.Block,
			}, span)
		}
		if _, done := p.settled[nonce]; done {
			// Replay protection. The event may be here because a
			// reorganisation re-included it; that changes nothing.
			continue
		}
		a := p.attestationFor(nonce, ev)
		if !ev.Finalised {
			p.pending[nonce] = a
			res.Waiting++
			continue
		}
		sigs := Collect(a, p.cfg.Signers)
		n, err := p.cfg.SignerSet.Verify(a, sigs)
		if err != nil {
			p.pending[nonce] = a
			res.Waiting++
			p.raiseOnce(&res, Alert{
				Kind: AlertQuorumUnavailable, Nonce: nonce, Symbol: a.Symbol, Holder: a.Holder,
				Quantity: a.Quantity, SourceTxHash: a.SourceTxHash, SourceLedger: p.from.Name(),
				Severity: "high",
				Detail:   fmt.Sprintf("%d of %d signatures gathered, %d required: %v", n, p.cfg.SignerSet.Size(), p.cfg.SignerSet.Quorum(), err),
			})
			continue
		}
		p.metrics.QuorumSize.Observe(float64(n))

		rc, err := p.settle(ctx, a)
		if err != nil {
			p.pending[nonce] = a
			res.Waiting++
			p.raiseOnce(&res, Alert{
				Kind: AlertSubmissionFailed, Nonce: nonce, Symbol: a.Symbol, Holder: a.Holder,
				Quantity: a.Quantity, SourceTxHash: a.SourceTxHash, SourceLedger: p.from.Name(),
				Severity: "high",
				Detail:   fmt.Sprintf("submission to %s failed after %d attempts: %v", p.to.Name(), p.cfg.Retry.Attempts, err),
			})
			p.record(p.auditResponse, nonce, audit.OutcomeError, map[string]any{
				"ledger": string(p.to.Name()), "symbol": a.Symbol, "quantity": a.Quantity, "error": err.Error(),
			}, span)
			continue
		}
		now := p.cfg.Clock.Now()
		s := Settlement{
			Nonce: nonce, Direction: p.dir, Attestation: a, Signatures: sigs,
			TargetTxHash: rc.TxHash, TargetBlock: rc.Block, SettledAt: now,
		}
		p.settled[nonce] = s
		delete(p.pending, nonce)
		delete(p.alerted, nonce)
		res.Settled = append(res.Settled, s)
		p.metrics.Settled.Inc()
		p.metrics.SettlementLag.ObserveDuration(now.Sub(a.ObservedAt))

		details := map[string]any{
			"ledger": string(p.to.Name()), "symbol": a.Symbol, "quantity": a.Quantity,
			"holder": a.Holder, "tx_hash": rc.TxHash, "block": rc.Block,
			"attestation_digest": a.DigestHex(), "signers": n, "quorum": p.cfg.SignerSet.Quorum(),
		}
		p.record(p.auditResponse, nonce, audit.OutcomeOK, details, span)
		p.record(p.auditToken, nonce, audit.OutcomeOK, details, span)
	}
	p.metrics.Pending.Set(float64(len(p.pending)))
	span.SetAttrs(map[string]any{
		"tcrp.bridge.observed": res.Observed,
		"tcrp.bridge.settled":  len(res.Settled),
		"tcrp.bridge.waiting":  res.Waiting,
		"tcrp.bridge.alerts":   len(res.Alerts),
	})
	return res, nil
}

// settle writes the response on the target ledger, retrying with exponential
// backoff. It is idempotent by nonce: before every attempt it asks the target
// whether a response for this nonce is already on the chain, so a submission
// that succeeded but whose acknowledgement was lost is never written twice.
func (p *pipeline) settle(ctx context.Context, a Attestation) (ledger.Receipt, error) {
	policy := p.cfg.Retry
	delay := policy.Base
	var last error
	for attempt := 1; attempt <= policy.Attempts; attempt++ {
		if rc, ok := p.alreadyResponded(a.Nonce); ok {
			return rc, nil
		}
		rc, err := p.submit(ctx, a)
		if err == nil {
			return rc, nil
		}
		last = err
		// A revert is the target ledger refusing on its own rules. It will
		// refuse the identical transaction again, so retrying only delays the
		// alert.
		if errors.Is(err, ledger.ErrReverted) {
			return rc, err
		}
		if attempt < policy.Attempts {
			p.metrics.Retries.Inc()
			policy.wait(delay)
			if delay *= 2; delay > policy.Max {
				delay = policy.Max
			}
		}
	}
	return ledger.Receipt{}, last
}

// alreadyResponded reports whether the target ledger already carries this
// flow's response.
func (p *pipeline) alreadyResponded(nonce uint64) (ledger.Receipt, bool) {
	for _, ev := range p.to.Events(0) {
		if ev.Kind != p.responseKind {
			continue
		}
		op, n, ok := ledger.ParseBridgeMemo(ev.Memo)
		if ok && op == p.responseOp && n == nonce {
			return ledger.Receipt{TxHash: ev.TxHash, Block: ev.Block, Success: true}, true
		}
	}
	return ledger.Receipt{}, false
}

// liveTriggers indexes the canonical chain's trigger events by nonce. A nonce
// carried by two events is a misconfiguration on the source ledger rather than
// something to resolve here; the later event wins and the supply control will
// find the difference.
func (p *pipeline) liveTriggers() map[uint64]domain.LedgerEvent {
	out := map[uint64]domain.LedgerEvent{}
	for _, ev := range p.from.Events(0) {
		if ev.Kind != p.triggerKind || !p.symbols[ev.Symbol] {
			continue
		}
		op, nonce, ok := ledger.ParseBridgeMemo(ev.Memo)
		if !ok || op != p.triggerOp {
			continue
		}
		out[nonce] = ev
	}
	return out
}

func (p *pipeline) attestationFor(nonce uint64, ev domain.LedgerEvent) Attestation {
	return Attestation{
		Nonce: nonce, SourceLedger: p.from.Name(), TargetLedger: p.to.Name(),
		Symbol: ev.Symbol, Holder: triggerHolder(ev), Quantity: ev.Quantity,
		SourceTxHash: ev.TxHash, SourceBlock: ev.Block, ObservedAt: ev.ObservedAt.UTC(),
	}
}

// triggerHolder is the party a lock or a burn was made on behalf of. Both carry
// it in From, because both take units away from somebody.
func triggerHolder(ev domain.LedgerEvent) string {
	if ev.From != "" {
		return ev.From
	}
	return ev.To
}

func (p *pipeline) raise(res *PollResult, a Alert) {
	a.Direction = p.dir
	a.At = p.cfg.Clock.Now()
	p.alerts = append(p.alerts, a)
	res.Alerts = append(res.Alerts, a)
	p.metrics.Alerts.Inc()
	if p.alerted[a.Nonce] == nil {
		p.alerted[a.Nonce] = map[AlertKind]bool{}
	}
	p.alerted[a.Nonce][a.Kind] = true
}

// raiseOnce suppresses a repeat of the same alert for the same nonce, because
// a relayer polling every second must not page an operator every second for one
// condition.
func (p *pipeline) raiseOnce(res *PollResult, a Alert) {
	if p.alerted[a.Nonce][a.Kind] {
		return
	}
	p.raise(res, a)
}

func (p *pipeline) record(action audit.Action, nonce uint64, outcome string, details map[string]any, span *obs.Span) {
	if p.cfg.Audit == nil || action == "" {
		return
	}
	details["nonce"] = nonce
	details["direction"] = string(p.dir)
	r := audit.Record{
		Time: p.cfg.Clock.Now(), Actor: p.cfg.Actor, Tenant: p.cfg.Tenant,
		Action: action, Subject: fmt.Sprintf("bridge/%s/%d", p.dir, nonce),
		Outcome: outcome, Details: details,
	}
	if sc := span.Context(); sc.Valid() {
		r.TraceID, r.SpanID = sc.TraceID, sc.SpanID
	}
	_, _ = p.cfg.Audit.Append(r)
}

// Alerts returns every alert raised so far, oldest first.
func (p *pipeline) Alerts() []Alert {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Alert(nil), p.alerts...)
}

// Settlements returns the completed flows, ordered by nonce.
func (p *pipeline) Settlements() []Settlement {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Settlement, 0, len(p.settled))
	for _, n := range sortedNonces(p.settled) {
		out = append(out, p.settled[n])
	}
	return out
}

// Pending returns the nonces waiting on finality, a quorum or a retry.
func (p *pipeline) Pending() []uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return sortedNonces(p.pending)
}

// nextNonce allocates the next unused nonce on the watched ledger. It is
// derived from the chain rather than kept in memory so that a restarted relayer
// does not reissue a nonce somebody has already used.
func (p *pipeline) nextNonce() uint64 {
	var max uint64
	for _, ev := range p.from.Events(0) {
		op, n, ok := ledger.ParseBridgeMemo(ev.Memo)
		if ok && op == p.triggerOp && n > max {
			max = n
		}
	}
	return max + 1
}

func sortedNonces[V any](m map[uint64]V) []uint64 {
	out := make([]uint64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedEventNonces(m map[uint64]domain.LedgerEvent) []uint64 { return sortedNonces(m) }

// Relayer is the lock-and-mint direction: it watches the source ledger for
// bridge locks, waits for them to become final, gathers a quorum of
// attestations, and mints the same quantity on the target.
type Relayer struct {
	p       *pipeline
	resolve TargetAddress
}

// NewRelayer builds the lock-and-mint relayer.
func NewRelayer(cfg Config) (*Relayer, error) {
	p, err := newPipeline(cfg, DirectionMint)
	if err != nil {
		return nil, err
	}
	p.from, p.to = cfg.Source, cfg.Target
	p.triggerKind, p.triggerOp = domain.EventLock, ledger.BridgeOpLock
	p.responseKind, p.responseOp = domain.EventMint, ledger.BridgeOpMint
	p.auditTrigger = audit.ActionBridgeLocked
	p.auditResponse = audit.ActionBridgeMinted
	p.auditToken = audit.ActionTokensMinted
	r := &Relayer{p: p}
	p.submit = func(ctx context.Context, a Attestation) (ledger.Receipt, error) {
		return p.to.Mint(ctx, p.cfg.TargetOperator, r.targetAddress(a.Holder), a.Symbol, a.Quantity,
			ledger.BridgeMemo(ledger.BridgeOpMint, a.Nonce))
	}
	return r, nil
}

// TargetAddress translates a counterparty identifier into the target ledger's
// address or party. The relayer needs it because the two ledgers do not share
// an address format and an attestation names the legal entity, not a key.
type TargetAddress func(holder string) string

// SetTargetAddress installs the holder translation. Without it the relayer
// mints to the counterparty identifier itself, which is right only when the
// target ledger is addressed that way.
func (r *Relayer) SetTargetAddress(f TargetAddress) { r.resolve = f }

func (r *Relayer) targetAddress(holder string) string {
	if r.resolve == nil {
		return holder
	}
	return r.resolve(holder)
}

// Lock initiates a flow: it allocates the next nonce and locks units into the
// source ledger's bridge vault. The mint does not happen here — it happens on
// a later poll, once the lock is final and a quorum has attested to it, which
// is the whole point of a lock-and-mint bridge.
//
// holder is the holder's address or party on the source ledger, not the
// counterparty identifier: this call writes to a chain, and a chain has never
// heard of CP-ALPHA.
//
// The relayer's lock is held across the ledger write. Allocating a nonce and
// then racing another caller to use it would put two flows on one replay key,
// which is the one thing the nonce exists to prevent.
func (r *Relayer) Lock(ctx context.Context, holder, symbol string, qty int64) (uint64, ledger.Receipt, error) {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	if !r.p.symbols[symbol] {
		return 0, ledger.Receipt{}, fmt.Errorf("bridge: %s is not relayed", symbol)
	}
	nonce := r.p.nextNonce()
	rc, err := r.p.from.Lock(ctx, r.p.cfg.SourceOperator, holder, symbol, qty,
		ledger.BridgeMemo(ledger.BridgeOpLock, nonce))
	return nonce, rc, err
}

// Poll runs one relay cycle.
func (r *Relayer) Poll(ctx context.Context) (PollResult, error) { return r.p.Poll(ctx) }

// Alerts returns every alert raised so far.
func (r *Relayer) Alerts() []Alert { return r.p.Alerts() }

// Settlements returns the completed mints, ordered by nonce.
func (r *Relayer) Settlements() []Settlement { return r.p.Settlements() }

// Pending returns the nonces waiting on finality, a quorum or a retry.
func (r *Relayer) Pending() []uint64 { return r.p.Pending() }
