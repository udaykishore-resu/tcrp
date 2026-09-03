package guard

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Surface names what is being inspected.
type Surface string

// Inspection surfaces.
const (
	SurfacePrompt  Surface = "prompt"
	SurfaceContext Surface = "context"
	SurfaceOutput  Surface = "output"
)

// Verdict is the pipeline's decision.
type Verdict string

// Verdicts.
const (
	VerdictAllow  Verdict = "allow"
	VerdictRedact Verdict = "redact"
	VerdictFlag   Verdict = "flag"
	VerdictBlock  Verdict = "block"
)

// Blocking reports whether the verdict stops the turn.
func (v Verdict) Blocking() bool { return v == VerdictBlock }

// Tier names the risk tier of the tool the turn is heading towards. It is a
// string here rather than an imported enum so the guardrail service has no
// dependency on the agent runtime and can be deployed on its own.
const (
	TierReadOnly    = "read_only"
	TierStateChange = "state_change"
	TierMoneyMove   = "money_movement"
)

// Request is one inspection.
type Request struct {
	Surface  Surface  `json:"surface"`
	Text     string   `json:"text"`
	Tier     string   `json:"tier"`
	RunID    string   `json:"run_id,omitempty"`
	Step     int      `json:"step,omitempty"`
	Subject  string   `json:"subject,omitempty"`
	Tenant   string   `json:"tenant,omitempty"`
	Grounded []string `json:"grounded,omitempty"`
	Source   string   `json:"source,omitempty"` // provenance of retrieved context
}

// StageResult records one detector's contribution and its cost.
type StageResult struct {
	Stage    string        `json:"stage"`
	Findings int           `json:"findings"`
	MaxSev   string        `json:"max_severity"`
	Duration time.Duration `json:"duration_ns"`
	Skipped  bool          `json:"skipped,omitempty"`
	Error    string        `json:"error,omitempty"`
}

// Result is the pipeline's output.
type Result struct {
	Verdict          Verdict           `json:"verdict"`
	Reason           string            `json:"reason,omitempty"`
	Findings         []Finding         `json:"findings,omitempty"`
	Redacted         string            `json:"redacted,omitempty"`
	Placeholders     map[string]string `json:"placeholders,omitempty"`
	Stages           []StageResult     `json:"stages"`
	Duration         time.Duration     `json:"duration_ns"`
	BudgetExceeded   bool              `json:"budget_exceeded"`
	FailMode         string            `json:"fail_mode,omitempty"`
	RetrievalTrusted bool              `json:"retrieval_trusted"`
	PolicyVersion    string            `json:"policy_version"`
}

// Summary renders a one-line form.
func (r Result) Summary() string {
	return fmt.Sprintf("%s (%d findings, %s)", r.Verdict, len(r.Findings), r.Duration.Round(time.Microsecond))
}

// Config is the guardrail's operating policy.
type Config struct {
	Version string
	// Budget is the wall-clock allowance for the whole pipeline.
	Budget time.Duration
	// StageBudget caps any single detector.
	StageBudget time.Duration
	// BlockAt maps a tier to the severity at which a finding blocks the turn.
	BlockAt map[string]Severity
	// FailOpen maps a tier to whether budget exhaustion allows the turn.
	FailOpen map[string]bool
	// InjectionThreshold is the score above which injection is treated as high.
	InjectionThreshold int
	// RedactOnOutput redacts rather than blocks when PII appears in output.
	RedactOnOutput bool
}

// DefaultConfig is the shipped policy: a 100ms budget, fail-open for read-only
// traffic and fail-closed for anything that changes state or moves money.
//
// The asymmetry is the point. Failing open on a read query costs an unfiltered
// answer; failing open on a wire costs the wire.
func DefaultConfig() Config {
	return Config{
		Version:     "guard-2026.09.1",
		Budget:      100 * time.Millisecond,
		StageBudget: 30 * time.Millisecond,
		BlockAt: map[string]Severity{
			TierReadOnly:    SevCritical,
			TierStateChange: SevHigh,
			TierMoneyMove:   SevMedium,
		},
		FailOpen: map[string]bool{
			TierReadOnly:    true,
			TierStateChange: false,
			TierMoneyMove:   false,
		},
		InjectionThreshold: 50,
		RedactOnOutput:     true,
	}
}

