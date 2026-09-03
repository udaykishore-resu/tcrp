// Package conform is the I=E spine: intent equals evidence.
//
// A process is declared once, as a BPMN model with control annotations. The
// invariants the model declares are evaluated against evidence collected from
// what the system actually did — the hash-chained audit log, the OpenTelemetry
// spans, the two ledgers, the bridge's supply reconciliation, the lakehouse —
// and each evaluation yields a verdict that cites the specific records it read.
// The verdicts and the digests of the evidence behind them are packaged into a
// signed, content-addressed bundle that a CI gate consumes.
//
// The claim this package lets the platform make is not "we designed a control".
// It is "here is the control, here is what the system did, and here is the
// machine-checked statement that the two agree". Everything else follows from
// that sentence: a model that declares an invariant nothing implements is a
// hard error rather than a warning, because an intent with no evidence path is
// the failure being guarded against; an invariant that cannot find its evidence
// returns inconclusive rather than pass, because absence of evidence is not
// evidence of conformance; and the gate treats a critical inconclusive as
// failing, because the alternative is a build that goes green when the
// telemetry stops arriving.
//
// Nothing here reads the wall clock, and nothing here has a side effect on the
// system it is judging. Same evidence in, byte-identical verdicts and bundle
// digest out.
package conform

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/agent"
	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/bridge"
	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/ledger"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Status is an invariant's outcome.
type Status string

// Outcomes. Inconclusive is a first-class result, not a shade of pass: an
// invariant that could not find the records it needs has proved nothing, and
// reporting that as a pass is how a monitoring gap becomes a green build.
const (
	StatusPass         Status = "pass"
	StatusFail         Status = "fail"
	StatusInconclusive Status = "inconclusive"
)

// Severity is how much a failure of this invariant matters. Only critical
// failures and critical inconclusives stop the gate.
type Severity string

// Severities.
const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
)

// Domains an invariant can belong to. The domain is what a reader scans first,
// so it names the part of the platform the evidence came from.
const (
	DomainAgent      = "agent"
	DomainSettlement = "settlement"
	DomainData       = "data"
)

// RefKind names the kind of evidence a Ref points at.
type RefKind string

// Reference kinds.
const (
	RefAudit       RefKind = "audit"
	RefSpan        RefKind = "span"
	RefRun         RefKind = "run"
	RefLedger      RefKind = "ledger"
	RefBreak       RefKind = "break"
	RefAttestation RefKind = "attestation"
	RefQuality     RefKind = "quality"
	RefLineage     RefKind = "lineage"
)

// Ref points at one piece of evidence.
//
// It exists so a verdict can be walked back to the fact behind it. A report
// that says "AGENT-1 failed" is an accusation; a report that says "AGENT-1
// failed, see audit record 47 whose hash is 9f3c…, and audit record 44 whose
// hash is 1b02…" is something an auditor can check without the platform's help.
type Ref struct {
	// Kind names the sort of artefact this points at.
	Kind RefKind `json:"kind"`
	// ID identifies the artefact within its kind: an audit sequence number, a
	// span id, a run id, a break id, a bridge nonce, a ledger block height.
	ID string `json:"id"`
	// Hash is the artefact's own content hash where it has one: the audit
	// record's chain hash, the attestation digest, the ledger block hash.
	Hash string `json:"hash,omitempty"`
	// Detail is a short human-readable locator, such as the action name or the
	// symbol, so a reader does not have to resolve the id to understand it.
	Detail string `json:"detail,omitempty"`
}

// String renders the reference for a text report.
func (r Ref) String() string {
	out := string(r.Kind) + ":" + r.ID
	if r.Detail != "" {
		out += " (" + r.Detail + ")"
	}
	if r.Hash != "" {
		out += " " + shortHash(r.Hash)
	}
	return out
}

// AuditRef points at one audit record by sequence number and chain hash.
func AuditRef(r audit.Record) Ref {
	return Ref{Kind: RefAudit, ID: strconv.FormatUint(r.Seq, 10), Hash: r.Hash, Detail: string(r.Action)}
}

