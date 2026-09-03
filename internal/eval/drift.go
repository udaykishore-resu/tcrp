package eval

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/agent"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Span attribute keys the runtime writes that are not obs.Attr* constants. They
// are read here defensively: the drift detector is downstream of the runtime
// and must not break when an attribute is renamed or absent, so every read has
// a zero-valued fallback and a missing feature is reported as "no data" rather
// than as a shift.
const (
	attrRunStatus = "tcrp.run.status"
	attrRunSteps  = "tcrp.run.steps"
	attrRunTools  = "tcrp.run.tools"
)

// Feature names. They are constants because a baseline stored by CI is keyed by
// them and a typo would silently compare a feature against nothing.
const (
	FeatureToolMix      = "tool_mix"
	FeatureSteps        = "steps_per_run"
	FeatureGuardTrips   = "guardrail_trip_rate"
	FeatureDenials      = "policy_denial_rate"
	FeatureApprovals    = "approval_rate"
	FeatureRefusals     = "refusal_rate"
	FeatureLatency      = "latency_ms"
	FeatureAnswerLength = "answer_length"
)

// PSI thresholds. These are the conventional retail-credit cutoffs and they are
// used here unchanged, because an invented threshold is one more number nobody
// can calibrate: below 0.1 the population has not moved, 0.1 to 0.25 is worth a
// look, above 0.25 is a different population.
const (
	PSIWarn  = 0.10
	PSIAlert = 0.25
)

// ksAlpha05 is the Kolmogorov-Smirnov coefficient for a 5% two-sided test. The
// critical value is c(alpha) * sqrt((n+m)/(n*m)).
const ksAlpha05 = 1.36

// DriftStatus is a feature's verdict.
type DriftStatus string

// Drift statuses.
const (
	DriftStable DriftStatus = "stable"
	DriftWarn   DriftStatus = "warn"
	DriftAlert  DriftStatus = "alert"
	DriftNoData DriftStatus = "no_data"
)

func (d DriftStatus) rank() int {
	switch d {
	case DriftAlert:
		return 3
	case DriftWarn:
		return 2
	case DriftStable:
		return 1
	default:
		return 0
	}
}

// Window is a population of agent runs reduced to the features the detector
// compares. Both the baseline and the production window are Windows, so the
// same extraction code produces both and a difference between them cannot come
// from the extractor.
type Window struct {
	// Runs is how many runs the window covers.
	Runs int `json:"runs"`
	// ToolCounts is the raw tool-invocation count per tool name.
	ToolCounts map[string]int `json:"tool_counts,omitempty"`
	// Steps holds one steps-per-run observation per run.
	Steps []float64 `json:"steps,omitempty"`
	// LatencyMS holds one end-to-end duration per run.
	LatencyMS []float64 `json:"latency_ms,omitempty"`
	// AnswerLength holds one answer length in characters per run. It is empty
	// when the window came from spans, which do not carry answer text.
	AnswerLength []float64 `json:"answer_length,omitempty"`
	// GuardTrips, PolicyDenials, Approvals and Refusals count runs, not events:
	// a run that trips the guardrail four times is one tripped run, because the
	// rate being tracked is "how often does this happen to a request".
	GuardTrips    int `json:"guard_trips"`
	PolicyDenials int `json:"policy_denials"`
	Approvals     int `json:"approvals"`
	Refusals      int `json:"refusals"`
}

// ToolMix returns the tool-invocation distribution, normalised to sum to one.
func (w Window) ToolMix() map[string]float64 {
	total := 0
	for _, n := range w.ToolCounts {
		total += n
	}
	out := make(map[string]float64, len(w.ToolCounts))
	if total == 0 {
		return out
	}
	for k, n := range w.ToolCounts {
		out[k] = float64(n) / float64(total)
	}
	return out
}

func (w Window) rate(n int) float64 {
	if w.Runs == 0 {
		return 0
	}
	return float64(n) / float64(w.Runs)
}

// GuardTripRate is the fraction of runs in which a guardrail fired.
func (w Window) GuardTripRate() float64 { return w.rate(w.GuardTrips) }

// PolicyDenialRate is the fraction of runs in which the gateway refused a call.
func (w Window) PolicyDenialRate() float64 { return w.rate(w.PolicyDenials) }

