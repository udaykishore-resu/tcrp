package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Attribute keys follow the OpenTelemetry semantic conventions, including the
// GenAI conventions for agent and tool spans. Emitting these names is what lets
// the trajectory land in any OTel-native viewer without a translation layer.
const (
	AttrGenAISystem        = "gen_ai.system"
	AttrGenAIOperationName = "gen_ai.operation.name"
	AttrGenAIAgentName     = "gen_ai.agent.name"
	AttrGenAIToolName      = "gen_ai.tool.name"
	AttrGenAIToolCallID    = "gen_ai.tool.call.id"
	AttrGenAIRequestModel  = "gen_ai.request.model"
	AttrGenAIUsageIn       = "gen_ai.usage.input_tokens"
	AttrGenAIUsageOut      = "gen_ai.usage.output_tokens"

	AttrPrincipalSubject = "tcrp.principal.subject"
	AttrPrincipalTenant  = "tcrp.principal.tenant"
	AttrPrincipalScopes  = "tcrp.principal.scopes"
	AttrToolTier         = "tcrp.tool.tier"
	AttrPolicyDecision   = "tcrp.policy.decision"
	AttrPolicyRule       = "tcrp.policy.rule"
	AttrGuardVerdict     = "tcrp.guard.verdict"
	AttrGuardStage       = "tcrp.guard.stage"
	AttrApprovalID       = "tcrp.approval.id"
	AttrApprovalBy       = "tcrp.approval.by"
	AttrRunID            = "tcrp.run.id"
	AttrAuditSeq         = "tcrp.audit.seq"
)

// SpanKind mirrors the OTel span kinds the platform uses.
type SpanKind string

// Span kinds.
const (
	KindInternal SpanKind = "INTERNAL"
	KindServer   SpanKind = "SERVER"
	KindClient   SpanKind = "CLIENT"
	KindProducer SpanKind = "PRODUCER"
	KindConsumer SpanKind = "CONSUMER"
)

// StatusCode mirrors the OTel status codes.
type StatusCode string

// Status codes.
const (
	StatusUnset StatusCode = "UNSET"
	StatusOK    StatusCode = "OK"
	StatusError StatusCode = "ERROR"
)

// SpanContext identifies a span for propagation.
type SpanContext struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
	Sampled bool   `json:"sampled"`
}

// Valid reports whether the context carries usable identifiers.
func (sc SpanContext) Valid() bool { return len(sc.TraceID) == 32 && len(sc.SpanID) == 16 }

// TraceParent renders the context as a W3C traceparent header value.
func (sc SpanContext) TraceParent() string {
	flags := "00"
	if sc.Sampled {
		flags = "01"
	}
	return fmt.Sprintf("00-%s-%s-%s", sc.TraceID, sc.SpanID, flags)
}

// ParseTraceParent reads a W3C traceparent header value.
func ParseTraceParent(v string) (SpanContext, bool) {
	parts := strings.Split(strings.TrimSpace(v), "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 {
		return SpanContext{}, false
	}
	return SpanContext{TraceID: parts[1], SpanID: parts[2], Sampled: parts[3] == "01"}, true
}

// Event is a timestamped annotation on a span.
type Event struct {
	Name       string         `json:"name"`
	Time       time.Time      `json:"time"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// SpanData is the exported form of a finished span.
type SpanData struct {
	TraceID    string         `json:"trace_id"`
	SpanID     string         `json:"span_id"`
	ParentID   string         `json:"parent_span_id,omitempty"`
	Name       string         `json:"name"`
	Kind       SpanKind       `json:"kind"`
	Start      time.Time      `json:"start_time"`
	End        time.Time      `json:"end_time"`
	DurationNS int64          `json:"duration_ns"`
	Status     StatusCode     `json:"status"`
	StatusMsg  string         `json:"status_message,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Events     []Event        `json:"events,omitempty"`
	Resource   map[string]any `json:"resource,omitempty"`
}

// Attr reads a string attribute.
func (s SpanData) Attr(k string) string {
	v, ok := s.Attributes[k]
	if !ok {
		return ""
	}
	if str, ok := v.(string); ok {
		return str
	}
	return fmt.Sprint(v)
}

