package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/agent"
	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/identity"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Executor starts a governed run. It is the slice of *agent.Runtime the runner
// needs, named as an interface so the runner's own tests can drive it without
// standing up a policy engine, a guardrail pipeline and a tool registry — none
// of which are what those tests are about.
type Executor interface {
	Start(ctx context.Context, req agent.Request) (*agent.Run, error)
}

// Gate decides a pending approval. *agent.ApprovalService satisfies it.
type Gate interface {
	Grant(ctx context.Context, approvalID string, p identity.Principal, note string) (*agent.Approval, error)
	Deny(ctx context.Context, approvalID string, p identity.Principal, note string) (*agent.Approval, error)
}

// RunLoader reads a run back after an approval has resumed it. agent.Store
// satisfies it.
type RunLoader interface {
	LoadRun(ctx context.Context, id string) (*agent.Run, error)
}

// Instance is one scenario's isolated world.
//
// Every scenario gets its own runtime, exporter and audit log. Sharing them
// would make the suite order-dependent in exactly the places that matter: a run
// that poisons retrieval, exhausts a rate limit or leaves an approval open must
// not be able to change the verdict of the scenario that happens to run after
// it.
type Instance struct {
	// Runtime is the governed runtime under test. When it is set, Exec, Approvals
	// and Runs default to it.
	Runtime *agent.Runtime
	// Exporter holds this scenario's spans, for the drift baseline and the
	// OpenInference export.
	Exporter *obs.MemoryExporter
	// Audit is this scenario's audit log.
	Audit *audit.Log
	// Approvers are the principals the runner uses to decide a gate. They must
	// differ from the scenario's principal or the four-eyes rule refuses them,
	// and there must be at least as many as the largest RequiredCount the
	// policy bundle can demand.
	Approvers []identity.Principal

	// Exec, Approvals and Runs override the defaults taken from Runtime.
	Exec      Executor
	Approvals Gate
	Runs      RunLoader

	// Close releases whatever the harness allocated.
	Close func()
}

func (i *Instance) resolve() error {
	if i.Exec == nil && i.Runtime != nil {
		i.Exec = i.Runtime
	}
	if i.Approvals == nil && i.Runtime != nil {
		i.Approvals = i.Runtime.Approvals()
	}
	if i.Runs == nil && i.Runtime != nil {
		i.Runs = i.Runtime.Store()
	}
	if i.Exec == nil {
		return fmt.Errorf("eval: harness returned an instance with no runtime and no executor")
	}
	return nil
}

func (i *Instance) close() {
	if i.Close != nil {
		i.Close()
	}
}

// Harness builds a fresh instance for a scenario.
type Harness interface {
	New(ctx context.Context, sc Scenario) (*Instance, error)
}

// HarnessFunc adapts a function to Harness.
type HarnessFunc func(ctx context.Context, sc Scenario) (*Instance, error)

// New builds the instance.
func (f HarnessFunc) New(ctx context.Context, sc Scenario) (*Instance, error) { return f(ctx, sc) }

// Runner executes a suite.
type Runner struct {
	// Harness builds one isolated instance per scenario. Required.
	Harness Harness
	// Judge grades answers. Nil means RubricJudge.
	Judge Judge
	// Clock is the runner's only source of time.
	Clock obs.Clock
	// Concurrency bounds how many scenarios run at once. Zero means
	// runtime.NumCPU().
	Concurrency int
	// Timeout bounds one scenario, approval round trips included. Zero means
	// two minutes.
	Timeout time.Duration
	// MaxApprovalRounds bounds how many times the runner will decide a gate for
	// one run, so a runtime that re-pauses forever cannot hang the suite.
	MaxApprovalRounds int
}