// ApprovalRate is the fraction of runs that opened a human approval gate.
func (w Window) ApprovalRate() float64 { return w.rate(w.Approvals) }

// RefusalRate is the fraction of runs that ended by declining to act.
func (w Window) RefusalRate() float64 { return w.rate(w.Refusals) }

// Baseline is a Window plus the provenance CI needs to store it and to know
// what it is comparing against later.
type Baseline struct {
	Suite      string    `json:"suite"`
	Version    string    `json:"version"`
	CapturedAt time.Time `json:"captured_at"`
	Window     Window    `json:"window"`
}

// NewBaseline captures a window as a baseline, stamped from the given clock.
func NewBaseline(suite, version string, w Window, clock obs.Clock) Baseline {
	if clock == nil {
		clock = obs.RealClock{}
	}
	return Baseline{Suite: suite, Version: version, CapturedAt: clock.Now(), Window: w}
}

// FromRuns builds a window from completed runs. This is the baseline path: the
// benchmark holds the runs themselves, so it can see the answer text that spans
// do not carry.
func FromRuns(runs []*agent.Run) Window {
	w := Window{ToolCounts: map[string]int{}}
	for _, run := range runs {
		if run == nil {
			continue
		}
		w.Runs++
		for _, t := range run.Trajectory() {
			w.ToolCounts[t]++
		}
		w.Steps = append(w.Steps, float64(len(run.Steps)))
		var ms float64
		for _, st := range run.Steps {
			ms += st.DurationMS
		}
		w.LatencyMS = append(w.LatencyMS, ms)
		w.AnswerLength = append(w.AnswerLength, float64(len(run.Answer)))

		guard, denied := false, false
		for _, st := range run.Steps {
			switch st.Outcome {
			case agent.OutcomeDenied:
				denied = true
			case agent.OutcomeBlocked:
				guard = true
			}
			if v := strings.ToLower(st.GuardVerdict); v == "block" || v == "redact" || v == "flag" {
				guard = true
			}
		}
		if guard {
			w.GuardTrips++
		}
		if denied {
			w.PolicyDenials++
		}
		if len(run.ApprovalIDs) > 0 {
			w.Approvals++
		}
		if Refused(run) {
			w.Refusals++
		}
	}
	w.sort()
	return w
}