// Exporter receives finished spans. Implementations must be safe for
// concurrent use and must not block the caller for long.
type Exporter interface {
	Export(SpanData)
}

// Span is an in-flight unit of work.
type Span struct {
	tr     *Tracer
	mu     sync.Mutex
	data   SpanData
	ended  bool
	parent *Span
}

// Context returns the span's propagation context.
func (s *Span) Context() SpanContext {
	if s == nil {
		return SpanContext{}
	}
	return SpanContext{TraceID: s.data.TraceID, SpanID: s.data.SpanID, Sampled: true}
}

// SetAttr records an attribute.
func (s *Span) SetAttr(k string, v any) *Span {
	if s == nil {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Attributes == nil {
		s.data.Attributes = map[string]any{}
	}
	s.data.Attributes[k] = v
	return s
}

// SetAttrs records several attributes.
func (s *Span) SetAttrs(kv map[string]any) *Span {
	for k, v := range kv {
		s.SetAttr(k, v)
	}
	return s
}

// AddEvent records a timestamped event.
func (s *Span) AddEvent(name string, attrs map[string]any) *Span {
	if s == nil {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Events = append(s.data.Events, Event{Name: name, Time: s.tr.clock.Now(), Attributes: attrs})
	return s
}

// SetStatus records the outcome.
func (s *Span) SetStatus(code StatusCode, msg string) *Span {
	if s == nil {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Status, s.data.StatusMsg = code, msg
	return s
}

// RecordError marks the span failed and annotates it with the error.
func (s *Span) RecordError(err error) *Span {
	if s == nil || err == nil {
		return s
	}
	s.AddEvent("exception", map[string]any{"exception.message": err.Error()})
	return s.SetStatus(StatusError, err.Error())
}

// End finishes the span and hands it to the exporters exactly once.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.data.End = s.tr.clock.Now()
	s.data.DurationNS = s.data.End.Sub(s.data.Start).Nanoseconds()
	if s.data.Status == StatusUnset {
		s.data.Status = StatusOK
	}
	out := s.data
	s.mu.Unlock()
	s.tr.export(out)
}

// Tracer creates spans and fans finished spans out to exporters.
type Tracer struct {
	clock     Clock
	service   string
	mu        sync.RWMutex
	exporters []Exporter
	resource  map[string]any
	idmu      sync.Mutex
	seq       uint64
	det       bool
}

// NewTracer builds a tracer for a named service.
func NewTracer(service string, clock Clock, exporters ...Exporter) *Tracer {
	if clock == nil {
		clock = RealClock{}
	}
	return &Tracer{
		clock:     clock,
		service:   service,
		exporters: exporters,
		resource:  map[string]any{"service.name": service, "telemetry.sdk.language": "go"},
	}
}

// Deterministic switches ID generation to a counter so that a replay produces
// byte-identical trace files. Used by the demo and by golden tests.
func (t *Tracer) Deterministic(on bool) *Tracer {
	t.idmu.Lock()
	defer t.idmu.Unlock()
	t.det = on
	t.seq = 0
	return t
}

// AddExporter registers an additional exporter.
func (t *Tracer) AddExporter(e Exporter) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.exporters = append(t.exporters, e)
}

// Clock exposes the tracer's clock so components share one time source.
func (t *Tracer) Clock() Clock { return t.clock }

type ctxKey struct{}

// Start begins a span, parenting it to any span already in ctx.
func (t *Tracer) Start(ctx context.Context, name string, kind SpanKind) (context.Context, *Span) {
	parent, _ := ctx.Value(ctxKey{}).(*Span)
	sp := &Span{tr: t, parent: parent}
	sp.data = SpanData{
		Name:     name,
		Kind:     kind,
		Start:    t.clock.Now(),
		Status:   StatusUnset,
		Resource: t.resource,
	}
	if parent != nil {
		sp.data.TraceID = parent.data.TraceID
		sp.data.ParentID = parent.data.SpanID
		// Inherit principal identity down the trajectory so every span can be
		// attributed without the caller restating it.
		for _, k := range []string{AttrPrincipalSubject, AttrPrincipalTenant, AttrRunID} {
			if v, ok := parent.data.Attributes[k]; ok {
				sp.SetAttr(k, v)
			}
		}
	} else if rc, ok := ctx.Value(remoteKey{}).(SpanContext); ok && rc.Valid() {
		sp.data.TraceID = rc.TraceID
		sp.data.ParentID = rc.SpanID
	} else {
		sp.data.TraceID = t.newID(16)
	}
	sp.data.SpanID = t.newID(8)
	return context.WithValue(ctx, ctxKey{}, sp), sp
}

// SpanFromContext returns the active span, or nil.
func SpanFromContext(ctx context.Context) *Span {
	sp, _ := ctx.Value(ctxKey{}).(*Span)
	return sp
}

type remoteKey struct{}

// WithRemoteContext seeds ctx with an upstream span context, so a span started
// in this process joins the caller's trace.
func WithRemoteContext(ctx context.Context, sc SpanContext) context.Context {
	return context.WithValue(ctx, remoteKey{}, sc)
}

func (t *Tracer) newID(n int) string {
	t.idmu.Lock()
	defer t.idmu.Unlock()
	if t.det {
		t.seq++
		b := make([]byte, n)
		v := t.seq
		for i := n - 1; i >= 0; i-- {
			b[i] = byte(v)
			v >>= 8
		}
		return hex.EncodeToString(b)
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not survivable for an audited system.
		panic(fmt.Sprintf("obs: entropy unavailable: %v", err))
	}
	return hex.EncodeToString(b)
}

func (t *Tracer) export(d SpanData) {
	t.mu.RLock()
	exps := make([]Exporter, len(t.exporters))
	copy(exps, t.exporters)
	t.mu.RUnlock()
	for _, e := range exps {
		e.Export(d)
	}
}

// MemoryExporter keeps spans in memory for assertions, eval scoring and the
// drift detector.
type MemoryExporter struct {
	mu    sync.RWMutex
	spans []SpanData
	limit int
}

// NewMemoryExporter returns an exporter retaining at most limit spans
// (0 means unbounded).
func NewMemoryExporter(limit int) *MemoryExporter { return &MemoryExporter{limit: limit} }

// Export stores a span.
func (m *MemoryExporter) Export(d SpanData) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spans = append(m.spans, d)
	if m.limit > 0 && len(m.spans) > m.limit {
		m.spans = m.spans[len(m.spans)-m.limit:]
	}
}

