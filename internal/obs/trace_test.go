package obs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var seedTime = time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)

func newTestTracer(t *testing.T) (*Tracer, *ManualClock, *MemoryExporter) {
	t.Helper()
	clock := NewManualClock(seedTime)
	exp := NewMemoryExporter(0)
	return NewTracer("tcrp-test", clock, exp), clock, exp
}

// TestSpanParentChildLinkage proves a child span joins its parent's trace and
// names it as parent, and that a span started from a bare context begins a new
// trace of its own.
func TestSpanParentChildLinkage(t *testing.T) {
	tr, clock, exp := newTestTracer(t)

	ctx, root := tr.Start(context.Background(), "agent.run", KindServer)
	clock.Advance(time.Millisecond)
	ctx, child := tr.Start(ctx, "agent.step", KindInternal)
	clock.Advance(time.Millisecond)
	_, grand := tr.Start(ctx, "agent.tool.get_exposure", KindClient)
	clock.Advance(time.Millisecond)

	grand.End()
	child.End()
	root.End()

	spans := exp.Spans()
	if len(spans) != 3 {
		t.Fatalf("exported %d spans, want 3", len(spans))
	}
	byName := map[string]SpanData{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	r, c, g := byName["agent.run"], byName["agent.step"], byName["agent.tool.get_exposure"]

	if r.TraceID != c.TraceID || c.TraceID != g.TraceID {
		t.Fatalf("children left the trace: %s / %s / %s", r.TraceID, c.TraceID, g.TraceID)
	}
	if r.ParentID != "" {
		t.Fatalf("root span has parent %q", r.ParentID)
	}
	if c.ParentID != r.SpanID {
		t.Fatalf("step parent = %s, want the run's span id %s", c.ParentID, r.SpanID)
	}
	if g.ParentID != c.SpanID {
		t.Fatalf("tool parent = %s, want the step's span id %s", g.ParentID, c.SpanID)
	}
	if r.SpanID == c.SpanID || c.SpanID == g.SpanID {
		t.Fatal("span ids must be distinct")
	}

	// A second root shares nothing with the first.
	_, other := tr.Start(context.Background(), "unrelated", KindInternal)
	other.End()
	if got := exp.Spans()[3]; got.TraceID == r.TraceID {
		t.Fatal("a span started from a bare context must begin its own trace")
	}

	// Ending twice exports once.
	root.End()
	if len(exp.Spans()) != 4 {
		t.Fatalf("End is not idempotent: %d spans", len(exp.Spans()))
	}
}

// TestPrincipalAttributesInheritToChildren proves the acting identity travels
// down the trajectory without every call site restating it, which is what makes
// conformance invariant AGENT-4 satisfiable.
func TestPrincipalAttributesInheritToChildren(t *testing.T) {
	tr, _, exp := newTestTracer(t)

	ctx, root := tr.Start(context.Background(), "agent.run", KindServer)
	root.SetAttrs(map[string]any{
		AttrPrincipalSubject: "j.okafor",
		AttrPrincipalTenant:  "collateral-ops",
		AttrRunID:            "run_1",
		AttrPrincipalScopes:  "collateral:read,exposure:read",
	})
	ctx, child := tr.Start(ctx, "agent.step", KindInternal)
	_, grand := tr.Start(ctx, "guard.inspect", KindServer)

	grand.End()
	child.End()
	root.End()

	for _, s := range exp.Spans() {
		if s.Name == "agent.run" {
			continue
		}
		for _, k := range []string{AttrPrincipalSubject, AttrPrincipalTenant, AttrRunID} {
			if s.Attr(k) == "" {
				t.Fatalf("span %s did not inherit %s", s.Name, k)
			}
		}
		if s.Attr(AttrPrincipalSubject) != "j.okafor" || s.Attr(AttrRunID) != "run_1" {
			t.Fatalf("span %s inherited the wrong identity: %v", s.Name, s.Attributes)
		}
		// Only the three propagated keys travel; scopes stay on the root.
		if s.Attr(AttrPrincipalScopes) != "" {
			t.Fatalf("span %s inherited %s, which is not a propagated key", s.Name, AttrPrincipalScopes)
		}
	}
}

// TestTraceParentRoundTrip proves the W3C header the guardrail client sends is
// the header the guardrail handler can read back, and that a malformed one is
// rejected rather than half-accepted.
func TestTraceParentRoundTrip(t *testing.T) {
	sc := SpanContext{
		TraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:  "00f067aa0ba902b7",
		Sampled: true,
	}
	hdr := sc.TraceParent()
	if want := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"; hdr != want {
		t.Fatalf("TraceParent() = %q, want %q", hdr, want)
	}
	got, ok := ParseTraceParent(hdr)
	if !ok || got != sc {
		t.Fatalf("round trip: %v -> %q -> %v (ok=%v)", sc, hdr, got, ok)
	}
	unsampled := SpanContext{TraceID: sc.TraceID, SpanID: sc.SpanID}
	back, ok := ParseTraceParent(unsampled.TraceParent())
	if !ok || back.Sampled {
		t.Fatalf("an unsampled context round-tripped as %v", back)
	}

	bad := []string{
		"", "00", "not-a-header",
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", // unknown version
		"00-short-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-short-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",
	}
	for _, v := range bad {
		if _, ok := ParseTraceParent(v); ok {
			t.Fatalf("ParseTraceParent(%q) accepted a malformed header", v)
		}
	}
	if (SpanContext{}).Valid() {
		t.Fatal("the empty span context must not report Valid")
	}
	if !sc.Valid() {
		t.Fatal("a well-formed span context must report Valid")
	}
}