// FromTraces builds a window from exported spans.
//
// This is the production path: the platform's traces are the only artefact that
// exists for live traffic, and it is deliberately lossy. Answer text is not on a
// span, so the answer-length feature comes back empty and Compare reports it as
// having no data rather than as a shift towards zero. Every attribute read here
// tolerates absence, because a drift detector that crashes on an unfamiliar
// span is a monitor that is off exactly when the system has changed.
func FromTraces(spans []obs.SpanData) Window {
	w := Window{ToolCounts: map[string]int{}}

	type runAgg struct {
		steps     int
		latencyNS int64
		hasRoot   bool
		guard     bool
		denied    bool
		approved  bool
		refused   bool
		toolSpans int
		// fallback is the trajectory the root span records, used only when the
		// run's own tool spans were sampled away.
		fallback []string
	}
	byRun := map[string]*runAgg{}
	order := []string{}
	get := func(key string) *runAgg {
		if key == "" {
			return nil
		}
		a, ok := byRun[key]
		if !ok {
			a = &runAgg{}
			byRun[key] = a
			order = append(order, key)
		}
		return a
	}

	for _, s := range spans {
		key := s.Attr(obs.AttrRunID)
		if key == "" {
			key = s.TraceID
		}
		a := get(key)
		if a == nil {
			continue
		}

		if tool := s.Attr(obs.AttrGenAIToolName); tool != "" && strings.HasPrefix(s.Name, "agent.tool.") {
			a.toolSpans++
			if s.Status != obs.StatusError {
				w.ToolCounts[tool]++
			}
		}
		if s.Name == "agent.step" {
			a.steps++
		}
		switch s.Name {
		case "agent.run", "agent.run.resume":
			a.hasRoot = true
			a.latencyNS += s.DurationNS
			if status := s.Attr(attrRunStatus); status != "" {
				switch agent.RunStatus(status) {
				case agent.StatusBlocked, agent.StatusRejected, agent.StatusFailed:
					a.refused = true
				}
			}
			if n := s.Attr(attrRunSteps); n != "" && a.steps == 0 {
				if v, err := parseFloat(n); err == nil {
					a.steps = int(v)
				}
			}
			if tools := s.Attr(attrRunTools); tools != "" {
				// Held for a trace whose tool spans were sampled away: the root
				// span carries the trajectory as a comma-separated list. It is
				// applied only if this run produced no tool spans of its own,
				// so a sampled trace is still counted and a complete one is not
				// counted twice.
				for _, t := range strings.Split(tools, ",") {
					if t = strings.TrimSpace(t); t != "" {
						a.fallback = append(a.fallback, t)
					}
				}
			}
		}
		switch strings.ToLower(s.Attr(obs.AttrGuardVerdict)) {
		case "block", "redact", "flag":
			a.guard = true
		}
		if strings.EqualFold(s.Attr(obs.AttrPolicyDecision), "deny") {
			a.denied = true
		}
		if s.Attr(obs.AttrApprovalID) != "" {
			a.approved = true
		}
		for _, ev := range s.Events {
			switch ev.Name {
			case "guard.context_quarantined":
				a.guard = true
			case "approval.required":
				a.approved = true
			}
		}
	}

	sort.Strings(order)
	for _, key := range order {
		a := byRun[key]
		if !a.hasRoot && a.steps == 0 {
			continue // not an agent run; probably an unrelated trace
		}
		w.Runs++
		w.Steps = append(w.Steps, float64(a.steps))
		w.LatencyMS = append(w.LatencyMS, float64(a.latencyNS)/1e6)
		if a.toolSpans == 0 {
			for _, t := range a.fallback {
				w.ToolCounts[t]++
			}
		}
		if a.guard {
			w.GuardTrips++
		}
		if a.denied {
			w.PolicyDenials++
		}
		if a.approved {
			w.Approvals++
		}
		if a.refused {
			w.Refusals++
		}
	}
	w.sort()
	return w
}

func (w *Window) sort() {
	sort.Float64s(w.Steps)
	sort.Float64s(w.LatencyMS)
	sort.Float64s(w.AnswerLength)
}

func parseFloat(s string) (float64, error) {
	var v float64
	_, err := fmt.Sscanf(s, "%g", &v)
	return v, err
}

// Summary is a feature's shape on one side of the comparison.
type Summary struct {
	N    int     `json:"n"`
	Mean float64 `json:"mean,omitempty"`
	P50  float64 `json:"p50,omitempty"`
	P95  float64 `json:"p95,omitempty"`
	// Buckets is the normalised distribution the PSI was computed over, so a
	// report can show which bucket moved rather than only that something did.
	Buckets map[string]float64 `json:"buckets,omitempty"`
}

// FeatureDrift is one feature's comparison.
type FeatureDrift struct {
	Name string `json:"name"`
	// PSI is the population stability index over the feature's buckets.
	PSI float64 `json:"psi"`
	// KS is the two-sample Kolmogorov-Smirnov statistic, set only for the
	// continuous features. It is zero for categorical and rate features.
	KS float64 `json:"ks"`
	// KSCritical is the 5% critical value for the two sample sizes. KS above
	// it is a distribution change rather than sampling noise.
	KSCritical float64 `json:"ks_critical,omitempty"`
	// KSSignificant is KS > KSCritical.
	KSSignificant bool        `json:"ks_significant,omitempty"`
	Baseline      Summary     `json:"baseline"`
	Observed      Summary     `json:"observed"`
	Status        DriftStatus `json:"status"`
	Note          string      `json:"note,omitempty"`
}

// DriftReport is the detector's output.
type DriftReport struct {
	Suite      string         `json:"suite"`
	Version    string         `json:"version"`
	BaselineAt time.Time      `json:"baseline_at"`
	Features   []FeatureDrift `json:"features"`
	Status     DriftStatus    `json:"status"`
	Alerts     []string       `json:"alerts,omitempty"`
}