// SpanRef points at one exported span.
func SpanRef(s obs.SpanData) Ref {
	return Ref{Kind: RefSpan, ID: s.SpanID, Detail: s.Name}
}

// RunRef points at one agent run.
func RunRef(r *agent.Run) Ref {
	if r == nil {
		return Ref{Kind: RefRun, ID: "-"}
	}
	return Ref{Kind: RefRun, ID: r.ID, Detail: string(r.Status)}
}

// LedgerRef points at one ledger event by block and transaction.
func LedgerRef(e domain.LedgerEvent) Ref {
	return Ref{
		Kind:   RefLedger,
		ID:     strconv.FormatUint(e.Block, 10),
		Hash:   e.TxHash,
		Detail: string(e.Ledger) + " " + string(e.Kind) + " " + e.Symbol,
	}
}

// BreakRef points at one reconciliation break.
func BreakRef(b domain.Break) Ref {
	return Ref{Kind: RefBreak, ID: b.ID, Detail: string(b.Kind) + " " + b.Symbol}
}

// AttestationRef points at one bridge attestation by nonce and digest.
func AttestationRef(a bridge.Attestation) Ref {
	return Ref{
		Kind:   RefAttestation,
		ID:     strconv.FormatUint(a.Nonce, 10),
		Hash:   a.DigestHex(),
		Detail: string(a.SourceLedger) + "->" + string(a.TargetLedger) + " " + a.Symbol,
	}
}

// Verdict is one invariant's result against one body of evidence.
type Verdict struct {
	// ID, Title and Domain are the invariant's own identity, copied in so a
	// verdict read out of a bundle stands on its own.
	ID     string `json:"id"`
	Title  string `json:"title"`
	Domain string `json:"domain"`
	// Status is the outcome.
	Status Status `json:"status"`
	// Severity is the invariant's own, and is what the gate reads.
	Severity Severity `json:"severity"`
	// Observed is what the evidence said, in the invariant's own terms.
	Observed string `json:"observed"`
	// Expected is what the model declared should have been true.
	Expected string `json:"expected"`
	// Detail is the sentence a person reads first when this fails.
	Detail string `json:"detail,omitempty"`
	// EvidenceRefs cite the records the verdict rests on, failing ones first.
	EvidenceRefs []Ref `json:"evidence_refs,omitempty"`
	// CheckedAt is stamped by the engine from its clock. An invariant
	// evaluated directly leaves it zero, because an invariant has no clock and
	// must not acquire one.
	CheckedAt time.Time `json:"checked_at,omitzero"`
}

// Critical reports whether a non-pass of this verdict should stop a release.
func (v Verdict) Critical() bool { return v.Severity == SeverityCritical }

// Blocks reports whether this verdict stops the gate: a critical failure, or a
// critical invariant that could not reach a conclusion.
func (v Verdict) Blocks() bool {
	return v.Critical() && (v.Status == StatusFail || v.Status == StatusInconclusive)
}

// Invariant is one machine-checkable statement derived from the model.
//
// Evaluate must be pure: it reads the evidence, it writes nothing, it calls no
// clock, and the same evidence produces the same verdict including the order of
// the references. That is what makes a bundle digest reproducible by somebody
// who does not trust the process that produced it.
type Invariant interface {
	// ID is the identifier a BPMN annotation names, such as "AGENT-1".
	ID() string
	// Title states the invariant in one line, in the same words as the model.
	Title() string
	// Domain groups the invariant for reporting.
	Domain() string
	// Evaluate renders the verdict.
	Evaluate(ctx context.Context, ev Evidence) Verdict
}

// LedgerView is one settlement network as the conformance engine sees it.
//
// The holder and registry accessors are functions rather than a concrete ledger
// type because the two simulators expose their registries differently and
// because a test must be able to supply four holders and a predicate without
// standing up a chain. A nil accessor is not an empty answer: it makes the
// invariants that need it report inconclusive.
type LedgerView struct {
	// Name is the network this view describes.
	Name domain.Ledger
	// Ledger is the read surface. Nil means no ledger evidence was collected.
	Ledger ledger.Ledger
	// Holders returns every account holding the symbol. Nil means holder
	// evidence is unavailable.
	Holders func(symbol string) []string
	// Registered reports whether a holder is in the identity registry. Nil
	// means registry evidence is unavailable.
	Registered func(holder string) bool
	// Exempt names accounts that hold units but are structurally outside the
	// registry — the bridge vault is the only one. Nil means the two default
	// vault accounts.
	Exempt []string
}