// TestRemoteContextJoinsUpstreamTrace proves a span started in a service that
// received a traceparent continues the caller's trace rather than starting a
// second one.
func TestRemoteContextJoinsUpstreamTrace(t *testing.T) {
	tr, _, exp := newTestTracer(t)
	upstream := SpanContext{
		TraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:  "00f067aa0ba902b7",
		Sampled: true,
	}
	ctx := WithRemoteContext(context.Background(), upstream)
	_, sp := tr.Start(ctx, "guard.inspect", KindServer)
	sp.End()

	got := exp.Spans()[0]
	if got.TraceID != upstream.TraceID {
		t.Fatalf("trace id = %s, want the upstream %s", got.TraceID, upstream.TraceID)
	}
	if got.ParentID != upstream.SpanID {
		t.Fatalf("parent = %s, want the upstream span %s", got.ParentID, upstream.SpanID)
	}

	// An invalid remote context is ignored rather than producing a broken trace.
	exp.Reset()
	_, sp2 := tr.Start(WithRemoteContext(context.Background(), SpanContext{TraceID: "xx"}), "guard.inspect", KindServer)
	sp2.End()
	if got := exp.Spans()[0]; got.ParentID != "" || len(got.TraceID) != 32 {
		t.Fatalf("an invalid remote context produced %+v", got)
	}
}

// TestDeterministicIDsAreIdenticalAcrossTracers proves the replay mode: two
// tracers configured the same way, driven through the same sequence, produce
// the same identifiers, which is what makes a demo trace file byte-identical
// run to run.
func TestDeterministicIDsAreIdenticalAcrossTracers(t *testing.T) {
	drive := func() []SpanData {
		clock := NewManualClock(seedTime)
		exp := NewMemoryExporter(0)
		tr := NewTracer("tcrp", clock, exp).Deterministic(true)
		ctx, root := tr.Start(context.Background(), "agent.run", KindServer)
		clock.Advance(time.Millisecond)
		_, a := tr.Start(ctx, "agent.step", KindInternal)
		clock.Advance(time.Millisecond)
		_, b := tr.Start(ctx, "agent.step", KindInternal)
		a.End()
		b.End()
		root.End()
		return exp.Spans()
	}
	first, second := drive(), drive()
	if len(first) != 3 || len(second) != 3 {
		t.Fatalf("expected 3 spans from each tracer, got %d and %d", len(first), len(second))
	}
	for i := range first {
		if first[i].TraceID != second[i].TraceID || first[i].SpanID != second[i].SpanID {
			t.Fatalf("span %d differs: %s/%s vs %s/%s",
				i, first[i].TraceID, first[i].SpanID, second[i].TraceID, second[i].SpanID)
		}
		if first[i].Start != second[i].Start || first[i].DurationNS != second[i].DurationNS {
			t.Fatalf("span %d timings differ under a manual clock", i)
		}
	}
	// Ids are still well-formed hex of the right width.
	if len(first[0].TraceID) != 32 || len(first[0].SpanID) != 16 {
		t.Fatalf("deterministic ids are the wrong width: %+v", first[0])
	}
	// A non-deterministic tracer does not repeat itself.
	tr, _, exp := newTestTracer(t)
	_, s1 := tr.Start(context.Background(), "x", KindInternal)
	s1.End()
	_, s2 := tr.Start(context.Background(), "x", KindInternal)
	s2.End()
	if got := exp.Spans(); got[0].TraceID == got[1].TraceID {
		t.Fatal("random trace ids collided, which means they are not random")
	}
}

