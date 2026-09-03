// Package obs is the observability kernel: a clock every component takes as a
// dependency, an OpenTelemetry-shaped tracer that emits spans in OTLP/JSON, and
// a metric registry with a Prometheus text exposition.
//
// Nothing on a decision path calls time.Now directly. Every engine takes a
// Clock, which is what makes a trajectory replayable and a test deterministic;
// an architecture test in test/arch enforces it.
package obs

import (
	"sync"
	"time"
)

// Clock is the only source of time in the platform.
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
}

// RealClock reads the wall clock.
type RealClock struct{}

// Now returns the current UTC time.
func (RealClock) Now() time.Time { return time.Now().UTC() }

// Since returns the elapsed time since t.
func (RealClock) Since(t time.Time) time.Duration { return time.Since(t) }

// ManualClock is a deterministic clock for tests, replays and the demo.
type ManualClock struct {
	mu sync.Mutex
	t  time.Time
}

// NewManualClock starts a manual clock at t.
func NewManualClock(t time.Time) *ManualClock { return &ManualClock{t: t.UTC()} }

// Now returns the current manual time.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Since returns the manual elapsed time since t.
func (c *ManualClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }

// Advance moves the clock forward.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Set moves the clock to an absolute time.
func (c *ManualClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t.UTC()
}