// Compare measures the observed window against the baseline.
//
// Features are emitted in a fixed order and every number is rounded, so the
// report is byte-stable for a fixed pair of windows. A feature with no data on
// either side is reported as no_data rather than dropped: a monitor that
// silently stops watching a feature is worse than one that says it has stopped.
func Compare(b Baseline, observed Window) DriftReport {
	rep := DriftReport{Suite: b.Suite, Version: b.Version, BaselineAt: b.CapturedAt}

	rep.Features = append(rep.Features, categoricalDrift(FeatureToolMix, b.Window.ToolMix(), observed.ToolMix()))
	rep.Features = append(rep.Features, continuousDrift(FeatureSteps, b.Window.Steps, observed.Steps))
	rep.Features = append(rep.Features, rateDrift(FeatureGuardTrips, b.Window, observed, Window.GuardTripRate))
	rep.Features = append(rep.Features, rateDrift(FeatureDenials, b.Window, observed, Window.PolicyDenialRate))
	rep.Features = append(rep.Features, rateDrift(FeatureApprovals, b.Window, observed, Window.ApprovalRate))
	rep.Features = append(rep.Features, rateDrift(FeatureRefusals, b.Window, observed, Window.RefusalRate))
	rep.Features = append(rep.Features, continuousDrift(FeatureLatency, b.Window.LatencyMS, observed.LatencyMS))
	rep.Features = append(rep.Features, continuousDrift(FeatureAnswerLength, b.Window.AnswerLength, observed.AnswerLength))

	rep.Status = DriftStable
	for _, f := range rep.Features {
		if f.Status.rank() > rep.Status.rank() {
			rep.Status = f.Status
		}
		switch f.Status {
		case DriftAlert:
			rep.Alerts = append(rep.Alerts, fmt.Sprintf("%s: PSI %.4f above the %.2f alert threshold", f.Name, f.PSI, PSIAlert))
		case DriftWarn:
			rep.Alerts = append(rep.Alerts, fmt.Sprintf("%s: PSI %.4f above the %.2f warning threshold", f.Name, f.PSI, PSIWarn))
		case DriftNoData:
			rep.Alerts = append(rep.Alerts, fmt.Sprintf("%s: %s", f.Name, f.Note))
		}
		if f.KSSignificant {
			rep.Alerts = append(rep.Alerts, fmt.Sprintf("%s: KS %.4f above the alpha=0.05 critical value %.4f", f.Name, f.KS, f.KSCritical))
		}
	}
	// DriftNoData ranks below DriftStable, so a feature nobody could measure
	// never sets the report's status. It still produces an alert line, because
	// a feature that has stopped being observable is worth saying out loud
	// without being worth failing a deploy over.
	return rep
}

// categoricalDrift computes PSI over a shared key set. Both distributions are
// re-normalised over the union of keys, so a tool that appears only in
// production is a bucket the baseline holds at zero rather than a key that is
// quietly ignored — which is the case worth alerting on.
func categoricalDrift(name string, base, cur map[string]float64) FeatureDrift {
	f := FeatureDrift{Name: name}
	if len(base) == 0 && len(cur) == 0 {
		f.Status = DriftNoData
		f.Note = "no observations on either side"
		return f
	}
	keys := map[string]bool{}
	for k := range base {
		keys[k] = true
	}
	for k := range cur {
		keys[k] = true
	}
	ordered := sortedKeys(keys)

	e := make([]float64, len(ordered))
	o := make([]float64, len(ordered))
	f.Baseline.Buckets = map[string]float64{}
	f.Observed.Buckets = map[string]float64{}
	for i, k := range ordered {
		e[i], o[i] = base[k], cur[k]
		f.Baseline.Buckets[k] = round4(base[k])
		f.Observed.Buckets[k] = round4(cur[k])
	}
	f.Baseline.N, f.Observed.N = len(base), len(cur)
	f.PSI = round4(PSI(e, o))
	f.Status = psiStatus(f.PSI)
	if len(base) == 0 || len(cur) == 0 {
		f.Status = DriftNoData
		f.Note = "one side has no observations"
	}
	return f
}

