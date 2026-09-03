package agent

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/udaykishore-resu/tcrp/internal/identity"
)

// Role names the author of a message.
type Role string

// Message roles.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
	RoleSystem    Role = "system"
)

// Message is one turn in the conversation.
type Message struct {
	Role       Role   `json:"role"`
	Content    string `json:"content"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
}

// ToolCall is a model's request to invoke a tool.
type ToolCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// Step is what a model produces: either a tool call or a final answer.
type Step struct {
	Reasoning string    `json:"reasoning,omitempty"`
	Call      *ToolCall `json:"call,omitempty"`
	Final     string    `json:"final,omitempty"`
}

// State is what the model sees.
type State struct {
	Query     string
	Messages  []Message
	Tools     []ToolSpec
	Principal identity.Principal
	Step      int
	Failures  int

	// toolData carries the structured results of earlier successful calls, so
	// a later argument can be taken from an engine's output rather than from
	// the model's recollection of it.
	toolData []map[string]any
}

// AddToolData records a tool's structured result for later steps.
func (s *State) AddToolData(d map[string]any) {
	if len(d) > 0 {
		s.toolData = append(s.toolData, d)
	}
}

// ToolData returns the merged structured results so far.
func (s State) ToolData() map[string]any { return priorData(s) }

// LastToolError returns the most recent failed tool result, if the previous
// message was one.
func (s State) LastToolError() (Message, bool) {
	if len(s.Messages) == 0 {
		return Message{}, false
	}
	m := s.Messages[len(s.Messages)-1]
	if m.Role == RoleTool && m.IsError {
		return m, true
	}
	return Message{}, false
}

// CalledTools returns the tool names called so far, in order. This is the
// trajectory the eval harness scores.
func (s State) CalledTools() []string {
	var out []string
	for _, m := range s.Messages {
		if m.Role == RoleTool && m.ToolName != "" {
			out = append(out, m.ToolName)
		}
	}
	return out
}

// Model turns state into the next step.
type Model interface {
	Name() string
	Next(ctx context.Context, s State) (Step, error)
}

// ---------------------------------------------------------------------------
// Deterministic planner
// ---------------------------------------------------------------------------

// Planner is a deterministic, rule-based model.
//
// It exists so the platform's controls can be exercised, scored and regression
// tested without a network call or a nondeterministic sampler. Every property
// this repository claims about the runtime — that money movement always pauses
// for approval, that an injected instruction never reaches a tool, that a
// denied call is recovered from rather than retried forever — is a property of
// the runtime, not of the model, and must hold for any model. Running the suite
// against a deterministic planner is what proves that.
//
// Swap in AnthropicModel and the same assertions still hold; that is the test.
type Planner struct{}

// Name identifies the model.
func (Planner) Name() string { return "planner/deterministic-v1" }

var (
	reCounterparty = regexp.MustCompile(`(?i)\bCP-[A-Z0-9]{2,8}\b`)
	reAmount       = regexp.MustCompile(`(?i)([\d][\d,_]*(?:\.\d+)?)\s*(m|mm|million|k|bn|billion)?\b`)
	reDate         = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2})\b`)
	reBps          = regexp.MustCompile(`(?i)\b(\d{1,4})\s*(?:bps|basis points)\b`)
)

type plannedCall struct {
	tool string
	args func(q string, prior map[string]any) map[string]any
}