// Report is the outcome of a suite run.
type Report struct {
	Suite   string `json:"suite"`
	Version string `json:"version"`
	// RanAt comes from the runner's clock, so a manual clock yields a
	// byte-identical report across runs.
	RanAt        time.Time                `json:"ran_at"`
	Scenarios    []Result                 `json:"scenarios"`
	Passed       int                      `json:"passed"`
	Failed       int                      `json:"failed"`
	OverallScore float64                  `json:"overall_score"`
	ByCategory   map[string]CategoryScore `json:"by_category"`
	Duration     time.Duration            `json:"duration_ns"`
}

// CategoryScore aggregates one category.
type CategoryScore struct {
	Category string  `json:"category"`
	Total    int     `json:"total"`
	Passed   int     `json:"passed"`
	Failed   int     `json:"failed"`
	Score    float64 `json:"score"`
}

// Run executes every scenario in the suite and returns the report.
//
// A scenario that fails to build, times out or panics becomes a failed result
// rather than an error: a suite that stops at the first bad scenario tells you
// about one defect when it could have told you about all of them.
func (r *Runner) Run(ctx context.Context, suite Suite) (Report, error) {
	if r.Harness == nil {
		return Report{}, fmt.Errorf("eval: runner has no harness")
	}
	if err := suite.Validate(); err != nil {
		return Report{}, err
	}
	clock := r.Clock
	if clock == nil {
		clock = obs.RealClock{}
	}
	judge := r.Judge
	if judge == nil {
		judge = RubricJudge{}
	}
	limit := r.Concurrency
	if limit <= 0 {
		limit = runtime.NumCPU()
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	rounds := r.MaxApprovalRounds
	if rounds <= 0 {
		rounds = 4
	}

	ordered := suite.Sorted()
	started := clock.Now()
	results := make([]Result, len(ordered.Scenarios))

	// A semaphore channel plus a WaitGroup is what errgroup.SetLimit would give
	// us; this repository has no third-party modules, and a bounded pool is
	// twelve lines.
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range ordered.Scenarios {
		select {
		case <-ctx.Done():
			// Cancellation stops dispatch; already-running scenarios finish
			// under their own timeouts.
			results[i] = Result{
				ScenarioID: ordered.Scenarios[i].ID,
				Name:       ordered.Scenarios[i].Name,
				Category:   ordered.Scenarios[i].Category,
				Weight:     ordered.Scenarios[i].EffectiveWeight(),
				Error:      ctx.Err().Error(),
			}
			continue
		default:
		}
		wg.Add(1)
		go func(idx int, sc Scenario) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx] = r.runScenario(ctx, sc, judge, clock, timeout, rounds)
		}(i, ordered.Scenarios[i])
	}
	wg.Wait()

	rep := Report{
		Suite:      suite.Name,
		Version:    suite.Version,
		RanAt:      started,
		Scenarios:  results,
		ByCategory: map[string]CategoryScore{},
		Duration:   clock.Since(started),
	}
	rep.summarise()
	return rep, nil
}

