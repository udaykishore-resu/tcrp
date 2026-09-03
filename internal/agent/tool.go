// Package agent is the governed agent runtime.
//
// The loop is deliberately ours rather than a framework's. Every step passes
// through the same four controls in the same order — guardrail, policy, human
// approval where the tier demands it, audit — and the ordering is the security
// property. A framework that lets a tool be called before the gateway has
// answered is not a governed runtime, however good its ergonomics.
package agent

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Tier is a tool's risk tier. The tier, not the tool name, is what policy,
// guardrails and approvals are written against, so adding a tool to an existing
// tier inherits its controls rather than requiring a new rule.
type Tier string

// Risk tiers.
const (
	// TierRead answers questions. It cannot change any state.
	TierRead Tier = "read_only"
	// TierState writes a proposal, draft or recommendation that a human will
	// act on. Nothing settles.
	TierState Tier = "state_change"
	// TierMoney moves value or issues an obligation. Always human-approved.
	TierMoney Tier = "money_movement"
)

// Rank orders tiers by blast radius.
func (t Tier) Rank() int {
	switch t {
	case TierMoney:
		return 3
	case TierState:
		return 2
	case TierRead:
		return 1
	default:
		return 0
	}
}

// Valid reports whether t is a declared tier.
func (t Tier) Valid() bool { return t.Rank() > 0 }

// Param describes one tool argument.
type Param struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

// ToolSpec is a tool's declared contract.
type ToolSpec struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Tier           Tier     `json:"tier"`
	RequiredScopes []string `json:"required_scopes"`
	Reversible     bool     `json:"reversible"`
	Params         []Param  `json:"params,omitempty"`
	// ResourceKind lets the policy gateway reason about what is touched
	// without parsing arguments it does not understand.
	ResourceKind string `json:"resource_kind,omitempty"`
}

// JSONSchema renders the spec as a JSON Schema object, which is the form a
// model API expects.
func (s ToolSpec) JSONSchema() map[string]any {
	props := map[string]any{}
	var required []string
	for _, p := range s.Params {
		entry := map[string]any{"type": p.Type}
		if p.Description != "" {
			entry["description"] = p.Description
		}
		if len(p.Enum) > 0 {
			entry["enum"] = p.Enum
		}
		props[p.Name] = entry
		if p.Required {
			required = append(required, p.Name)
		}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// ToolResult is what a tool returns.
type ToolResult struct {
	// Content is the natural-language rendering shown to the model.
	Content string `json:"content"`
	// Data is the structured result, recorded in the audit trail.
	Data map[string]any `json:"data,omitempty"`
	// Grounded lists every number this result authorises the model to state.
	// The output guardrail rejects any figure that is not in this set, which is
	// how the platform makes an invented exposure number impossible rather
	// than merely unlikely.
	Grounded []string `json:"grounded,omitempty"`
	// Effect names the state change, if any, for the audit record.
	Effect string `json:"effect,omitempty"`
	// Retryable marks a failure the agent may sensibly retry.
	Retryable bool `json:"retryable,omitempty"`
}

// Tool is an executable capability.
type Tool interface {
	Spec() ToolSpec
	Invoke(ctx context.Context, args map[string]any) (ToolResult, error)
}

// ToolFunc adapts a function to the Tool interface.
type ToolFunc struct {
	S ToolSpec
	F func(ctx context.Context, args map[string]any) (ToolResult, error)
}

// Spec returns the tool's contract.
func (t ToolFunc) Spec() ToolSpec { return t.S }

// Invoke runs the tool.
func (t ToolFunc) Invoke(ctx context.Context, args map[string]any) (ToolResult, error) {
	return t.F(ctx, args)
}

// Registry holds the tools available to a runtime.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

// Register adds a tool, rejecting a spec that does not declare a valid tier or
// the scopes it needs. A tool with no declared scope on a state-changing tier
// is a tool nobody can authorize correctly, so it is refused at registration.
func (r *Registry) Register(t Tool) error {
	s := t.Spec()
	if s.Name == "" {
		return fmt.Errorf("agent: tool has no name")
	}
	if !s.Tier.Valid() {
		return fmt.Errorf("agent: tool %q declares no valid tier", s.Name)
	}
	if s.Tier != TierRead && len(s.RequiredScopes) == 0 {
		return fmt.Errorf("agent: tool %q is tier %s and must declare required scopes", s.Name, s.Tier)
	}
	if s.Tier == TierMoney && s.Reversible {
		return fmt.Errorf("agent: tool %q claims money movement is reversible", s.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[s.Name]; exists {
		return fmt.Errorf("agent: tool %q already registered", s.Name)
	}
	r.tools[s.Name] = t
	return nil
}

// MustRegister registers tools and panics on error.
func (r *Registry) MustRegister(ts ...Tool) {
	for _, t := range ts {
		if err := r.Register(t); err != nil {
			panic(err)
		}
	}
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Specs returns every spec, ordered by tier then name, so a model always sees
// the same tool list and a trajectory is comparable across runs.
func (r *Registry) Specs() []ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ToolSpec, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t.Spec())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tier.Rank() != out[j].Tier.Rank() {
			return out[i].Tier.Rank() < out[j].Tier.Rank()
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// SpecsForTiers returns the specs at or below the highest permitted tier. A
// principal who cannot move money is never shown the tools that do, which
// removes a whole class of failure before the policy gateway is even consulted.
func (r *Registry) SpecsForTiers(max Tier) []ToolSpec {
	all := r.Specs()
	out := make([]ToolSpec, 0, len(all))
	for _, s := range all {
		if s.Tier.Rank() <= max.Rank() {
			out = append(out, s)
		}
	}
	return out
}

// Names returns registered tool names in a stable order.
func (r *Registry) Names() []string {
	specs := r.Specs()
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// Len returns the number of registered tools.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}
