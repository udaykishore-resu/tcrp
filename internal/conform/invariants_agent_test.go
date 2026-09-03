package conform_test

import (
	"context"
	"strings"
	"testing"

	"github.com/udaykishore-resu/tcrp/internal/agent"
	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/conform"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// evaluate runs one invariant by id against evidence and returns its verdict.
func evaluate(t *testing.T, id string, ev conform.Evidence) conform.Verdict {
	t.Helper()
	inv, ok := conform.Default().Get(id)
	if !ok {
		t.Fatalf("%s is not registered", id)
	}
	return inv.Evaluate(context.Background(), ev)
}

// wantStatus asserts a verdict's status and returns it, printing the detail on
// failure because that is the line a person would read.
func wantStatus(t *testing.T, v conform.Verdict, want conform.Status) conform.Verdict {
	t.Helper()
	if v.Status != want {
		t.Fatalf("%s: status %s, want %s\n  observed: %s\n  detail:   %s",
			v.ID, v.Status, want, v.Observed, v.Detail)
	}
	return v
}

// wantRefs asserts the verdict cites at least n pieces of evidence. A verdict
// with no reference is an opinion.
func wantRefs(t *testing.T, v conform.Verdict, n int) {
	t.Helper()
	if len(v.EvidenceRefs) < n {
		t.Fatalf("%s cites %d refs, want at least %d", v.ID, len(v.EvidenceRefs), n)
	}
}

// approvedMoneyRun is the passing trajectory every agent fixture starts from:
// a run that queried, was authorised, was approved by a second person, and
// moved money once.
func approvedMoneyRun(t *testing.T) *chain {
	t.Helper()
	c := newChain(t)
	c.add(runStarted("run-1", "agent.collateral"))
	c.add(guardVerdict("run-1", "prompt"))
	c.add(policyDecision("run-1", "get_exposure", audit.OutcomeAllowed, nil))
	c.add(readInvoked("run-1", "get_exposure"))
	c.add(policyDecision("run-1", "issue_margin_call", audit.OutcomeAllowed, nil))
	c.add(approvalGranted("run-1", "risk.chen", "apr-1"))
	c.add(policyDecision("run-1", "issue_margin_call", audit.OutcomeAllowed,
		map[string]any{"stage": "dispatch"}))
	c.add(moneyInvoked("run-1", "issue_margin_call", "apr-1"))
	c.add(guardVerdict("run-1", "output"))
	c.add(runCompleted("run-1", "agent.collateral"))
	return c
}

// ---------- AGENT-1 ----------

func TestAgent1PassesWhenASecondPersonApproved(t *testing.T) {
	ev := conform.Evidence{Records: approvedMoneyRun(t).records()}
	v := wantStatus(t, evaluate(t, "AGENT-1", ev), conform.StatusPass)
	wantRefs(t, v, 2)
	if v.Severity != conform.SeverityCritical {
		t.Fatalf("AGENT-1 severity %s, want critical", v.Severity)
	}
}

func TestAgent1FailsWhenNoApprovalWasCited(t *testing.T) {
	c := newChain(t)
	c.add(runStarted("run-1", "agent.collateral"))
	c.add(policyDecision("run-1", "issue_margin_call", audit.OutcomeAllowed, nil))
	c.add(moneyInvoked("run-1", "issue_margin_call", ""))
	v := wantStatus(t, evaluate(t, "AGENT-1", conform.Evidence{Records: c.records()}), conform.StatusFail)
	if !strings.Contains(v.Detail, "cites no approval") {
		t.Fatalf("detail does not name the failure: %s", v.Detail)
	}
	wantRefs(t, v, 1)
}

func TestAgent1FailsWhenTheRequesterApprovedTheirOwnCall(t *testing.T) {
	c := newChain(t)
	c.add(runStarted("run-1", "agent.collateral"))
	c.add(policyDecision("run-1", "issue_margin_call", audit.OutcomeAllowed, nil))
	// The approval exists, was granted, and is cited. It is worthless: the
	// human who approved is the human the agent is acting for.
	c.add(approvalGranted("run-1", "trader.rossi", "apr-1"))
	c.add(moneyInvoked("run-1", "issue_margin_call", "apr-1"))
	v := wantStatus(t, evaluate(t, "AGENT-1", conform.Evidence{Records: c.records()}), conform.StatusFail)
	if !strings.Contains(v.Detail, "who requested it") {
		t.Fatalf("detail does not name self-approval: %s", v.Detail)
	}
}

func TestAgent1FailsWhenTheApprovalWasRecordedAfterTheFact(t *testing.T) {
	c := newChain(t)
	c.add(runStarted("run-1", "agent.collateral"))
	c.add(policyDecision("run-1", "issue_margin_call", audit.OutcomeAllowed, nil))
	c.add(moneyInvoked("run-1", "issue_margin_call", "apr-1"))
	c.add(approvalGranted("run-1", "risk.chen", "apr-1"))
	v := wantStatus(t, evaluate(t, "AGENT-1", conform.Evidence{Records: c.records()}), conform.StatusFail)
	if !strings.Contains(v.Detail, "before its approval") {
		t.Fatalf("detail does not name the ordering: %s", v.Detail)
	}
}

func TestAgent1IsInconclusiveWithNoLog(t *testing.T) {
	wantStatus(t, evaluate(t, "AGENT-1", conform.Evidence{}), conform.StatusInconclusive)
}

// ---------- AGENT-2 ----------

func TestAgent2PassesWhenEveryToolWasAuthorised(t *testing.T) {
	ev := conform.Evidence{Records: approvedMoneyRun(t).records()}
	wantStatus(t, evaluate(t, "AGENT-2", ev), conform.StatusPass)
}

func TestAgent2FailsWhenAToolRanWithNoDecision(t *testing.T) {
	c := newChain(t)
	c.add(runStarted("run-1", "agent.collateral"))
	c.add(readInvoked("run-1", "get_exposure"))
	v := wantStatus(t, evaluate(t, "AGENT-2", conform.Evidence{Records: c.records()}), conform.StatusFail)
	if !strings.Contains(v.Detail, "no policy decision on the record") {
		t.Fatalf("detail: %s", v.Detail)
	}
}

func TestAgent2FailsWhenTheStandingDecisionWasAPropose(t *testing.T) {
	c := newChain(t)
	c.add(runStarted("run-1", "agent.collateral"))
	c.add(policyDecision("run-1", "issue_margin_call", audit.OutcomeAllowed,
		map[string]any{"stage": "propose"}))
	c.add(approvalGranted("run-1", "risk.chen", "apr-1"))
	// No dispatch-stage re-check: the approval could be replayed.
	c.add(moneyInvoked("run-1", "issue_margin_call", "apr-1"))
	v := wantStatus(t, evaluate(t, "AGENT-2", conform.Evidence{Records: c.records()}), conform.StatusFail)
	if !strings.Contains(v.Detail, "propose-stage decision") {
		t.Fatalf("detail: %s", v.Detail)
	}
}

// ---------- AGENT-3 ----------

func TestAgent3PassesWhenBothSurfacesWereInspected(t *testing.T) {
	ev := conform.Evidence{Records: approvedMoneyRun(t).records()}
	wantStatus(t, evaluate(t, "AGENT-3", ev), conform.StatusPass)
}

func TestAgent3FailsWhenACompletedRunHasNoOutputVerdict(t *testing.T) {
	c := newChain(t)
	c.add(runStarted("run-1", "agent.collateral"))
	c.add(guardVerdict("run-1", "prompt"))
	c.add(runCompleted("run-1", "agent.collateral"))
	v := wantStatus(t, evaluate(t, "AGENT-3", conform.Evidence{Records: c.records()}), conform.StatusFail)
	if !strings.Contains(v.Detail, "no guard verdict on the output") {
		t.Fatalf("detail: %s", v.Detail)
	}
}

// TestAgent3DoesNotPunishARunThatProducedNoOutput: a run that failed had no
// answer to inspect, and turning that into a guardrail finding would teach a
// reader to skip this line.
func TestAgent3DoesNotPunishARunThatProducedNoOutput(t *testing.T) {
	c := newChain(t)
	c.add(runStarted("run-1", "agent.collateral"))
	c.add(guardVerdict("run-1", "prompt"))
	c.add(audit.Record{
		Actor: "agent.collateral", Tenant: "acme", Action: audit.ActionRunFailed,
		Subject: "raise a margin call", Outcome: audit.OutcomeError, RunID: "run-1",
	})
	wantStatus(t, evaluate(t, "AGENT-3", conform.Evidence{Records: c.records()}), conform.StatusPass)
}

// ---------- AGENT-4 ----------

func TestAgent4PassesWhenEverySpanIsAttributed(t *testing.T) {
	const trace = "0000000000000000000000000000ab01"
	ev := conform.Evidence{
		Records: approvedMoneyRun(t).records(),
		Spans: []obs.SpanData{
			span(trace, "00000000000000a1", "agent.run", "agent.collateral", "acme", "run-1"),
			span(trace, "00000000000000a2", "policy.evaluate", "agent.collateral", "acme", "run-1"),
			span(trace, "00000000000000a3", "agent.tool.issue_margin_call", "agent.collateral", "acme", "run-1"),
		},
	}
	v := wantStatus(t, evaluate(t, "AGENT-4", ev), conform.StatusPass)
	if !strings.Contains(v.Observed, "3 spans") {
		t.Fatalf("observed: %s", v.Observed)
	}
}

func TestAgent4FailsWhenASpanDropsTheIdentity(t *testing.T) {
	const trace = "0000000000000000000000000000ab01"
	ev := conform.Evidence{
		Records: approvedMoneyRun(t).records(),
		Spans: []obs.SpanData{
			span(trace, "00000000000000a1", "agent.run", "agent.collateral", "acme", "run-1"),
			span(trace, "00000000000000a2", "ledger.transfer", "", "acme", "run-1"),
		},
	}
	v := wantStatus(t, evaluate(t, "AGENT-4", ev), conform.StatusFail)
	if !strings.Contains(v.Detail, obs.AttrPrincipalSubject) {
		t.Fatalf("detail does not name the missing attribute: %s", v.Detail)
	}
	wantRefs(t, v, 1)
}

func TestAgent4IsInconclusiveWithoutSpans(t *testing.T) {
	ev := conform.Evidence{Records: approvedMoneyRun(t).records()}
	wantStatus(t, evaluate(t, "AGENT-4", ev), conform.StatusInconclusive)
}

// ---------- AGENT-5 ----------

func TestAgent5PassesOnAnIntactChain(t *testing.T) {
	ev := conform.Evidence{Records: approvedMoneyRun(t).records()}
	v := wantStatus(t, evaluate(t, "AGENT-5", ev), conform.StatusPass)
	wantRefs(t, v, 1)
}

func TestAgent5FailsWhenARecordWasEdited(t *testing.T) {
	recs := approvedMoneyRun(t).records()
	// Change an outcome without recomputing the hash: exactly what somebody
	// tidying up an audit log would do.
	recs[7].Outcome = audit.OutcomeDenied
	v := wantStatus(t, evaluate(t, "AGENT-5", conform.Evidence{Records: recs}), conform.StatusFail)
	if !strings.Contains(v.Detail, "content hash") {
		t.Fatalf("detail: %s", v.Detail)
	}
}

func TestAgent5FailsWhenARecordWasRemoved(t *testing.T) {
	recs := approvedMoneyRun(t).records()
	recs = append(recs[:4], recs[5:]...)
	wantStatus(t, evaluate(t, "AGENT-5", conform.Evidence{Records: recs}), conform.StatusFail)
}

// ---------- AGENT-6 ----------

func run(id string, status agent.RunStatus, steps int) *agent.Run {
	r := &agent.Run{ID: id, Status: status, CreatedAt: epoch, UpdatedAt: epoch}
	for i := 0; i < steps; i++ {
		r.Steps = append(r.Steps, agent.StepRecord{N: i + 1, Outcome: agent.OutcomeInvoked, At: epoch})
	}
	return r
}

func TestAgent6PassesUnderBudget(t *testing.T) {
	ev := conform.Evidence{
		Records:    approvedMoneyRun(t).records(),
		Runs:       []*agent.Run{run("run-1", agent.StatusCompleted, 4)},
		StepBudget: 12,
	}
	wantStatus(t, evaluate(t, "AGENT-6", ev), conform.StatusPass)
}

func TestAgent6FailsWhenARunOverranItsBudget(t *testing.T) {
	ev := conform.Evidence{
		Records:    approvedMoneyRun(t).records(),
		Runs:       []*agent.Run{run("run-1", agent.StatusCompleted, 13)},
		StepBudget: 12,
	}
	v := wantStatus(t, evaluate(t, "AGENT-6", ev), conform.StatusFail)
	if !strings.Contains(v.Detail, "13 steps against a budget of 12") {
		t.Fatalf("detail: %s", v.Detail)
	}
}

func TestAgent6FailsWhenARunSitsAtBudgetStillRunning(t *testing.T) {
	ev := conform.Evidence{
		Records:    approvedMoneyRun(t).records(),
		Runs:       []*agent.Run{run("run-1", agent.StatusRunning, 12)},
		StepBudget: 12,
	}
	wantStatus(t, evaluate(t, "AGENT-6", ev), conform.StatusFail)
}

// TestAgent6AllowsARunParkedAtBudgetAwaitingAHuman: a run waiting on an
// approver is at rest, not looping.
func TestAgent6AllowsARunParkedAtBudgetAwaitingAHuman(t *testing.T) {
	ev := conform.Evidence{
		Records:    approvedMoneyRun(t).records(),
		Runs:       []*agent.Run{run("run-1", agent.StatusAwaiting, 12)},
		StepBudget: 12,
	}
	wantStatus(t, evaluate(t, "AGENT-6", ev), conform.StatusPass)
}

func TestAgent6IsInconclusiveWithoutRuns(t *testing.T) {
	ev := conform.Evidence{Records: approvedMoneyRun(t).records()}
	wantStatus(t, evaluate(t, "AGENT-6", ev), conform.StatusInconclusive)
}

// ---------- AGENT-7 ----------

func TestAgent7PassesWhenARefusalWasAccepted(t *testing.T) {
	c := newChain(t)
	c.add(runStarted("run-1", "agent.collateral"))
	c.add(policyDecision("run-1", "execute_transfer", audit.OutcomeDenied, nil))
	c.add(policyDecision("run-1", "get_exposure", audit.OutcomeAllowed, nil))
	c.add(readInvoked("run-1", "get_exposure"))
	c.add(runCompleted("run-1", "agent.collateral"))
	v := wantStatus(t, evaluate(t, "AGENT-7", conform.Evidence{Records: c.records()}), conform.StatusPass)
	if !strings.Contains(v.Observed, "1 policy denial") {
		t.Fatalf("observed: %s", v.Observed)
	}
}

func TestAgent7FailsWhenTheRefusalWasWorkedAround(t *testing.T) {
	c := newChain(t)
	c.add(runStarted("run-1", "agent.collateral"))
	c.add(policyDecision("run-1", "execute_transfer", audit.OutcomeDenied, nil))
	// Same run, same tool, second attempt, allowed this time.
	c.add(policyDecision("run-1", "execute_transfer", audit.OutcomeAllowed, nil))
	c.add(approvalGranted("run-1", "risk.chen", "apr-1"))
	c.add(audit.Record{
		Actor: "agent.collateral", OnBehalf: "trader.rossi", Tenant: "acme",
		Action: audit.ActionTransferExecuted, Subject: "execute_transfer",
		Outcome: audit.OutcomeOK, RunID: "run-1",
		Details: map[string]any{"tier": "money_movement", "approval_id": "apr-1"},
	})
	v := wantStatus(t, evaluate(t, "AGENT-7", conform.Evidence{Records: c.records()}), conform.StatusFail)
	if !strings.Contains(v.Detail, "was denied at seq") {
		t.Fatalf("detail: %s", v.Detail)
	}
	wantRefs(t, v, 2)
}

// TestAgent7DoesNotCrossRuns: a refusal in one run says nothing about another.
func TestAgent7DoesNotCrossRuns(t *testing.T) {
	c := newChain(t)
	c.add(policyDecision("run-1", "execute_transfer", audit.OutcomeDenied, nil))
	c.add(policyDecision("run-2", "execute_transfer", audit.OutcomeAllowed, nil))
	c.add(audit.Record{
		Actor: "agent.collateral", Tenant: "acme", Action: audit.ActionTransferExecuted,
		Subject: "execute_transfer", Outcome: audit.OutcomeOK, RunID: "run-2",
		Details: map[string]any{"tier": "money_movement"},
	})
	wantStatus(t, evaluate(t, "AGENT-7", conform.Evidence{Records: c.records()}), conform.StatusPass)
}