// runScenario executes one scenario under its own timeout, recovering any panic
// so that one malformed tool cannot take the suite down with it.
func (r *Runner) runScenario(ctx context.Context, sc Scenario, judge Judge, clock obs.Clock, timeout time.Duration, rounds int) (res Result) {
	base := Result{
		ScenarioID: sc.ID,
		Name:       sc.Name,
		Category:   sc.Category,
		Weight:     sc.EffectiveWeight(),
	}
	res = base
	started := clock.Now()
	defer func() {
		if rec := recover(); rec != nil {
			res = base
			res.Panic = fmt.Sprintf("%v", rec)
			res.Failures = []string{"the scenario panicked: " + res.Panic, "stack: " + firstStackFrame(debug.Stack())}
			res.Grade = Grade{Judge: "rubric", Reasons: []string{"the run panicked"}}
			res.Scores = Score(sc, nil, res.Grade)
		}
		res.DurationMS = float64(clock.Since(started).Microseconds()) / 1000
	}()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	inst, err := r.Harness.New(runCtx, sc)
	if err != nil {
		res.Error = err.Error()
		res.Failures = []string{"the harness could not build an instance: " + err.Error()}
		return res
	}
	if inst == nil {
		res.Error = "harness returned a nil instance"
		res.Failures = []string{res.Error}
		return res
	}
	defer inst.close()
	if err := inst.resolve(); err != nil {
		res.Error = err.Error()
		res.Failures = []string{err.Error()}
		return res
	}

	run, err := inst.Exec.Start(runCtx, agent.Request{Query: sc.Query, Principal: sc.Principal})
	if err != nil {
		res.Error = err.Error()
		res.Failures = []string{"the run could not start: " + err.Error()}
		return res
	}

	var gateNotes []string
	run, gateNotes, err = r.decideGates(runCtx, inst, sc, run, rounds)
	if err != nil {
		res.Error = err.Error()
	}

	g, gerr := judge.Grade(runCtx, sc, run)
	if gerr != nil {
		g = Grade{Judge: "rubric", Reasons: []string{"the judge failed: " + gerr.Error()}}
	}
	res = Evaluate(sc, run, g)
	if err != nil {
		res.Error = err.Error()
		res.Failures = append(res.Failures, "approval handling: "+err.Error())
		res.Passed = false
	}
	res.Failures = append(res.Failures, gateNotes...)
	if len(res.Failures) > 0 {
		res.Passed = false
	}
	return res
}

// decideGates answers every approval the run opens.
//
// It grants with a principal that is not the requester, because the four-eyes
// control refuses anything else and a harness that could self-approve would be
// testing a different system. The one scenario named by DenyApprovalScenarioID
// is denied instead, and the assertion afterwards is that no money-movement
// tool ran — the point being that a denial is not merely recorded, it is
// load-bearing.
func (r *Runner) decideGates(ctx context.Context, inst *Instance, sc Scenario, run *agent.Run, rounds int) (*agent.Run, []string, error) {
	var notes []string
	deny := sc.ID == DenyApprovalScenarioID

	for round := 0; run != nil && run.Status == agent.StatusAwaiting && round < rounds; round++ {
		if run.Pending == nil {
			return run, notes, fmt.Errorf("run %s is awaiting approval with no pending call", run.ID)
		}
		if inst.Approvals == nil {
			return run, notes, fmt.Errorf("scenario %s pauses for approval but the instance exposes no approval gate", sc.ID)
		}
		if len(inst.Approvers) == 0 {
			return run, notes, fmt.Errorf("scenario %s pauses for approval but the instance supplies no approver", sc.ID)
		}
		approvalID := run.Pending.ApprovalID

		if deny {
			if _, err := inst.Approvals.Deny(ctx, approvalID, inst.Approvers[0], "eval: designated denial"); err != nil {
				return run, notes, fmt.Errorf("denying %s: %w", approvalID, err)
			}
		} else {
			granted := false
			var lastErr error
			for _, p := range inst.Approvers {
				if p.Subject == sc.Principal.Subject || (sc.Principal.OnBehalf != "" && p.Subject == sc.Principal.OnBehalf) {
					continue // four eyes: the requester may not approve
				}
				if _, err := inst.Approvals.Grant(ctx, approvalID, p, "eval: harness approval"); err != nil {
					lastErr = err
					continue
				}
				granted = true
				// A gate needing two approvers is satisfied only once the run
				// has left the awaiting state, so keep granting until it does.
				next, err := r.reload(ctx, inst, run)
				if err != nil {
					return run, notes, err
				}
				run = next
				if run.Status != agent.StatusAwaiting || run.Pending == nil || run.Pending.ApprovalID != approvalID {
					break
				}
			}
			if !granted {
				if lastErr == nil {
					lastErr = fmt.Errorf("no approver distinct from the requester")
				}
				return run, notes, fmt.Errorf("granting %s: %w", approvalID, lastErr)
			}
		}

		next, err := r.reload(ctx, inst, run)
		if err != nil {
			return run, notes, err
		}
		if next.Status == agent.StatusAwaiting && next.Pending != nil && next.Pending.ApprovalID == approvalID {
			// The gate was decided and the run did not move: that is a defect
			// in the resume path, not a scenario failure, so it is reported.
			return next, notes, fmt.Errorf("run %s did not resume after approval %s was decided", next.ID, approvalID)
		}
		run = next
	}

	if run != nil && run.Status == agent.StatusAwaiting {
		notes = append(notes, fmt.Sprintf("the run was still awaiting approval after %d rounds", rounds))
	}
	if deny && run != nil {
		if moved := moneyMoved(run); len(moved) > 0 {
			notes = append(notes, "the approval was denied and money still moved via "+strings.Join(moved, ", "))
		}
		if run.Status == agent.StatusAwaiting {
			notes = append(notes, "the run did not resume after the denial")
		}
	}
	return run, notes, nil
}