// Next returns the planner's next step.
func (p Planner) Next(ctx context.Context, s State) (Step, error) {
	q := strings.ToLower(s.Query)
	prior := priorData(s)

	// Error recovery. A denial is terminal for that path: the correct
	// behaviour is to stop and explain, not to try the same call again or to
	// look for another tool that does the same thing. A transient failure is
	// retried exactly once.
	if m, ok := s.LastToolError(); ok {
		if strings.Contains(m.Content, "policy denied") || strings.Contains(m.Content, "guardrail") {
			return Step{
				Reasoning: "the gateway refused this action; report the refusal rather than route around it",
				Final: "I could not complete that action. " + m.Content +
					" No state was changed. If this is expected to be permitted, the policy bundle or your scopes need to change — I cannot work around the control.",
			}, nil
		}
		if s.Failures <= 1 && strings.Contains(m.Content, "retryable") {
			if call := p.retry(m); call != nil {
				return Step{Reasoning: "transient tool failure; retrying once", Call: call}, nil
			}
		}
		if s.Failures >= 2 {
			return Step{
				Reasoning: "repeated tool failures; stop rather than loop",
				Final:     "I was unable to complete that request: " + m.Content + ". Nothing was changed.",
			}, nil
		}
	}

	plan := p.plan(q)
	// A request that asks to read something before acting reads it first. This
	// is also the path an indirect-injection attempt travels: the note is
	// fetched, the guardrail marks the retrieval untrusted, and the gateway
	// then refuses whatever the note was trying to cause.
	if containsAny(q, "note", "memo", "comment", "file on") && plan[0].tool != "get_counterparty_notes" {
		plan = append([]plannedCall{{"get_counterparty_notes", func(query string, _ map[string]any) map[string]any {
			return map[string]any{"counterparty": counterpartyOf(query)}
		}}}, plan...)
	}
	done := s.CalledTools()
	next := len(done)
	if next < len(plan) {
		call := plan[next]
		return Step{
			Reasoning: fmt.Sprintf("step %d of %d: %s", next+1, len(plan), call.tool),
			Call: &ToolCall{
				ID:   fmt.Sprintf("call_%d", next+1),
				Name: call.tool,
				Args: call.args(s.Query, prior),
			},
		}, nil
	}
	return Step{Reasoning: "plan complete", Final: p.summarize(s)}, nil
}

func (p Planner) retry(m Message) *ToolCall {
	if m.ToolName == "" {
		return nil
	}
	return &ToolCall{ID: "retry_" + m.ToolCallID, Name: m.ToolName, Args: map[string]any{"retry": true}}
}

// plan maps an intent to a tool sequence. The sequences are short and explicit
// because a collateral desk's questions are, and because an expected trajectory
// that a human can read is an expected trajectory the eval harness can score.
func (p Planner) plan(q string) []plannedCall {
	cp := func(query string, _ map[string]any) map[string]any {
		return map[string]any{"counterparty": counterpartyOf(query)}
	}

	switch {
	case containsAny(q, "margin call", "undermargined", "under-margined", "short of margin"):
		return []plannedCall{
			{"get_exposure", cp},
			{"get_collateral_value", cp},
			{"propose_margin_call", func(query string, prior map[string]any) map[string]any {
				return map[string]any{
					"counterparty": counterpartyOf(query),
					"shortfall":    prior["shortfall"],
					"currency":     firstNonEmpty(str(prior["currency"]), "USD"),
				}
			}},
			{"issue_margin_call", func(query string, prior map[string]any) map[string]any {
				return map[string]any{
					"counterparty": counterpartyOf(query),
					"amount":       prior["shortfall"],
					"currency":     firstNonEmpty(str(prior["currency"]), "USD"),
					"proposal_id":  prior["proposal_id"],
				}
			}},
		}

	case containsAny(q, "transfer", "move shares", "send shares", "deliver"):
		return []plannedCall{
			{"check_whitelist", func(query string, _ map[string]any) map[string]any {
				return map[string]any{"counterparty": counterpartyOf(query), "ledger": ledgerOf(query)}
			}},
			{"draft_transfer", func(query string, _ map[string]any) map[string]any {
				return map[string]any{
					"counterparty": counterpartyOf(query),
					"quantity":     amountOf(query),
					"ledger":       ledgerOf(query),
				}
			}},
			{"execute_transfer", func(query string, prior map[string]any) map[string]any {
				return map[string]any{
					"counterparty": counterpartyOf(query),
					"quantity":     amountOf(query),
					"ledger":       ledgerOf(query),
					"draft_id":     prior["draft_id"],
				}
			}},
		}

	case containsAny(q, "redeem", "redemption"):
		return []plannedCall{
			{"get_position", cp},
			{"redeem_shares", func(query string, _ map[string]any) map[string]any {
				return map[string]any{"counterparty": counterpartyOf(query), "quantity": amountOf(query)}
			}},
		}

	case containsAny(q, "stress", "shock", "scenario"):
		return []plannedCall{
			{"run_stress_scenario", func(query string, _ map[string]any) map[string]any {
				return map[string]any{"scenario": scenarioOf(query), "shock_bps": bpsOf(query)}
			}},
		}

	case containsAny(q, "concentration", "limit", "breach"):
		return []plannedCall{{"get_concentration", cp}}

	case containsAny(q, "break", "reconcil", "mismatch", "out of balance"):
		return []plannedCall{{"list_recon_breaks", func(string, map[string]any) map[string]any { return map[string]any{} }}}

	case containsAny(q, "as of", "as-of", "point in time", "yesterday", "last week"):
		return []plannedCall{{"query_asof", func(query string, _ map[string]any) map[string]any {
			return map[string]any{"counterparty": counterpartyOf(query), "as_of": dateOf(query)}
		}}}

	case containsAny(q, "collateral value", "haircut", "advance rate", "eligible collateral"):
		return []plannedCall{{"get_collateral_value", cp}}

	case containsAny(q, "position", "holdings", "balance"):
		return []plannedCall{{"get_position", cp}}

	case containsAny(q, "supply", "total supply", "across networks", "both ledgers"):
		return []plannedCall{{"get_supply_reconciliation", func(string, map[string]any) map[string]any { return map[string]any{} }}}

	case containsAny(q, "exposure", "net exposure", "risk to"):
		return []plannedCall{{"get_exposure", cp}}

	default:
		// Unrecognised intent: answer from the exposure summary rather than
		// guessing at a state-changing tool.
		return []plannedCall{{"get_exposure", cp}}
	}
}

