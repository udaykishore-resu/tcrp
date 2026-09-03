package conform_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/udaykishore-resu/tcrp/internal/agent"
	"github.com/udaykishore-resu/tcrp/internal/conform"
	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// agentEvidence is a complete, clean body of evidence for the collateral
// process: the audit chain, the trajectory's spans and the run itself.
func agentEvidence(t *testing.T) conform.Evidence {
	t.Helper()
	const trace = "0000000000000000000000000000ab01"
	return conform.Evidence{
		Records: approvedMoneyRun(t).records(),
		Spans: []obs.SpanData{
			span(trace, "00000000000000a1", "agent.run", "agent.collateral", "acme", "run-1"),
			span(trace, "00000000000000a2", "policy.evaluate", "agent.collateral", "acme", "run-1"),
			span(trace, "00000000000000a3", "agent.tool.issue_margin_call", "agent.collateral", "acme", "run-1"),
		},
		Runs:               []*agent.Run{run("run-1", agent.StatusCompleted, 4)},
		StepBudget:         12,
		PolicyBundleDigest: "b6a1f0c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0",
		AsOf:               epoch,
	}
}

// settlementEvidence is a complete, clean body of evidence for the bridge
// process.
func settlementEvidence(t *testing.T) conform.Evidence {
	t.Helper()
	const trace = "0000000000000000000000000000ab01"
	ev := attestedEvidence(t, 400, 2)
	ev.Records = approvedMoneyRun(t).records()
	ev.Spans = []obs.SpanData{
		span(trace, "00000000000000a1", "bridge.poll", "relayer", "acme", "run-1"),
	}
	ev.Source.Holders = func(string) []string { return []string{"0xalice", "0xbob"} }
	ev.Source.Registered = func(string) bool { return true }
	ev.Target.Holders = func(string) []string { return []string{"Alice::1220"} }
	ev.Target.Registered = func(string) bool { return true }
	q := domain.QualityReport{Status: domain.QualityPass, RanAt: epoch}
	ev.Quality = &q
	ev.Lineage = func() (int, int) { return 64, 64 }
	ev.PolicyBundleDigest = "b6a1f0c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0"
	return ev
}

func mustParse(t *testing.T, name string) *conform.Process {
	t.Helper()
	p, err := conform.ParseFile(filepath.Join(modelsDir(), name))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	if err := p.Validate(conform.Default()); err != nil {
		t.Fatalf("validate %s: %v", name, err)
	}
	return p
}

func TestEngineRunsTheCollateralProcessClean(t *testing.T) {
	p := mustParse(t, "collateral-margin-call.bpmn")
	eng := conform.NewEngine(conform.Default(), testClock())
	rep := eng.Run(context.Background(), p, agentEvidence(t))

	if rep.Process != "tcrp-collateral-margin-call" {
		t.Fatalf("process %q", rep.Process)
	}
	if rep.ProcessDigest != p.SourceDigest {
		t.Fatal("report does not carry the model digest")
	}
	if len(rep.Verdicts) != 7 {
		t.Fatalf("%d verdicts, want 7", len(rep.Verdicts))
	}
	if rep.Failed != 0 || rep.Inconclusive != 0 {
		t.Fatalf("clean evidence produced %d fail, %d inconclusive:\n%s",
			rep.Failed, rep.Inconclusive, rep.Text())
	}
	if err := rep.Gate(); err != nil {
		t.Fatalf("gate: %v\n%s", err, rep.Text())
	}
	for _, v := range rep.Verdicts {
		if v.CheckedAt.IsZero() {
			t.Fatalf("%s has no CheckedAt", v.ID)
		}
	}
}

func TestEngineRunsTheBridgeProcessClean(t *testing.T) {
	p := mustParse(t, "bridge-lock-and-mint.bpmn")
	eng := conform.NewEngine(conform.Default(), testClock())
	rep := eng.Run(context.Background(), p, settlementEvidence(t))
	if rep.Failed != 0 || rep.Inconclusive != 0 {
		t.Fatalf("clean evidence produced %d fail, %d inconclusive:\n%s",
			rep.Failed, rep.Inconclusive, rep.Text())
	}
	if err := rep.Gate(); err != nil {
		t.Fatalf("gate: %v", err)
	}
}

