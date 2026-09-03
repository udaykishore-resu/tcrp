// Package policy is the authorization gateway every tool call passes through.
//
// The agent does not decide what it is allowed to do. It proposes an action;
// this package decides. Evaluation is deny-overrides over a versioned bundle of
// rules, it runs in-process in microseconds, and it returns not just a verdict
// but the full list of rules that were considered and how each evaluated —
// because "the model was denied" is not evidence, and "rule
// deny-money-movement-without-approval matched with this input hash at this
// bundle version" is.
package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/identity"
	"github.com/udaykishore-resu/tcrp/internal/obs"
	"github.com/udaykishore-resu/tcrp/internal/policy/expr"
)

// Errors returned by this package.
var (
	ErrBadBundle = errors.New("policy: invalid bundle")
	ErrNoBundle  = errors.New("policy: no bundle loaded")
)

// Effect is a rule's verdict when its condition matches.
type Effect string

// Effects.
const (
	Allow Effect = "allow"
	Deny  Effect = "deny"
)

// Obligation is a condition attached to an allow. The gateway does not merely
// permit the call; it permits it *provided* the obligations are discharged, and
// the runtime is required to honour them. An unknown obligation type is a hard
// failure rather than a no-op, so that adding an obligation to a bundle can
// never silently weaken enforcement.
type Obligation struct {
	Type   string         `json:"type"`
	Params map[string]any `json:"params,omitempty"`
}

// Known obligation types.
const (
	ObligationHumanApproval = "require_human_approval"
	ObligationDualApproval  = "require_dual_approval"
	ObligationRedactPII     = "redact_pii"
	ObligationNotify        = "notify"
	ObligationRateLimit     = "rate_limit"
	ObligationSimulateFirst = "simulate_first"
)

// KnownObligations is the set the runtime knows how to discharge.
var KnownObligations = map[string]bool{
	ObligationHumanApproval: true,
	ObligationDualApproval:  true,
	ObligationRedactPII:     true,
	ObligationNotify:        true,
	ObligationRateLimit:     true,
	ObligationSimulateFirst: true,
}

// Rule is one policy statement.
type Rule struct {
	ID          string       `json:"id"`
	Description string       `json:"description,omitempty"`
	Effect      Effect       `json:"effect"`
	When        string       `json:"when"`
	Reason      string       `json:"reason,omitempty"`
	Obligations []Obligation `json:"obligations,omitempty"`
	Priority    int          `json:"priority,omitempty"`

	prog *expr.Program
}

// Bundle is a versioned set of rules.
type Bundle struct {
	Package       string `json:"package"`
	Version       string `json:"version"`
	Description   string `json:"description,omitempty"`
	DefaultEffect Effect `json:"default_effect"`
	Rules         []Rule `json:"rules"`

	digest string
}

// Digest is the content hash of the bundle, recorded on every decision so a
// verdict can be tied to the exact policy text that produced it.
func (b *Bundle) Digest() string { return b.digest }

// Compile validates and compiles every rule. A bundle that does not compile is
// never installed, so a policy typo fails at load time in CI rather than at
// 3am on the money-movement path.
func (b *Bundle) Compile() error {
	if b.Package == "" {
		return fmt.Errorf("%w: missing package", ErrBadBundle)
	}
	if b.Version == "" {
		return fmt.Errorf("%w: missing version", ErrBadBundle)
	}
	switch b.DefaultEffect {
	case Allow:
		return fmt.Errorf("%w: default_effect must be deny; an allow-by-default policy is not a policy", ErrBadBundle)
	case Deny, "":
		b.DefaultEffect = Deny
	default:
		return fmt.Errorf("%w: unknown default_effect %q", ErrBadBundle, b.DefaultEffect)
	}
	seen := map[string]bool{}
	for i := range b.Rules {
		r := &b.Rules[i]
		if r.ID == "" {
			return fmt.Errorf("%w: rule %d has no id", ErrBadBundle, i)
		}
		if seen[r.ID] {
			return fmt.Errorf("%w: duplicate rule id %q", ErrBadBundle, r.ID)
		}
		seen[r.ID] = true
		if r.Effect != Allow && r.Effect != Deny {
			return fmt.Errorf("%w: rule %q has effect %q", ErrBadBundle, r.ID, r.Effect)
		}
		if r.Effect == Deny && len(r.Obligations) > 0 {
			return fmt.Errorf("%w: rule %q is a deny and cannot carry obligations", ErrBadBundle, r.ID)
		}
		for _, o := range r.Obligations {
			if !KnownObligations[o.Type] {
				return fmt.Errorf("%w: rule %q has unknown obligation %q", ErrBadBundle, r.ID, o.Type)
			}
		}
		p, err := expr.Compile(r.When)
		if err != nil {
			return fmt.Errorf("%w: rule %q: %v", ErrBadBundle, r.ID, err)
		}
		r.prog = p
	}
	// Deterministic evaluation order: priority descending, then id.
	sort.SliceStable(b.Rules, func(i, j int) bool {
		if b.Rules[i].Priority != b.Rules[j].Priority {
			return b.Rules[i].Priority > b.Rules[j].Priority
		}
		return b.Rules[i].ID < b.Rules[j].ID
	})
	raw, err := json.Marshal(struct {
		P string `json:"package"`
		V string `json:"version"`
		R []Rule `json:"rules"`
	}{b.Package, b.Version, b.Rules})
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	b.digest = hex.EncodeToString(sum[:])
	return nil
}

