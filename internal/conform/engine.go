package conform

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// ErrGate is returned by Report.Gate when the report must not pass CI.
var ErrGate = errors.New("conform: conformance gate failed")

// ErrDuplicateInvariant is returned when two implementations claim one id.
var ErrDuplicateInvariant = errors.New("conform: invariant id already registered")

// Registry maps invariant ids to implementations.
//
// It exists so the BPMN parser can answer one question without importing the
// invariants: does something implement what this model declares. That question
// is the whole of the I=E contract, and answering it at parse time is what
// turns a mis-typed annotation into a build failure rather than a control that
// silently never ran.
type Registry struct {
	mu   sync.RWMutex
	byID map[string]Invariant
}

// NewRegistry builds a registry holding invs. Duplicate ids panic, because a
// registry is assembled at start-up from a literal list and a duplicate there
// is a programming error rather than a runtime condition.
func NewRegistry(invs ...Invariant) *Registry {
	r := &Registry{byID: map[string]Invariant{}}
	for _, inv := range invs {
		if err := r.Register(inv); err != nil {
			panic(err.Error())
		}
	}
	return r
}

// Register adds an invariant.
func (r *Registry) Register(inv Invariant) error {
	if inv == nil {
		return errors.New("conform: cannot register a nil invariant")
	}
	if inv.ID() == "" {
		return errors.New("conform: cannot register an invariant with no id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byID[inv.ID()]; dup {
		return fmt.Errorf("%w: %s", ErrDuplicateInvariant, inv.ID())
	}
	r.byID[inv.ID()] = inv
	return nil
}

// Get returns an invariant by id.
func (r *Registry) Get(id string) (Invariant, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	inv, ok := r.byID[id]
	return inv, ok
}

// IDs returns every registered id, sorted.
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byID))
	for id := range r.byID {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// All returns every registered invariant, ordered by id.
func (r *Registry) All() []Invariant {
	ids := r.IDs()
	out := make([]Invariant, 0, len(ids))
	for _, id := range ids {
		inv, _ := r.Get(id)
		out = append(out, inv)
	}
	return out
}

// Default returns the registry of every invariant this package ships.
func Default() *Registry {
	return NewRegistry(
		AgentMoneyNeedsApproval{},
		AgentToolNeedsDecision{},
		AgentGuardrailRan{},
		AgentSpansCarryIdentity{},
		AgentAuditChainVerifies{},
		AgentStepBudgetHeld{},
		AgentDenialNotRetried{},
		SettleSupplyConserved{},
		SettleHoldersRegistered{},
		SettleMintIsAttested{},
		SettleNonceNotReplayed{},
		SettleIssuanceIdentity{},
		DataGoldTracesToBronze{},
	)
}

// Engine evaluates a process model's invariants against evidence.
type Engine struct {
	reg   *Registry
	clock obs.Clock
}

// NewEngine builds an engine. A nil registry means the shipped default; a nil
// clock means the real one, which is the only concession to convenience here
// and is why the deterministic tests pass a manual clock.
func NewEngine(reg *Registry, clock obs.Clock) *Engine {
	if reg == nil {
		reg = Default()
	}
	if clock == nil {
		clock = obs.RealClock{}
	}
	return &Engine{reg: reg, clock: clock}
}

// Registry returns the engine's registry.
func (e *Engine) Registry() *Registry { return e.reg }

// Run evaluates the invariants a process declares and returns the report.
//
// The set of invariants comes from the model, sorted by id so that two runs
// over the same evidence produce the same bytes. A nil process means run every
// registered invariant, which is what an operator wants when asking "what does
// the platform currently believe" rather than "does this process conform".
//
// The engine stamps CheckedAt from its clock rather than letting an invariant
// read one, which is what keeps the invariants pure and the bundle digest
// reproducible under a manual clock.
func (e *Engine) Run(ctx context.Context, p *Process, ev Evidence) Report {
	rep := Report{RanAt: e.clock.Now()}
	var ids []string
	if p != nil {
		rep.Process, rep.ProcessName, rep.ProcessDigest = p.ID, p.Name, p.SourceDigest
		ids = p.DeclaredInvariants()
	} else {
		ids = e.reg.IDs()
	}

	for _, id := range ids {
		inv, ok := e.reg.Get(id)
		if !ok {
			// Validate is the place this is meant to be caught. Reaching here
			// means the caller skipped it, and the honest report of an
			// invariant that does not exist is that nothing was concluded.
			rep.Verdicts = append(rep.Verdicts, Verdict{
				ID: id, Title: "no implementation registered", Domain: "unknown",
				Status: StatusInconclusive, Severity: SeverityCritical,
				Observed: "no evidence",
				Expected: "an implementation registered for " + id,
				Detail: "the model declares " + id +
					" but nothing implements it; run Process.Validate before Run",
				CheckedAt: rep.RanAt,
			})
			continue
		}
		v := inv.Evaluate(ctx, ev)
		v.CheckedAt = e.clock.Now()
		rep.Verdicts = append(rep.Verdicts, v)
	}
	rep.tally()
	return rep
}

// Report is the outcome of one conformance run.
type Report struct {
	// Process, ProcessName and ProcessDigest identify the model that declared
	// the invariants. They are empty when the engine ran every registered
	// invariant rather than one process's.
	Process       string `json:"process,omitempty"`
	ProcessName   string `json:"process_name,omitempty"`
	ProcessDigest string `json:"process_digest,omitempty"`
	// RanAt is the engine's clock at the start of the run.
	RanAt time.Time `json:"ran_at"`
	// Verdicts are the results, ordered by invariant id.
	Verdicts []Verdict `json:"verdicts"`
	// Passed, Failed and Inconclusive are the tally.
	Passed       int `json:"passed"`
	Failed       int `json:"failed"`
	Inconclusive int `json:"inconclusive"`
}

func (r *Report) tally() {
	r.Passed, r.Failed, r.Inconclusive = 0, 0, 0
	for _, v := range r.Verdicts {
		switch v.Status {
		case StatusPass:
			r.Passed++
		case StatusFail:
			r.Failed++
		case StatusInconclusive:
			r.Inconclusive++
		}
	}
}

// Verdict returns one verdict by invariant id.
func (r Report) Verdict(id string) (Verdict, bool) {
	for _, v := range r.Verdicts {
		if v.ID == id {
			return v, true
		}
	}
	return Verdict{}, false
}

// Blocking returns the verdicts that stop the gate, in report order.
func (r Report) Blocking() []Verdict {
	var out []Verdict
	for _, v := range r.Verdicts {
		if v.Blocks() {
			out = append(out, v)
		}
	}
	return out
}

// GateError is a failed gate, carrying the verdicts that caused it.
type GateError struct {
	// Blocking are the verdicts that stopped the release.
	Blocking []Verdict
}

// Error names the invariants that stopped the gate.
func (e *GateError) Error() string {
	parts := make([]string, 0, len(e.Blocking))
	for _, v := range e.Blocking {
		parts = append(parts, fmt.Sprintf("%s %s", v.ID, v.Status))
	}
	return fmt.Sprintf("%v: %s", ErrGate, strings.Join(parts, ", "))
}

// Unwrap lets callers match ErrGate.
func (e *GateError) Unwrap() error { return ErrGate }

// Gate reports whether the run may ship. It returns a non-nil error when any
// critical invariant failed, and also when any critical invariant was
// inconclusive.
//
// The asymmetry is deliberate and it is the most load-bearing decision in this
// package. A failing critical invariant says a control did not hold. An
// inconclusive one says the engine could not tell — the audit log was empty,
// the spans never arrived, the ledger handle was nil. Those two are different
// facts about the world but they are the same fact about the release: in
// neither case does anybody have evidence that the control held, and shipping
// on the second one means the way to get a green build is to stop collecting
// evidence. Non-critical invariants are reported and do not stop anything, so
// that the gate stays a small set of statements a team will actually keep
// green.
func (r Report) Gate() error {
	blocking := r.Blocking()
	if len(blocking) == 0 {
		return nil
	}
	return &GateError{Blocking: blocking}
}

// Text renders the report for a terminal.
func (r Report) Text() string {
	var b strings.Builder
	title := r.Process
	if r.ProcessName != "" {
		title = r.ProcessName + " (" + r.Process + ")"
	}
	if title == "" {
		title = "all registered invariants"
	}
	fmt.Fprintf(&b, "conformance: %s\n", title)
	if r.ProcessDigest != "" {
		fmt.Fprintf(&b, "model:       %s\n", shortHash(r.ProcessDigest))
	}
	fmt.Fprintf(&b, "ran at:      %s\n", r.RanAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "result:      %d pass, %d fail, %d inconclusive\n\n", r.Passed, r.Failed, r.Inconclusive)
	for _, v := range r.Verdicts {
		mark := " "
		if v.Blocks() {
			mark = "!"
		}
		fmt.Fprintf(&b, "%s %-9s %-8s %-8s %s\n", mark, v.ID, v.Status, v.Severity, v.Title)
		fmt.Fprintf(&b, "    observed: %s\n", v.Observed)
		fmt.Fprintf(&b, "    expected: %s\n", v.Expected)
		if v.Detail != "" {
			fmt.Fprintf(&b, "    detail:   %s\n", v.Detail)
		}
		for _, ref := range v.EvidenceRefs {
			fmt.Fprintf(&b, "    evidence: %s\n", ref)
		}
	}
	if err := r.Gate(); err != nil {
		fmt.Fprintf(&b, "\nGATE FAILED: %v\n", err)
	} else {
		b.WriteString("\nGATE PASSED\n")
	}
	return b.String()
}

// Markdown renders the report for a pull request comment.
func (r Report) Markdown() string {
	var b strings.Builder
	title := r.Process
	if r.ProcessName != "" {
		title = r.ProcessName
	}
	if title == "" {
		title = "All registered invariants"
	}
	fmt.Fprintf(&b, "# Conformance: %s\n\n", title)
	if r.Process != "" {
		fmt.Fprintf(&b, "Process `%s`", r.Process)
		if r.ProcessDigest != "" {
			fmt.Fprintf(&b, ", model digest `%s`", shortHash(r.ProcessDigest))
		}
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Ran at %s. **%d pass, %d fail, %d inconclusive.**\n\n",
		r.RanAt.UTC().Format(time.RFC3339), r.Passed, r.Failed, r.Inconclusive)

	b.WriteString("| | Invariant | Severity | Status | Observed |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, v := range r.Verdicts {
		mark := ""
		if v.Blocks() {
			mark = "**blocks**"
		}
		fmt.Fprintf(&b, "| %s | `%s` %s | %s | %s | %s |\n",
			mark, v.ID, mdEscape(v.Title), v.Severity, v.Status, mdEscape(v.Observed))
	}

	var detailed []Verdict
	for _, v := range r.Verdicts {
		if v.Status != StatusPass {
			detailed = append(detailed, v)
		}
	}
	if len(detailed) > 0 {
		b.WriteString("\n## Findings\n")
		for _, v := range detailed {
			fmt.Fprintf(&b, "\n### %s — %s\n\n", v.ID, mdEscape(v.Title))
			fmt.Fprintf(&b, "- **Expected:** %s\n", mdEscape(v.Expected))
			fmt.Fprintf(&b, "- **Observed:** %s\n", mdEscape(v.Observed))
			if v.Detail != "" {
				fmt.Fprintf(&b, "- **Detail:** %s\n", mdEscape(v.Detail))
			}
			if len(v.EvidenceRefs) > 0 {
				b.WriteString("- **Evidence:**\n")
				for _, ref := range v.EvidenceRefs {
					fmt.Fprintf(&b, "  - `%s`\n", ref)
				}
			}
		}
	}

	if err := r.Gate(); err != nil {
		fmt.Fprintf(&b, "\n**Gate failed.** %v\n", err)
	} else {
		b.WriteString("\n**Gate passed.**\n")
	}
	return b.String()
}

// mdEscape neutralises the two characters that break a Markdown table cell.
func mdEscape(s string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ").Replace(s)
}
