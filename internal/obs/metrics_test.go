package obs

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCounterAndGauge covers the two scalar metric shapes, including that a
// gauge goes both ways and a counter only accumulates.
func TestCounterAndGauge(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("tcrp_policy_decisions_total", "Policy decisions rendered.", nil)
	c.Inc()
	c.Inc()
	c.Add(3)
	if got := c.Value(); got != 5 {
		t.Fatalf("counter = %v, want 5", got)
	}
	// The same name and labels return the same instrument rather than a new one.
	if r.Counter("tcrp_policy_decisions_total", "", nil).Value() != 5 {
		t.Fatal("re-fetching a counter must return the same instrument")
	}

	g := r.Gauge("tcrp_runs_in_flight", "Runs in flight.", nil)
	g.Set(4)
	g.Add(-3)
	if got := g.Value(); got != 1 {
		t.Fatalf("gauge = %v, want 1", got)
	}

	// Labels make a distinct series.
	a := r.Counter("tcrp_tool_calls_total", "Tool calls.", map[string]string{"tier": "read_only"})
	b := r.Counter("tcrp_tool_calls_total", "Tool calls.", map[string]string{"tier": "money_movement"})
	a.Inc()
	if b.Value() != 0 {
		t.Fatal("labelled series must not share state")
	}
}

// TestHistogramExactQuantiles checks the quantiles against values computed by
// hand from the definition the implementation documents: the sample at
// ceil(q*n)-1 of the sorted observations, clamped to the ends.
func TestHistogramExactQuantiles(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("tcrp_latency_seconds", "Latency.", []float64{1, 5, 10}, nil)
	// Deliberately out of order; the quantile sorts.
	for _, v := range []float64{7, 3, 10, 1, 9, 2, 8, 4, 6, 5} {
		h.Observe(v)
	}

	cases := []struct {
		q    float64
		want float64
		why  string
	}{
		{0, 1, "clamped to the smallest sample"},
		{0.1, 1, "ceil(1)-1 = index 0"},
		{0.25, 3, "ceil(2.5)-1 = index 2"},
		{0.5, 5, "ceil(5)-1 = index 4"},
		{0.9, 9, "ceil(9)-1 = index 8"},
		{0.95, 10, "ceil(9.5)-1 = index 9"},
		{0.99, 10, "ceil(9.9)-1 = index 9"},
		{1, 10, "clamped to the largest sample"},
	}
	for _, c := range cases {
		if got := h.Quantile(c.q); got != c.want {
			t.Fatalf("Quantile(%v) = %v, want %v (%s)", c.q, got, c.want, c.why)
		}
	}
	if got := h.Count(); got != 10 {
		t.Fatalf("Count = %d, want 10", got)
	}
	if got := h.Mean(); got != 5.5 {
		t.Fatalf("Mean = %v, want 5.5", got)
	}

	// An empty histogram answers zero rather than panicking on an empty slice.
	empty := r.Histogram("tcrp_empty_seconds", "Empty.", []float64{1}, nil)
	if empty.Quantile(0.5) != 0 || empty.Mean() != 0 || empty.Count() != 0 {
		t.Fatal("an empty histogram must report zeros")
	}
}

// TestHistogramBucketsAreCumulativeAtOrBelow proves the bucket counts follow the
// le semantics Prometheus expects: a bucket holds every observation at or below
// its bound.
func TestHistogramBucketsAreCumulativeAtOrBelow(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("tcrp_stage_seconds", "Stage latency.", []float64{0.01, 0.1, 1}, nil)
	for _, d := range []time.Duration{
		5 * time.Millisecond,   // <= 0.01, 0.1, 1
		10 * time.Millisecond,  // exactly 0.01, so counted in every bucket
		50 * time.Millisecond,  // <= 0.1, 1
		500 * time.Millisecond, // <= 1
		2 * time.Second,        // above every bound
	} {
		h.ObserveDuration(d)
	}
	out := r.Render()
	for _, want := range []string{
		`tcrp_stage_seconds_bucket{le="0.01"} 2`,
		`tcrp_stage_seconds_bucket{le="0.1"} 3`,
		`tcrp_stage_seconds_bucket{le="1"} 4`,
		`tcrp_stage_seconds_bucket{le="+Inf"} 5`,
		`tcrp_stage_seconds_count 5`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendering does not contain %q:\n%s", want, out)
		}
	}
}