// LoadBundle reads and compiles a bundle from JSON.
func LoadBundle(data []byte) (*Bundle, error) {
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadBundle, err)
	}
	if err := b.Compile(); err != nil {
		return nil, err
	}
	return &b, nil
}

// LoadBundleFile reads and compiles a bundle from a file.
func LoadBundleFile(path string) (*Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadBundle(data)
}

// ToolInfo describes the tool being invoked.
type ToolInfo struct {
	Name          string   `json:"name"`
	Tier          string   `json:"tier"`
	RequiredScope []string `json:"required_scopes"`
	Reversible    bool     `json:"reversible"`
}

// ApprovalInfo describes a recorded human approval, if any.
type ApprovalInfo struct {
	Recorded bool      `json:"recorded"`
	ID       string    `json:"id,omitempty"`
	By       string    `json:"by,omitempty"`
	ByRoles  []string  `json:"by_roles,omitempty"`
	At       time.Time `json:"at,omitzero"`
	Count    int       `json:"count"`
	Method   string    `json:"method,omitempty"`
}

// ResourceInfo describes what the action touches.
type ResourceInfo struct {
	Kind         string `json:"kind,omitempty"`
	Counterparty string `json:"counterparty,omitempty"`
	Ledger       string `json:"ledger,omitempty"`
	Asset        string `json:"asset,omitempty"`
	Currency     string `json:"currency,omitempty"`
	AmountUnits  int64  `json:"amount_units,omitempty"`
	Whitelisted  bool   `json:"whitelisted"`
}

// Stage names the point in the tool lifecycle at which a decision is asked
// for. The gateway is consulted twice for anything that moves money: once when
// the agent proposes the call, and again immediately before dispatch. The
// second call is what makes a stolen or replayed approval useless, and it is
// the check that conformance invariant AGENT-1 is written against.
type Stage string

// Decision stages.
const (
	StagePropose  Stage = "propose"
	StageDispatch Stage = "dispatch"
)

// RunInfo describes the agent run the call belongs to.
type RunInfo struct {
	ID               string `json:"id,omitempty"`
	Stage            Stage  `json:"stage,omitempty"`
	Step             int    `json:"step"`
	Environment      string `json:"environment,omitempty"`
	GuardVerdict     string `json:"guard_verdict,omitempty"`
	RetrievalTrusted bool   `json:"retrieval_trusted"`
	PriorFailures    int    `json:"prior_failures"`
}

// Input is the document a decision is made against. It is deliberately a flat,
// explicit struct rather than an arbitrary map: everything a policy may read is
// declared here, so a policy cannot come to depend on an incidental field.
type Input struct {
	Principal identity.Principal `json:"principal"`
	Tool      ToolInfo           `json:"tool"`
	Args      map[string]any     `json:"args,omitempty"`
	Approval  ApprovalInfo       `json:"approval"`
	Resource  ResourceInfo       `json:"resource"`
	Run       RunInfo            `json:"run"`
	Now       time.Time          `json:"now"`
}

// Map renders the input as the evaluation environment. It is written by hand
// rather than by reflection because it runs on every tool call and because the
// set of readable fields is part of the policy contract.
func (i Input) Map() map[string]any {
	return map[string]any{
		"input": map[string]any{
			"principal": map[string]any{
				"subject":   i.Principal.Subject,
				"kind":      string(i.Principal.Kind),
				"tenant":    i.Principal.Tenant,
				"desk":      i.Principal.Desk,
				"roles":     toAnyList(i.Principal.Roles),
				"scopes":    toAnyList(i.Principal.Scopes),
				"on_behalf": i.Principal.OnBehalf,
			},
			"tool": map[string]any{
				"name":            i.Tool.Name,
				"tier":            i.Tool.Tier,
				"required_scopes": toAnyList(i.Tool.RequiredScope),
				"reversible":      i.Tool.Reversible,
			},
			"args": i.Args,
			"approval": map[string]any{
				"recorded": i.Approval.Recorded,
				"id":       i.Approval.ID,
				"by":       i.Approval.By,
				"by_roles": toAnyList(i.Approval.ByRoles),
				"count":    float64(i.Approval.Count),
				"method":   i.Approval.Method,
				"age_s":    approvalAge(i),
			},
			"resource": map[string]any{
				"kind":         i.Resource.Kind,
				"counterparty": i.Resource.Counterparty,
				"ledger":       i.Resource.Ledger,
				"asset":        i.Resource.Asset,
				"currency":     i.Resource.Currency,
				"amount_units": float64(i.Resource.AmountUnits),
				"whitelisted":  i.Resource.Whitelisted,
			},
			"run": map[string]any{
				"id":                i.Run.ID,
				"stage":             string(i.Run.Stage),
				"step":              float64(i.Run.Step),
				"environment":       i.Run.Environment,
				"guard_verdict":     i.Run.GuardVerdict,
				"retrieval_trusted": i.Run.RetrievalTrusted,
				"prior_failures":    float64(i.Run.PriorFailures),
			},
		},
	}
}

