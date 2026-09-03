package risk

import (
	"time"

	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Metric names. They are constants because a dashboard, an alert rule and a
// test all have to agree on the string, and a typo in any one of them is a
// silent gap rather than a failure.
const (
	MetricCalculations   = "tcrp_risk_calculations_total"
	MetricCalcErrors     = "tcrp_risk_calculation_errors_total"
	MetricCalcSeconds    = "tcrp_risk_calculation_seconds"
	MetricOpenBreaches   = "tcrp_risk_open_breaches"
	MetricStressSeconds  = "tcrp_risk_stress_scenario_seconds"
	MetricRecomputeLag   = "tcrp_risk_recompute_lag_seconds"
	MetricRecomputes     = "tcrp_risk_recomputes_total"
	MetricBreachesRaised = "tcrp_risk_breaches_raised_total"
)

// Metrics is the risk engine's instrumentation.
//
// It is a struct rather than a set of package-level variables so that a test
// can hold its own registry and assert on exact counts. Every constructor
// tolerates a nil registry by making a private one, which means instrumentation
// is never a reason for a caller to have to build observability plumbing it
// does not otherwise need.
type Metrics struct {
	reg *obs.Registry
	// Calculations counts completed engine calls, labelled by operation.
	Calculations *obs.Counter
	// Errors counts engine calls that returned an error.
	Errors *obs.Counter
	// OpenBreaches is the number of breaches the register currently holds open.
	OpenBreaches *obs.Gauge
	// BreachesRaised counts first detections, so a flapping limit is
	// distinguishable from a limit that has been broken once and left open.
	BreachesRaised *obs.Counter
	// Recomputes counts event-driven recompute cycles.
	Recomputes *obs.Counter
	// RecomputeLag records the delay between the newest observed event and the
	// moment its recompute finished.
	RecomputeLag *obs.Histogram
}

// NewMetrics registers the engine's metrics on r. A nil registry is replaced by
// a private one, so the returned Metrics is always safe to use.
func NewMetrics(r *obs.Registry) *Metrics {
	if r == nil {
		r = obs.NewRegistry()
	}
	return &Metrics{
		reg:            r,
		Calculations:   r.Counter(MetricCalculations, "risk calculations completed", nil),
		Errors:         r.Counter(MetricCalcErrors, "risk calculations that failed", nil),
		OpenBreaches:   r.Gauge(MetricOpenBreaches, "limit breaches currently open", nil),
		BreachesRaised: r.Counter(MetricBreachesRaised, "limit breaches raised for the first time", nil),
		Recomputes:     r.Counter(MetricRecomputes, "event-driven recompute cycles", nil),
		RecomputeLag:   r.Histogram(MetricRecomputeLag, "seconds from newest observed event to recompute completion", obs.DefaultBuckets, nil),
	}
}

// Registry returns the registry the metrics are registered on.
func (m *Metrics) Registry() *obs.Registry { return m.reg }

// CalculationLatency returns the latency histogram for one operation. The
// histogram is created on first use and memoised, so callers may fetch it on a
// hot path.
func (m *Metrics) CalculationLatency(op string) *obs.Histogram {
	return m.reg.Histogram(MetricCalcSeconds, "seconds spent in a risk calculation",
		obs.DefaultBuckets, map[string]string{"op": op})
}

// StressLatency returns the latency histogram for one stress scenario.
func (m *Metrics) StressLatency(scenario string) *obs.Histogram {
	return m.reg.Histogram(MetricStressSeconds, "seconds spent running one stress scenario",
		obs.DefaultBuckets, map[string]string{"scenario": scenario})
}

// ObserveCalculation records one engine call: its latency, and whether it
// succeeded. It is the single place the engine touches metrics, so an operation
// cannot be counted without also being timed.
func (m *Metrics) ObserveCalculation(op string, d time.Duration, err error) {
	if m == nil {
		return
	}
	m.Calculations.Inc()
	if err != nil {
		m.Errors.Inc()
	}
	m.CalculationLatency(op).ObserveDuration(d)
}

// ObserveStress records one scenario run.
func (m *Metrics) ObserveStress(scenario string, d time.Duration) {
	if m == nil {
		return
	}
	m.StressLatency(scenario).ObserveDuration(d)
}

// ObserveRecompute records one recompute cycle and the lag it closed.
func (m *Metrics) ObserveRecompute(lag time.Duration) {
	if m == nil {
		return
	}
	m.Recomputes.Inc()
	if lag < 0 {
		lag = 0
	}
	m.RecomputeLag.ObserveDuration(lag)
}
