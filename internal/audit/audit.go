// Package audit is the platform's immutable record.
//
// Every record is hash-chained to its predecessor over a canonical encoding, so
// a record cannot be altered, reordered or removed without breaking the chain.
// Verify walks the chain and reports the first sequence number at which it
// breaks; conformance invariant AGENT-5 runs it in CI.
package audit

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Errors returned by this package.
var (
	ErrChainBroken = errors.New("audit: hash chain broken")
	ErrClosed      = errors.New("audit: log closed")
)

// Action names the audited operation. They are constants because the
// conformance engine matches on them and a typo must not silently produce an
// unmatched invariant.
type Action string

// Audited actions.
const (
	ActionRunStarted       Action = "agent.run.started"
	ActionRunCompleted     Action = "agent.run.completed"
	ActionRunFailed        Action = "agent.run.failed"
	ActionToolProposed     Action = "agent.tool.proposed"
	ActionToolInvoked      Action = "agent.tool.invoked"
	ActionToolFailed       Action = "agent.tool.failed"
	ActionPolicyDecision   Action = "policy.decision"
	ActionGuardVerdict     Action = "guard.verdict"
	ActionApprovalRequired Action = "approval.required"
	ActionApprovalGranted  Action = "approval.granted"
	ActionApprovalDenied   Action = "approval.denied"
	ActionApprovalExpired  Action = "approval.expired"
	ActionTransferExecuted Action = "ledger.transfer.executed"
	ActionMarginCallIssued Action = "collateral.margin_call.issued"
	ActionTokensMinted     Action = "token.minted"
	ActionTokensBurned     Action = "token.burned"
	ActionBridgeLocked     Action = "bridge.locked"
	ActionBridgeMinted     Action = "bridge.minted"
	ActionBreakOpened      Action = "recon.break.opened"
	ActionBreakResolved    Action = "recon.break.resolved"
	ActionLimitBreached    Action = "risk.limit.breached"
	ActionConformVerdict   Action = "conformance.verdict"
)