// rateDrift treats a per-run rate as a two-bucket distribution: happened, and
// did not. PSI over two buckets is a blunt instrument, but it is the same
// instrument as the other features, which is what makes the thresholds mean the
// same thing across the report.
func rateDrift(name string, base, cur Window, rate func(Window) float64) FeatureDrift {
	f := FeatureDrift{Name: name}
	if base.Runs == 0 || cur.Runs == 0 {
		f.Status = DriftNoData
		f.Note = "one side has no runs"
		f.Baseline.N, f.Observed.N = base.Runs, cur.Runs
		return f
	}
	br, or := rate(base), rate(cur)
	f.Baseline = Summary{N: base.Runs, Mean: round4(br), Buckets: map[string]float64{"yes": round4(br), "no": round4(1 - br)}}
	f.Observed = Summary{N: cur.Runs, Mean: round4(or), Buckets: map[string]float64{"yes": round4(or), "no": round4(1 - or)}}
	f.PSI = round4(PSI([]float64{br, 1 - br}, []float64{or, 1 - or}))
	f.Status = psiStatus(f.PSI)
	return f
}

// continuousDrift bins the observed sample using the baseline's own quantile
// edges. Binning on the baseline is what makes PSI comparable over time: fixed
// edges chosen once mean the same bucket always means the same range.
func continuousDrift(name string, base, cur []float64) FeatureDrift {
	f := FeatureDrift{Name: name}
	if len(base) == 0 || len(cur) == 0 {
		f.Status = DriftNoData
		f.Note = "one side has no observations"
		f.Baseline = summarise(base, nil)
		f.Observed = summarise(cur, nil)
		return f
	}
	edges := QuantileEdges(base, 10)
	be := histogram(base, edges)
	oe := histogram(cur, edges)
	f.Baseline = summarise(base, bucketMap(be, edges))
	f.Observed = summarise(cur, bucketMap(oe, edges))
	f.PSI = round4(PSI(be, oe))
	f.KS = round4(KS(base, cur))
	f.KSCritical = round4(KSCritical(len(base), len(cur)))
	f.KSSignificant = f.KS > f.KSCritical
	f.Status = psiStatus(f.PSI)
	if f.KSSignificant && f.Status == DriftStable {
		f.Status = DriftWarn
		f.Note = "PSI is stable but the KS test rejects the null at alpha=0.05"
	}
	return f
}

func psiStatus(psi float64) DriftStatus {
	switch {
	case psi >= PSIAlert:
		return DriftAlert
	case psi >= PSIWarn:
		return DriftWarn
	default:
		return DriftStable
	}
}

func summarise(v []float64, buckets map[string]float64) Summary {
	s := Summary{N: len(v), Buckets: buckets}
	if len(v) == 0 {
		return s
	}
	sorted := append([]float64(nil), v...)
	sort.Float64s(sorted)
	var sum float64
	for _, x := range sorted {
		sum += x
	}
	s.Mean = round4(sum / float64(len(sorted)))
	s.P50 = round4(Quantile(sorted, 0.50))
	s.P95 = round4(Quantile(sorted, 0.95))
	return s
}

func bucketMap(props []float64, edges []float64) map[string]float64 {
	out := make(map[string]float64, len(props))
	for i, p := range props {
		lo := "-inf"
		hi := "+inf"
		if i > 0 {
			lo = fmt.Sprintf("%g", edges[i-1])
		}
		if i < len(edges) {
			hi = fmt.Sprintf("%g", edges[i])
		}
		out[lo+".."+hi] = round4(p)
	}
	return out
}

// PSI returns the population stability index between an expected and an
// observed discrete distribution.
//
//	PSI = sum over buckets of (o - e) * ln(o / e)
//
// Empty buckets are floored at a small epsilon rather than skipped: a bucket
// that was populated and is now empty is precisely the signal, and dropping it
// would report stability. The two inputs must be the same length and are
// re-normalised, so counts and proportions both work.
func PSI(expected, observed []float64) float64 {
	if len(expected) != len(observed) || len(expected) == 0 {
		return 0
	}
	const eps = 1e-6
	e := normalise(expected)
	o := normalise(observed)
	var psi float64
	for i := range e {
		ei, oi := e[i], o[i]
		if ei < eps {
			ei = eps
		}
		if oi < eps {
			oi = eps
		}
		psi += (oi - ei) * math.Log(oi/ei)
	}
	if math.IsNaN(psi) || math.IsInf(psi, 0) {
		return 0
	}
	return psi
}

