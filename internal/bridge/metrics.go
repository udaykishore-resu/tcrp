package bridge

import "github.com/udaykishore-resu/tcrp/internal/obs"

// Metric names. They are constants because a dashboard, an alert rule and a
// test all have to agree on the string, and a typo in any one of them is a
// silent gap rather than a failure.
const (
	MetricObserved      = "tcrp_bridge_triggers_observed_total"
	MetricSettled       = "tcrp_bridge_settlements_total"
	MetricAbandoned     = "tcrp_bridge_abandoned_total"
	MetricAlerts        = "tcrp_bridge_alerts_total"
	MetricRetries       = "tcrp_bridge_submission_retries_total"
	MetricPending       = "tcrp_bridge_pending"
	MetricQuorumSize    = "tcrp_bridge_quorum_signatures"
	MetricSettlementLag = "tcrp_bridge_settlement_seconds"
	MetricSupplyDelta   = "tcrp_bridge_supply_delta"
	MetricSupplyBreaks  = "tcrp_bridge_supply_breaks"
)

// Metrics is the bridge's instrumentation, labelled by direction so that the
// mint path and the redeem path are separable on a dashboard. A nil registry is
// replaced by a private one, so instrumentation is never a reason for a caller
// to have to build observability plumbing it does not otherwise need.
type Metrics struct {
	reg *obs.Registry
	// Observed counts trigger events seen for the first time.
	Observed *obs.Counter
	// Settled counts completed flows.
	Settled *obs.Counter
	// Abandoned counts flows whose source transaction was orphaned before
	// finality.
	Abandoned *obs.Counter
	// Alerts counts raised alerts.
	Alerts *obs.Counter
	// Retries counts target submission retries.
	Retries *obs.Counter
	// Pending is the number of flows waiting on finality, a quorum or a retry.
	Pending *obs.Gauge
	// QuorumSize records how many signatures each settlement carried, so an
	// operator can see a quorum degrading before it fails.
	QuorumSize *obs.Histogram
	// SettlementLag records the delay between observing a trigger and writing
	// the response.
	SettlementLag *obs.Histogram
}

// quorumBuckets count signatures rather than seconds, so the latency buckets
// would be meaningless here.
var quorumBuckets = []float64{1, 2, 3, 4, 5, 7, 9, 11}

// NewMetrics registers the bridge's metrics on r for one direction.
func NewMetrics(r *obs.Registry, dir Direction) *Metrics {
	if r == nil {
		r = obs.NewRegistry()
	}
	lb := map[string]string{"direction": string(dir)}
	return &Metrics{
		reg:           r,
		Observed:      r.Counter(MetricObserved, "bridge trigger events observed", lb),
		Settled:       r.Counter(MetricSettled, "bridge flows settled on the target ledger", lb),
		Abandoned:     r.Counter(MetricAbandoned, "bridge flows abandoned after a pre-finality reorganisation", lb),
		Alerts:        r.Counter(MetricAlerts, "bridge alerts raised", lb),
		Retries:       r.Counter(MetricRetries, "bridge target submission retries", lb),
		Pending:       r.Gauge(MetricPending, "bridge flows waiting on finality, quorum or retry", lb),
		QuorumSize:    r.Histogram(MetricQuorumSize, "signatures carried by each settlement", quorumBuckets, lb),
		SettlementLag: r.Histogram(MetricSettlementLag, "seconds between observing a trigger and writing the response", nil, lb),
	}
}

// Registry exposes the registry the metrics were registered on, which is what
// the supply control writes its gauges to.
func (m *Metrics) Registry() *obs.Registry { return m.reg }
