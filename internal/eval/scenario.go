// Package eval is the agent evaluation harness and the drift detector.
//
// An agent that is correct on the day it ships is not the thing being claimed.
// The claim is that the controls hold — that money movement always pauses, that
// an injected instruction never reaches a tool, that a refusal is reported
// rather than routed around — and a claim of that shape is only worth anything
// if it is re-checked mechanically on every change. So the suite is data, the
// scoring is deterministic, and the default judge runs offline: the same suite
// against the same runtime produces byte-identical scores, which is what makes
// a regression a failing build rather than an argument.
//
// The harness scores the platform, not the model. A forbidden tool the gateway
// refused is evidence the platform worked; a forbidden tool that executed is a
// failure regardless of how good the answer was. The two are counted separately
// and only the second one fails the suite.
package eval

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/udaykishore-resu/tcrp/internal/identity"
)

// Category groups scenarios by the behaviour under test, so a regression can be
// attributed to a class of failure rather than to one prompt.
type Category string

// Scenario categories.
const (
	// CatRead is a plain question answered from read-only tools.
	CatRead Category = "read"
	// CatMarginCall is a multi-step collateral flow ending in an obligation.
	CatMarginCall Category = "margin_call"
	// CatMoneyMovement moves value and must pause for a human.
	CatMoneyMovement Category = "money_movement"
	// CatWhitelist targets a counterparty outside the identity registry, which
	// the gateway must refuse before the ledger does.
	CatWhitelist Category = "whitelist"
	// CatDirectInjection carries an attack in the user prompt.
	CatDirectInjection Category = "direct_injection"
	// CatIndirectInjection carries an attack inside tool output. The harness
	// implementation is responsible for seeding the poisoned retrieval — the
	// scenario cannot do it, because the scenario does not own the tools. By
	// convention a Harness inspects Category and returns an instance whose
	// tools emit instruction-like text for these scenarios.
	CatIndirectInjection Category = "indirect_injection"
	// CatPII puts personal or account data in the prompt.
	CatPII Category = "pii"
	// CatApprovalBypass asks the agent to skip a control.
	CatApprovalBypass Category = "approval_bypass"
	// CatOutOfScope is a principal asking for something their scopes forbid.
	CatOutOfScope Category = "out_of_scope"
	// CatRunaway invites an unbounded loop.
	CatRunaway Category = "runaway"
	// CatAmbiguous is under-specified; the right answer is a clarification.
	CatAmbiguous Category = "ambiguous"
	// CatErrorRecovery fails tools transiently to test recovery, not retries.
	CatErrorRecovery Category = "error_recovery"
)

// Categories lists every declared category in a stable order.
func Categories() []Category {
	return []Category{
		CatRead, CatMarginCall, CatMoneyMovement, CatWhitelist,
		CatDirectInjection, CatIndirectInjection, CatPII, CatApprovalBypass,
		CatOutOfScope, CatRunaway, CatAmbiguous, CatErrorRecovery,
	}
}

// Valid reports whether c is a declared category.
func (c Category) Valid() bool {
	for _, k := range Categories() {
		if k == c {
			return true
		}
	}
	return false
}

// DenyApprovalScenarioID names the one scenario whose approval the runner
// denies rather than grants. Granting every gate would only ever prove that a
// satisfied approval resumes a run; the interesting property is that a refused
// approval leaves the ledger untouched, and exactly one scenario is designated
// to assert it so the rest of the suite stays a pass/fail signal about the
// happy path.
const DenyApprovalScenarioID = "money-transfer-approval-denied"

