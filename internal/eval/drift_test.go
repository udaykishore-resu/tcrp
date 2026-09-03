package eval

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/agent"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

const tol = 1e-9

// TestPSIHandComputed checks PSI against values worked out by hand from the
// definition PSI = sum (o - e) * ln(o / e).
//
//	e = [0.5, 0.5], o = [0.4, 0.6]
//	  (0.4-0.5)*ln(0.4/0.5) = -0.1 * ln(0.8) = -0.1 * -0.22314355131420976 = 0.022314355131420976
//	  (0.6-0.5)*ln(0.6/0.5) =  0.1 * ln(1.2) =  0.1 *  0.18232155679395463 = 0.018232155679395463
//	  PSI = 0.04054651081081644                                   (below the 0.10 warning line)
//
//	e = [0.5, 0.5], o = [0.3, 0.7]
//	  -0.2 * ln(0.6) = -0.2 * -0.5108256237659907 = 0.10216512475319814
//	   0.2 * ln(1.4) =  0.2 *  0.3364722366212129 = 0.06729444732424258
//	  PSI = 0.16945957207744072                                   (warning band)
//
//	e = [0.5, 0.5], o = [0.25, 0.75]
//	  -0.25 * ln(0.5) = -0.25 * -0.6931471805599453 = 0.17328679513998632
//	   0.25 * ln(1.5) =  0.25 *  0.4054651081081644 = 0.1013662770270411
//	  PSI = 0.27465307216702742                                   (alert band)
func TestPSIHandComputed(t *testing.T) {
	tests := []struct {
		name             string
		expected, actual []float64
		want             float64
		wantStatus       DriftStatus
	}{
		{"identical", []float64{0.5, 0.5}, []float64{0.5, 0.5}, 0, DriftStable},
		{"small shift", []float64{0.5, 0.5}, []float64{0.4, 0.6}, 0.04054651081081644, DriftStable},
		{"moderate shift", []float64{0.5, 0.5}, []float64{0.3, 0.7}, 0.16945957207744072, DriftWarn},
		{"large shift", []float64{0.5, 0.5}, []float64{0.25, 0.75}, 0.27465307216702742, DriftAlert},
		// Counts normalise to the same proportions, so the PSI is identical.
		{"counts equal proportions", []float64{50, 50}, []float64{25, 75}, 0.27465307216702742, DriftAlert},
		{"mismatched lengths", []float64{0.5, 0.5}, []float64{1}, 0, DriftStable},
		{"empty", nil, nil, 0, DriftStable},
		{"all zero", []float64{0, 0}, []float64{0, 0}, 0, DriftStable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PSI(tt.expected, tt.actual)
			if math.Abs(got-tt.want) > tol {
				t.Fatalf("PSI = %.17g, want %.17g", got, tt.want)
			}
			if s := psiStatus(round4(got)); s != tt.wantStatus {
				t.Fatalf("status = %s, want %s", s, tt.wantStatus)
			}
		})
	}
}

// TestPSIEmptyBucketIsFloored checks that a bucket which was populated and is
// now empty produces a large PSI rather than being skipped. With the 1e-6 floor
// and e = [0.5, 0.5], o = [1, 0]:
//
//	(1 - 0.5) * ln(1/0.5)          = 0.5 * 0.6931471805599453  = 0.34657359027997264
//	(1e-6 - 0.5) * ln(1e-6 / 0.5)  = -0.499999 * -13.122363377404328 ≈ 6.5611685...
//
// The exact value is not the point; the point is that it is far above the alert
// threshold, which is the behaviour a vanished tool must produce.
func TestPSIEmptyBucketIsFloored(t *testing.T) {
	got := PSI([]float64{0.5, 0.5}, []float64{1, 0})
	if got < PSIAlert {
		t.Fatalf("PSI = %v, want a value above the alert threshold %v", got, PSIAlert)
	}
	want := 0.5*math.Log(2) + (1e-6-0.5)*math.Log(1e-6/0.5)
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("PSI = %.17g, want %.17g", got, want)
	}
}

