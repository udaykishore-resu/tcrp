package obs

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Registry holds the platform's metrics and renders them in Prometheus text
// exposition format. It is deliberately small: counters, gauges and histograms
// with explicit buckets, which is everything the SLOs in docs/slo.md need.
type Registry struct {
	mu    sync.RWMutex
	items map[string]metric
	order []string
}

type metric interface {
	render(name string, sb *strings.Builder)
	help() string
	kind() string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{items: map[string]metric{}} }

// DefaultBuckets are latency buckets in seconds, chosen around the 100ms
// guardrail budget so that budget breaches are visible without interpolation.
var DefaultBuckets = []float64{0.001, 0.005, 0.010, 0.025, 0.050, 0.075, 0.100, 0.150, 0.250, 0.500, 1, 2.5, 5}

func key(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	ks := make([]string, 0, len(labels))
	for k := range labels {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var sb strings.Builder
	sb.WriteString(name)
	sb.WriteByte('{')
	for i, k := range ks {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%s=%q", k, labels[k])
	}
	sb.WriteByte('}')
	return sb.String()
}

func (r *Registry) get(name string, labels map[string]string, mk func() metric) metric {
	k := key(name, labels)
	r.mu.RLock()
	m, ok := r.items[k]
	r.mu.RUnlock()
	if ok {
		return m
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok = r.items[k]; ok {
		return m
	}
	m = mk()
	r.items[k] = m
	r.order = append(r.order, k)
	return m
}

// Counter is a monotonically increasing value.
type Counter struct {
	mu  sync.Mutex
	v   float64
	doc string
}

// Inc adds one.
func (c *Counter) Inc() { c.Add(1) }

// Add adds d.
func (c *Counter) Add(d float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.v += d
}

// Value reads the counter.
func (c *Counter) Value() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.v
}

func (c *Counter) help() string { return c.doc }
func (c *Counter) kind() string { return "counter" }
func (c *Counter) render(name string, sb *strings.Builder) {
	fmt.Fprintf(sb, "%s %g\n", name, c.Value())
}

// Gauge is a value that goes up and down.
type Gauge struct {
	mu  sync.Mutex
	v   float64
	doc string
}

// Set writes the gauge.
func (g *Gauge) Set(v float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.v = v
}

// Add adds d to the gauge.
func (g *Gauge) Add(d float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.v += d
}

// Value reads the gauge.
func (g *Gauge) Value() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.v
}

func (g *Gauge) help() string { return g.doc }
func (g *Gauge) kind() string { return "gauge" }
func (g *Gauge) render(name string, sb *strings.Builder) {
	fmt.Fprintf(sb, "%s %g\n", name, g.Value())
}

// Histogram records a distribution with fixed buckets and keeps the raw
// observations so exact quantiles can be reported in the demo output.
type Histogram struct {
	mu      sync.Mutex
	buckets []float64
	counts  []uint64
	sum     float64
	n       uint64
	samples []float64
	doc     string
}

// Observe records a value.
func (h *Histogram) Observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sum += v
	h.n++
	if len(h.samples) < 100_000 {
		h.samples = append(h.samples, v)
	}
	for i, b := range h.buckets {
		if v <= b {
			h.counts[i]++
		}
	}
}

// ObserveDuration records a duration in seconds.
func (h *Histogram) ObserveDuration(d time.Duration) { h.Observe(d.Seconds()) }

// Count returns the number of observations.
func (h *Histogram) Count() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// Quantile returns the exact q-quantile of the retained samples.
func (h *Histogram) Quantile(q float64) float64 {
	h.mu.Lock()
	s := make([]float64, len(h.samples))
	copy(s, h.samples)
	h.mu.Unlock()
	if len(s) == 0 {
		return 0
	}
	sort.Float64s(s)
	idx := int(math.Ceil(q*float64(len(s)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

// Mean returns the arithmetic mean of observations.
func (h *Histogram) Mean() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n == 0 {
		return 0
	}
	return h.sum / float64(h.n)
}

func (h *Histogram) help() string { return h.doc }
func (h *Histogram) kind() string { return "histogram" }
func (h *Histogram) render(name string, sb *strings.Builder) {
	h.mu.Lock()
	defer h.mu.Unlock()
	base, labels, _ := strings.Cut(name, "{")
	suffix := ""
	if labels != "" {
		suffix = strings.TrimSuffix(labels, "}")
	}
	for i, b := range h.buckets {
		lb := fmt.Sprintf(`le="%g"`, b)
		if suffix != "" {
			lb = suffix + "," + lb
		}
		fmt.Fprintf(sb, "%s_bucket{%s} %d\n", base, lb, h.counts[i])
	}
	lb := `le="+Inf"`
	if suffix != "" {
		lb = suffix + "," + lb
	}
	fmt.Fprintf(sb, "%s_bucket{%s} %d\n", base, lb, h.n)
	if suffix != "" {
		fmt.Fprintf(sb, "%s_sum{%s} %g\n%s_count{%s} %d\n", base, suffix, h.sum, base, suffix, h.n)
	} else {
		fmt.Fprintf(sb, "%s_sum %g\n%s_count %d\n", base, h.sum, base, h.n)
	}
}

// Counter returns (creating if needed) a counter.
func (r *Registry) Counter(name, doc string, labels map[string]string) *Counter {
	return r.get(name, labels, func() metric { return &Counter{doc: doc} }).(*Counter)
}

// Gauge returns (creating if needed) a gauge.
func (r *Registry) Gauge(name, doc string, labels map[string]string) *Gauge {
	return r.get(name, labels, func() metric { return &Gauge{doc: doc} }).(*Gauge)
}

// Histogram returns (creating if needed) a histogram.
func (r *Registry) Histogram(name, doc string, buckets []float64, labels map[string]string) *Histogram {
	if buckets == nil {
		buckets = DefaultBuckets
	}
	return r.get(name, labels, func() metric {
		return &Histogram{buckets: buckets, counts: make([]uint64, len(buckets)), doc: doc}
	}).(*Histogram)
}

// Render produces the Prometheus text exposition of every metric.
func (r *Registry) Render() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var sb strings.Builder
	seen := map[string]bool{}
	keys := append([]string(nil), r.order...)
	sort.Strings(keys)
	for _, k := range keys {
		m := r.items[k]
		base, _, _ := strings.Cut(k, "{")
		if !seen[base] {
			seen[base] = true
			if h := m.help(); h != "" {
				fmt.Fprintf(&sb, "# HELP %s %s\n", base, h)
			}
			fmt.Fprintf(&sb, "# TYPE %s %s\n", base, m.kind())
		}
		m.render(k, &sb)
	}
	return sb.String()
}
