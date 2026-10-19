package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/identity"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// ApprovalStatus is the state of a human approval gate.
type ApprovalStatus string

// Approval states.
const (
	ApprovalPending ApprovalStatus = "pending"
	ApprovalGranted ApprovalStatus = "granted"
	ApprovalDenied  ApprovalStatus = "denied"
	ApprovalExpired ApprovalStatus = "expired"
)

// Errors returned by the approval service.
var (
	ErrApprovalClosed  = errors.New("agent: approval is no longer pending")
	ErrSelfApproval    = errors.New("agent: the requester may not approve their own action")
	ErrDuplicateGrant  = errors.New("agent: this approver has already responded")
	ErrApproverRole    = errors.New("agent: approver does not hold an authorising role")
	ErrApproverScope   = errors.New("agent: approver does not hold the approval scope")
	ErrApprovalExpired = errors.New("agent: approval window has closed")
)

// Grant is one human's response.
type Grant struct {
	By    string    `json:"by"`
	Roles []string  `json:"roles,omitempty"`
	At    time.Time `json:"at"`
	Note  string    `json:"note,omitempty"`
}

// Approval is a human-in-the-loop gate on one tool call.
//
// It is a first-class persisted object rather than a channel or a callback,
// because the run it gates may outlive the process that started it: an approver
// who responds forty minutes later, after a deploy, must still resume the same
// run against the same arguments.
type Approval struct {
	ID            string         `json:"id"`
	RunID         string         `json:"run_id"`
	Step          int            `json:"step"`
	Tool          string         `json:"tool"`
	Tier          Tier           `json:"tier"`
	Summary       string         `json:"summary"`
	Args          map[string]any `json:"args,omitempty"`
	Requester     string         `json:"requester"`
	OnBehalfOf    string         `json:"on_behalf_of,omitempty"`
	RequiredRoles []string       `json:"required_roles"`
	RequiredCount int            `json:"required_count"`
	Grants        []Grant        `json:"grants,omitempty"`
	Denials       []Grant        `json:"denials,omitempty"`
	Status        ApprovalStatus `json:"status"`
	CreatedAt     time.Time      `json:"created_at"`
	ExpiresAt     time.Time      `json:"expires_at"`
	DecidedAt     time.Time      `json:"decided_at,omitzero"`
	PolicyRule    string         `json:"policy_rule,omitempty"`
	ArgsHash      string         `json:"args_hash,omitempty"`
}

// Satisfied reports whether enough distinct approvals have been recorded.
func (a *Approval) Satisfied() bool { return len(a.Grants) >= a.RequiredCount }

// Approvers returns who approved.
func (a *Approval) Approvers() []string {
	out := make([]string, 0, len(a.Grants))
	for _, g := range a.Grants {
		out = append(out, g.By)
	}
	return out
}