// TestMemoryExporterTraceOrdering proves Trace returns exactly one trace's
// spans, ordered by start time, regardless of the order they finished in.
func TestMemoryExporterTraceOrdering(t *testing.T) {
	tr, clock, exp := newTestTracer(t)

	ctx, root := tr.Start(context.Background(), "first", KindServer)
	clock.Advance(10 * time.Millisecond)
	_, mid := tr.Start(ctx, "second", KindInternal)
	clock.Advance(10 * time.Millisecond)
	_, last := tr.Start(ctx, "third", KindInternal)

	// Finish in reverse order of starting.
	root.End()
	mid.End()
	last.End()

	// An unrelated trace must not appear.
	_, other := tr.Start(context.Background(), "other", KindInternal)
	other.End()

	traceID := root.Context().TraceID
	got := exp.Trace(traceID)
	if len(got) != 3 {
		t.Fatalf("Trace returned %d spans, want 3", len(got))
	}
	for i, want := range []string{"first", "second", "third"} {
		if got[i].Name != want {
			t.Fatalf("Trace order = %s at %d, want %s", got[i].Name, i, want)
		}
	}
	if exp.Trace("no-such-trace") == nil {
		t.Fatal("Trace of an unknown id should return an empty slice, not nil")
	}

	traces := exp.Traces()
	if len(traces) != 2 {
		t.Fatalf("Traces() grouped into %d traces, want 2", len(traces))
	}
	if len(traces[traceID]) != 3 {
		t.Fatalf("Traces()[%s] holds %d spans, want 3", traceID, len(traces[traceID]))
	}
}

// TestMemoryExporterLimitKeepsTheNewest proves a bounded buffer drops the oldest
// spans rather than refusing new ones, so a long run does not lose its tail.
func TestMemoryExporterLimitKeepsTheNewest(t *testing.T) {
	exp := NewMemoryExporter(3)
	for i := 0; i < 10; i++ {
		exp.Export(SpanData{Name: string(rune('a' + i))})
	}
	got := exp.Spans()
	if len(got) != 3 {
		t.Fatalf("retained %d spans, want 3", len(got))
	}
	if got[0].Name != "h" || got[2].Name != "j" {
		t.Fatalf("retained %v, want the last three", []string{got[0].Name, got[1].Name, got[2].Name})
	}
	exp.Reset()
	if len(exp.Spans()) != 0 {
		t.Fatal("Reset did not drop the retained spans")
	}
}

// TestSpanStatusAndEvents proves the outcome and annotations a span carries,
// including that an error both records an event and sets the status, and that
// an unset status defaults to OK at End.
func TestSpanStatusAndEvents(t *testing.T) {
	tr, clock, exp := newTestTracer(t)

	_, ok := tr.Start(context.Background(), "fine", KindInternal)
	clock.Advance(5 * time.Millisecond)
	ok.AddEvent("approval.required", map[string]any{"approval_id": "apr_1"})
	ok.End()

	_, bad := tr.Start(context.Background(), "broken", KindInternal)
	bad.RecordError(context.DeadlineExceeded)
	bad.End()

	spans := exp.Spans()
	if spans[0].Status != StatusOK {
		t.Fatalf("an unset status must end as OK, got %s", spans[0].Status)
	}
	if spans[0].DurationNS != int64(5*time.Millisecond) {
		t.Fatalf("duration = %d ns, want 5ms from the manual clock", spans[0].DurationNS)
	}
	if len(spans[0].Events) != 1 || spans[0].Events[0].Name != "approval.required" {
		t.Fatalf("events = %+v", spans[0].Events)
	}
	if spans[0].Events[0].Time != seedTime.Add(5*time.Millisecond) {
		t.Fatalf("event time %v does not come from the tracer's clock", spans[0].Events[0].Time)
	}
	if spans[1].Status != StatusError || spans[1].StatusMsg == "" {
		t.Fatalf("RecordError left status %s / %q", spans[1].Status, spans[1].StatusMsg)
	}
	if len(spans[1].Events) != 1 || spans[1].Events[0].Name != "exception" {
		t.Fatalf("RecordError did not add an exception event: %+v", spans[1].Events)
	}
	// A nil error changes nothing.
	if got := (*Span)(nil).RecordError(nil); got != nil {
		t.Fatal("RecordError on a nil span must stay nil")
	}
}