// exempt reports whether holder is one of the accounts outside the registry by
// construction.
func (v LedgerView) exempt(holder string) bool {
	list := v.Exempt
	if list == nil {
		list = []string{ledger.VaultAddress, ledger.VaultParty}
	}
	for _, e := range list {
		if e == holder {
			return true
		}
	}
	return false
}

// LineageCounter reports how many gold rows a lakehouse snapshot produced and
// how many of those trace back to a bronze event.
//
// It is a function rather than a lakehouse handle so that this package does not
// have to depend on the lakehouse's storage layout, and so that a test can
// state the two numbers directly. A nil counter makes DATA-1 inconclusive.
type LineageCounter func() (gold int, traced int)

// AttestedMint is one bridge mint and the attestation that authorised it.
type AttestedMint struct {
	// Attestation is the statement the signers endorsed.
	Attestation bridge.Attestation `json:"attestation"`
	// Signatures are the endorsements, checked against Evidence.Signers.
	Signatures []bridge.Signature `json:"signatures"`
}

// Evidence is everything the invariants are allowed to read.
//
// It is a flat, explicit struct rather than a bag of interfaces for the same
// reason policy.Input is: the set of facts an invariant may consult is part of
// the contract, and an invariant that could reach into the running platform
// would be measuring the platform with itself.
type Evidence struct {
	// Records is the audit chain, in sequence order.
	Records []audit.Record
	// AuditPublicKey verifies the chain's signatures. Nil skips the signature
	// check but still verifies the hash chain.
	AuditPublicKey ed25519.PublicKey
	// Spans are the exported OpenTelemetry spans.
	Spans []obs.SpanData
	// Runs are the agent runs, as the store holds them.
	Runs []*agent.Run
	// StepBudget is the runtime's configured step budget. Zero means the
	// shipped default.
	StepBudget int

	// Source and Target are the two settlement networks, in the direction the
	// bridge model declares: lock on Source, mint on Target.
	Source LedgerView
	Target LedgerView
	// Symbols are the symbols to reconcile.
	Symbols []string
	// AsOf is the instant the ledger evidence describes. It is passed to the
	// supply reconciliation rather than read from a clock, so a break raised
	// today can be reproduced from the same figures next year.
	AsOf time.Time
	// Supply is a precomputed reconciliation. When nil the settlement
	// invariants compute one from Source and Target.
	Supply *bridge.SupplyReport
	// Mints are the attested bridge mints the relayer settled.
	Mints []AttestedMint
	// Signers is the attester set a mint's signatures are checked against.
	Signers *bridge.SignerSet

	// Quality is the lakehouse's data-quality verdict for the snapshot the
	// gold rows came from.
	Quality *domain.QualityReport
	// Lineage counts gold rows against traced gold rows.
	Lineage LineageCounter

	// PolicyBundleDigest is the digest of the policy bundle in force. It is
	// recorded on the evidence bundle so a verdict about an authorization can
	// be tied to the exact policy text that produced it.
	PolicyBundleDigest string
}

// SupplyReport returns the cross-network reconciliation, computing it from the
// two ledgers when one was not supplied. The second result is false when there
// is nothing to reconcile against, which is what makes SETTLE-1 and SETTLE-5
// inconclusive rather than vacuously passing.
func (e Evidence) SupplyReport() (bridge.SupplyReport, bool) {
	if e.Supply != nil {
		return *e.Supply, true
	}
	if e.Source.Ledger == nil || e.Target.Ledger == nil || len(e.Symbols) == 0 {
		return bridge.SupplyReport{}, false
	}
	return bridge.Reconcile(e.Source.Ledger, e.Target.Ledger, e.Symbols, e.AsOf), true
}