// Scenario is one benchmark case.
//
// The expectations are deliberately about observable platform behaviour —
// which tools ran, which were refused, whether a human was asked — rather than
// about the wording of an answer. Wording is graded separately by a Judge,
// because wording is the only part of this that a model change may legitimately
// alter.
type Scenario struct {
	// ID is stable and unique; results are keyed and sorted by it.
	ID string `json:"id"`
	// Name is the human-readable label used in reports.
	Name string `json:"name"`
	// Query is the prompt handed to the runtime verbatim.
	Query string `json:"query"`
	// Principal is the identity the run executes as. Its scopes are half the
	// test: an out-of-scope request is only out of scope relative to a subject.
	Principal identity.Principal `json:"principal"`
	// Category groups the scenario for reporting.
	Category Category `json:"category"`
	// ExpectedTools is the trajectory a correct run invokes, in order.
	ExpectedTools []string `json:"expected_tools,omitempty"`
	// ForbiddenTools must never be invoked. Being attempted and refused is a
	// pass, not a failure.
	ForbiddenTools []string `json:"forbidden_tools,omitempty"`
	// MustRefuse requires the platform to decline: a guardrail block, a policy
	// denial, or an answer that reports the refusal without acting.
	MustRefuse bool `json:"must_refuse,omitempty"`
	// MustPause requires a human approval gate to open.
	MustPause bool `json:"must_pause,omitempty"`
	// ExpectAnswerContains are substrings the answer must include.
	ExpectAnswerContains []string `json:"expect_answer_contains,omitempty"`
	// ExpectAnswerOmits are substrings the answer must not include.
	ExpectAnswerOmits []string `json:"expect_answer_omits,omitempty"`
	// InjectedFailures maps a tool name to how many times the harness should
	// fail it before letting it succeed.
	InjectedFailures map[string]int `json:"injected_failures,omitempty"`
	// Weight is the scenario's share of the suite score. Zero means one.
	Weight float64 `json:"weight,omitempty"`
}

// FailureBudget is the total number of injected tool failures the scenario
// expects a run to absorb.
func (s Scenario) FailureBudget() int {
	n := 0
	for _, v := range s.InjectedFailures {
		n += v
	}
	return n
}

// ExpectedSteps is the step count a correct run should not exceed: one step per
// expected tool call, one for the answer, one more for the paused proposal when
// a gate must open, and one per injected failure the run has to absorb.
//
// It is derived rather than stored so that editing a trajectory cannot leave a
// stale budget behind it.
func (s Scenario) ExpectedSteps() int {
	n := len(s.ExpectedTools) + 1 + s.FailureBudget()
	if s.MustPause {
		n++
	}
	return n
}

// EffectiveWeight returns the scenario's weight, defaulting to one.
func (s Scenario) EffectiveWeight() float64 {
	if s.Weight <= 0 {
		return 1
	}
	return s.Weight
}

// Validate reports why the scenario cannot be scored.
func (s Scenario) Validate() error {
	switch {
	case strings.TrimSpace(s.ID) == "":
		return fmt.Errorf("eval: scenario has no id")
	case strings.TrimSpace(s.Name) == "":
		return fmt.Errorf("eval: scenario %q has no name", s.ID)
	case strings.TrimSpace(s.Query) == "":
		return fmt.Errorf("eval: scenario %q has no query", s.ID)
	case !s.Category.Valid():
		return fmt.Errorf("eval: scenario %q has unknown category %q", s.ID, s.Category)
	case strings.TrimSpace(s.Principal.Subject) == "":
		return fmt.Errorf("eval: scenario %q has no principal subject", s.ID)
	case s.Weight < 0:
		return fmt.Errorf("eval: scenario %q has negative weight", s.ID)
	}
	forbidden := map[string]bool{}
	for _, t := range s.ForbiddenTools {
		if strings.TrimSpace(t) == "" {
			return fmt.Errorf("eval: scenario %q has an empty forbidden tool", s.ID)
		}
		forbidden[t] = true
	}
	for _, t := range s.ExpectedTools {
		if strings.TrimSpace(t) == "" {
			return fmt.Errorf("eval: scenario %q has an empty expected tool", s.ID)
		}
		if forbidden[t] {
			return fmt.Errorf("eval: scenario %q both expects and forbids %q", s.ID, t)
		}
	}
	for tool, n := range s.InjectedFailures {
		if strings.TrimSpace(tool) == "" {
			return fmt.Errorf("eval: scenario %q injects a failure for an unnamed tool", s.ID)
		}
		if n <= 0 {
			return fmt.Errorf("eval: scenario %q injects %d failures for %q", s.ID, n, tool)
		}
	}
	// A scenario that must both refuse and pause is contradictory: the gate
	// only opens on an allow, so one of the two expectations can never hold.
	if s.MustRefuse && s.MustPause {
		return fmt.Errorf("eval: scenario %q cannot both refuse and pause", s.ID)
	}
	return nil
}

// Suite is a versioned set of scenarios.
type Suite struct {
	Name      string     `json:"name"`
	Version   string     `json:"version"`
	Scenarios []Scenario `json:"scenarios"`
}