// TestNilSpanIsSafe proves the nil span the runtime passes around when tracing
// is off absorbs every call rather than panicking.
func TestNilSpanIsSafe(t *testing.T) {
	var s *Span
	s.SetAttr("k", "v").SetAttrs(map[string]any{"a": 1}).AddEvent("e", nil).SetStatus(StatusOK, "")
	s.End()
	if s.Context() != (SpanContext{}) {
		t.Fatal("a nil span must yield the empty context")
	}
	if SpanFromContext(context.Background()) != nil {
		t.Fatal("a context with no span must yield nil")
	}
}

// TestWriterExporterEmitsOneJSONObjectPerSpan proves the on-disk trace format
// the eval harness and drift detector read back.
func TestWriterExporterEmitsOneJSONObjectPerSpan(t *testing.T) {
	var buf bytes.Buffer
	clock := NewManualClock(seedTime)
	tr := NewTracer("tcrp", clock, NewWriterExporter(&buf)).Deterministic(true)

	ctx, root := tr.Start(context.Background(), "agent.run", KindServer)
	root.SetAttr(AttrRunID, "run_1")
	_, child := tr.Start(ctx, "agent.step", KindInternal)
	child.End()
	root.End()

	sc := bufio.NewScanner(&buf)
	var lines []SpanData
	for sc.Scan() {
		var s SpanData
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatalf("line is not a span object: %v (%s)", err, sc.Text())
		}
		lines = append(lines, s)
	}
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want 2", len(lines))
	}
	if lines[0].Name != "agent.step" || lines[1].Name != "agent.run" {
		t.Fatalf("lines are written in finishing order, got %s then %s", lines[0].Name, lines[1].Name)
	}
	if lines[1].Resource["service.name"] != "tcrp" {
		t.Fatalf("resource missing the service name: %v", lines[1].Resource)
	}
}

// TestMultiExporterFansOut proves both the memory buffer and the file receive
// every span, which is how the platform writes a trace file and still answers
// assertions in-process.
func TestMultiExporterFansOut(t *testing.T) {
	a, b := NewMemoryExporter(0), NewMemoryExporter(0)
	var buf bytes.Buffer
	m := MultiExporter{a, b, NewWriterExporter(&buf)}
	tr := NewTracer("tcrp", NewManualClock(seedTime), m)

	_, sp := tr.Start(context.Background(), "x", KindInternal)
	sp.End()

	if len(a.Spans()) != 1 || len(b.Spans()) != 1 || !strings.Contains(buf.String(), `"name":"x"`) {
		t.Fatalf("fan-out incomplete: %d / %d / %q", len(a.Spans()), len(b.Spans()), buf.String())
	}

	// AddExporter reaches spans finished after it was installed.
	c := NewMemoryExporter(0)
	tr.AddExporter(c)
	_, sp2 := tr.Start(context.Background(), "y", KindInternal)
	sp2.End()
	if len(c.Spans()) != 1 || len(a.Spans()) != 2 {
		t.Fatalf("AddExporter: c=%d a=%d", len(c.Spans()), len(a.Spans()))
	}
}

// TestManualClockIsTheOnlyTimeSource proves the deterministic clock advances
// only when a test tells it to, and that Since is measured against it.
func TestManualClockIsTheOnlyTimeSource(t *testing.T) {
	c := NewManualClock(seedTime)
	if c.Now() != seedTime {
		t.Fatalf("Now() = %v, want the seed instant", c.Now())
	}
	start := c.Now()
	c.Advance(90 * time.Second)
	if got := c.Since(start); got != 90*time.Second {
		t.Fatalf("Since = %v, want 90s", got)
	}
	c.Set(seedTime)
	if c.Now() != seedTime {
		t.Fatalf("Set did not rewind the clock: %v", c.Now())
	}
	// A tracer given no clock still works, on the real one.
	tr := NewTracer("x", nil)
	if tr.Clock() == nil {
		t.Fatal("a tracer with no clock must adopt the real one")
	}
}

// TestSpanDataAttrCoercesNonStrings proves Attr is safe to use for reporting
// regardless of how an attribute was recorded.
func TestSpanDataAttrCoercesNonStrings(t *testing.T) {
	s := SpanData{Attributes: map[string]any{"n": 42, "s": "text", "b": true}}
	if got := s.Attr("s"); got != "text" {
		t.Fatalf("Attr(s) = %q", got)
	}
	if got := s.Attr("n"); got != "42" {
		t.Fatalf("Attr(n) = %q, want the formatted number", got)
	}
	if got := s.Attr("b"); got != "true" {
		t.Fatalf("Attr(b) = %q", got)
	}
	if got := s.Attr("missing"); got != "" {
		t.Fatalf("Attr of an absent key = %q, want empty", got)
	}
}
