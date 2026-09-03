package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/identity"
	"github.com/udaykishore-resu/tcrp/internal/policy"
)

// RunStatus is the state of an agent run.
type RunStatus string

// Run states. The set is closed and the transitions are checked, because
// "awaiting approval" is a state a system must survive a restart in.
const (
	StatusPending   RunStatus = "pending"
	StatusRunning   RunStatus = "running"
	StatusAwaiting  RunStatus = "awaiting_approval"
	StatusCompleted RunStatus = "completed"
	StatusFailed    RunStatus = "failed"
	StatusBlocked   RunStatus = "blocked"
	StatusRejected  RunStatus = "rejected"
)

// Terminal reports whether the run can no longer progress.
func (s RunStatus) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusBlocked, StatusRejected:
		return true
	}
	return false
}

// Errors returned by this package.
var (
	ErrRunNotFound      = errors.New("agent: run not found")
	ErrApprovalNotFound = errors.New("agent: approval not found")
	ErrNotAwaiting      = errors.New("agent: run is not awaiting approval")
	ErrBadTransition    = errors.New("agent: illegal state transition")
)

// PendingCall is the tool call a run is paused on.
type PendingCall struct {
	ApprovalID  string          `json:"approval_id"`
	ToolCallID  string          `json:"tool_call_id"`
	ToolName    string          `json:"tool_name"`
	Tier        Tier            `json:"tier"`
	Args        map[string]any  `json:"args,omitempty"`
	Decision    policy.Decision `json:"decision"`
	Summary     string          `json:"summary"`
	RequestedAt time.Time       `json:"requested_at"`
}

// StepRecord is one governed step, retained in full so a trajectory can be
// replayed and scored without re-running the model.
type StepRecord struct {
	N            int            `json:"n"`
	Tool         string         `json:"tool,omitempty"`
	Tier         Tier           `json:"tier,omitempty"`
	Args         map[string]any `json:"args,omitempty"`
	Reasoning    string         `json:"reasoning,omitempty"`
	PolicyAllow  bool           `json:"policy_allow"`
	PolicyRule   string         `json:"policy_rule,omitempty"`
	PolicyReason string         `json:"policy_reason,omitempty"`
	Obligations  []string       `json:"obligations,omitempty"`
	GuardVerdict string         `json:"guard_verdict,omitempty"`
	GuardReason  string         `json:"guard_reason,omitempty"`
	ApprovalID   string         `json:"approval_id,omitempty"`
	ApprovedBy   []string       `json:"approved_by,omitempty"`
	Outcome      string         `json:"outcome"`
	Error        string         `json:"error,omitempty"`
	DurationMS   float64        `json:"duration_ms"`
	SpanID       string         `json:"span_id,omitempty"`
	At           time.Time      `json:"at"`
}

// Step outcomes.
const (
	OutcomeInvoked      = "invoked"
	OutcomeDenied       = "policy_denied"
	OutcomeBlocked      = "guard_blocked"
	OutcomeAwaiting     = "awaiting_approval"
	OutcomeToolError    = "tool_error"
	OutcomeAnswered     = "answered"
	OutcomeUnknownTool  = "unknown_tool"
	OutcomeBudgetExceed = "step_budget_exceeded"
)

// Run is a governed agent execution.
type Run struct {
	ID               string             `json:"id"`
	Query            string             `json:"query"`
	Principal        identity.Principal `json:"principal"`
	Environment      string             `json:"environment"`
	Status           RunStatus          `json:"status"`
	TraceID          string             `json:"trace_id,omitempty"`
	Steps            []StepRecord       `json:"steps"`
	Messages         []Message          `json:"messages"`
	ToolData         []map[string]any   `json:"tool_data,omitempty"`
	Grounded         []string           `json:"grounded,omitempty"`
	Answer           string             `json:"answer,omitempty"`
	Error            string             `json:"error,omitempty"`
	Pending          *PendingCall       `json:"pending,omitempty"`
	ApprovalIDs      []string           `json:"approval_ids,omitempty"`
	Failures         int                `json:"failures"`
	RetrievalTrusted bool               `json:"retrieval_trusted"`
	CreatedAt        time.Time          `json:"created_at"`
	UpdatedAt        time.Time          `json:"updated_at"`
	ModelName        string             `json:"model"`
	PolicyBundle     string             `json:"policy_bundle,omitempty"`
	GuardPolicy      string             `json:"guard_policy,omitempty"`
}

// Trajectory returns the tool names called, in order — the sequence the eval
// harness scores against an expected trajectory.
func (r *Run) Trajectory() []string {
	out := make([]string, 0, len(r.Steps))
	for _, s := range r.Steps {
		if s.Tool != "" && s.Outcome == OutcomeInvoked {
			out = append(out, s.Tool)
		}
	}
	return out
}