// TestKSHandComputed checks the two-sample Kolmogorov-Smirnov statistic against
// empirical distribution functions worked out by hand.
//
//	a = [1,2,3,4]  F1(1)=.25 F1(2)=.50 F1(3)=.75 F1(4)=1.00
//	b = [3,4,5,6]  F2(1)=0   F2(2)=0   F2(3)=.25 F2(4)=.50 F2(5)=.75 F2(6)=1.00
//	|F1-F2| at x=1 is .25, at x=2 is .50, at x=3 is .50, at x=4 is .50
//	D = 0.5
//
// Critical value at alpha = 0.05 for n = m = 4:
//
//	1.36 * sqrt((4+4)/(4*4)) = 1.36 * sqrt(0.5) = 1.36 * 0.7071067811865476 = 0.9616652224137048
//
// D = 0.5 < 0.9617, so four runs against four runs cannot demonstrate a shift —
// which is the test correctly refusing to call noise a change.
func TestKSHandComputed(t *testing.T) {
	tests := []struct {
		name     string
		a, b     []float64
		wantD    float64
		wantCrit float64
		wantSig  bool
	}{
		{"identical", []float64{1, 2, 3, 4}, []float64{1, 2, 3, 4}, 0, 0.9616652224137048, false},
		{"shifted by two", []float64{1, 2, 3, 4}, []float64{3, 4, 5, 6}, 0.5, 0.9616652224137048, false},
		// Fully disjoint: D is 1 by definition, but with two observations a side
		// the critical value is 1.36, so the test still does not reject.
		{"disjoint tiny samples", []float64{1, 2}, []float64{3, 4}, 1, 1.36, false},
		// The same separation with twenty observations a side does reject:
		// 1.36 * sqrt(40/400) = 1.36 * 0.31622776601683794 = 0.4300697617828996.
		{"disjoint larger samples", repeatSeq(1, 20), repeatSeq(100, 20), 1, 0.4300697617828996, true},
		{"empty baseline", nil, []float64{1}, 0, math.Inf(1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := KS(tt.a, tt.b)
			if math.Abs(d-tt.wantD) > tol {
				t.Fatalf("KS = %.17g, want %.17g", d, tt.wantD)
			}
			crit := KSCritical(len(tt.a), len(tt.b))
			if math.IsInf(tt.wantCrit, 1) {
				if !math.IsInf(crit, 1) {
					t.Fatalf("KSCritical = %v, want +Inf", crit)
				}
			} else if math.Abs(crit-tt.wantCrit) > tol {
				t.Fatalf("KSCritical = %.17g, want %.17g", crit, tt.wantCrit)
			}
			if got := d > crit; got != tt.wantSig {
				t.Fatalf("significant = %v, want %v (D=%v crit=%v)", got, tt.wantSig, d, crit)
			}
		})
	}
}

func repeatSeq(start float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = start + float64(i)
	}
	return out
}

func TestQuantile(t *testing.T) {
	sorted := []float64{1, 2, 3, 4, 5}
	tests := []struct {
		q, want float64
	}{
		{0, 1}, {0.5, 3}, {1, 5}, {0.25, 2}, {0.75, 4},
		// Interpolated: pos = 0.1 * 4 = 0.4, so 1*(0.6) + 2*(0.4) = 1.4.
		{0.1, 1.4},
	}
	for _, tt := range tests {
		if got := Quantile(sorted, tt.q); math.Abs(got-tt.want) > tol {
			t.Fatalf("Quantile(%v) = %v, want %v", tt.q, got, tt.want)
		}
	}
	if got := Quantile(nil, 0.5); got != 0 {
		t.Fatalf("Quantile of an empty sample = %v, want 0", got)
	}
}

