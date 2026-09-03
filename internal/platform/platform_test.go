package platform

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/agent"
	"github.com/udaykishore-resu/tcrp/internal/identity"
	"github.com/udaykishore-resu/tcrp/internal/obs"
	"github.com/udaykishore-resu/tcrp/internal/policy"
)

func newTestPlatform(t *testing.T, opts Options) *Platform {
	t.Helper()
	if opts.Environment == "" {
		opts.Environment = "test"
	}
	opts.Deterministic = true
	p, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestTheAssemblyRefusesToStartOnAPolicyItCannotCompile. A policy typo must fail
// at startup in CI, not at three in the morning on the money-movement path.
func TestTheAssemblyRefusesToStartOnAPolicyItCannotCompile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	cases := []struct {
		name string
		path string
		want string
	}{
		{
			name: "a path that does not exist",
			path: filepath.Join(dir, "absent.json"),
			want: "policy",
		},
		{
			name: "a file that is not json",
			path: write("garbage.json", "this is not a policy"),
			want: "invalid bundle",
		},
		{
			name: "a bundle with no version",
			path: write("noversion.json", `{"package":"tcrp.test","default_effect":"deny","rules":[]}`),
			want: "missing version",
		},
		{
			name: "a bundle that allows by default, which is not a policy",
			path: write("allowall.json", `{"package":"tcrp.test","version":"1","default_effect":"allow","rules":[]}`),
			want: "default_effect must be deny",
		},
		{
			name: "a rule whose condition does not compile",
			path: write("badrule.json", `{"package":"tcrp.test","version":"1","default_effect":"deny",
				"rules":[{"id":"broken","effect":"allow","when":"input.tool.tier ==== \"read_only\""}]}`),
			want: `rule "broken"`,
		},
		{
			name: "a rule carrying an obligation nobody knows how to discharge",
			path: write("badobligation.json", `{"package":"tcrp.test","version":"1","default_effect":"deny",
				"rules":[{"id":"r","effect":"allow","when":"true","obligations":[{"type":"require_a_blood_oath"}]}]}`),
			want: "unknown obligation",
		},
		{
			name: "a deny rule carrying obligations, which cannot be discharged",
			path: write("denywithobligation.json", `{"package":"tcrp.test","version":"1","default_effect":"deny",
				"rules":[{"id":"r","effect":"deny","when":"true","obligations":[{"type":"notify"}]}]}`),
			want: "cannot carry obligations",
		},
		{
			name: "two rules sharing an id, so a decision cannot be traced to one of them",
			path: write("dupe.json", `{"package":"tcrp.test","version":"1","default_effect":"deny",
				"rules":[{"id":"r","effect":"allow","when":"true"},{"id":"r","effect":"deny","when":"false"}]}`),
			want: "duplicate rule id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(context.Background(), Options{
				Deterministic: true, Environment: "test", PolicyPath: tc.path,
			})
			if err == nil {
				_ = p.Close()
				t.Fatal("the platform started on a policy it should have refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestAValidPolicyOverrideReplacesTheEmbeddedBundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "override.json")
	body := `{"package":"tcrp.test","version":"override-1","default_effect":"deny","rules":[
		{"id":"deny-everything","effect":"deny","priority":100,"when":"true","reason":"the desk is closed"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newTestPlatform(t, Options{PolicyPath: path})
	if got := p.Policy.Bundle().Version; got != "override-1" {
		t.Fatalf("bundle version = %q, want the override", got)
	}
	run, err := p.Runtime.Start(context.Background(), agent.Request{
		Query: "What is our exposure to CP-DELTA?", Principal: AgentPrincipal,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Trajectory()) != 0 {
		t.Fatalf("a deny-everything bundle must reach no tool, got %v", run.Trajectory())
	}
	if d := run.Denials(); len(d) != 1 || d[0].PolicyRule != "deny-everything" {
		t.Fatalf("denials = %+v", d)
	}
}

func TestAnUnknownModelIsRefusedRatherThanDefaultedTo(t *testing.T) {
	p, err := New(context.Background(), Options{Deterministic: true, Environment: "test", Model: "oracle"})
	if err == nil {
		_ = p.Close()
		t.Fatal("New accepted an unknown model")
	}
	if !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("error = %q", err)
	}
}

func TestThePlannerIsTheDefaultModelUnderEverySpellingOfIt(t *testing.T) {
	for _, name := range []string{"", "planner", "deterministic"} {
		p := newTestPlatform(t, Options{Model: name})
		if got := p.Model.Name(); got != (agent.Planner{}).Name() {
			t.Fatalf("model %q built %q", name, got)
		}
	}
}

// TestTwoDeterministicPlatformsAnswerIdentically. Determinism is what makes an
// eval score a measurement rather than an anecdote, and what lets a regression
// be attributed to a change rather than to sampling.
func TestTwoDeterministicPlatformsAnswerIdentically(t *testing.T) {
	queries := []string{
		"What is our exposure to CP-DELTA?",
		"What is the haircut adjusted collateral value for CP-GOLF?",
		"Are there any reconciliation breaks?",
		"Are we over any concentration limit?",
		"Run a rate shock scenario at 250 bps",
		"Does total supply match across both ledgers?",
		"Transfer 5,000,000 shares to CP-HOTEL on evm",
	}
	type outcome struct {
		status     agent.RunStatus
		answer     string
		trajectory []string
		attempted  []string
		rules      []string
	}
	collect := func(t *testing.T) map[string]outcome {
		t.Helper()
		p := newTestPlatform(t, Options{})
		out := map[string]outcome{}
		for _, q := range queries {
			run, err := p.Runtime.Start(context.Background(), agent.Request{Query: q, Principal: AgentPrincipal})
			if err != nil {
				t.Fatalf("%q: %v", q, err)
			}
			var rules []string
			for _, s := range run.Steps {
				rules = append(rules, s.PolicyRule+"/"+s.Outcome)
			}
			out[q] = outcome{
				status: run.Status, answer: run.Answer,
				trajectory: run.Trajectory(), attempted: run.AttemptedTools(), rules: rules,
			}
		}
		return out
	}

	first, second := collect(t), collect(t)
	for _, q := range queries {
		a, b := first[q], second[q]
		if a.status != b.status {
			t.Errorf("%q: status %s vs %s", q, a.status, b.status)
		}
		if a.answer != b.answer {
			t.Errorf("%q: answers differ\n--- first ---\n%s\n--- second ---\n%s", q, a.answer, b.answer)
		}
		if !reflect.DeepEqual(a.trajectory, b.trajectory) {
			t.Errorf("%q: trajectory %v vs %v", q, a.trajectory, b.trajectory)
		}
		if !reflect.DeepEqual(a.attempted, b.attempted) {
			t.Errorf("%q: attempted %v vs %v", q, a.attempted, b.attempted)
		}
		if !reflect.DeepEqual(a.rules, b.rules) {
			t.Errorf("%q: policy decisions %v vs %v", q, a.rules, b.rules)
		}
	}
}

func TestAMoneyMovementReachesTheSameApprovalCardEveryTime(t *testing.T) {
	const q = "CP-DELTA looks undermargined, issue a margin call"
	summarise := func(t *testing.T) (string, int, []string) {
		t.Helper()
		p := newTestPlatform(t, Options{})
		run, err := p.Runtime.Start(context.Background(), agent.Request{Query: q, Principal: AgentPrincipal})
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != agent.StatusAwaiting || run.Pending == nil {
			t.Fatalf("status = %s, want the run paused for a human", run.Status)
		}
		ap, err := p.Approvals.Get(context.Background(), run.Pending.ApprovalID)
		if err != nil {
			t.Fatal(err)
		}
		return run.Pending.Summary, ap.RequiredCount, ap.RequiredRoles
	}
	s1, n1, r1 := summarise(t)
	s2, n2, r2 := summarise(t)
	if s1 != s2 || n1 != n2 || !reflect.DeepEqual(r1, r2) {
		t.Fatalf("approval cards differ:\n%q %d %v\n%q %d %v", s1, n1, r1, s2, n2, r2)
	}
	if !strings.Contains(s1, "issue_margin_call") || !strings.Contains(s1, "CP-DELTA") {
		t.Fatalf("the card must name the action and the counterparty: %q", s1)
	}
	if n1 != 2 {
		t.Fatalf("required approvers = %d; the shortfall is above the bundle's dual-approval threshold", n1)
	}
}

// TestADeterministicPlatformPinsItsClockToTheSeedInstant, which is what makes
// every figure, span id and evidence digest reproduce.
func TestADeterministicPlatformPinsItsClockToTheSeedInstant(t *testing.T) {
	p := newTestPlatform(t, Options{})
	mc, ok := p.Clock.(*obs.ManualClock)
	if !ok {
		t.Fatalf("clock = %T, want a manual clock", p.Clock)
	}
	if !mc.Now().Equal(p.Seed.AsOf) {
		t.Fatalf("clock = %s, want the seed instant %s", mc.Now(), p.Seed.AsOf)
	}
	p.Advance(90 * time.Minute)
	if want := p.Seed.AsOf.Add(90 * time.Minute); !p.Clock.Now().Equal(want) {
		t.Fatalf("after Advance the clock is %s, want %s", p.Clock.Now(), want)
	}
}

func TestAdvanceIsANoOpOnARealClockSoCallersNeedNotBranch(t *testing.T) {
	p, err := New(context.Background(), Options{Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, ok := p.Clock.(obs.RealClock); !ok {
		t.Fatalf("clock = %T, want the real clock when determinism is off", p.Clock)
	}
	p.Advance(time.Hour) // must not panic
}

// TestNothingInTheAssembledGraphIsNil: platform.New is the only place a
// component is allowed to be nil, and the answer is that none of them are.
func TestNothingInTheAssembledGraphIsNil(t *testing.T) {
	p := newTestPlatform(t, Options{})
	components := map[string]any{
		"clock": p.Clock, "tracer": p.Tracer, "spans": p.Spans, "metrics": p.Metrics,
		"log": p.Log, "audit": p.Audit, "signer": p.Signer, "policy": p.Policy,
		"guard": p.Guard, "inspector": p.Inspector, "seed": p.Seed, "lake": p.Lake,
		"world": p.World, "risk": p.Risk, "tools": p.Tools, "bridge": p.Bridge,
		"signers": p.Signers, "registry": p.Registry, "store": p.Store,
		"approvals": p.Approvals, "runtime": p.Runtime, "model": p.Model,
	}
	for name, c := range components {
		if c == nil {
			t.Errorf("%s is nil", name)
			continue
		}
		switch v := reflect.ValueOf(c); v.Kind() {
		case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func:
			if v.IsNil() {
				t.Errorf("%s is nil", name)
			}
		}
	}
	if len(p.AuditKey) == 0 {
		t.Error("no audit verification key")
	}
	if p.Registry.Len() == 0 {
		t.Error("no tools registered")
	}
	if p.Dir() == "" {
		t.Error("no state directory")
	}
}

func TestTheStateDirectoryIsReusedRatherThanRecreatedWhenOneIsNamed(t *testing.T) {
	dir := t.TempDir()
	p := newTestPlatform(t, Options{Dir: dir})
	if p.Dir() != dir {
		t.Fatalf("dir = %q, want %q", p.Dir(), dir)
	}
	if _, err := p.Runtime.Start(context.Background(), agent.Request{
		Query: "What is our exposure to CP-DELTA?", Principal: AgentPrincipal,
	}); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"lake", "runs", "audit.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err != nil {
			t.Errorf("%s: %v", sub, err)
		}
	}
}

func TestATemporaryStateDirectoryIsRemovedWithTheProcess(t *testing.T) {
	p, err := New(context.Background(), Options{Deterministic: true, Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	dir := p.Dir()
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the temporary directory survived Close: %v", err)
	}
}

func TestATraceFileReceivesOneObjectPerFinishedSpan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	p := newTestPlatform(t, Options{TraceFile: path})
	if _, err := p.Runtime.Start(context.Background(), agent.Request{
		Query: "What is our exposure to CP-DELTA?", Principal: AgentPrincipal,
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		t.Fatal("the trace file is empty")
	}
	if len(p.Spans.Spans()) == 0 {
		t.Fatal("the in-memory exporter recorded no spans")
	}
}

func TestTheDeclaredPrincipalsHoldExactlyTheAuthorityTheirRoleImplies(t *testing.T) {
	cases := []struct {
		name       string
		p          identity.Principal
		canApprove bool
		canMove    bool
	}{
		{"analyst", Analyst, false, false},
		{"operator", Operator, false, false},
		{"agent", AgentPrincipal, false, true},
		{"risk-manager", RiskManager, true, false},
		{"treasurer", Treasurer, true, false},
		{"auditor", Auditor, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PrincipalByName(tc.name)
			if !ok || got.Subject != tc.p.Subject {
				t.Fatalf("PrincipalByName(%q) = %+v, %v", tc.name, got, ok)
			}
			if bySubject, ok := PrincipalByName(tc.p.Subject); !ok || bySubject.Subject != tc.p.Subject {
				t.Fatalf("a principal must also resolve by subject")
			}
			if got := tc.p.HasScope(identity.ScopeApprove); got != tc.canApprove {
				t.Errorf("approvals:grant = %v, want %v", got, tc.canApprove)
			}
			moves := tc.p.HasScope(identity.ScopeTransferExecute) ||
				tc.p.HasScope(identity.ScopeMarginIssue) || tc.p.HasScope(identity.ScopeRedeem)
			if moves != tc.canMove {
				t.Errorf("money-movement scopes = %v, want %v", moves, tc.canMove)
			}
			// Nobody both moves money and approves it. That is the four-eyes
			// rule expressed in the identities rather than only in the gate.
			if moves && tc.p.HasScope(identity.ScopeApprove) {
				t.Error("this principal could authorise its own money movement")
			}
		})
	}
	if _, ok := PrincipalByName("nobody"); ok {
		t.Error("PrincipalByName invented a principal")
	}
	if got := PrincipalNames(); len(got) != len(cases) {
		t.Errorf("PrincipalNames = %v, want one entry per declared principal", got)
	}
}

func TestTheEvalHarnessGivesEachScenarioItsOwnWorld(t *testing.T) {
	h := NewEvalHarness(Options{Environment: "evaluation", Dir: "/should/be/ignored"})
	if !h.Base.Deterministic {
		t.Fatal("an eval platform must be deterministic")
	}
	if h.Base.Dir != "" {
		t.Fatalf("the base directory must be cleared so instances do not share state, got %q", h.Base.Dir)
	}
	if got := NewEvalHarness(Options{}).Base.Environment; got != "evaluation" {
		t.Fatalf("environment = %q, want evaluation", got)
	}
}

func TestTheBundleTheRuntimeRecordsIsTheOneItEvaluated(t *testing.T) {
	p := newTestPlatform(t, Options{})
	run, err := p.Runtime.Start(context.Background(), agent.Request{
		Query: "What is our exposure to CP-DELTA?", Principal: AgentPrincipal,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.PolicyBundle != p.Policy.Bundle().Version {
		t.Fatalf("run records bundle %q, engine has %q", run.PolicyBundle, p.Policy.Bundle().Version)
	}
	if run.GuardPolicy != p.Guard.Config().Version {
		t.Fatalf("run records guard policy %q, engine has %q", run.GuardPolicy, p.Guard.Config().Version)
	}
	if run.ModelName != p.Model.Name() {
		t.Fatalf("run records model %q, platform built %q", run.ModelName, p.Model.Name())
	}
	if run.Environment != "test" {
		t.Fatalf("environment = %q", run.Environment)
	}
	if _, err := policy.Default(); err != nil {
		t.Fatalf("the embedded bundle must compile: %v", err)
	}
}
