package policy

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/identity"
)

func TestSmoke(t *testing.T) {
	b := MustDefault()
	e := NewEngine(b, nil, nil, nil)
	in := Input{
		Principal: identity.Principal{Subject: "agent-1", Kind: identity.KindAgent, Tenant: "t1", OnBehalf: "uday",
			Roles:  []string{"collateral-ops"},
			Scopes: []string{"collateral:read", "transfer:execute"}},
		Tool:     ToolInfo{Name: "execute_transfer", Tier: "money_movement", RequiredScope: []string{"transfer:execute"}},
		Resource: ResourceInfo{Kind: "transfer", Whitelisted: true, AmountUnits: 1_000_000_000},
		Run:      RunInfo{Stage: StagePropose, RetrievalTrusted: true},
		Now:      time.Now(),
	}
	d := e.Evaluate(context.Background(), in)
	t.Log("\n" + d.Explain())
	if !d.Allow || !d.RequiresApproval() {
		t.Fatalf("want allow+approval, got %v", d.Summary())
	}
	in.Run.Stage = StageDispatch
	d2 := e.Evaluate(context.Background(), in)
	if d2.Allow {
		t.Fatalf("dispatch without approval must deny, got %v", d2.Summary())
	}
	t.Log(d2.Summary())
}