func TestQuantileEdgesCollapseDuplicates(t *testing.T) {
	// A degenerate baseline where every observation is the same value yields a
	// single edge rather than nine identical ones.
	edges := QuantileEdges([]float64{2, 2, 2, 2}, 10)
	if len(edges) != 1 || edges[0] != 2 {
		t.Fatalf("QuantileEdges = %v, want [2]", edges)
	}
	if got := QuantileEdges(nil, 10); got != nil {
		t.Fatalf("QuantileEdges(nil) = %v, want nil", got)
	}
	if got := QuantileEdges([]float64{1, 2}, 1); got != nil {
		t.Fatalf("QuantileEdges with one bin = %v, want nil", got)
	}
}

// TestContinuousDriftKSBacksUpPSI covers the case quantile binning cannot see:
// a baseline with one distinct value collapses to a single edge, both samples
// land in the same bucket, and the PSI is 0 even though the population moved.
// The KS test is what catches it, and the feature is reported as a warning.
func TestContinuousDriftKSBacksUpPSI(t *testing.T) {
	base := []float64{2, 2, 2, 2}
	obs := []float64{3, 3, 3, 3}
	f := continuousDrift(FeatureSteps, base, obs)
	if f.PSI != 0 {
		t.Fatalf("PSI = %v, want 0 for a collapsed binning", f.PSI)
	}
	if f.KS != 1 {
		t.Fatalf("KS = %v, want 1", f.KS)
	}
	if !f.KSSignificant {
		t.Fatalf("KS %v against critical %v should be significant", f.KS, f.KSCritical)
	}
	if f.Status != DriftWarn {
		t.Fatalf("status = %s, want %s", f.Status, DriftWarn)
	}
}

func TestCompareDetectsAToolMixShift(t *testing.T) {
	clock := obs.NewManualClock(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	base := Window{
		Runs:       100,
		ToolCounts: map[string]int{"get_exposure": 50, "get_position": 50},
		Steps:      repeatSeq(1, 100),
		LatencyMS:  repeatSeq(1, 100),
		Approvals:  10,
	}
	b := NewBaseline("collateral-ops-agent", "2026.09.1", base, clock)

	shifted := Window{
		Runs:       100,
		ToolCounts: map[string]int{"get_exposure": 25, "get_position": 75},
		Steps:      repeatSeq(1, 100),
		LatencyMS:  repeatSeq(1, 100),
		Approvals:  10,
	}
	rep := Compare(b, shifted)
	if rep.Status != DriftAlert {
		t.Fatalf("status = %s, want %s\n%s", rep.Status, DriftAlert, rep.TextSummary())
	}
	var mix FeatureDrift
	for _, f := range rep.Features {
		if f.Name == FeatureToolMix {
			mix = f
		}
	}
	// Same 50/50 -> 25/75 split as the hand-computed PSI above.
	if math.Abs(mix.PSI-round4(0.27465307216702742)) > tol {
		t.Fatalf("tool mix PSI = %v, want %v", mix.PSI, round4(0.27465307216702742))
	}
	if len(rep.Alerts) == 0 {
		t.Fatal("an alerting report must carry an alert string")
	}
	if b.CapturedAt != clock.Now() {
		t.Fatal("NewBaseline must stamp the baseline from the supplied clock")
	}
}

func TestCompareIsStableWhenNothingMoved(t *testing.T) {
	w := Window{
		Runs:       50,
		ToolCounts: map[string]int{"get_exposure": 30, "get_position": 20},
		Steps:      repeatSeq(1, 50),
		LatencyMS:  repeatSeq(5, 50),
		GuardTrips: 5, PolicyDenials: 4, Approvals: 3, Refusals: 2,
	}
	rep := Compare(NewBaseline("s", "1", w, obs.NewManualClock(time.Unix(0, 0))), w)
	if rep.Status != DriftStable {
		t.Fatalf("status = %s, want stable\n%s", rep.Status, rep.TextSummary())
	}
	for _, f := range rep.Features {
		if f.Name == FeatureAnswerLength {
			if f.Status != DriftNoData {
				t.Fatalf("answer_length status = %s, want %s when no answers were captured", f.Status, DriftNoData)
			}
		} else if f.PSI != 0 {
			t.Fatalf("%s PSI = %v, want 0 for identical windows", f.Name, f.PSI)
		}
	}
}

func TestBaselineJSONRoundTrip(t *testing.T) {
	w := Window{
		Runs:       3,
		ToolCounts: map[string]int{"get_exposure": 3},
		Steps:      []float64{2, 2, 3},
		LatencyMS:  []float64{1.5, 2, 2.5},
		Approvals:  1,
	}
	b := NewBaseline("collateral-ops-agent", "2026.09.1", w, obs.NewManualClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)))
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Baseline
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Suite != b.Suite || back.Version != b.Version || !back.CapturedAt.Equal(b.CapturedAt) {
		t.Fatalf("baseline metadata did not survive: %+v", back)
	}
	if back.Window.Runs != w.Runs || back.Window.ToolCounts["get_exposure"] != 3 {
		t.Fatalf("baseline window did not survive: %+v", back.Window)
	}
	again, err := json.Marshal(back)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(again) != string(raw) {
		t.Fatal("a baseline must serialise identically on a second pass")
	}
}