// TestVerdictOrderIsDeterministic: the report is a diffable artefact, so the
// order has to come from the invariant ids rather than from map iteration.
func TestVerdictOrderIsDeterministic(t *testing.T) {
	p := mustParse(t, "collateral-margin-call.bpmn")
	eng := conform.NewEngine(conform.Default(), testClock())
	var first []string
	for i := 0; i < 20; i++ {
		rep := eng.Run(context.Background(), p, agentEvidence(t))
		var ids []string
		for _, v := range rep.Verdicts {
			ids = append(ids, v.ID)
		}
		if i == 0 {
			first = ids
			continue
		}
		if strings.Join(ids, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d ordered verdicts %v, first run gave %v", i, ids, first)
		}
	}
	want := "AGENT-1,AGENT-2,AGENT-3,AGENT-4,AGENT-5,AGENT-6,AGENT-7"
	if strings.Join(first, ",") != want {
		t.Fatalf("order %v, want %s", first, want)
	}
}

func TestGatePassesOnACleanRun(t *testing.T) {
	rep := conform.NewEngine(nil, testClock()).Run(context.Background(),
		mustParse(t, "collateral-margin-call.bpmn"), agentEvidence(t))
	if err := rep.Gate(); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if !strings.Contains(rep.Text(), "GATE PASSED") {
		t.Fatal("text report does not say the gate passed")
	}
	if !strings.Contains(rep.Markdown(), "**Gate passed.**") {
		t.Fatal("markdown report does not say the gate passed")
	}
}

func TestGateFailsOnACriticalFailure(t *testing.T) {
	ev := agentEvidence(t)
	// Break the chain: AGENT-5 is critical.
	ev.Records[3].Actor = "somebody.else"
	rep := conform.NewEngine(nil, testClock()).Run(context.Background(),
		mustParse(t, "collateral-margin-call.bpmn"), ev)
	err := rep.Gate()
	if err == nil {
		t.Fatal("gate passed on a broken audit chain")
	}
	if !errors.Is(err, conform.ErrGate) {
		t.Fatalf("error %v does not wrap ErrGate", err)
	}
	if !strings.Contains(err.Error(), "AGENT-5 fail") {
		t.Fatalf("error does not name the invariant: %v", err)
	}
	var ge *conform.GateError
	if !errors.As(err, &ge) || len(ge.Blocking) == 0 {
		t.Fatalf("error carries no blocking verdicts: %v", err)
	}
}

// TestGateFailsOnACriticalInconclusive is the asymmetry this package argues
// for: an invariant that could not find its evidence has proved nothing, and
// nothing is not a pass.
func TestGateFailsOnACriticalInconclusive(t *testing.T) {
	ev := agentEvidence(t)
	ev.Records = nil // the audit log never arrived
	rep := conform.NewEngine(nil, testClock()).Run(context.Background(),
		mustParse(t, "collateral-margin-call.bpmn"), ev)
	if rep.Failed != 0 {
		t.Fatalf("expected no outright failures, got %d:\n%s", rep.Failed, rep.Text())
	}
	if rep.Inconclusive == 0 {
		t.Fatal("expected inconclusive verdicts")
	}
	err := rep.Gate()
	if err == nil {
		t.Fatalf("gate passed with no evidence at all:\n%s", rep.Text())
	}
	if !strings.Contains(err.Error(), "inconclusive") {
		t.Fatalf("gate error does not say inconclusive: %v", err)
	}
}

// TestANonCriticalInconclusiveDoesNotStopTheGate: the gate stays a small set of
// statements a team will keep green, so the high-severity invariants report and
// do not block.
func TestANonCriticalInconclusiveDoesNotStopTheGate(t *testing.T) {
	ev := agentEvidence(t)
	ev.Spans = nil // AGENT-4 is high, not critical
	ev.Runs = nil  // so is AGENT-6
	rep := conform.NewEngine(nil, testClock()).Run(context.Background(),
		mustParse(t, "collateral-margin-call.bpmn"), ev)
	if rep.Inconclusive != 2 {
		t.Fatalf("%d inconclusive, want 2:\n%s", rep.Inconclusive, rep.Text())
	}
	if err := rep.Gate(); err != nil {
		t.Fatalf("gate stopped on a non-critical inconclusive: %v", err)
	}
}

