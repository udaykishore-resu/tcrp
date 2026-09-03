package platform

import (
	"context"
	"github.com/udaykishore-resu/tcrp/internal/identity"
	"testing"

	"github.com/udaykishore-resu/tcrp/internal/agent"
)

func TestSmokeRun(t *testing.T) {
	ctx := context.Background()
	p, err := New(ctx, Options{Deterministic: true, Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, q := range []string{
		"What is our exposure to CP-DELTA?",
		"CP-DELTA looks undermargined, issue a margin call",
		"Transfer 5,000,000 shares to CP-HOTEL",
		"Are there any reconciliation breaks?",
	} {
		run, err := p.Runtime.Start(ctx, agent.Request{Query: q, Principal: AgentPrincipal})
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		t.Logf("\n=== %s\nstatus=%s steps=%d trajectory=%v\n%s", q, run.Status, len(run.Steps), run.AttemptedTools(), run.Answer)
		for _, s := range run.Steps {
			t.Logf("  step %d tool=%-24s outcome=%-18s rule=%s %s", s.N, s.Tool, s.Outcome, s.PolicyRule, s.Error)
		}
		for _, approver := range []struct {
			name string
			p    identity.Principal
		}{{"risk-manager", RiskManager}, {"treasurer", Treasurer}} {
			cur, _ := p.Store.LoadRun(ctx, run.ID)
			if cur.Status != agent.StatusAwaiting || cur.Pending == nil {
				break
			}
			ap, err := p.Approvals.Grant(ctx, cur.Pending.ApprovalID, approver.p, "checked against the exposure report")
			if err != nil {
				t.Fatalf("grant by %s: %v", approver.name, err)
			}
			t.Logf("  approval %s after %s: %s (%d/%d)", ap.ID, approver.name, ap.Status, len(ap.Grants), ap.RequiredCount)
		}
		if after, _ := p.Store.LoadRun(ctx, run.ID); after.Status != run.Status {
			t.Logf("  after resume: status=%s trajectory=%v\n%s", after.Status, after.Trajectory(), after.Answer)
		}
	}
	if err := p.Audit.Verify(); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	t.Logf("audit records: %d", p.Audit.Len())
}