func TestFromRuns(t *testing.T) {
	runs := []*agent.Run{
		{
			Status: agent.StatusCompleted, Answer: "exposure is 1,000,000",
			Steps: []agent.StepRecord{
				{Tool: "get_exposure", Outcome: agent.OutcomeInvoked, DurationMS: 4},
				{Outcome: agent.OutcomeAnswered, DurationMS: 1},
			},
		},
		{
			Status: agent.StatusCompleted, Answer: "I could not complete that action.", ApprovalIDs: []string{"apr_1"},
			Steps: []agent.StepRecord{
				{Tool: "execute_transfer", Outcome: agent.OutcomeDenied, DurationMS: 2},
				{Outcome: agent.OutcomeAnswered, DurationMS: 1, GuardVerdict: "flag"},
			},
		},
		nil,
	}
	w := FromRuns(runs)
	if w.Runs != 2 {
		t.Fatalf("Runs = %d, want 2 (a nil run must be skipped)", w.Runs)
	}
	if w.ToolCounts["get_exposure"] != 1 || w.ToolCounts["execute_transfer"] != 0 {
		t.Fatalf("ToolCounts = %v, want only the invoked tool counted", w.ToolCounts)
	}
	if w.PolicyDenials != 1 || w.Approvals != 1 || w.GuardTrips != 1 || w.Refusals != 1 {
		t.Fatalf("rates = denials %d approvals %d guard %d refusals %d, want 1 each",
			w.PolicyDenials, w.Approvals, w.GuardTrips, w.Refusals)
	}
	if len(w.AnswerLength) != 2 {
		t.Fatalf("AnswerLength has %d entries, want 2", len(w.AnswerLength))
	}
	if w.RefusalRate() != 0.5 {
		t.Fatalf("RefusalRate = %v, want 0.5", w.RefusalRate())
	}
	if got := (Window{}).RefusalRate(); got != 0 {
		t.Fatalf("RefusalRate of an empty window = %v, want 0", got)
	}
}