// AuditHead returns the chain head hash, or the genesis hash for an empty log.
func (e Evidence) AuditHead() string {
	if len(e.Records) == 0 {
		return audit.GenesisHash
	}
	return e.Records[len(e.Records)-1].Hash
}

// stepBudget returns the budget to hold runs to.
func (e Evidence) stepBudget() int {
	if e.StepBudget > 0 {
		return e.StepBudget
	}
	return agent.DefaultConfig().MaxSteps
}

// Artefact is one evidence input, named and digested.
type Artefact struct {
	// Name identifies the artefact, and is what the manifest is sorted by.
	Name string `json:"name"`
	// Kind groups artefacts of the same sort.
	Kind string `json:"kind"`
	// Digest is the sha256 of the artefact's canonical encoding.
	Digest string `json:"digest"`
	// Count is how many items the artefact holds, where that is meaningful.
	Count int `json:"count,omitempty"`
}

// Manifest lists every evidence artefact with its digest, sorted by name.
//
// It is what makes the bundle checkable rather than merely signed: the
// signature says the verdicts were not altered, and the manifest says which
// body of evidence they were rendered against. Somebody re-running the engine
// against a different audit log gets a different manifest and therefore a
// different bundle id, which is the point.
func (e Evidence) Manifest() []Artefact {
	var out []Artefact
	add := func(name, kind string, count int, v any) {
		out = append(out, Artefact{Name: name, Kind: kind, Digest: digestOf(v), Count: count})
	}
	if len(e.Records) > 0 {
		hashes := make([]string, 0, len(e.Records))
		for _, r := range e.Records {
			hashes = append(hashes, r.Hash)
		}
		add("audit.records", "audit", len(e.Records), hashes)
	}
	if len(e.Spans) > 0 {
		add("otel.spans", "span", len(e.Spans), e.Spans)
	}
	if len(e.Runs) > 0 {
		add("agent.runs", "run", len(e.Runs), e.Runs)
	}
	if rep, ok := e.SupplyReport(); ok {
		add("bridge.supply", "supply", len(rep.Symbols), rep)
	}
	if len(e.Mints) > 0 {
		add("bridge.mints", "attestation", len(e.Mints), e.Mints)
	}
	if e.Quality != nil {
		add("lake.quality", "quality", len(e.Quality.Checks), *e.Quality)
	}
	if e.Lineage != nil {
		gold, traced := e.Lineage()
		add("lake.lineage", "lineage", gold, []int{gold, traced})
	}
	for _, v := range []LedgerView{e.Source, e.Target} {
		if v.Ledger == nil {
			continue
		}
		head := v.Ledger.Head()
		out = append(out, Artefact{
			Name:   "ledger." + string(v.Ledger.Name()) + ".head",
			Kind:   "ledger",
			Digest: head.Hash,
			Count:  int(head.Height),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// digestOf is the sha256 of a value's JSON encoding. Go's encoder sorts map
// keys, so the encoding is stable for the shapes used here.
func digestOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// base seeds a verdict from an invariant's identity, so an implementation
// states only what it found.
func base(inv Invariant, sev Severity, expected string) Verdict {
	return Verdict{
		ID: inv.ID(), Title: inv.Title(), Domain: inv.Domain(),
		Status: StatusPass, Severity: sev, Expected: expected,
	}
}

// inconclusive marks a verdict as unable to reach a conclusion and says why.
func inconclusive(v Verdict, why string) Verdict {
	v.Status = StatusInconclusive
	v.Observed = "no evidence"
	v.Detail = why
	return v
}

// fail marks a verdict failed.
func fail(v Verdict, observed, detail string, refs ...Ref) Verdict {
	v.Status = StatusFail
	v.Observed = observed
	v.Detail = detail
	v.EvidenceRefs = append(v.EvidenceRefs, refs...)
	return v
}

// pass marks a verdict passed.
func pass(v Verdict, observed string, refs ...Ref) Verdict {
	v.Status = StatusPass
	v.Observed = observed
	v.EvidenceRefs = append(v.EvidenceRefs, refs...)
	return v
}

// plural renders a count with a noun, so report lines read like sentences.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
