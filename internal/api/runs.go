package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/agent"
	"github.com/udaykishore-resu/tcrp/internal/identity"
	"github.com/udaykishore-resu/tcrp/internal/obs"
	"github.com/udaykishore-resu/tcrp/internal/platform"
)

// StartRunRequest is the body of POST /v1/runs.
type StartRunRequest struct {
	// Query is the desk's question, in words.
	Query string `json:"query"`
	// Principal names the identity the run executes as, from
	// platform.PrincipalNames. It is empty for a run that executes as the
	// caller; see resolveRunPrincipal for when naming another one is permitted.
	Principal string `json:"principal,omitempty"`
}

// ToolChip is one step of a run's trajectory, reduced to what a reader scans:
// which tool, at which tier, and how it ended.
type ToolChip struct {
	Step    int    `json:"step"`
	Tool    string `json:"tool"`
	Tier    string `json:"tier"`
	Outcome string `json:"outcome"`
}

// RunSummary is a run as the list view shows it.
type RunSummary struct {
	ID              string     `json:"id"`
	Query           string     `json:"query"`
	Status          string     `json:"status"`
	Principal       string     `json:"principal"`
	OnBehalf        string     `json:"on_behalf,omitempty"`
	Model           string     `json:"model"`
	Environment     string     `json:"environment"`
	TraceID         string     `json:"trace_id,omitempty"`
	Steps           int        `json:"steps"`
	Trajectory      []ToolChip `json:"trajectory"`
	PendingApproval string     `json:"pending_approval,omitempty"`
	PendingTool     string     `json:"pending_tool,omitempty"`
	Retrieval       string     `json:"retrieval"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// RunDetail is a run in full, with the trajectory chips the list view uses so a
// client does not have to derive them twice.
type RunDetail struct {
	*agent.Run
	// Trajectory is the run's steps reduced to tool, tier and outcome.
	Trajectory []ToolChip `json:"trajectory"`
	// Approval is the gate the run is paused on, when it is paused.
	Approval *ApprovalView `json:"approval,omitempty"`
}

func runSummary(r *agent.Run) RunSummary {
	out := RunSummary{
		ID: r.ID, Query: r.Query, Status: string(r.Status),
		Principal: r.Principal.Subject, OnBehalf: r.Principal.OnBehalf,
		Model: r.ModelName, Environment: r.Environment, TraceID: r.TraceID,
		Steps: len(r.Steps), Trajectory: chips(r), Retrieval: "trusted",
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if !r.RetrievalTrusted {
		out.Retrieval = "quarantined"
	}
	if r.Pending != nil {
		out.PendingApproval, out.PendingTool = r.Pending.ApprovalID, r.Pending.ToolName
	}
	return out
}

// chips renders the trajectory. Every attempted tool appears, including the
// ones the gateway refused: a trajectory that showed only the successful calls
// would hide exactly the behaviour a governance dashboard exists to show.
func chips(r *agent.Run) []ToolChip {
	out := make([]ToolChip, 0, len(r.Steps))
	for _, s := range r.Steps {
		if s.Tool == "" {
			continue
		}
		out = append(out, ToolChip{Step: s.N, Tool: s.Tool, Tier: string(s.Tier), Outcome: s.Outcome})
	}
	return out
}

// handleStartRun starts a governed run and returns it once it has completed or
// paused. The call is synchronous because a run that pauses for approval has
// already produced everything the caller can act on, and a caller that had to
// poll to discover it was paused would learn it later than the approver does.
func (s *server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var req StartRunRequest
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, r, err)
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeError(w, r, http.StatusBadRequest, CodeBadRequest, "query is required")
		return
	}
	caller, err := identity.MustFromContext(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	runAs, err := s.resolveRunPrincipal(caller, req.Principal)
	if err != nil {
		fail(w, r, err)
		return
	}
	run, err := s.p.Runtime.Start(r.Context(), agent.Request{Query: req.Query, Principal: runAs})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.runDetail(r.Context(), run))
}

// resolveRunPrincipal decides which identity a run executes as.
//
// The default is the caller. Naming another principal is permitted in exactly
// two cases: the named principal is an agent workload acting on behalf of the
// caller, which is the legitimate case — an operator dispatching the agent that
// acts for them — or the development principal header is enabled, in which case
// the deployment has already declared that identity is not being enforced here.
// Anything else is a privilege escalation dressed as a request body.
func (s *server) resolveRunPrincipal(caller identity.Principal, name string) (identity.Principal, error) {
	if strings.TrimSpace(name) == "" {
		return caller, nil
	}
	p, ok := platform.PrincipalByName(name)
	if !ok {
		return identity.Principal{}, Error{
			Code:    CodeBadRequest,
			Message: "unknown principal " + name + "; expected one of " + strings.Join(platform.PrincipalNames(), ", "),
		}
	}
	switch {
	case p.Subject == caller.Subject:
		return caller, nil
	case p.Kind == identity.KindAgent && p.OnBehalf == caller.Subject:
		return p, nil
	case s.opts.DevPrincipalHeader:
		return p, nil
	default:
		return identity.Principal{}, Error{
			Code:    CodeForbidden,
			Message: caller.Subject + " may not start a run as " + p.Subject,
		}
	}
}

// handleListRuns returns the runs, newest first.
func (s *server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.p.Store.ListRuns(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
	limit := intQuery(r, "limit", 50, 1, 500)
	if len(runs) > limit {
		runs = runs[:limit]
	}
	out := make([]RunSummary, 0, len(runs))
	for _, run := range runs {
		out = append(out, runSummary(run))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out, "count": len(out)})
}

// handleGetRun returns one run in full.
func (s *server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.p.Store.LoadRun(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.runDetail(r.Context(), run))
}

func (s *server) runDetail(ctx context.Context, run *agent.Run) RunDetail {
	d := RunDetail{Run: run, Trajectory: chips(run)}
	if run.Pending != nil {
		if ap, err := s.p.Approvals.Get(ctx, run.Pending.ApprovalID); err == nil {
			v := s.approvalView(ap)
			d.Approval = &v
		}
	}
	return d
}

// handleRunTrace returns the spans of a run's trace.
func (s *server) handleRunTrace(w http.ResponseWriter, r *http.Request) {
	run, err := s.p.Store.LoadRun(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, r, err)
		return
	}
	spans := []obs.SpanData{}
	if run.TraceID != "" && s.p.Spans != nil {
		spans = s.p.Spans.Trace(run.TraceID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run": run.ID, "trace_id": run.TraceID, "spans": spans, "count": len(spans),
	})
}

// handleRunAudit returns the audit records the run produced, in chain order.
func (s *server) handleRunAudit(w http.ResponseWriter, r *http.Request) {
	run, err := s.p.Store.LoadRun(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, r, err)
		return
	}
	records := s.p.Audit.ByRun(run.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"run": run.ID, "records": records, "count": len(records), "head": s.p.Audit.Head(),
	})
}

// ---------------------------------------------------------------------------
// approvals
// ---------------------------------------------------------------------------

// ApprovalDecisionRequest is the body of a grant or a deny.
//
// It carries a note and nothing else. In particular it does not carry an
// approver: the approver is the authenticated principal, always. A body-supplied
// approver would let the caller name somebody else as the second pair of eyes,
// which is the exact control the four-eyes rule exists to enforce, so the field
// does not exist rather than being validated.
type ApprovalDecisionRequest struct {
	// Note is the approver's reason, recorded on the audit chain.
	Note string `json:"note,omitempty"`
}

// ApprovalView is an approval with the two figures a console needs that the
// stored record does not hold: how long is left, and who may respond.
type ApprovalView struct {
	*agent.Approval
	// ExpiresInSeconds is the remaining window against the platform's clock.
	// It is negative once the window has closed.
	ExpiresInSeconds float64 `json:"expires_in_seconds"`
	// Expired reports whether the window has already closed.
	Expired bool `json:"expired"`
	// EligibleApprovers names the principals that hold an authorising role and
	// the approval scope, and are not the requester.
	EligibleApprovers []string `json:"eligible_approvers"`
	// Outstanding is how many more grants are needed.
	Outstanding int `json:"outstanding"`
}

func (s *server) approvalView(a *agent.Approval) ApprovalView {
	now := s.clock.Now()
	out := ApprovalView{
		Approval:          a,
		ExpiresInSeconds:  a.ExpiresAt.Sub(now).Seconds(),
		Expired:           now.After(a.ExpiresAt),
		EligibleApprovers: eligibleApprovers(a),
		Outstanding:       max(0, a.RequiredCount-len(a.Grants)),
	}
	return out
}

// eligibleApprovers lists the declared principals that could satisfy this gate.
// It is derived from the same checks ApprovalService.Grant enforces, so the
// console never offers a button that the service will refuse.
func eligibleApprovers(a *agent.Approval) []string {
	var out []string
	for _, name := range platform.PrincipalNames() {
		p, ok := platform.PrincipalByName(name)
		if !ok {
			continue
		}
		if p.Subject == a.Requester || (a.OnBehalfOf != "" && p.Subject == a.OnBehalfOf) {
			continue
		}
		if !p.HasScope(identity.ScopeApprove) {
			continue
		}
		if len(a.RequiredRoles) > 0 && !holdsAny(p, a.RequiredRoles) {
			continue
		}
		out = append(out, p.Subject)
	}
	return out
}

func holdsAny(p identity.Principal, roles []string) bool {
	for _, role := range roles {
		if p.HasRole(role) {
			return true
		}
	}
	return false
}

// handleListApprovals returns the open gates, oldest first.
func (s *server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	pending, err := s.p.Approvals.Pending(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	out := make([]ApprovalView, 0, len(pending))
	for _, a := range pending {
		out = append(out, s.approvalView(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": out, "count": len(out)})
}

// handleGetApproval returns one gate whatever its state.
func (s *server) handleGetApproval(w http.ResponseWriter, r *http.Request) {
	a, err := s.p.Approvals.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.approvalView(a))
}

// handleGrantApproval records one human's approval.
func (s *server) handleGrantApproval(w http.ResponseWriter, r *http.Request) {
	s.decideApproval(w, r, true)
}

// handleDenyApproval closes a gate against the action.
func (s *server) handleDenyApproval(w http.ResponseWriter, r *http.Request) {
	s.decideApproval(w, r, false)
}

func (s *server) decideApproval(w http.ResponseWriter, r *http.Request, grant bool) {
	var req ApprovalDecisionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, r, err)
		return
	}
	// The approver is taken from the authenticated principal and never from the
	// body. This is the four-eyes control's anchor: the service checks that the
	// approver is not the requester, holds an authorising role and has not
	// already responded, and all three checks are worthless if the caller can
	// choose which identity they are checked against.
	approver, err := identity.MustFromContext(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	id := r.PathValue("id")

	var a *agent.Approval
	if grant {
		a, err = s.p.Approvals.Grant(r.Context(), id, approver, req.Note)
	} else {
		a, err = s.p.Approvals.Deny(r.Context(), id, approver, req.Note)
	}
	if err != nil {
		fail(w, r, err)
		return
	}

	// Granting resumes the run synchronously through the approval service's
	// callback, so by the time this returns the run has already moved. Reading
	// it back here is what makes the console's next screen correct without a
	// poll.
	body := map[string]any{"approval": s.approvalView(a)}
	if run, err := s.p.Store.LoadRun(r.Context(), a.RunID); err == nil {
		body["run"] = s.runDetail(r.Context(), run)
	}
	writeJSON(w, http.StatusOK, body)
}