func (p Planner) summarize(s State) string {
	var parts []string
	for _, m := range s.Messages {
		if m.Role == RoleTool && !m.IsError && strings.TrimSpace(m.Content) != "" {
			parts = append(parts, strings.TrimSpace(m.Content))
		}
	}
	if len(parts) == 0 {
		return "I could not retrieve anything for that request."
	}
	return strings.Join(parts, "\n")
}

// priorData merges the structured data returned by earlier tool calls, so a
// later step can reference a figure an earlier step produced without the model
// having to restate it — which is also what keeps the number grounded.
func priorData(s State) map[string]any {
	out := map[string]any{}
	for _, d := range s.toolData {
		for k, v := range d {
			out[k] = v
		}
	}
	return out
}

func containsAny(q string, subs ...string) bool {
	for _, s := range subs {
		if strings.Contains(q, s) {
			return true
		}
	}
	return false
}

func counterpartyOf(q string) string {
	if m := reCounterparty.FindString(q); m != "" {
		return strings.ToUpper(m)
	}
	return ""
}

func ledgerOf(q string) string {
	l := strings.ToLower(q)
	switch {
	case strings.Contains(l, "canton") || strings.Contains(l, "daml"):
		return "canton"
	default:
		return "evm"
	}
}

func dateOf(q string) string {
	if m := reDate.FindString(q); m != "" {
		return m
	}
	return ""
}

func bpsOf(q string) float64 {
	if m := reBps.FindStringSubmatch(q); len(m) == 2 {
		v, _ := strconv.ParseFloat(m[1], 64)
		return v
	}
	return 0
}

func scenarioOf(q string) string {
	l := strings.ToLower(q)
	switch {
	case strings.Contains(l, "redemption"):
		return "redemption_run"
	case strings.Contains(l, "haircut"):
		return "haircut_widening"
	case strings.Contains(l, "credit") || strings.Contains(l, "default"):
		return "counterparty_default"
	case strings.Contains(l, "rate"):
		return "rate_shock"
	default:
		return "rate_shock"
	}
}

// amountOf extracts a quantity, honouring the k/m/bn suffixes a desk actually
// types. It returns 0 when no quantity is present, and a tool that needs one
// rejects the call rather than inventing a default.
func amountOf(q string) float64 {
	for _, m := range reAmount.FindAllStringSubmatch(q, -1) {
		raw := strings.NewReplacer(",", "", "_", "").Replace(m[1])
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(m[2]) {
		case "k":
			v *= 1_000
		case "m", "mm", "million":
			v *= 1_000_000
		case "bn", "billion":
			v *= 1_000_000_000
		}
		if v >= 1 {
			return v
		}
	}
	return 0
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