// Classifier is an optional model-backed stage. It runs last, inside whatever
// budget remains, and its unavailability never changes the verdict of the
// deterministic stages that already ran.
type Classifier interface {
	Name() string
	Classify(ctx context.Context, req Request) ([]Finding, error)
}

// Engine runs the detector pipeline.
type Engine struct {
	cfg        Config
	clock      obs.Clock
	tracer     *obs.Tracer
	classifier Classifier

	mu       sync.RWMutex
	detState map[Surface][]Detector

	metrics *guardMetrics
}

type guardMetrics struct {
	inspections *obs.Counter
	blocks      *obs.Counter
	redactions  *obs.Counter
	budgetBust  *obs.Counter
	latency     *obs.Histogram
	stage       map[string]*obs.Histogram
	mu          sync.Mutex
	reg         *obs.Registry
}

// NewEngine builds a guardrail engine.
func NewEngine(cfg Config, clock obs.Clock, tracer *obs.Tracer, reg *obs.Registry) *Engine {
	if clock == nil {
		clock = obs.RealClock{}
	}
	if cfg.Budget == 0 {
		cfg = DefaultConfig()
	}
	e := &Engine{cfg: cfg, clock: clock, tracer: tracer}
	e.detState = map[Surface][]Detector{
		SurfacePrompt: {
			PIIDetector{}, SecretDetector{},
			InjectionDetector{Threshold: cfg.InjectionThreshold},
			DomainDetector{},
		},
		SurfaceContext: {
			SecretDetector{},
			InjectionDetector{Threshold: cfg.InjectionThreshold},
			PIIDetector{},
		},
		SurfaceOutput: {
			PIIDetector{}, SecretDetector{},
		},
	}
	if reg != nil {
		e.metrics = &guardMetrics{
			inspections: reg.Counter("tcrp_guard_inspections_total", "Guardrail inspections performed.", nil),
			blocks:      reg.Counter("tcrp_guard_blocks_total", "Turns blocked by the guardrail.", nil),
			redactions:  reg.Counter("tcrp_guard_redactions_total", "Turns redacted by the guardrail.", nil),
			budgetBust:  reg.Counter("tcrp_guard_budget_exceeded_total", "Inspections that exceeded the latency budget.", nil),
			latency:     reg.Histogram("tcrp_guard_inspection_seconds", "End-to-end guardrail inspection latency.", nil, nil),
			stage:       map[string]*obs.Histogram{},
			reg:         reg,
		}
	}
	return e
}

// SetClassifier installs an optional model-backed stage.
func (e *Engine) SetClassifier(c Classifier) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.classifier = c
}

// Config returns the operating policy.
func (e *Engine) Config() Config { return e.cfg }