// TestRenderShape pins the Prometheus text exposition: one HELP and one TYPE
// line per metric name, series sorted, labels preserved on every bucket line of
// a labelled histogram.
func TestRenderShape(t *testing.T) {
	r := NewRegistry()
	r.Counter("tcrp_guard_blocks_total", "Turns blocked by the guardrail.", nil).Add(2)
	r.Gauge("tcrp_lake_rows", "Rows in the lake.", nil).Set(1500)
	r.Histogram("tcrp_guard_stage_seconds", "Per-stage guardrail latency.",
		[]float64{0.01, 0.1}, map[string]string{"stage": "pii"}).Observe(0.005)
	r.Histogram("tcrp_guard_stage_seconds", "Per-stage guardrail latency.",
		[]float64{0.01, 0.1}, map[string]string{"stage": "injection"}).Observe(0.05)

	out := r.Render()

	for _, want := range []string{
		"# HELP tcrp_guard_blocks_total Turns blocked by the guardrail.\n# TYPE tcrp_guard_blocks_total counter\ntcrp_guard_blocks_total 2\n",
		"# HELP tcrp_lake_rows Rows in the lake.\n# TYPE tcrp_lake_rows gauge\ntcrp_lake_rows 1500\n",
		"# TYPE tcrp_guard_stage_seconds histogram\n",
		`tcrp_guard_stage_seconds_bucket{stage="injection",le="0.01"} 0`,
		`tcrp_guard_stage_seconds_bucket{stage="injection",le="0.1"} 1`,
		`tcrp_guard_stage_seconds_bucket{stage="pii",le="0.01"} 1`,
		`tcrp_guard_stage_seconds_sum{stage="pii"} 0.005`,
		`tcrp_guard_stage_seconds_count{stage="pii"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendering does not contain %q:\n%s", want, out)
		}
	}

	// HELP and TYPE appear exactly once per metric name even with two series.
	if n := strings.Count(out, "# TYPE tcrp_guard_stage_seconds histogram"); n != 1 {
		t.Fatalf("TYPE line for a two-series histogram appears %d times", n)
	}
	if n := strings.Count(out, "# HELP tcrp_guard_stage_seconds"); n != 1 {
		t.Fatalf("HELP line appears %d times", n)
	}

	// Series are emitted in sorted key order, which keeps a scrape diffable.
	iBlocks := strings.Index(out, "tcrp_guard_blocks_total ")
	iInject := strings.Index(out, `stage="injection"`)
	iPII := strings.Index(out, `stage="pii"`)
	iRows := strings.Index(out, "tcrp_lake_rows ")
	if !(iBlocks < iInject && iInject < iPII && iPII < iRows) {
		t.Fatalf("series are not in sorted order:\n%s", out)
	}

	// An empty registry renders nothing rather than a stray header.
	if got := NewRegistry().Render(); got != "" {
		t.Fatalf("an empty registry rendered %q", got)
	}
}

// TestRegistryIsSafeForConcurrentUse proves instruments can be created and
// written from many goroutines, which is what the guardrail's per-stage
// histograms do under load. It is meaningful under -race.
func TestRegistryIsSafeForConcurrentUse(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.Counter("tcrp_shared_total", "Shared.", nil).Inc()
				r.Histogram("tcrp_shared_seconds", "Shared.", []float64{1}, nil).Observe(0.5)
				r.Gauge("tcrp_shared_gauge", "Shared.", nil).Set(float64(j))
				_ = r.Render()
			}
		}(i)
	}
	wg.Wait()
	if got := r.Counter("tcrp_shared_total", "", nil).Value(); got != 1600 {
		t.Fatalf("counter = %v, want 1600", got)
	}
	if got := r.Histogram("tcrp_shared_seconds", "", []float64{1}, nil).Count(); got != 1600 {
		t.Fatalf("histogram count = %d, want 1600", got)
	}
}

// TestDefaultBucketsStraddleTheGuardrailBudget proves the shipped latency
// buckets have a bound exactly at the 100ms guardrail budget, so a breach shows
// up without interpolating between buckets.
func TestDefaultBucketsStraddleTheGuardrailBudget(t *testing.T) {
	var found bool
	for _, b := range DefaultBuckets {
		if b == 0.100 {
			found = true
		}
	}
	if !found {
		t.Fatalf("DefaultBuckets has no bound at the 100ms budget: %v", DefaultBuckets)
	}
	r := NewRegistry()
	// A nil bucket list adopts the defaults.
	h := r.Histogram("tcrp_default_seconds", "Defaults.", nil, nil)
	h.ObserveDuration(150 * time.Millisecond)
	out := r.Render()
	if !strings.Contains(out, `tcrp_default_seconds_bucket{le="0.1"} 0`) {
		t.Fatalf("a 150ms observation must fall outside the 100ms bucket:\n%s", out)
	}
	if !strings.Contains(out, `tcrp_default_seconds_bucket{le="0.15"} 1`) {
		t.Fatalf("a 150ms observation must fall inside the 150ms bucket:\n%s", out)
	}
}