// Record is one entry in the chain. Hash covers every other field including
// PrevHash, which is what links the chain.
type Record struct {
	Seq       uint64         `json:"seq"`
	Time      time.Time      `json:"time"`
	Actor     string         `json:"actor"`
	OnBehalf  string         `json:"on_behalf,omitempty"`
	Tenant    string         `json:"tenant"`
	Action    Action         `json:"action"`
	Subject   string         `json:"subject"`
	Outcome   string         `json:"outcome"`
	RunID     string         `json:"run_id,omitempty"`
	TraceID   string         `json:"trace_id,omitempty"`
	SpanID    string         `json:"span_id,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
	PrevHash  string         `json:"prev_hash"`
	Hash      string         `json:"hash"`
	Signature string         `json:"signature,omitempty"`
}

// Outcomes.
const (
	OutcomeAllowed = "allowed"
	OutcomeDenied  = "denied"
	OutcomeOK      = "ok"
	OutcomeError   = "error"
	OutcomePending = "pending"
)

// canonical returns the deterministic byte encoding hashed for the chain. Map
// keys are sorted and the hash and signature fields are excluded.
func (r Record) canonical() []byte {
	type flat struct {
		Seq      uint64    `json:"seq"`
		Time     time.Time `json:"time"`
		Actor    string    `json:"actor"`
		OnBehalf string    `json:"on_behalf,omitempty"`
		Tenant   string    `json:"tenant"`
		Action   Action    `json:"action"`
		Subject  string    `json:"subject"`
		Outcome  string    `json:"outcome"`
		RunID    string    `json:"run_id,omitempty"`
		TraceID  string    `json:"trace_id,omitempty"`
		SpanID   string    `json:"span_id,omitempty"`
		Details  string    `json:"details,omitempty"`
		PrevHash string    `json:"prev_hash"`
	}
	f := flat{
		Seq: r.Seq, Time: r.Time.UTC(), Actor: r.Actor, OnBehalf: r.OnBehalf,
		Tenant: r.Tenant, Action: r.Action, Subject: r.Subject, Outcome: r.Outcome,
		RunID: r.RunID, TraceID: r.TraceID, SpanID: r.SpanID,
		Details: CanonicalJSON(r.Details), PrevHash: r.PrevHash,
	}
	b, _ := json.Marshal(f)
	return b
}

// ComputeHash returns the record's content hash.
func (r Record) ComputeHash() string {
	sum := sha256.Sum256(r.canonical())
	return hex.EncodeToString(sum[:])
}

// CanonicalJSON encodes v with sorted keys so hashing is stable across runs.
func CanonicalJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	var any1 any
	if err := json.Unmarshal(b, &any1); err != nil {
		return string(b)
	}
	out, err := json.Marshal(sortValue(any1))
	if err != nil {
		return string(b)
	}
	return string(out)
}

func sortValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		m := make(orderedMap, 0, len(keys))
		for _, k := range keys {
			m = append(m, kv{k, sortValue(t[k])})
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = sortValue(t[i])
		}
		return out
	default:
		return v
	}
}

type kv struct {
	K string
	V any
}
type orderedMap []kv

// MarshalJSON writes the map with keys in sorted order.
func (m orderedMap) MarshalJSON() ([]byte, error) {
	buf := []byte{'{'}
	for i, e := range m {
		if i > 0 {
			buf = append(buf, ',')
		}
		k, err := json.Marshal(e.K)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(e.V)
		if err != nil {
			return nil, err
		}
		buf = append(buf, k...)
		buf = append(buf, ':')
		buf = append(buf, v...)
	}
	return append(buf, '}'), nil
}

// GenesisHash anchors the chain.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Log is an append-only hash-chained audit log.
type Log struct {
	mu       sync.RWMutex
	clock    obs.Clock
	records  []Record
	last     string
	seq      uint64
	sink     io.WriteCloser
	sinkMu   sync.Mutex // serialises journal writes in chain order
	signKey  ed25519.PrivateKey
	verifyPK ed25519.PublicKey
	closed   bool
	subs     []chan Record
}

// Option configures a Log.
type Option func(*Log)

// WithFile appends every record to path as JSON lines, so the log survives a
// restart and can be shipped to a WORM store.
func WithFile(path string) Option {
	return func(l *Log) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err == nil {
			l.sink = f
		}
	}
}

// WithWriter appends every record to w.
func WithWriter(w io.WriteCloser) Option { return func(l *Log) { l.sink = w } }

// WithSigning signs each record's hash with an ed25519 key, so a third party
// can verify the chain without trusting the process that wrote it.
func WithSigning(priv ed25519.PrivateKey) Option {
	return func(l *Log) {
		l.signKey = priv
		l.verifyPK = priv.Public().(ed25519.PublicKey)
	}
}

// New creates a log.
func New(clock obs.Clock, opts ...Option) *Log {
	if clock == nil {
		clock = obs.RealClock{}
	}
	l := &Log{clock: clock, last: GenesisHash}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Append writes r, filling in sequence, time and chain fields.
func (l *Log) Append(r Record) (Record, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return Record{}, ErrClosed
	}
	l.seq++
	r.Seq = l.seq
	if r.Time.IsZero() {
		r.Time = l.clock.Now()
	}
	r.Time = r.Time.UTC()
	r.PrevHash = l.last
	r.Hash = r.ComputeHash()
	if l.signKey != nil {
		r.Signature = hex.EncodeToString(ed25519.Sign(l.signKey, []byte(r.Hash)))
	}
	l.last = r.Hash
	l.records = append(l.records, r)
	sink := l.sink
	subs := append([]chan Record(nil), l.subs...)
	// The journal lock is taken while the chain lock is still held, so records
	// reach the file in the order they entered the chain. Without that, two
	// concurrent appends can be journalled out of sequence and the exported
	// file fails verification even though the in-memory chain is sound.
	if sink != nil {
		l.sinkMu.Lock()
	}
	l.mu.Unlock()

	if sink != nil {
		if b, err := json.Marshal(r); err == nil {
			_, _ = sink.Write(append(b, '\n'))
		}
		l.sinkMu.Unlock()
	}
	for _, c := range subs {
		select {
		case c <- r:
		default: // a slow subscriber never blocks the audited path
		}
	}
	return r, nil
}

// Subscribe returns a channel receiving newly appended records. The channel is
// buffered and drops rather than blocking the writer.
func (l *Log) Subscribe(buf int) <-chan Record {
	if buf <= 0 {
		buf = 64
	}
	c := make(chan Record, buf)
	l.mu.Lock()
	l.subs = append(l.subs, c)
	l.mu.Unlock()
	return c
}

// Records returns a copy of the chain.
func (l *Log) Records() []Record {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Record, len(l.records))
	copy(out, l.records)
	return out
}

// Len returns the number of records.
func (l *Log) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.records)
}

// Head returns the current chain head hash.
func (l *Log) Head() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.last
}

// Find returns records matching a predicate.
func (l *Log) Find(pred func(Record) bool) []Record {
	out := []Record{}
	for _, r := range l.Records() {
		if pred(r) {
			out = append(out, r)
		}
	}
	return out
}

// ByRun returns the records of one agent run, in order.
func (l *Log) ByRun(runID string) []Record {
	return l.Find(func(r Record) bool { return r.RunID == runID })
}

// Verify walks the chain and returns an error naming the first bad sequence.
func (l *Log) Verify() error { return VerifyRecords(l.Records(), l.verifyPK) }

// VerifyRecords checks an arbitrary slice, which is how an auditor verifies a
// file exported from the platform without running the platform.
func VerifyRecords(recs []Record, pub ed25519.PublicKey) error {
	prev := GenesisHash
	for i, r := range recs {
		if r.Seq != uint64(i+1) {
			return fmt.Errorf("%w: record %d has seq %d", ErrChainBroken, i+1, r.Seq)
		}
		if r.PrevHash != prev {
			return fmt.Errorf("%w: seq %d prev_hash %s want %s", ErrChainBroken, r.Seq, short(r.PrevHash), short(prev))
		}
		if want := r.ComputeHash(); want != r.Hash {
			return fmt.Errorf("%w: seq %d content hash %s want %s", ErrChainBroken, r.Seq, short(r.Hash), short(want))
		}
		if pub != nil && r.Signature != "" {
			sig, err := hex.DecodeString(r.Signature)
			if err != nil || !ed25519.Verify(pub, []byte(r.Hash), sig) {
				return fmt.Errorf("%w: seq %d signature invalid", ErrChainBroken, r.Seq)
			}
		}
		prev = r.Hash
	}
	return nil
}

// Export writes the chain as JSON lines for offline verification.
func (l *Log) Export(w io.Writer) error {
	for _, r := range l.Records() {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// PublicKey returns the verification key, if the log is signing.
func (l *Log) PublicKey() ed25519.PublicKey { return l.verifyPK }

// Close releases the sink.
func (l *Log) Close() error {
	l.mu.Lock()
	l.closed = true
	sink := l.sink
	l.mu.Unlock()
	if sink == nil {
		return nil
	}
	// Wait for any journal write already in flight before closing under it.
	l.sinkMu.Lock()
	defer l.sinkMu.Unlock()
	return sink.Close()
}

func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