func TestFromTraces(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	span := func(name, runID string, dur time.Duration, attrs map[string]any, events ...obs.Event) obs.SpanData {
		return obs.SpanData{
			TraceID: "t-" + runID, SpanID: name + runID, Name: name,
			Start: start, End: start.Add(dur), DurationNS: dur.Nanoseconds(),
			Status: obs.StatusOK, Attributes: attrs, Events: events,
		}
	}
	spans := []obs.SpanData{
		span("agent.run", "r1", 20*time.Millisecond, map[string]any{
			obs.AttrRunID: "r1", attrRunStatus: "completed", attrRunTools: "get_exposure",
		}),
		span("agent.step", "r1", 5*time.Millisecond, map[string]any{obs.AttrRunID: "r1"}),
		span("agent.tool.get_exposure", "r1", 4*time.Millisecond, map[string]any{
			obs.AttrRunID: "r1", obs.AttrGenAIToolName: "get_exposure",
		}),
		span("agent.run", "r2", 30*time.Millisecond, map[string]any{
			obs.AttrRunID: "r2", attrRunStatus: "blocked",
		}, obs.Event{Name: "approval.required"}),
		span("agent.step", "r2", 6*time.Millisecond, map[string]any{
			obs.AttrRunID: "r2", obs.AttrGuardVerdict: "block", obs.AttrPolicyDecision: "deny",
		}),
		// A span with no run id and no recognisable name must not invent a run.
		{TraceID: "unrelated", SpanID: "x", Name: "http.request", Start: start, End: start},
	}

	w := FromTraces(spans)
	if w.Runs != 2 {
		t.Fatalf("Runs = %d, want 2", w.Runs)
	}
	if w.ToolCounts["get_exposure"] != 1 {
		t.Fatalf("ToolCounts = %v, want get_exposure once", w.ToolCounts)
	}
	if w.GuardTrips != 1 || w.PolicyDenials != 1 || w.Approvals != 1 || w.Refusals != 1 {
		t.Fatalf("rates = guard %d denials %d approvals %d refusals %d, want 1 each",
			w.GuardTrips, w.PolicyDenials, w.Approvals, w.Refusals)
	}
	if len(w.LatencyMS) != 2 || w.LatencyMS[0] != 20 || w.LatencyMS[1] != 30 {
		t.Fatalf("LatencyMS = %v, want [20 30]", w.LatencyMS)
	}
	if len(w.AnswerLength) != 0 {
		t.Fatal("spans carry no answer text, so AnswerLength must stay empty")
	}
}

func TestFromTracesToleratesMissingAttributes(t *testing.T) {
	// Every attribute absent: the extractor must still produce a usable window
	// rather than panicking or reporting a shift it cannot see.
	spans := []obs.SpanData{
		{TraceID: "t1", SpanID: "s1", Name: "agent.run"},
		{TraceID: "t1", SpanID: "s2", Name: "agent.step"},
		{TraceID: "t1", SpanID: "s3", Name: "agent.tool.get_exposure"},
	}
	w := FromTraces(spans)
	if w.Runs != 1 {
		t.Fatalf("Runs = %d, want 1", w.Runs)
	}
	if len(w.ToolCounts) != 0 {
		t.Fatalf("ToolCounts = %v, want empty when no tool-name attribute is present", w.ToolCounts)
	}
	if w.Steps[0] != 1 {
		t.Fatalf("Steps = %v, want [1]", w.Steps)
	}
	rep := Compare(NewBaseline("s", "1", Window{}, obs.NewManualClock(time.Unix(0, 0))), w)
	if len(rep.Features) == 0 {
		t.Fatal("Compare must still emit features for an empty baseline")
	}
}

func TestCompareIsDeterministic(t *testing.T) {
	base := Window{Runs: 20, ToolCounts: map[string]int{"a": 10, "b": 10}, Steps: repeatSeq(1, 20), LatencyMS: repeatSeq(1, 20)}
	cur := Window{Runs: 20, ToolCounts: map[string]int{"a": 5, "b": 15}, Steps: repeatSeq(2, 20), LatencyMS: repeatSeq(3, 20)}
	b := NewBaseline("s", "1", base, obs.NewManualClock(time.Unix(0, 0)))

	first, err := json.Marshal(Compare(b, cur))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for i := 0; i < 20; i++ {
		got, err := json.Marshal(Compare(b, cur))
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(got) != string(first) {
			t.Fatalf("iteration %d produced a different report", i)
		}
	}
}