// reload reads the run back through the store, because a resume driven by the
// approval callback writes there rather than to the pointer we hold.
func (r *Runner) reload(ctx context.Context, inst *Instance, run *agent.Run) (*agent.Run, error) {
	if inst.Runs == nil {
		return run, fmt.Errorf("eval: instance exposes no run loader")
	}
	next, err := inst.Runs.LoadRun(ctx, run.ID)
	if err != nil {
		return run, fmt.Errorf("reloading run %s: %w", run.ID, err)
	}
	if next == nil {
		return run, fmt.Errorf("reloading run %s: store returned nothing", run.ID)
	}
	return next, nil
}

// moneyMoved returns the money-movement tools that actually executed.
func moneyMoved(run *agent.Run) []string {
	seen := map[string]bool{}
	for _, st := range run.Steps {
		if st.Outcome == agent.OutcomeInvoked && st.Tier == agent.TierMoney {
			seen[st.Tool] = true
		}
	}
	return sortedKeys(seen)
}

func firstStackFrame(stack []byte) string {
	lines := strings.Split(string(stack), "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if strings.Contains(l, "/internal/") && !strings.Contains(l, "runner.go") {
			return l
		}
	}
	if len(lines) > 0 {
		return strings.TrimSpace(lines[0])
	}
	return ""
}

// summarise computes the aggregate numbers. Results are already ordered by
// scenario id, and every aggregate is a weighted mean over that fixed order, so
// two runs of the same suite produce the same bytes.
func (rep *Report) summarise() {
	catWeight := map[string]float64{}
	catScore := map[string]float64{}
	var totalWeight, weighted float64

	for _, res := range rep.Scenarios {
		w := res.Weight
		if w <= 0 {
			w = 1
		}
		if res.Passed {
			rep.Passed++
		} else {
			rep.Failed++
		}
		totalWeight += w
		weighted += w * res.Scores.Overall

		key := string(res.Category)
		cs := rep.ByCategory[key]
		cs.Category = key
		cs.Total++
		if res.Passed {
			cs.Passed++
		} else {
			cs.Failed++
		}
		rep.ByCategory[key] = cs
		catWeight[key] += w
		catScore[key] += w * res.Scores.Overall
	}

	for key, cs := range rep.ByCategory {
		if catWeight[key] > 0 {
			cs.Score = round4(catScore[key] / catWeight[key])
		}
		rep.ByCategory[key] = cs
	}
	if totalWeight > 0 {
		rep.OverallScore = round4(weighted / totalWeight)
	}
}

// JSON renders the report as indented JSON, which is what CI stores.
func (rep Report) JSON() ([]byte, error) {
	return json.MarshalIndent(rep, "", "  ")
}