// TestRunReportsAnUnimplementedInvariantRatherThanSkippingIt: Validate is where
// this is meant to be caught, and a caller who skipped it gets an inconclusive
// that blocks rather than a silently shorter report.
func TestRunReportsAnUnimplementedInvariantRatherThanSkippingIt(t *testing.T) {
	src := wrap(`<bpmn:process id="p" name="p">
		<bpmn:startEvent id="s"/>
		<bpmn:endEvent id="e"/>
		<bpmn:sequenceFlow id="f" sourceRef="s" targetRef="e"/>
		<bpmn:textAnnotation id="n"><bpmn:text>@invariant AGENT-99 nobody wrote this one</bpmn:text></bpmn:textAnnotation>
		<bpmn:association id="a" sourceRef="s" targetRef="n"/>
	</bpmn:process>`)
	p, err := conform.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rep := conform.NewEngine(nil, testClock()).Run(context.Background(), p, agentEvidence(t))
	v, ok := rep.Verdict("AGENT-99")
	if !ok {
		t.Fatal("report omits the undeclared invariant entirely")
	}
	if v.Status != conform.StatusInconclusive || !v.Blocks() {
		t.Fatalf("verdict %s severity %s, want a blocking inconclusive", v.Status, v.Severity)
	}
}

// TestRunWithNoProcessEvaluatesEverything answers "what does the platform
// currently believe", which is a different question from "does this process
// conform".
func TestRunWithNoProcessEvaluatesEverything(t *testing.T) {
	rep := conform.NewEngine(nil, testClock()).Run(context.Background(), nil, agentEvidence(t))
	if len(rep.Verdicts) != len(conform.Default().IDs()) {
		t.Fatalf("%d verdicts, want %d", len(rep.Verdicts), len(conform.Default().IDs()))
	}
	if rep.Process != "" {
		t.Fatalf("report names a process it was not given: %q", rep.Process)
	}
}

func TestReportRenderersNameTheFinding(t *testing.T) {
	ev := agentEvidence(t)
	ev.Records[3].Actor = "somebody.else"
	rep := conform.NewEngine(nil, testClock()).Run(context.Background(),
		mustParse(t, "collateral-margin-call.bpmn"), ev)

	text := rep.Text()
	for _, want := range []string{"AGENT-5", "GATE FAILED", "evidence:", "expected:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text report is missing %q:\n%s", want, text)
		}
	}
	md := rep.Markdown()
	for _, want := range []string{"# Conformance:", "## Findings", "`AGENT-5`", "**Gate failed.**"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown report is missing %q:\n%s", want, md)
		}
	}
	// A pipe in a title or an observed value must not break the table.
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "| ") && strings.Count(line, "|")-strings.Count(line, "\\|") != 6 {
			t.Fatalf("markdown table row has the wrong cell count: %q", line)
		}
	}
}

func TestRegistryRefusesADuplicateID(t *testing.T) {
	reg := conform.NewRegistry(conform.AgentAuditChainVerifies{})
	if err := reg.Register(conform.AgentAuditChainVerifies{}); !errors.Is(err, conform.ErrDuplicateInvariant) {
		t.Fatalf("error %v, want ErrDuplicateInvariant", err)
	}
	if err := reg.Register(nil); err == nil {
		t.Fatal("registering nil succeeded")
	}
}

func TestDefaultRegistryCoversEveryDeclaredInvariant(t *testing.T) {
	reg := conform.Default()
	want := []string{
		"AGENT-1", "AGENT-2", "AGENT-3", "AGENT-4", "AGENT-5", "AGENT-6", "AGENT-7",
		"DATA-1", "SETTLE-1", "SETTLE-2", "SETTLE-3", "SETTLE-4", "SETTLE-5",
	}
	if strings.Join(reg.IDs(), ",") != strings.Join(want, ",") {
		t.Fatalf("registry holds %v, want %v", reg.IDs(), want)
	}
	for _, inv := range reg.All() {
		if inv.Title() == "" || inv.Domain() == "" {
			t.Fatalf("%s has no title or domain", inv.ID())
		}
	}
}