// AttemptedTools returns every tool the model tried to call, including the ones
// the gateway refused. Scoring only the successful calls would hide exactly the
// behaviour worth measuring.
func (r *Run) AttemptedTools() []string {
	out := make([]string, 0, len(r.Steps))
	for _, s := range r.Steps {
		if s.Tool != "" {
			out = append(out, s.Tool)
		}
	}
	return out
}

// Denials returns the steps the policy gateway refused.
func (r *Run) Denials() []StepRecord {
	var out []StepRecord
	for _, s := range r.Steps {
		if s.Outcome == OutcomeDenied {
			out = append(out, s)
		}
	}
	return out
}

// Clone returns a deep copy, so a caller can inspect a run without racing the
// runtime that is still writing to it.
func (r *Run) Clone() *Run {
	b, err := json.Marshal(r)
	if err != nil {
		return r
	}
	var out Run
	if err := json.Unmarshal(b, &out); err != nil {
		return r
	}
	return &out
}

// Summary renders a one-line description.
func (r *Run) Summary() string {
	return fmt.Sprintf("%s %s [%s] tools=%s", r.ID, r.Status, r.Principal.Subject, strings.Join(r.Trajectory(), "→"))
}

// Store persists runs and approvals across a restart.
//
// In production this is Postgres; migrations/0001_agent.sql carries the schema
// and the uniqueness constraints that make double-approval impossible at the
// database level rather than only in application code. The file store below is
// the same contract with the same semantics, so the platform runs from a clean
// clone and the interface is exercised by the same tests.
type Store interface {
	SaveRun(ctx context.Context, r *Run) error
	LoadRun(ctx context.Context, id string) (*Run, error)
	ListRuns(ctx context.Context) ([]*Run, error)
	SaveApproval(ctx context.Context, a *Approval) error
	LoadApproval(ctx context.Context, id string) (*Approval, error)
	ListApprovals(ctx context.Context) ([]*Approval, error)
}

// MemoryStore keeps state in memory and, when a directory is configured, also
// on disk as JSON so a restart resumes cleanly.
type MemoryStore struct {
	mu        sync.RWMutex
	runs      map[string]*Run
	approvals map[string]*Approval
	dir       string
}

// NewMemoryStore returns an in-memory store. If dir is non-empty, every write
// is mirrored to disk and the store is rehydrated from it on construction.
func NewMemoryStore(dir string) (*MemoryStore, error) {
	s := &MemoryStore{runs: map[string]*Run{}, approvals: map[string]*Approval{}, dir: dir}
	if dir == "" {
		return s, nil
	}
	for _, sub := range []string{"runs", "approvals"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, err
		}
	}
	if err := s.rehydrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *MemoryStore) rehydrate() error {
	runs, _ := filepath.Glob(filepath.Join(s.dir, "runs", "*.json"))
	for _, p := range runs {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var r Run
		if json.Unmarshal(b, &r) == nil && r.ID != "" {
			s.runs[r.ID] = &r
		}
	}
	aps, _ := filepath.Glob(filepath.Join(s.dir, "approvals", "*.json"))
	for _, p := range aps {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var a Approval
		if json.Unmarshal(b, &a) == nil && a.ID != "" {
			s.approvals[a.ID] = &a
		}
	}
	return nil
}

func (s *MemoryStore) persist(sub, id string, v any) error {
	if s.dir == "" {
		return nil
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, sub, id+".json.tmp")
	final := filepath.Join(s.dir, sub, id+".json")
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// SaveRun writes a run.
func (s *MemoryStore) SaveRun(ctx context.Context, r *Run) error {
	s.mu.Lock()
	s.runs[r.ID] = r.Clone()
	s.mu.Unlock()
	return s.persist("runs", r.ID, r)
}

// LoadRun reads a run.
func (s *MemoryStore) LoadRun(ctx context.Context, id string) (*Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runs[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrRunNotFound, id)
	}
	return r.Clone(), nil
}

// ListRuns returns runs ordered by creation time.
func (s *MemoryStore) ListRuns(ctx context.Context) ([]*Run, error) {
	s.mu.RLock()
	out := make([]*Run, 0, len(s.runs))
	for _, r := range s.runs {
		out = append(out, r.Clone())
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// SaveApproval writes an approval.
func (s *MemoryStore) SaveApproval(ctx context.Context, a *Approval) error {
	s.mu.Lock()
	s.approvals[a.ID] = a.Clone()
	s.mu.Unlock()
	return s.persist("approvals", a.ID, a)
}

// LoadApproval reads an approval.
func (s *MemoryStore) LoadApproval(ctx context.Context, id string) (*Approval, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.approvals[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrApprovalNotFound, id)
	}
	return a.Clone(), nil
}

// ListApprovals returns approvals ordered by creation time.
func (s *MemoryStore) ListApprovals(ctx context.Context) ([]*Approval, error) {
	s.mu.RLock()
	out := make([]*Approval, 0, len(s.approvals))
	for _, a := range s.approvals {
		out = append(out, a.Clone())
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}