// Inspect runs the pipeline for one request.
func (e *Engine) Inspect(ctx context.Context, req Request) Result {
	start := e.clock.Now()
	var sp *obs.Span
	if e.tracer != nil {
		ctx, sp = e.tracer.Start(ctx, "guard.inspect", obs.KindServer)
		sp.SetAttrs(map[string]any{
			obs.AttrGuardStage: string(req.Surface),
			obs.AttrToolTier:   req.Tier,
			obs.AttrRunID:      req.RunID,
			"tcrp.guard.bytes": len(req.Text),
		})
		defer sp.End()
	}

	res := Result{Verdict: VerdictAllow, RetrievalTrusted: true, PolicyVersion: e.cfg.Version}
	budget := e.cfg.Budget
	deadline := start.Add(budget)

	e.mu.RLock()
	dets := append([]Detector(nil), e.detState[req.Surface]...)
	classifier := e.classifier
	e.mu.RUnlock()

	if req.Surface == SurfaceOutput && len(req.Grounded) > 0 {
		dets = append(dets, GroundingDetector{Grounded: req.Grounded, AllowSmall: true})
	}

	for _, d := range dets {
		if e.clock.Now().After(deadline) {
			res.BudgetExceeded = true
			res.Stages = append(res.Stages, StageResult{Stage: d.Name(), Skipped: true})
			continue
		}
		st := e.clock.Now()
		fs := d.Detect(req.Text)
		dur := e.clock.Since(st)
		res.Findings = append(res.Findings, fs...)
		res.Stages = append(res.Stages, StageResult{
			Stage: d.Name(), Findings: len(fs), MaxSev: MaxSeverity(fs).String(), Duration: dur,
		})
		e.observeStage(d.Name(), dur)
	}

	// The classifier is best-effort and strictly additive.
	if classifier != nil && !e.clock.Now().After(deadline) {
		remaining := deadline.Sub(e.clock.Now())
		cctx, cancel := context.WithTimeout(ctx, remaining)
		st := e.clock.Now()
		fs, err := classifier.Classify(cctx, req)
		cancel()
		dur := e.clock.Since(st)
		sr := StageResult{Stage: classifier.Name(), Findings: len(fs), MaxSev: MaxSeverity(fs).String(), Duration: dur}
		if err != nil {
			sr.Error = err.Error()
			if errors.Is(err, context.DeadlineExceeded) {
				res.BudgetExceeded = true
			}
		}
		res.Findings = append(res.Findings, fs...)
		res.Stages = append(res.Stages, sr)
		e.observeStage(classifier.Name(), dur)
	}

	if e.clock.Now().After(deadline) {
		res.BudgetExceeded = true
	}

	e.decide(&res, req)
	res.Duration = e.clock.Since(start)

	if e.metrics != nil {
		e.metrics.inspections.Inc()
		e.metrics.latency.ObserveDuration(res.Duration)
		if res.Verdict == VerdictBlock {
			e.metrics.blocks.Inc()
		}
		if res.Verdict == VerdictRedact {
			e.metrics.redactions.Inc()
		}
		if res.BudgetExceeded {
			e.metrics.budgetBust.Inc()
		}
	}
	if sp != nil {
		sp.SetAttrs(map[string]any{
			obs.AttrGuardVerdict:      string(res.Verdict),
			"tcrp.guard.findings":     len(res.Findings),
			"tcrp.guard.budget_bust":  res.BudgetExceeded,
			"tcrp.guard.fail_mode":    res.FailMode,
			"tcrp.guard.duration_ms":  float64(res.Duration.Microseconds()) / 1000,
			"tcrp.guard.trusted_ctx":  res.RetrievalTrusted,
			"tcrp.guard.policy":       e.cfg.Version,
			"tcrp.guard.max_severity": MaxSeverity(res.Findings).String(),
		})
		if res.Verdict.Blocking() {
			sp.AddEvent("guard.blocked", map[string]any{"reason": res.Reason})
			sp.SetStatus(obs.StatusError, res.Reason)
		}
	}
	return res
}

func (e *Engine) observeStage(name string, d time.Duration) {
	if e.metrics == nil {
		return
	}
	e.metrics.mu.Lock()
	h, ok := e.metrics.stage[name]
	if !ok {
		h = e.metrics.reg.Histogram("tcrp_guard_stage_seconds", "Per-stage guardrail latency.",
			[]float64{0.000_010, 0.000_050, 0.000_100, 0.000_500, 0.001, 0.005, 0.010, 0.030, 0.100},
			map[string]string{"stage": name})
		e.metrics.stage[name] = h
	}
	e.metrics.mu.Unlock()
	h.ObserveDuration(d)
}