func approvalAge(i Input) float64 {
	if !i.Approval.Recorded || i.Approval.At.IsZero() || i.Now.IsZero() {
		return 0
	}
	return i.Now.Sub(i.Approval.At).Seconds()
}

func toAnyList(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// Hash returns a stable digest of the input, recorded on the decision so the
// exact question that was asked can be reproduced from the audit log.
func (i Input) Hash() string {
	b, err := json.Marshal(i)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// RuleEval records how one rule evaluated.
type RuleEval struct {
	RuleID  string `json:"rule_id"`
	Effect  Effect `json:"effect"`
	Matched bool   `json:"matched"`
	Error   string `json:"error,omitempty"`
}

// Decision is the gateway's verdict plus everything needed to justify it.
type Decision struct {
	Allow         bool          `json:"allow"`
	RuleID        string        `json:"rule_id"`
	Reason        string        `json:"reason"`
	Obligations   []Obligation  `json:"obligations,omitempty"`
	Evaluated     []RuleEval    `json:"evaluated"`
	BundleVersion string        `json:"bundle_version"`
	BundleDigest  string        `json:"bundle_digest"`
	InputHash     string        `json:"input_hash"`
	Duration      time.Duration `json:"duration_ns"`
	Errors        []string      `json:"errors,omitempty"`
}

// RequiresApproval reports whether the decision obliges a human approval.
func (d Decision) RequiresApproval() bool {
	for _, o := range d.Obligations {
		if o.Type == ObligationHumanApproval || o.Type == ObligationDualApproval {
			return true
		}
	}
	return false
}

// ApprovalsRequired returns how many distinct humans must approve.
func (d Decision) ApprovalsRequired() int {
	n := 0
	for _, o := range d.Obligations {
		switch o.Type {
		case ObligationHumanApproval:
			if n < 1 {
				n = 1
			}
		case ObligationDualApproval:
			n = 2
		}
	}
	return n
}

// Summary renders a one-line form for logs and spans.
func (d Decision) Summary() string {
	verdict := "DENY"
	if d.Allow {
		verdict = "ALLOW"
	}
	return fmt.Sprintf("%s by %s (%s)", verdict, d.RuleID, d.Reason)
}

// Engine evaluates decisions against the installed bundle. It is safe for
// concurrent use and supports hot reload without dropping a request.
type Engine struct {
	mu      sync.RWMutex
	bundle  *Bundle
	clock   obs.Clock
	tracer  *obs.Tracer
	metrics *engineMetrics
}

type engineMetrics struct {
	decisions *obs.Counter
	denies    *obs.Counter
	errs      *obs.Counter
	latency   *obs.Histogram
}

// NewEngine builds an engine with a compiled bundle.
func NewEngine(b *Bundle, clock obs.Clock, tracer *obs.Tracer, reg *obs.Registry) *Engine {
	if clock == nil {
		clock = obs.RealClock{}
	}
	e := &Engine{bundle: b, clock: clock, tracer: tracer}
	if reg != nil {
		e.metrics = &engineMetrics{
			decisions: reg.Counter("tcrp_policy_decisions_total", "Policy decisions rendered.", nil),
			denies:    reg.Counter("tcrp_policy_denies_total", "Policy decisions that denied the action.", nil),
			errs:      reg.Counter("tcrp_policy_rule_errors_total", "Rule conditions that failed to evaluate.", nil),
			latency: reg.Histogram("tcrp_policy_evaluation_seconds", "Policy evaluation latency.",
				[]float64{0.000_001, 0.000_005, 0.000_010, 0.000_050, 0.000_100, 0.000_500, 0.001, 0.005}, nil),
		}
	}
	return e
}

// Reload installs a new bundle atomically.
func (e *Engine) Reload(b *Bundle) error {
	if b == nil {
		return ErrNoBundle
	}
	if b.digest == "" {
		if err := b.Compile(); err != nil {
			return err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.bundle = b
	return nil
}

// Bundle returns the installed bundle.
func (e *Engine) Bundle() *Bundle {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.bundle
}

// Evaluate renders a decision. Deny-overrides: every rule is evaluated, a
// matching deny wins outright, and an allow only stands if no deny matched.
// A rule whose condition errors is treated as a deny for that rule, never as a
// pass, so a broken policy fails closed.
func (e *Engine) Evaluate(ctx context.Context, in Input) Decision {
	e.mu.RLock()
	b := e.bundle
	e.mu.RUnlock()

	start := e.clock.Now()
	d := Decision{InputHash: in.Hash()}
	if b == nil {
		d.RuleID, d.Reason = "no-bundle", "no policy bundle is installed"
		return d
	}
	d.BundleVersion, d.BundleDigest = b.Version, b.digest

	if in.Now.IsZero() {
		in.Now = start
	}
	env := in.Map()

	var (
		firstDeny  *Rule
		bestAllow  *Rule
		evaluated  = make([]RuleEval, 0, len(b.Rules))
		ruleErrors []string
	)
	for i := range b.Rules {
		r := &b.Rules[i]
		matched, err := r.prog.EvalBool(env)
		ev := RuleEval{RuleID: r.ID, Effect: r.Effect, Matched: matched}
		if err != nil {
			ev.Error = err.Error()
			ev.Matched = false
			ruleErrors = append(ruleErrors, fmt.Sprintf("%s: %v", r.ID, err))
			if e.metrics != nil {
				e.metrics.errs.Inc()
			}
			// A rule that cannot be evaluated must not be able to authorise.
			// If it was an allow, we simply do not count it; if it was a deny,
			// we fail closed and treat the whole evaluation as a deny.
			if r.Effect == Deny && firstDeny == nil {
				firstDeny = r
			}
		}
		evaluated = append(evaluated, ev)
		if !ev.Matched {
			continue
		}
		if r.Effect == Deny {
			if firstDeny == nil {
				firstDeny = r
			}
			continue
		}
		if bestAllow == nil {
			bestAllow = r
		}
	}

	d.Evaluated = evaluated
	d.Errors = ruleErrors
	switch {
	case firstDeny != nil:
		d.Allow = false
		d.RuleID = firstDeny.ID
		d.Reason = firstDeny.Reason
		if d.Reason == "" {
			d.Reason = firstDeny.Description
		}
	case bestAllow != nil:
		d.Allow = true
		d.RuleID = bestAllow.ID
		d.Reason = bestAllow.Reason
		if d.Reason == "" {
			d.Reason = bestAllow.Description
		}
		d.Obligations = bestAllow.Obligations
	default:
		d.Allow = false
		d.RuleID = "default"
		d.Reason = "no rule permitted this action"
	}
	d.Duration = e.clock.Since(start)

	if e.metrics != nil {
		e.metrics.decisions.Inc()
		if !d.Allow {
			e.metrics.denies.Inc()
		}
		e.metrics.latency.ObserveDuration(d.Duration)
	}
	if e.tracer != nil {
		_, sp := e.tracer.Start(ctx, "policy.evaluate", obs.KindInternal)
		sp.SetAttrs(map[string]any{
			obs.AttrPolicyDecision: verdictString(d.Allow),
			obs.AttrPolicyRule:     d.RuleID,
			obs.AttrToolTier:       in.Tool.Tier,
			obs.AttrGenAIToolName:  in.Tool.Name,
			"tcrp.policy.bundle":   b.Version,
			"tcrp.policy.input":    d.InputHash,
			"tcrp.policy.rules":    len(evaluated),
		})
		if !d.Allow {
			sp.AddEvent("policy.denied", map[string]any{"reason": d.Reason})
		}
		sp.End()
	}
	return d
}

func verdictString(allow bool) string {
	if allow {
		return "allow"
	}
	return "deny"
}

// Explain renders a human-readable trace of a decision, used by the CLI and
// the governance dashboard.
func (d Decision) Explain() string {
	out := fmt.Sprintf("decision: %s\nrule:     %s\nreason:   %s\nbundle:   %s (%s)\ninput:    %s\nlatency:  %s\n",
		verdictString(d.Allow), d.RuleID, d.Reason, d.BundleVersion, shortHash(d.BundleDigest), d.InputHash, d.Duration)
	if len(d.Obligations) > 0 {
		out += "obligations:\n"
		for _, o := range d.Obligations {
			out += fmt.Sprintf("  - %s %v\n", o.Type, o.Params)
		}
	}
	out += "rules evaluated:\n"
	for _, e := range d.Evaluated {
		mark := " "
		if e.Matched {
			mark = "*"
		}
		line := fmt.Sprintf("  %s %-44s %s", mark, e.RuleID, e.Effect)
		if e.Error != "" {
			line += "  ERROR: " + e.Error
		}
		out += line + "\n"
	}
	return out
}

func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
