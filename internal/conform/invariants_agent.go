package conform

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/udaykishore-resu/tcrp/internal/agent"
	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/guard"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// invocationActions are the audit actions that mean a tool actually ran.
//
// A failed invocation is in the set. A tool that reached the network and
// errored has still been dispatched, and an unapproved money movement that
// happened to fail is not a control that held.
var invocationActions = map[audit.Action]bool{
	audit.ActionToolInvoked:      true,
	audit.ActionToolFailed:       true,
	audit.ActionTransferExecuted: true,
	audit.ActionMarginCallIssued: true,
	audit.ActionTokensMinted:     true,
	audit.ActionTokensBurned:     true,
}

// detail reads a string field out of a record's details.
func detail(r audit.Record, key string) string {
	v, ok := r.Details[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// invocations returns the records that dispatched an agent tool, in sequence
// order.
//
// A run id is part of the test, not an incidental filter. The same audit
// actions are written by services that are not the agent — the bridge relayer
// mints tokens, the reconciler resolves breaks — and those are governed by
// their own controls rather than by the agent's policy gateway. An agent
// invocation always carries the run it belongs to; a record without one did not
// come from a run, and demanding a policy decision for it would make these
// invariants report a violation every time a background service did its job.
func invocations(recs []audit.Record) []audit.Record {
	out := make([]audit.Record, 0, len(recs))
	for _, r := range recs {
		if invocationActions[r.Action] && r.RunID != "" {
			out = append(out, r)
		}
	}
	return out
}

// runIDs returns every distinct run id in the log, sorted, so that a report is
// stable regardless of how the records were interleaved.
func runIDs(recs []audit.Record) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range recs {
		if r.RunID != "" && !seen[r.RunID] {
			seen[r.RunID] = true
			out = append(out, r.RunID)
		}
	}
	sort.Strings(out)
	return out
}

// ---------- AGENT-1 ----------

// AgentMoneyNeedsApproval is AGENT-1.
type AgentMoneyNeedsApproval struct{}

// ID returns the invariant id.
func (AgentMoneyNeedsApproval) ID() string { return "AGENT-1" }

// Title states the invariant.
func (AgentMoneyNeedsApproval) Title() string {
	return "no money-movement tool invocation without a granted approval from someone other than the requester"
}

// Domain groups the invariant.
func (AgentMoneyNeedsApproval) Domain() string { return DomainAgent }

// Evaluate joins every money-movement invocation to the approval that
// authorised it.
//
// Four things have to hold, and each of them is a separate way the control has
// failed in the wild: the invocation names an approval; that approval was
// granted, in the same run; the grant was recorded before the invocation rather
// than after it; and the approver is not the requester. The last one is the
// whole point of four-eyes and the easiest to lose, because the requester is
// usually the person with the console open.
func (i AgentMoneyNeedsApproval) Evaluate(ctx context.Context, ev Evidence) Verdict {
	v := base(i, SeverityCritical,
		"every money_movement invocation cites an approval.granted record from a different actor, recorded first")
	if len(ev.Records) == 0 {
		return inconclusive(v, "the audit log carries no records, so no invocation could be examined")
	}

	// Index the grants by run and approval id.
	type grant struct{ rec audit.Record }
	grants := map[string]grant{}
	for _, r := range ev.Records {
		if r.Action != audit.ActionApprovalGranted {
			continue
		}
		grants[r.RunID+"|"+detail(r, "approval_id")] = grant{rec: r}
	}

	var (
		checked int
		refs    []Ref
		bad     []string
	)
	for _, r := range invocations(ev.Records) {
		if detail(r, "tier") != string(agent.TierMoney) {
			continue
		}
		checked++
		apID := detail(r, "approval_id")
		if apID == "" {
			bad = append(bad, fmt.Sprintf("seq %d: %s cites no approval", r.Seq, r.Subject))
			refs = append(refs, AuditRef(r))
			continue
		}
		g, ok := grants[r.RunID+"|"+apID]
		if !ok {
			bad = append(bad, fmt.Sprintf("seq %d: %s cites approval %s, which was never granted in run %s",
				r.Seq, r.Subject, apID, r.RunID))
			refs = append(refs, AuditRef(r))
			continue
		}
		if g.rec.Seq > r.Seq {
			bad = append(bad, fmt.Sprintf("seq %d: %s ran before its approval %s was granted at seq %d",
				r.Seq, r.Subject, apID, g.rec.Seq))
			refs = append(refs, AuditRef(r), AuditRef(g.rec))
			continue
		}
		if g.rec.Actor == r.Actor || (r.OnBehalf != "" && g.rec.Actor == r.OnBehalf) {
			bad = append(bad, fmt.Sprintf("seq %d: %s was approved by %s, who requested it",
				r.Seq, r.Subject, g.rec.Actor))
			refs = append(refs, AuditRef(r), AuditRef(g.rec))
			continue
		}
		refs = append(refs, AuditRef(r), AuditRef(g.rec))
	}

	observed := plural(checked, "money-movement invocation", "money-movement invocations")
	if len(bad) > 0 {
		return fail(v, observed+", "+plural(len(bad), "unapproved", "unapproved"), strings.Join(bad, "; "), refs...)
	}
	return pass(v, observed+", all approved by a second party", refs...)
}

// ---------- AGENT-2 ----------

// AgentToolNeedsDecision is AGENT-2.
type AgentToolNeedsDecision struct{}

// ID returns the invariant id.
func (AgentToolNeedsDecision) ID() string { return "AGENT-2" }

// Title states the invariant.
func (AgentToolNeedsDecision) Title() string {
	return "every tool invocation is preceded by an allowing policy decision for that tool"
}

// Domain groups the invariant.
func (AgentToolNeedsDecision) Domain() string { return DomainAgent }

// Evaluate walks each run in sequence order and checks that the decision
// standing immediately before each invocation, for that tool, allowed it.
//
// The runtime evaluates policy twice — once when the model proposes the call
// and again immediately before dispatch — and it is the second evaluation that
// makes a stale approval useless. It audits the dispatch-stage decision only
// where an approval was involved, so what this invariant can prove from the log
// alone is that an allowing decision for that exact tool stood immediately
// before the call. Where a decision record does carry a stage, it is held to
// dispatch. That asymmetry is a limitation of what the log records, and it is
// stated here rather than papered over.
func (i AgentToolNeedsDecision) Evaluate(ctx context.Context, ev Evidence) Verdict {
	v := base(i, SeverityCritical,
		"an allowing policy.decision for the same tool stands immediately before every invocation")
	if len(ev.Records) == 0 {
		return inconclusive(v, "the audit log carries no records, so no invocation could be examined")
	}

	// last[run|tool] is the most recent decision seen for that pair.
	last := map[string]audit.Record{}
	var (
		checked int
		refs    []Ref
		bad     []string
	)
	for _, r := range ev.Records {
		switch {
		case r.Action == audit.ActionPolicyDecision:
			last[r.RunID+"|"+r.Subject] = r
		case invocationActions[r.Action] && r.RunID != "":
			checked++
			d, ok := last[r.RunID+"|"+r.Subject]
			if !ok {
				bad = append(bad, fmt.Sprintf("seq %d: %s ran with no policy decision on the record",
					r.Seq, r.Subject))
				refs = append(refs, AuditRef(r))
				continue
			}
			if d.Outcome != audit.OutcomeAllowed {
				bad = append(bad, fmt.Sprintf("seq %d: %s ran after a decision at seq %d that said %q",
					r.Seq, r.Subject, d.Seq, d.Outcome))
				refs = append(refs, AuditRef(r), AuditRef(d))
				continue
			}
			if stage := detail(d, "stage"); stage != "" && stage != "dispatch" {
				bad = append(bad, fmt.Sprintf("seq %d: %s ran against a %s-stage decision at seq %d",
					r.Seq, r.Subject, stage, d.Seq))
				refs = append(refs, AuditRef(r), AuditRef(d))
				continue
			}
			refs = append(refs, AuditRef(r), AuditRef(d))
		}
	}

	observed := plural(checked, "tool invocation", "tool invocations")
	if len(bad) > 0 {
		return fail(v, observed+", "+plural(len(bad), "unauthorized", "unauthorized"), strings.Join(bad, "; "), refs...)
	}
	return pass(v, observed+", each preceded by an allowing decision", refs...)
}

// ---------- AGENT-3 ----------

// AgentGuardrailRan is AGENT-3.
type AgentGuardrailRan struct{}

// ID returns the invariant id.
func (AgentGuardrailRan) ID() string { return "AGENT-3" }

// Title states the invariant.
func (AgentGuardrailRan) Title() string {
	return "the guardrail ran on every prompt and every output of every run"
}

// Domain groups the invariant.
func (AgentGuardrailRan) Domain() string { return DomainAgent }

// Evaluate requires a prompt inspection for every run and an output inspection
// for every run that produced an answer.
//
// A run that failed produced no output to inspect, so requiring one would turn
// a crash into a guardrail failure and teach a reader to ignore this line. A
// run that completed without an output inspection is the real finding: it means
// text reached a human without passing the filter.
func (i AgentGuardrailRan) Evaluate(ctx context.Context, ev Evidence) Verdict {
	v := base(i, SeverityHigh,
		"a guard.verdict record on the prompt of every run and on the output of every completed run")
	if len(ev.Records) == 0 {
		return inconclusive(v, "the audit log carries no records, so no run could be examined")
	}
	ids := runIDs(ev.Records)
	if len(ids) == 0 {
		return inconclusive(v, "no record carries a run id, so no trajectory could be located")
	}

	type state struct {
		started   bool
		completed bool
		prompt    *audit.Record
		output    *audit.Record
	}
	runs := map[string]*state{}
	get := func(id string) *state {
		s, ok := runs[id]
		if !ok {
			s = &state{}
			runs[id] = s
		}
		return s
	}
	for i := range ev.Records {
		r := ev.Records[i]
		if r.RunID == "" {
			continue
		}
		s := get(r.RunID)
		switch r.Action {
		case audit.ActionRunStarted:
			s.started = true
		case audit.ActionRunCompleted:
			s.completed = true
		case audit.ActionGuardVerdict:
			switch guard.Surface(r.Subject) {
			case guard.SurfacePrompt:
				s.prompt = &r
			case guard.SurfaceOutput:
				s.output = &r
			}
		}
	}

	var (
		refs []Ref
		bad  []string
	)
	for _, id := range ids {
		s := runs[id]
		if s.prompt == nil {
			bad = append(bad, "run "+id+": no guard verdict on the prompt")
		} else {
			refs = append(refs, AuditRef(*s.prompt))
		}
		if s.completed && s.output == nil {
			bad = append(bad, "run "+id+": completed with no guard verdict on the output")
		} else if s.output != nil {
			refs = append(refs, AuditRef(*s.output))
		}
	}

	observed := plural(len(ids), "run", "runs")
	if len(bad) > 0 {
		return fail(v, observed+", "+plural(len(bad), "gap", "gaps"), strings.Join(bad, "; "), refs...)
	}
	return pass(v, observed+", every surface inspected", refs...)
}

// ---------- AGENT-4 ----------

// AgentSpansCarryIdentity is AGENT-4.
type AgentSpansCarryIdentity struct{}

// ID returns the invariant id.
func (AgentSpansCarryIdentity) ID() string { return "AGENT-4" }

// Title states the invariant.
func (AgentSpansCarryIdentity) Title() string {
	return "every span in a run's trajectory carries the principal identity attributes"
}

// Domain groups the invariant.
func (AgentSpansCarryIdentity) Domain() string { return DomainAgent }

// Evaluate checks the identity attributes on every span of every run's trace.
//
// The trajectory is located by trace id rather than by looking for spans that
// happen to carry a run id, because a span that dropped the run id is exactly
// the failure being looked for and finding it by the attribute it is missing
// would make the check vacuous.
func (i AgentSpansCarryIdentity) Evaluate(ctx context.Context, ev Evidence) Verdict {
	v := base(i, SeverityHigh,
		"every span in a run's trace carries "+obs.AttrPrincipalSubject+", "+
			obs.AttrPrincipalTenant+" and "+obs.AttrRunID)
	if len(ev.Spans) == 0 {
		return inconclusive(v, "no spans were exported, so no trajectory could be examined")
	}

	// runID -> traceID, from the runs where available and from the log
	// otherwise, so the invariant works against either evidence source.
	traces := map[string]string{}
	for _, r := range ev.Runs {
		if r != nil && r.TraceID != "" {
			traces[r.ID] = r.TraceID
		}
	}
	for _, r := range ev.Records {
		if r.Action == audit.ActionRunStarted && r.RunID != "" && r.TraceID != "" {
			if _, ok := traces[r.RunID]; !ok {
				traces[r.RunID] = r.TraceID
			}
		}
	}
	if len(traces) == 0 {
		return inconclusive(v, "no run names a trace id, so its trajectory could not be located")
	}

	ids := make([]string, 0, len(traces))
	for id := range traces {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var (
		refs    []Ref
		bad     []string
		checked int
	)
	for _, runID := range ids {
		traceID := traces[runID]
		found := 0
		for _, s := range ev.Spans {
			if s.TraceID != traceID {
				continue
			}
			found++
			checked++
			var missing []string
			for _, k := range []string{obs.AttrPrincipalSubject, obs.AttrPrincipalTenant, obs.AttrRunID} {
				if s.Attr(k) == "" {
					missing = append(missing, k)
				}
			}
			if len(missing) > 0 {
				bad = append(bad, fmt.Sprintf("run %s span %s (%s) is missing %s",
					runID, s.SpanID, s.Name, strings.Join(missing, ", ")))
				refs = append(refs, SpanRef(s))
			}
		}
		if found == 0 {
			bad = append(bad, "run "+runID+": trace "+traceID+" exported no spans")
		}
	}

	observed := plural(checked, "span", "spans")
	if len(bad) > 0 {
		return fail(v, observed+", "+plural(len(bad), "unattributed", "unattributed"), strings.Join(bad, "; "), refs...)
	}
	return pass(v, observed+" across "+plural(len(ids), "trajectory", "trajectories")+", all attributed", refs...)
}

// ---------- AGENT-5 ----------

// AgentAuditChainVerifies is AGENT-5.
type AgentAuditChainVerifies struct{}

// ID returns the invariant id.
func (AgentAuditChainVerifies) ID() string { return "AGENT-5" }

// Title states the invariant.
func (AgentAuditChainVerifies) Title() string { return "the audit chain verifies end to end" }

// Domain groups the invariant.
func (AgentAuditChainVerifies) Domain() string { return DomainAgent }

// Evaluate walks the hash chain, and the signatures where a key was supplied.
//
// Every other invariant in this package reads the audit log and therefore
// depends on this one. If the chain does not verify, the rest of the report is
// a description of a document that may have been edited, which is why this is
// the invariant to read first when several fail at once.
func (i AgentAuditChainVerifies) Evaluate(ctx context.Context, ev Evidence) Verdict {
	v := base(i, SeverityCritical, "audit.VerifyRecords accepts the chain from genesis to head")
	if len(ev.Records) == 0 {
		return inconclusive(v, "the audit log carries no records, so there is no chain to verify")
	}
	if err := audit.VerifyRecords(ev.Records, ev.AuditPublicKey); err != nil {
		return fail(v, "chain broken", err.Error(),
			AuditRef(ev.Records[0]), AuditRef(ev.Records[len(ev.Records)-1]))
	}
	signed := ev.AuditPublicKey != nil
	observed := plural(len(ev.Records), "record", "records") + " chained to head " + shortHash(ev.AuditHead())
	if signed {
		observed += ", signatures verified"
	} else {
		observed += ", no verification key supplied"
	}
	return pass(v, observed, AuditRef(ev.Records[len(ev.Records)-1]))
}

// ---------- AGENT-6 ----------

// AgentStepBudgetHeld is AGENT-6.
type AgentStepBudgetHeld struct{}

// ID returns the invariant id.
func (AgentStepBudgetHeld) ID() string { return "AGENT-6" }

// Title states the invariant.
func (AgentStepBudgetHeld) Title() string {
	return "no run exceeded its step budget without a terminal record"
}

// Domain groups the invariant.
func (AgentStepBudgetHeld) Domain() string { return DomainAgent }

// Evaluate holds every run to its budget and requires the ones that reached it
// to have stopped and said so.
//
// A run parked at the budget awaiting a human is not a violation: it is at rest
// and its next step will end it. A run sitting at the budget in any other
// non-terminal state is a loop that did not notice its own limit, and a run
// past the budget is one that ran anyway.
func (i AgentStepBudgetHeld) Evaluate(ctx context.Context, ev Evidence) Verdict {
	budget := ev.stepBudget()
	v := base(i, SeverityHigh,
		fmt.Sprintf("no run runs past %d steps, and a run that reaches the budget is terminal and audited", budget))
	if len(ev.Runs) == 0 {
		return inconclusive(v, "no runs were supplied, so no step budget could be checked")
	}

	terminal := map[string]audit.Record{}
	for _, r := range ev.Records {
		if r.Action == audit.ActionRunCompleted || r.Action == audit.ActionRunFailed {
			terminal[r.RunID] = r
		}
	}

	runs := append([]*agent.Run(nil), ev.Runs...)
	sort.Slice(runs, func(a, b int) bool { return runs[a].ID < runs[b].ID })

	var (
		refs []Ref
		bad  []string
	)
	for _, r := range runs {
		if r == nil {
			continue
		}
		steps := len(r.Steps)
		switch {
		case steps > budget:
			bad = append(bad, fmt.Sprintf("run %s ran %d steps against a budget of %d", r.ID, steps, budget))
			refs = append(refs, RunRef(r))
			continue
		case steps < budget, r.Status == agent.StatusAwaiting:
			continue
		}
		if !r.Status.Terminal() {
			bad = append(bad, fmt.Sprintf("run %s reached the %d-step budget in state %s", r.ID, budget, r.Status))
			refs = append(refs, RunRef(r))
			continue
		}
		rec, ok := terminal[r.ID]
		if !ok {
			bad = append(bad, fmt.Sprintf("run %s reached the budget and stopped with no terminal audit record", r.ID))
			refs = append(refs, RunRef(r))
			continue
		}
		refs = append(refs, RunRef(r), AuditRef(rec))
	}

	observed := plural(len(runs), "run", "runs")
	if len(bad) > 0 {
		return fail(v, observed+", "+plural(len(bad), "over budget", "over budget"), strings.Join(bad, "; "), refs...)
	}
	return pass(v, observed+" within the "+fmt.Sprint(budget)+"-step budget", refs...)
}

// ---------- AGENT-7 ----------

// AgentDenialNotRetried is AGENT-7.
type AgentDenialNotRetried struct{}

// ID returns the invariant id.
func (AgentDenialNotRetried) ID() string { return "AGENT-7" }

// Title states the invariant.
func (AgentDenialNotRetried) Title() string {
	return "a policy denial is never followed by the same tool succeeding in the same run"
}

// Domain groups the invariant.
func (AgentDenialNotRetried) Domain() string { return DomainAgent }

// Evaluate looks for a refusal being worked around.
//
// This is the behaviour that distinguishes an agent that respects a control
// from one that treats it as an obstacle, and it is invisible to any check that
// only looks at final state: the money moved, the approval existed, the chain
// verifies, and the only trace of the problem is that the gateway said no first.
// A legitimate second attempt at a refused tool means the circumstances changed,
// and changed circumstances mean a new run.
func (i AgentDenialNotRetried) Evaluate(ctx context.Context, ev Evidence) Verdict {
	v := base(i, SeverityCritical,
		"no tool succeeds in a run after the policy gateway denied that tool in the same run")
	if len(ev.Records) == 0 {
		return inconclusive(v, "the audit log carries no records, so no denial could be examined")
	}

	denied := map[string]audit.Record{}
	var (
		refs    []Ref
		bad     []string
		denials int
	)
	for _, r := range ev.Records {
		key := r.RunID + "|" + r.Subject
		switch {
		case r.Action == audit.ActionPolicyDecision && r.Outcome == audit.OutcomeDenied:
			denied[key] = r
			denials++
		case invocationActions[r.Action] && r.Outcome == audit.OutcomeOK:
			d, ok := denied[key]
			if !ok {
				continue
			}
			bad = append(bad, fmt.Sprintf("run %s: %s was denied at seq %d and then succeeded at seq %d",
				r.RunID, r.Subject, d.Seq, r.Seq))
			refs = append(refs, AuditRef(d), AuditRef(r))
		}
	}

	observed := plural(denials, "policy denial", "policy denials")
	if len(bad) > 0 {
		return fail(v, observed+", "+plural(len(bad), "worked around", "worked around"), strings.Join(bad, "; "), refs...)
	}
	return pass(v, observed+", none followed by the same tool succeeding")
}