// ApproverRoles returns the union of the approvers' roles, which is what the
// policy gateway checks at dispatch.
func (a *Approval) ApproverRoles() []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range a.Grants {
		for _, r := range g.Roles {
			if !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	return out
}

// LastGrantAt returns the time of the most recent grant.
func (a *Approval) LastGrantAt() time.Time {
	var t time.Time
	for _, g := range a.Grants {
		if g.At.After(t) {
			t = g.At
		}
	}
	return t
}

// Clone returns a deep copy.
func (a *Approval) Clone() *Approval {
	b, err := json.Marshal(a)
	if err != nil {
		return a
	}
	var out Approval
	if err := json.Unmarshal(b, &out); err != nil {
		return a
	}
	return &out
}

// ApprovalService manages the human gates.
type ApprovalService struct {
	store  Store
	clock  obs.Clock
	audit  *audit.Log
	tracer *obs.Tracer
	window time.Duration

	mu       sync.Mutex
	onResume func(ctx context.Context, runID string)
	waiters  map[string][]chan *Approval
}

// NewApprovalService builds the service.
func NewApprovalService(store Store, clock obs.Clock, log *audit.Log, tracer *obs.Tracer, window time.Duration) *ApprovalService {
	if clock == nil {
		clock = obs.RealClock{}
	}
	if window <= 0 {
		window = 15 * time.Minute
	}
	return &ApprovalService{store: store, clock: clock, audit: log, tracer: tracer, window: window, waiters: map[string][]chan *Approval{}}
}

// OnResume registers the callback fired when an approval becomes satisfied.
func (s *ApprovalService) OnResume(fn func(ctx context.Context, runID string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onResume = fn
}

// Request opens a gate.
func (s *ApprovalService) Request(ctx context.Context, a *Approval) (*Approval, error) {
	now := s.clock.Now()
	a.CreatedAt = now
	if a.ExpiresAt.IsZero() {
		a.ExpiresAt = now.Add(s.window)
	}
	if a.RequiredCount <= 0 {
		a.RequiredCount = 1
	}
	a.Status = ApprovalPending
	a.ArgsHash = audit.CanonicalJSON(a.Args)
	if err := s.store.SaveApproval(ctx, a); err != nil {
		return nil, err
	}
	if s.audit != nil {
		_, _ = s.audit.Append(audit.Record{
			Actor: a.Requester, OnBehalf: a.OnBehalfOf, Action: audit.ActionApprovalRequired,
			Subject: a.Tool, Outcome: audit.OutcomePending, RunID: a.RunID,
			TraceID: traceIDOf(ctx), SpanID: spanIDOf(ctx),
			Details: map[string]any{
				"approval_id": a.ID, "tier": string(a.Tier), "required": a.RequiredCount,
				"roles": a.RequiredRoles, "expires_at": a.ExpiresAt, "args": a.Args,
			},
		})
	}
	return a.Clone(), nil
}

// Get returns an approval.
func (s *ApprovalService) Get(ctx context.Context, id string) (*Approval, error) {
	return s.store.LoadApproval(ctx, id)
}

// Pending returns the open gates, oldest first.
func (s *ApprovalService) Pending(ctx context.Context) ([]*Approval, error) {
	all, err := s.store.ListApprovals(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Approval, 0, len(all))
	for _, a := range all {
		if a.Status == ApprovalPending {
			out = append(out, a)
		}
	}
	return out, nil
}

// Grant records one approval.
//
// The checks here are the four-eyes control, and they are enforced at the
// service rather than left to the caller: the requester cannot approve, an
// approver cannot approve twice, the approver must hold both an authorising
// role and the approval scope, and an expired gate cannot be revived.
func (s *ApprovalService) Grant(ctx context.Context, id string, p identity.Principal, note string) (*Approval, error) {
	a, err := s.store.LoadApproval(ctx, id)
	if err != nil {
		return nil, err
	}
	now := s.clock.Now()
	switch {
	case a.Status != ApprovalPending:
		return nil, fmt.Errorf("%w: %s", ErrApprovalClosed, a.Status)
	case now.After(a.ExpiresAt):
		a.Status = ApprovalExpired
		a.DecidedAt = now
		_ = s.store.SaveApproval(ctx, a)
		s.record(ctx, a, p, audit.ActionApprovalExpired, "expired")
		return nil, ErrApprovalExpired
	case p.Subject == a.Requester || (a.OnBehalfOf != "" && p.Subject == a.OnBehalfOf):
		s.record(ctx, a, p, audit.ActionApprovalDenied, "self-approval refused")
		return nil, ErrSelfApproval
	case !p.HasScope(identity.ScopeApprove):
		s.record(ctx, a, p, audit.ActionApprovalDenied, "approver lacks approvals:grant")
		return nil, ErrApproverScope
	case !hasAnyRole(p, a.RequiredRoles):
		s.record(ctx, a, p, audit.ActionApprovalDenied, "approver lacks an authorising role")
		return nil, ErrApproverRole
	}
	for _, g := range append(a.Grants, a.Denials...) {
		if g.By == p.Subject {
			return nil, ErrDuplicateGrant
		}
	}

	a.Grants = append(a.Grants, Grant{By: p.Subject, Roles: p.Roles, At: now, Note: note})
	if a.Satisfied() {
		a.Status = ApprovalGranted
		a.DecidedAt = now
	}
	if err := s.store.SaveApproval(ctx, a); err != nil {
		return nil, err
	}
	s.record(ctx, a, p, audit.ActionApprovalGranted, note)
	s.notify(a)

	if a.Status == ApprovalGranted {
		s.mu.Lock()
		fn := s.onResume
		s.mu.Unlock()
		if fn != nil {
			fn(ctx, a.RunID)
		}
	}
	return a.Clone(), nil
}

// Deny closes a gate against the action.
func (s *ApprovalService) Deny(ctx context.Context, id string, p identity.Principal, note string) (*Approval, error) {
	a, err := s.store.LoadApproval(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Status != ApprovalPending {
		return nil, fmt.Errorf("%w: %s", ErrApprovalClosed, a.Status)
	}
	if !hasAnyRole(p, a.RequiredRoles) {
		return nil, ErrApproverRole
	}
	now := s.clock.Now()
	a.Denials = append(a.Denials, Grant{By: p.Subject, Roles: p.Roles, At: now, Note: note})
	a.Status = ApprovalDenied
	a.DecidedAt = now
	if err := s.store.SaveApproval(ctx, a); err != nil {
		return nil, err
	}
	s.record(ctx, a, p, audit.ActionApprovalDenied, note)
	s.notify(a)

	s.mu.Lock()
	fn := s.onResume
	s.mu.Unlock()
	if fn != nil {
		fn(ctx, a.RunID)
	}
	return a.Clone(), nil
}

// ExpireDue closes gates whose window has passed and returns them.
func (s *ApprovalService) ExpireDue(ctx context.Context) ([]*Approval, error) {
	pend, err := s.Pending(ctx)
	if err != nil {
		return nil, err
	}
	now := s.clock.Now()
	var out []*Approval
	for _, a := range pend {
		if now.After(a.ExpiresAt) {
			a.Status = ApprovalExpired
			a.DecidedAt = now
			if err := s.store.SaveApproval(ctx, a); err != nil {
				return out, err
			}
			s.record(ctx, a, identity.Principal{Subject: "system"}, audit.ActionApprovalExpired, "window closed")
			s.notify(a)
			out = append(out, a)
		}
	}
	return out, nil
}

// Wait blocks until the approval reaches a terminal state or ctx is done. It is
// used by the synchronous CLI path; the HTTP path resumes via the callback.
//
// The waiter is registered before the approval is read. Reading first would
// leave a window in which a Grant commits and notifies between the read and
// the registration: Wait would then see a stale pending snapshot, miss the only
// notification there will ever be, and block until its context expires.
func (s *ApprovalService) Wait(ctx context.Context, id string) (*Approval, error) {
	ch := make(chan *Approval, 4)
	s.mu.Lock()
	s.waiters[id] = append(s.waiters[id], ch)
	s.mu.Unlock()
	defer s.unsubscribe(id, ch)

	a, err := s.store.LoadApproval(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Status != ApprovalPending {
		return a, nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case got := <-ch:
			if got.Status != ApprovalPending {
				return got, nil
			}
		}
	}
}

// unsubscribe removes one waiter, so a Wait that returns early (a terminal
// read, an error, a cancelled context) does not leave its channel behind.
func (s *ApprovalService) unsubscribe(id string, ch chan *Approval) {
	s.mu.Lock()
	defer s.mu.Unlock()
	chans := s.waiters[id]
	for i, c := range chans {
		if c == ch {
			chans = append(chans[:i], chans[i+1:]...)
			break
		}
	}
	if len(chans) == 0 {
		delete(s.waiters, id)
		return
	}
	s.waiters[id] = chans
}

func (s *ApprovalService) notify(a *Approval) {
	s.mu.Lock()
	chans := s.waiters[a.ID]
	if a.Status != ApprovalPending {
		delete(s.waiters, a.ID)
	}
	s.mu.Unlock()
	for _, c := range chans {
		select {
		case c <- a.Clone():
		default:
		}
	}
}

func (s *ApprovalService) record(ctx context.Context, a *Approval, p identity.Principal, action audit.Action, note string) {
	if s.audit == nil {
		return
	}
	outcome := audit.OutcomeOK
	if action == audit.ActionApprovalDenied || action == audit.ActionApprovalExpired {
		outcome = audit.OutcomeDenied
	}
	_, _ = s.audit.Append(audit.Record{
		Actor: p.Subject, Tenant: p.Tenant, Action: action, Subject: a.Tool,
		Outcome: outcome, RunID: a.RunID, TraceID: traceIDOf(ctx), SpanID: spanIDOf(ctx),
		Details: map[string]any{
			"approval_id": a.ID, "note": note, "roles": p.Roles,
			"grants": len(a.Grants), "required": a.RequiredCount, "status": string(a.Status),
			"args_hash": a.ArgsHash,
		},
	})
}

func hasAnyRole(p identity.Principal, roles []string) bool {
	if len(roles) == 0 {
		return true
	}
	for _, r := range roles {
		if p.HasRole(r) {
			return true
		}
	}
	return false
}

func traceIDOf(ctx context.Context) string {
	if sp := obs.SpanFromContext(ctx); sp != nil {
		return sp.Context().TraceID
	}
	return ""
}

func spanIDOf(ctx context.Context) string {
	if sp := obs.SpanFromContext(ctx); sp != nil {
		return sp.Context().SpanID
	}
	return ""
}