// decide turns findings into a verdict under the tier's policy.
func (e *Engine) decide(res *Result, req Request) {
	tier := req.Tier
	if tier == "" {
		tier = TierReadOnly
	}
	blockAt, ok := e.cfg.BlockAt[tier]
	if !ok {
		blockAt = SevHigh
	}

	// Any injection signal in *retrieved context* marks the context untrusted.
	// The policy gateway then refuses to let it drive a state change. This is
	// the control that makes indirect prompt injection a non-event rather than
	// an incident: the instruction may be read, it can never be acted on.
	if req.Surface == SurfaceContext {
		for _, f := range res.Findings {
			if f.Category == CatInjection || f.Category == CatExfil || f.Category == CatJailbreak {
				res.RetrievalTrusted = false
				break
			}
		}
	}

	// Retrieved context is quarantined rather than blocked. Blocking it throws
	// away a legitimate tool result because a counterparty wrote something
	// hostile in a memo field, and the desk still needs the memo. Marking the
	// retrieval untrusted keeps the content and removes its authority: the
	// policy gateway then refuses to let it drive any state change, which is
	// the property that actually matters. A secret or a credential in a tool
	// result is different and still blocks, because that content must not
	// reach the model at all.
	if req.Surface == SurfaceContext && !res.RetrievalTrusted && !hasCategory(res.Findings, CatSecret) {
		res.Verdict = VerdictFlag
		res.Reason = "retrieved content quarantined: " + blockReason(res.Findings, SevLow)
		return
	}

	max := MaxSeverity(res.Findings)
	switch {
	case res.BudgetExceeded && !e.cfg.FailOpen[tier]:
		res.Verdict = VerdictBlock
		res.FailMode = "fail-closed"
		res.Reason = fmt.Sprintf("guardrail budget of %s exceeded on a %s turn", e.cfg.Budget, tier)
		return
	case res.BudgetExceeded:
		res.FailMode = "fail-open"
		res.Verdict = VerdictFlag
		res.Reason = fmt.Sprintf("guardrail budget of %s exceeded; turn allowed under fail-open policy for %s", e.cfg.Budget, tier)
	}

	if max >= blockAt && len(res.Findings) > 0 {
		// Output PII is redacted rather than blocked when policy allows, since
		// dropping a correct answer for a stray email address helps nobody.
		if req.Surface == SurfaceOutput && e.cfg.RedactOnOutput && onlyRedactable(res.Findings) {
			red, ph := Redact(req.Text, res.Findings)
			res.Verdict, res.Redacted, res.Placeholders = VerdictRedact, red, ph
			res.Reason = "sensitive identifiers redacted from the response"
			return
		}
		res.Verdict = VerdictBlock
		res.Reason = blockReason(res.Findings, blockAt)
		return
	}
	if len(res.Findings) > 0 && res.Verdict == VerdictAllow {
		if req.Surface == SurfaceOutput && hasCategory(res.Findings, CatPII, CatSecret) && e.cfg.RedactOnOutput {
			red, ph := Redact(req.Text, res.Findings)
			if red != req.Text {
				res.Verdict, res.Redacted, res.Placeholders = VerdictRedact, red, ph
				res.Reason = "sensitive identifiers redacted from the response"
				return
			}
		}
		res.Verdict = VerdictFlag
		res.Reason = fmt.Sprintf("%d finding(s), highest %s, below the %s block threshold for %s",
			len(res.Findings), max, blockAt, tier)
	}
}

func onlyRedactable(fs []Finding) bool {
	for _, f := range fs {
		if f.Category != CatPII && f.Category != CatSecret {
			return false
		}
	}
	return len(fs) > 0
}

func hasCategory(fs []Finding, cats ...Category) bool {
	for _, f := range fs {
		for _, c := range cats {
			if f.Category == c {
				return true
			}
		}
	}
	return false
}

func blockReason(fs []Finding, at Severity) string {
	worst := make([]string, 0, 3)
	sorted := append([]Finding(nil), fs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Severity > sorted[j].Severity })
	for _, f := range sorted {
		if f.Severity < at {
			break
		}
		worst = append(worst, fmt.Sprintf("%s/%s", f.Category, f.Rule))
		if len(worst) == 3 {
			break
		}
	}
	return "blocked on " + strings.Join(worst, ", ")
}
