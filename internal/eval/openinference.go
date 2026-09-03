package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// OpenInference attribute keys.
//
// This is a read-only export. Nothing in the platform reads these spans back,
// no control depends on them, and removing this file would change no behaviour:
// its whole purpose is that an OTel-native viewer such as Phoenix can render a
// trajectory without anyone writing a translation layer by hand. The platform's
// own spans stay on the OpenTelemetry GenAI conventions in internal/obs; this
// is a projection of them, not a second source of truth.
const (
	OISpanKind     = "openinference.span.kind"
	OIInputValue   = "input.value"
	OIOutputValue  = "output.value"
	OIToolName     = "tool.name"
	OILLMModelName = "llm.model_name"
	OISessionID    = "session.id"
	OIUserID       = "user.id"
)

// OpenInference span kinds.
const (
	OIKindAgent = "AGENT"
	OIKindLLM   = "LLM"
	OIKindTool  = "TOOL"
	OIKindChain = "CHAIN"
)

// OISpan is one span in the OpenInference shape.
type OISpan struct {
	TraceID    string         `json:"trace_id"`
	SpanID     string         `json:"span_id"`
	ParentID   string         `json:"parent_id,omitempty"`
	Name       string         `json:"name"`
	StartTime  time.Time      `json:"start_time"`
	EndTime    time.Time      `json:"end_time"`
	StatusCode string         `json:"status_code"`
	StatusMsg  string         `json:"status_message,omitempty"`
	Attributes map[string]any `json:"attributes"`
}

// SpanKindOf classifies a platform span.
//
// The mapping is by span name and by which attributes are present, because
// those are the only things the exporter can rely on: a tool span always names
// the tool, a run span always names the agent, and anything else is a chain
// rather than being guessed at.
func SpanKindOf(s obs.SpanData) string {
	switch {
	case strings.HasPrefix(s.Name, "agent.tool.") || s.Attr(obs.AttrGenAIToolName) != "":
		return OIKindTool
	case s.Name == "agent.run" || s.Name == "agent.run.resume" || s.Attr(obs.AttrGenAIAgentName) != "":
		return OIKindAgent
	case s.Name == "agent.step" || s.Attr(obs.AttrGenAIOperationName) == "chat":
		return OIKindLLM
	default:
		return OIKindChain
	}
}

// ToOpenInference projects a platform span into the OpenInference convention.
// Every original attribute is carried through unchanged alongside the mapped
// ones, so nothing is lost by exporting and a viewer that understands the
// GenAI conventions still sees them.
func ToOpenInference(s obs.SpanData) OISpan {
	attrs := make(map[string]any, len(s.Attributes)+8)
	for k, v := range s.Attributes {
		attrs[k] = v
	}
	attrs[OISpanKind] = SpanKindOf(s)

	if v := s.Attr(obs.AttrGenAIToolName); v != "" {
		attrs[OIToolName] = v
	}
	if v := s.Attr(obs.AttrGenAIRequestModel); v != "" {
		attrs[OILLMModelName] = v
	}
	if v := s.Attr(obs.AttrRunID); v != "" {
		attrs[OISessionID] = v
	} else if s.TraceID != "" {
		attrs[OISessionID] = s.TraceID
	}
	if v := s.Attr(obs.AttrPrincipalSubject); v != "" {
		attrs[OIUserID] = v
	}
	if v := inputValue(s); v != "" {
		attrs[OIInputValue] = v
	}
	if v := outputValue(s); v != "" {
		attrs[OIOutputValue] = v
	}

	return OISpan{
		TraceID:    s.TraceID,
		SpanID:     s.SpanID,
		ParentID:   s.ParentID,
		Name:       s.Name,
		StartTime:  s.Start.UTC(),
		EndTime:    s.End.UTC(),
		StatusCode: string(s.Status),
		StatusMsg:  s.StatusMsg,
		Attributes: attrs,
	}
}

// inputValue reconstructs what went into the span from the attributes the
// runtime records. The platform does not put prompts or tool arguments on
// spans — that is a deliberate choice about what a trace may contain — so the
// input here is the operation and its subject, not the payload.
func inputValue(s obs.SpanData) string {
	var parts []string
	if v := s.Attr(obs.AttrGenAIOperationName); v != "" {
		parts = append(parts, "operation="+v)
	}
	if v := s.Attr(obs.AttrGenAIToolName); v != "" {
		parts = append(parts, "tool="+v)
	}
	if v := s.Attr(obs.AttrToolTier); v != "" {
		parts = append(parts, "tier="+v)
	}
	if v := s.Attr(obs.AttrGenAIToolCallID); v != "" {
		parts = append(parts, "call_id="+v)
	}
	return strings.Join(parts, " ")
}

// outputValue reconstructs the span's result from the same attributes.
func outputValue(s obs.SpanData) string {
	var parts []string
	if v := s.Attr(attrRunStatus); v != "" {
		parts = append(parts, "status="+v)
	}
	if v := s.Attr(attrRunTools); v != "" {
		parts = append(parts, "trajectory="+v)
	}
	if v := s.Attr(obs.AttrPolicyDecision); v != "" {
		parts = append(parts, "policy="+v)
	}
	if v := s.Attr(obs.AttrPolicyRule); v != "" {
		parts = append(parts, "rule="+v)
	}
	if v := s.Attr(obs.AttrGuardVerdict); v != "" {
		parts = append(parts, "guard="+v)
	}
	if v := s.Attr(obs.AttrApprovalID); v != "" {
		parts = append(parts, "approval="+v)
	}
	if s.StatusMsg != "" {
		parts = append(parts, "message="+s.StatusMsg)
	}
	return strings.Join(parts, " ")
}

// WriteOpenInference writes the spans as JSON lines, one span per line, in the
// order given. JSON lines rather than one array because a trace file is
// appended to and tailed, and an array cannot be either.
func WriteOpenInference(w io.Writer, spans []obs.SpanData) error {
	enc := json.NewEncoder(w)
	for _, s := range spans {
		if err := enc.Encode(ToOpenInference(s)); err != nil {
			return fmt.Errorf("eval: writing OpenInference span %s: %w", s.SpanID, err)
		}
	}
	return nil
}