// Spans returns a copy of the retained spans.
func (m *MemoryExporter) Spans() []SpanData {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SpanData, len(m.spans))
	copy(out, m.spans)
	return out
}

// Trace returns the spans of one trace, ordered by start time.
func (m *MemoryExporter) Trace(traceID string) []SpanData {
	out := make([]SpanData, 0, 8)
	for _, s := range m.Spans() {
		if s.TraceID == traceID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// Traces groups retained spans by trace id.
func (m *MemoryExporter) Traces() map[string][]SpanData {
	out := map[string][]SpanData{}
	for _, s := range m.Spans() {
		out[s.TraceID] = append(out[s.TraceID], s)
	}
	for k := range out {
		v := out[k]
		sort.Slice(v, func(i, j int) bool { return v[i].Start.Before(v[j].Start) })
		out[k] = v
	}
	return out
}

// Reset drops retained spans.
func (m *MemoryExporter) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spans = nil
}

// WriterExporter writes one OTLP-shaped JSON object per span, which is the
// on-disk format the eval harness and the drift detector read back.
type WriterExporter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriterExporter wraps w.
func NewWriterExporter(w io.Writer) *WriterExporter { return &WriterExporter{w: w} }

// Export writes the span as a JSON line.
func (e *WriterExporter) Export(d SpanData) {
	b, err := json.Marshal(d)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = e.w.Write(append(b, '\n'))
}

// MultiExporter fans spans out to several exporters.
type MultiExporter []Exporter

// Export forwards the span to each child.
func (m MultiExporter) Export(d SpanData) {
	for _, e := range m {
		e.Export(d)
	}
}