func normalise(v []float64) []float64 {
	var sum float64
	for _, x := range v {
		if x > 0 {
			sum += x
		}
	}
	out := make([]float64, len(v))
	if sum <= 0 {
		return out
	}
	for i, x := range v {
		if x > 0 {
			out[i] = x / sum
		}
	}
	return out
}

// QuantileEdges returns up to bins-1 interior edges taken at equal quantiles of
// the sample. Duplicate edges are collapsed, so a sample with few distinct
// values yields fewer, wider buckets rather than empty ones that would inflate
// the PSI.
func QuantileEdges(sample []float64, bins int) []float64 {
	if len(sample) == 0 || bins < 2 {
		return nil
	}
	sorted := append([]float64(nil), sample...)
	sort.Float64s(sorted)
	var edges []float64
	for i := 1; i < bins; i++ {
		e := Quantile(sorted, float64(i)/float64(bins))
		if len(edges) > 0 && e <= edges[len(edges)-1] {
			continue
		}
		edges = append(edges, e)
	}
	return edges
}

// Quantile returns the q-quantile of a sorted sample by linear interpolation.
func Quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// histogram returns the proportion of the sample falling in each bucket defined
// by edges. A value equal to an edge falls in the lower bucket.
func histogram(sample, edges []float64) []float64 {
	counts := make([]float64, len(edges)+1)
	for _, v := range sample {
		idx := sort.SearchFloat64s(edges, v)
		if idx < len(edges) && edges[idx] == v {
			// A value exactly on an edge belongs to the bucket that ends there.
			// Anything else puts every tied value in the upper bucket and makes
			// a discrete feature such as steps-per-run look like it moved.
			idx++
		}
		if idx >= len(counts) {
			idx = len(counts) - 1
		}
		counts[idx]++
	}
	return normalise(counts)
}

// KS returns the two-sample Kolmogorov-Smirnov statistic: the largest absolute
// difference between the two empirical distribution functions.
func KS(a, b []float64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	x := append([]float64(nil), a...)
	y := append([]float64(nil), b...)
	sort.Float64s(x)
	sort.Float64s(y)

	var i, j int
	var d float64
	for i < len(x) && j < len(y) {
		var v float64
		switch {
		case x[i] <= y[j]:
			v = x[i]
		default:
			v = y[j]
		}
		for i < len(x) && x[i] <= v {
			i++
		}
		for j < len(y) && y[j] <= v {
			j++
		}
		diff := math.Abs(float64(i)/float64(len(x)) - float64(j)/float64(len(y)))
		if diff > d {
			d = diff
		}
	}
	return d
}

// KSCritical returns the two-sample critical value at alpha = 0.05:
//
//	1.36 * sqrt((n + m) / (n * m))
//
// Small samples give a critical value above 1, which is unreachable — that is
// the test correctly saying twelve runs cannot demonstrate a distribution
// change, and it is why the PSI thresholds carry the report on thin windows.
func KSCritical(n, m int) float64 {
	if n <= 0 || m <= 0 {
		return math.Inf(1)
	}
	return ksAlpha05 * math.Sqrt(float64(n+m)/(float64(n)*float64(m)))
}

// TextSummary renders the drift report as an aligned table.
func (d DriftReport) TextSummary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "drift %s %s vs baseline captured %s: %s\n\n",
		d.Suite, d.Version, d.BaselineAt.UTC().Format(time.RFC3339), d.Status)
	fmt.Fprintf(&b, "%-22s %10s %10s %10s  %s\n", "FEATURE", "PSI", "KS", "KS_CRIT", "STATUS")
	for _, f := range d.Features {
		fmt.Fprintf(&b, "%-22s %10.4f %10.4f %10.4f  %s\n", f.Name, f.PSI, f.KS, f.KSCritical, f.Status)
	}
	if len(d.Alerts) > 0 {
		b.WriteString("\nalerts:\n")
		for _, a := range d.Alerts {
			fmt.Fprintf(&b, "  %s\n", a)
		}
	}
	return b.String()
}