// categoriesInOrder returns the report's categories in the suite's declared
// order, with any unknown category appended alphabetically, so a printed report
// does not reorder itself between runs.
func (rep Report) categoriesInOrder() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range Categories() {
		if _, ok := rep.ByCategory[string(c)]; ok {
			out = append(out, string(c))
			seen[string(c)] = true
		}
	}
	var extra []string
	for k := range rep.ByCategory {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// TextSummary renders an aligned table for a terminal.
func (rep Report) TextSummary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "suite %s %s — %d passed, %d failed, overall %.4f (%s)\n\n",
		rep.Suite, rep.Version, rep.Passed, rep.Failed, rep.OverallScore, rep.Duration.Round(time.Millisecond))

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SCENARIO\tCATEGORY\tVERDICT\tOVERALL\tTRAJ\tF1\tANSWER\tEFF\tFORBID\tREFUSED\tVIOL\tSTATUS")
	for _, res := range rep.Scenarios {
		verdict := "FAIL"
		if res.Passed {
			verdict = "pass"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.4f\t%.2f\t%.2f\t%.2f\t%.2f\t%d\t%d\t%d\t%s\n",
			res.ScenarioID, res.Category, verdict, res.Scores.Overall,
			res.Scores.Trajectory, res.Scores.F1, res.Scores.Answer, res.Scores.Efficiency,
			res.Scores.ForbiddenCalled, res.Scores.RefusedAttempts, res.Scores.PolicyViolations,
			statusOrError(res))
	}
	_ = tw.Flush()

	b.WriteString("\n")
	tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CATEGORY\tTOTAL\tPASSED\tFAILED\tSCORE")
	for _, key := range rep.categoriesInOrder() {
		cs := rep.ByCategory[key]
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%.4f\n", cs.Category, cs.Total, cs.Passed, cs.Failed, cs.Score)
	}
	_ = tw.Flush()

	if rep.Failed > 0 {
		b.WriteString("\nfailures:\n")
		for _, res := range rep.Scenarios {
			if res.Passed {
				continue
			}
			for _, f := range res.Failures {
				fmt.Fprintf(&b, "  %s: %s\n", res.ScenarioID, f)
			}
		}
	}
	return b.String()
}

// MarkdownSummary renders the report for a pull request comment.
func (rep Report) MarkdownSummary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Agent evaluation — %s %s\n\n", rep.Suite, rep.Version)
	fmt.Fprintf(&b, "**%d passed, %d failed**, overall score **%.4f**, %s.\n\n",
		rep.Passed, rep.Failed, rep.OverallScore, rep.Duration.Round(time.Millisecond))

	b.WriteString("| Category | Total | Passed | Failed | Score |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: |\n")
	for _, key := range rep.categoriesInOrder() {
		cs := rep.ByCategory[key]
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %.4f |\n", cs.Category, cs.Total, cs.Passed, cs.Failed, cs.Score)
	}

	b.WriteString("\n| Scenario | Verdict | Overall | Trajectory | F1 | Answer | Forbidden | Refused | Violations |\n")
	b.WriteString("| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, res := range rep.Scenarios {
		verdict := "fail"
		if res.Passed {
			verdict = "pass"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %.4f | %.2f | %.2f | %.2f | %d | %d | %d |\n",
			res.ScenarioID, verdict, res.Scores.Overall, res.Scores.Trajectory, res.Scores.F1,
			res.Scores.Answer, res.Scores.ForbiddenCalled, res.Scores.RefusedAttempts, res.Scores.PolicyViolations)
	}

	if rep.Failed > 0 {
		b.WriteString("\n### Failures\n\n")
		for _, res := range rep.Scenarios {
			if res.Passed {
				continue
			}
			fmt.Fprintf(&b, "- **`%s`** (%s)\n", res.ScenarioID, res.Category)
			for _, f := range res.Failures {
				fmt.Fprintf(&b, "  - %s\n", f)
			}
		}
	}
	return b.String()
}

func statusOrError(res Result) string {
	switch {
	case res.Panic != "":
		return "panic"
	case res.Error != "":
		return "error"
	case res.Status == "":
		return "-"
	default:
		return string(res.Status)
	}
}
