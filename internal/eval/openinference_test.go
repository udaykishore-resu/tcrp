package eval

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/obs"
)

func TestSpanKindOf(t *testing.T) {
	tests := []struct {
		name string
		span obs.SpanData
		want string
	}{
		{"run span", obs.SpanData{Name: "agent.run"}, OIKindAgent},
		{"resume span", obs.SpanData{Name: "agent.run.resume"}, OIKindAgent},
		{"agent name attribute", obs.SpanData{Name: "something", Attributes: map[string]any{obs.AttrGenAIAgentName: "collateral-ops"}}, OIKindAgent},
		{"tool span", obs.SpanData{Name: "agent.tool.get_exposure"}, OIKindTool},
		{"tool name attribute wins over step", obs.SpanData{Name: "agent.step", Attributes: map[string]any{obs.AttrGenAIToolName: "get_exposure"}}, OIKindTool},
		{"step span", obs.SpanData{Name: "agent.step"}, OIKindLLM},
		{"chat operation", obs.SpanData{Name: "model.call", Attributes: map[string]any{obs.AttrGenAIOperationName: "chat"}}, OIKindLLM},
		{"anything else", obs.SpanData{Name: "http.request"}, OIKindChain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SpanKindOf(tt.span); got != tt.want {
				t.Fatalf("SpanKindOf = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestToOpenInference(t *testing.T) {
	start := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	in := obs.SpanData{
		TraceID: "trace-1", SpanID: "span-1", ParentID: "span-0",
		Name: "agent.tool.execute_transfer", Kind: obs.KindClient,
		Start: start, End: start.Add(12 * time.Millisecond), DurationNS: int64(12 * time.Millisecond),
		Status: obs.StatusOK,
		Attributes: map[string]any{
			obs.AttrRunID:              "run-1",
			obs.AttrGenAIToolName:      "execute_transfer",
			obs.AttrGenAIToolCallID:    "call_3",
			obs.AttrGenAIRequestModel:  "planner/deterministic-v1",
			obs.AttrGenAIOperationName: "execute_tool",
			obs.AttrPrincipalSubject:   "u.treasury",
			obs.AttrToolTier:           "money_movement",
			obs.AttrPolicyRule:         "allow-money-movement-single-approved",
			obs.AttrApprovalID:         "apr_1",
		},
	}

	out := ToOpenInference(in)
	if out.Attributes[OISpanKind] != OIKindTool {
		t.Fatalf("span kind = %v, want %s", out.Attributes[OISpanKind], OIKindTool)
	}
	checks := map[string]string{
		OIToolName:     "execute_transfer",
		OILLMModelName: "planner/deterministic-v1",
		OISessionID:    "run-1",
		OIUserID:       "u.treasury",
	}
	for k, want := range checks {
		if got := out.Attributes[k]; got != want {
			t.Fatalf("%s = %v, want %q", k, got, want)
		}
	}
	input, _ := out.Attributes[OIInputValue].(string)
	for _, want := range []string{"operation=execute_tool", "tool=execute_transfer", "tier=money_movement", "call_id=call_3"} {
		if !strings.Contains(input, want) {
			t.Fatalf("%s = %q, want it to contain %q", OIInputValue, input, want)
		}
	}
	output, _ := out.Attributes[OIOutputValue].(string)
	for _, want := range []string{"rule=allow-money-movement-single-approved", "approval=apr_1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("%s = %q, want it to contain %q", OIOutputValue, output, want)
		}
	}
	// The original attributes must survive the projection.
	if out.Attributes[obs.AttrToolTier] != "money_movement" {
		t.Fatal("the original OTel attributes must be carried through unchanged")
	}
	if out.TraceID != in.TraceID || out.SpanID != in.SpanID || out.ParentID != in.ParentID {
		t.Fatal("span identity must survive the projection")
	}
	if !out.StartTime.Equal(start) || !out.EndTime.Equal(start.Add(12*time.Millisecond)) {
		t.Fatal("span timing must survive the projection")
	}
}

func TestToOpenInferenceFallsBackToTheTraceID(t *testing.T) {
	out := ToOpenInference(obs.SpanData{TraceID: "trace-9", SpanID: "span-9", Name: "agent.run"})
	if out.Attributes[OISessionID] != "trace-9" {
		t.Fatalf("session.id = %v, want the trace id when no run id is present", out.Attributes[OISessionID])
	}
	if _, ok := out.Attributes[OIUserID]; ok {
		t.Fatal("user.id must be absent when no principal is on the span")
	}
}

func TestWriteOpenInferenceIsJSONLines(t *testing.T) {
	spans := []obs.SpanData{
		{TraceID: "t", SpanID: "s1", Name: "agent.run", Attributes: map[string]any{obs.AttrRunID: "r1"}},
		{TraceID: "t", SpanID: "s2", Name: "agent.step", Attributes: map[string]any{obs.AttrRunID: "r1"}},
		{TraceID: "t", SpanID: "s3", Name: "agent.tool.get_exposure", Attributes: map[string]any{
			obs.AttrRunID: "r1", obs.AttrGenAIToolName: "get_exposure",
		}},
	}
	var buf bytes.Buffer
	if err := WriteOpenInference(&buf, spans); err != nil {
		t.Fatalf("WriteOpenInference: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != len(spans) {
		t.Fatalf("wrote %d lines, want %d", len(lines), len(spans))
	}
	wantKinds := []string{OIKindAgent, OIKindLLM, OIKindTool}
	for i, line := range lines {
		var got OISpan
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d is not JSON: %v", i, err)
		}
		if got.SpanID != spans[i].SpanID {
			t.Fatalf("line %d has span %s, want %s (order must be preserved)", i, got.SpanID, spans[i].SpanID)
		}
		if got.Attributes[OISpanKind] != wantKinds[i] {
			t.Fatalf("line %d kind = %v, want %s", i, got.Attributes[OISpanKind], wantKinds[i])
		}
	}

	// Byte-stable: the export is read by CI diffs as well as by a viewer.
	var again bytes.Buffer
	if err := WriteOpenInference(&again, spans); err != nil {
		t.Fatalf("WriteOpenInference: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), again.Bytes()) {
		t.Fatal("the OpenInference export must be byte-identical across calls")
	}

	if err := WriteOpenInference(&buf, nil); err != nil {
		t.Fatalf("WriteOpenInference with no spans: %v", err)
	}
}