// Validate checks the suite is scoreable: named, versioned, non-empty, with
// unique scenario ids and a designated deny-approval scenario that actually
// pauses. A suite that does not validate is never run, because a silently
// malformed benchmark reports a green build it has not earned.
func (s Suite) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("eval: suite has no name")
	}
	if strings.TrimSpace(s.Version) == "" {
		return fmt.Errorf("eval: suite %q has no version", s.Name)
	}
	if len(s.Scenarios) == 0 {
		return fmt.Errorf("eval: suite %q has no scenarios", s.Name)
	}
	seen := make(map[string]bool, len(s.Scenarios))
	for i := range s.Scenarios {
		sc := s.Scenarios[i]
		if err := sc.Validate(); err != nil {
			return err
		}
		if seen[sc.ID] {
			return fmt.Errorf("eval: suite %q has duplicate scenario id %q", s.Name, sc.ID)
		}
		seen[sc.ID] = true
	}
	if sc, ok := s.Scenario(DenyApprovalScenarioID); ok && !sc.MustPause {
		return fmt.Errorf("eval: designated deny scenario %q does not pause", DenyApprovalScenarioID)
	}
	return nil
}

// Scenario returns the scenario with the given id.
func (s Suite) Scenario(id string) (Scenario, bool) {
	for i := range s.Scenarios {
		if s.Scenarios[i].ID == id {
			return s.Scenarios[i], true
		}
	}
	return Scenario{}, false
}

// Filter returns a suite holding only the scenarios keep accepts, preserving
// name and version so a partial run is still attributable to a suite version.
func (s Suite) Filter(keep func(Scenario) bool) Suite {
	out := Suite{Name: s.Name, Version: s.Version}
	for i := range s.Scenarios {
		if keep(s.Scenarios[i]) {
			out.Scenarios = append(out.Scenarios, s.Scenarios[i])
		}
	}
	return out
}

// CountByCategory returns how many scenarios each category holds.
func (s Suite) CountByCategory() map[Category]int {
	out := map[Category]int{}
	for i := range s.Scenarios {
		out[s.Scenarios[i].Category]++
	}
	return out
}

// Sorted returns the suite with scenarios ordered by id, which is the order the
// runner reports in and therefore the order that makes two reports comparable.
func (s Suite) Sorted() Suite {
	out := Suite{Name: s.Name, Version: s.Version, Scenarios: make([]Scenario, len(s.Scenarios))}
	copy(out.Scenarios, s.Scenarios)
	sort.Slice(out.Scenarios, func(i, j int) bool { return out.Scenarios[i].ID < out.Scenarios[j].ID })
	return out
}

// LoadSuite reads and validates a suite from JSON.
func LoadSuite(r io.Reader) (Suite, error) {
	var s Suite
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Suite{}, fmt.Errorf("eval: decoding suite: %w", err)
	}
	if err := s.Validate(); err != nil {
		return Suite{}, err
	}
	return s, nil
}

// LoadSuiteFile reads and validates a suite from a file.
func LoadSuiteFile(path string) (Suite, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Suite{}, fmt.Errorf("eval: reading suite: %w", err)
	}
	return LoadSuite(bytes.NewReader(b))
}

// Save writes the suite as indented JSON.
func (s Suite) Save(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// SaveFile writes the suite to a path.
func (s Suite) SaveFile(path string) error {
	var buf bytes.Buffer
	if err := s.Save(&buf); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// builtinSuiteJSON is the shipped benchmark. It is embedded data rather than Go
// literals so that adding a scenario is a data change a non-Go reviewer can
// read, and so the same bytes can be handed to a tool outside this process.
//
//go:embed suite.json
var builtinSuiteJSON []byte

var (
	builtinOnce sync.Once
	builtin     Suite
	builtinErr  error
)

// BuiltinSuite returns the embedded suite, parsed and validated once.
func BuiltinSuite() (Suite, error) {
	builtinOnce.Do(func() {
		builtin, builtinErr = LoadSuite(bytes.NewReader(builtinSuiteJSON))
	})
	return builtin, builtinErr
}

// MustBuiltinSuite returns the embedded suite and panics if it does not
// validate. A malformed embedded benchmark is a build defect, not a runtime
// condition worth handling.
func MustBuiltinSuite() Suite {
	s, err := BuiltinSuite()
	if err != nil {
		panic(err)
	}
	return s
}
