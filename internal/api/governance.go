package api

import (
	"context"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"

	"github.com/udaykishore-resu/tcrp/internal/agent"
	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/conform"
	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/platform"
	"github.com/udaykishore-resu/tcrp/internal/policy"
)

// handleAudit returns a window of the hash chain, oldest first.
//
// The window is the tail of the matching records rather than an arbitrary page,
// and it is returned in chain order, because adjacent records share a hash link
// and a reader checking one against the next needs them next to each other.
func (s *server) handleAudit(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.URL.Query().Get("run"))
	action := strings.TrimSpace(r.URL.Query().Get("action"))
	limit := intQuery(r, "limit", 100, 1, 1000)

	all := s.p.Audit.Records()
	matched := make([]audit.Record, 0, len(all))
	for _, rec := range all {
		if runID != "" && rec.RunID != runID {
			continue
		}
		if action != "" && string(rec.Action) != action {
			continue
		}
		matched = append(matched, rec)
	}
	total := len(matched)
	if total > limit {
		matched = matched[total-limit:]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"records": matched,
		"count":   len(matched),
		"total":   total,
		"head":    s.p.Audit.Head(),
		"length":  s.p.Audit.Len(),
	})
}

// AuditVerification is the result of walking the chain.
type AuditVerification struct {
	// OK is true when every link, content hash and signature verified.
	OK bool `json:"ok"`
	// Head is the chain head hash at the moment of verification.
	Head string `json:"head"`
	// Records is how many records were walked.
	Records int `json:"records"`
	// PublicKey is the hex ed25519 key the signatures were checked against.
	PublicKey string `json:"public_key,omitempty"`
	// Error names the first sequence at which the chain broke.
	Error string `json:"error,omitempty"`
}

// handleAuditVerify walks the chain and reports where, if anywhere, it breaks.
func (s *server) handleAuditVerify(w http.ResponseWriter, r *http.Request) {
	out := AuditVerification{
		Head:    s.p.Audit.Head(),
		Records: s.p.Audit.Len(),
	}
	if len(s.p.AuditKey) > 0 {
		out.PublicKey = hex.EncodeToString(s.p.AuditKey)
	}
	if err := s.p.Audit.Verify(); err != nil {
		out.Error = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.OK = true
	writeJSON(w, http.StatusOK, out)
}

// PolicyRuleView is one rule as the console shows it. The compiled program is
// deliberately absent: what a reader needs is the source text the digest covers.
type PolicyRuleView struct {
	ID          string              `json:"id"`
	Description string              `json:"description,omitempty"`
	Effect      string              `json:"effect"`
	When        string              `json:"when"`
	Reason      string              `json:"reason,omitempty"`
	Priority    int                 `json:"priority"`
	Obligations []policy.Obligation `json:"obligations,omitempty"`
}

// PolicyBundleView is the installed bundle.
type PolicyBundleView struct {
	Package       string           `json:"package"`
	Version       string           `json:"version"`
	Description   string           `json:"description,omitempty"`
	Digest        string           `json:"digest"`
	DefaultEffect string           `json:"default_effect"`
	Rules         []PolicyRuleView `json:"rules"`
	Count         int              `json:"count"`
}

// handlePolicy returns the installed bundle.
func (s *server) handlePolicy(w http.ResponseWriter, r *http.Request) {
	b := s.p.Policy.Bundle()
	if b == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeInternal, "no policy bundle is installed")
		return
	}
	out := PolicyBundleView{
		Package: b.Package, Version: b.Version, Description: b.Description,
		Digest: b.Digest(), DefaultEffect: string(b.DefaultEffect),
		Rules: make([]PolicyRuleView, 0, len(b.Rules)), Count: len(b.Rules),
	}
	for _, rule := range b.Rules {
		out.Rules = append(out.Rules, PolicyRuleView{
			ID: rule.ID, Description: rule.Description, Effect: string(rule.Effect),
			When: rule.When, Reason: rule.Reason, Priority: rule.Priority,
			Obligations: rule.Obligations,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// PolicyEvaluateRequest is a dry-run question for the gateway.
//
// The embedded policy.Input is the wire shape, so what a caller sends here is
// exactly what the runtime sends at a real decision — there is no second,
// friendlier input format that could drift from the one enforcement uses.
type PolicyEvaluateRequest struct {
	policy.Input
	// PrincipalName fills Principal from platform.PrincipalNames, so a console
	// can ask "what would the gateway say for the treasurer" without pasting a
	// principal document.
	PrincipalName string `json:"principal_name,omitempty"`
}

// handlePolicyEvaluate renders a decision without executing anything.
//
// It is a read: no tool is invoked, no approval is opened and nothing is
// audited beyond the engine's own metrics. That is what makes it safe to expose
// and useful to point at — the answer is the same evaluation the gateway would
// make, and the response carries the full list of rules considered.
func (s *server) handlePolicyEvaluate(w http.ResponseWriter, r *http.Request) {
	var req PolicyEvaluateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, r, err)
		return
	}
	in := req.Input
	if req.PrincipalName != "" {
		p, ok := platform.PrincipalByName(req.PrincipalName)
		if !ok {
			writeError(w, r, http.StatusBadRequest, CodeBadRequest, "unknown principal "+req.PrincipalName)
			return
		}
		in.Principal = p
	}
	if in.Tool.Name == "" {
		writeError(w, r, http.StatusBadRequest, CodeBadRequest, "tool.name is required")
		return
	}
	// A caller that names a registered tool but not its tier or scopes gets the
	// registry's declaration filled in, because a decision made against a tier
	// the tool does not actually have is a decision about nothing.
	if t, ok := s.p.Registry.Get(in.Tool.Name); ok {
		spec := t.Spec()
		if in.Tool.Tier == "" {
			in.Tool.Tier = string(spec.Tier)
		}
		if len(in.Tool.RequiredScope) == 0 {
			in.Tool.RequiredScope = spec.RequiredScopes
		}
		if in.Resource.Kind == "" {
			in.Resource.Kind = spec.ResourceKind
		}
	}
	if in.Run.Stage == "" {
		in.Run.Stage = policy.StagePropose
	}
	if in.Run.Environment == "" {
		in.Run.Environment = s.p.Opts.Environment
	}
	if in.Now.IsZero() {
		in.Now = s.clock.Now()
	}
	dec := s.p.Policy.Evaluate(r.Context(), in)
	writeJSON(w, http.StatusOK, map[string]any{
		"input":              in,
		"decision":           dec,
		"explain":            dec.Explain(),
		"requires_approval":  dec.RequiresApproval(),
		"approvals_required": dec.ApprovalsRequired(),
	})
}

// handleConformance runs every registered invariant over the platform's current
// evidence.
func (s *server) handleConformance(w http.ResponseWriter, r *http.Request) {
	ev := s.evidence(r.Context())
	rep := conform.NewEngine(nil, s.clock).Run(r.Context(), nil, ev)
	gate := ""
	if err := rep.Gate(); err != nil {
		gate = err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"report":      rep,
		"gate_passed": gate == "",
		"gate_error":  gate,
		"blocking":    rep.Blocking(),
		"manifest":    ev.Manifest(),
		"audit_head":  ev.AuditHead(),
	})
}

// evidence assembles what the invariants are allowed to read.
//
// Everything the platform actually holds is passed. What it does not hold is
// left nil rather than fabricated: the platform assembles no bridge relayer, so
// there are no attested mints to check, and it exposes no gold-row lineage
// counter, so DATA-1 has nothing to count. Both come back inconclusive, which
// is the honest answer — an invariant that passed because nobody collected the
// evidence is the failure mode the conformance package exists to prevent.
func (s *server) evidence(ctx context.Context) conform.Evidence {
	ev := conform.Evidence{
		Records:            s.p.Audit.Records(),
		AuditPublicKey:     s.p.AuditKey,
		StepBudget:         agent.DefaultConfig().MaxSteps,
		Symbols:            []string{domain.FundSymbol},
		AsOf:               s.clock.Now(),
		PolicyBundleDigest: bundleDigest(s.p.Policy),
	}
	if s.p.Spans != nil {
		ev.Spans = s.p.Spans.Spans()
	}
	if runs, err := s.p.Store.ListRuns(ctx); err == nil {
		ev.Runs = runs
	}
	if s.p.World != nil {
		ev.Source = conform.LedgerView{
			Name:   domain.LedgerEVM,
			Ledger: s.p.World.EVM,
			// The EVM simulator exposes both a holder list and the identity
			// registry, so SETTLE-2 can be answered for this network.
			Holders:    s.p.World.EVM.Holders,
			Registered: s.p.World.EVM.Verified,
		}
		// The Canton simulator has no holder list on its read surface, but its
		// contract store does: a holding contract names its owner. Deriving the
		// holder set from the contracts is what the participant node's own
		// query would do, so SETTLE-2 can be answered for this network too
		// rather than reported as unreadable.
		world := s.p.World
		ev.Target = conform.LedgerView{
			Name: domain.LedgerCanton, Ledger: world.Canton,
			Holders: func(symbol string) []string {
				seen := map[string]bool{}
				var out []string
				for _, c := range world.Canton.Contracts() {
					if c.Symbol != symbol || c.Archived {
						continue
					}
					if c.Owner != "" && !seen[c.Owner] {
						seen[c.Owner] = true
						out = append(out, c.Owner)
					}
				}
				sort.Strings(out)
				return out
			},
			Registered: func(party string) bool {
				if party == world.Issuer {
					return true
				}
				for _, cp := range world.Seed.Counterparties {
					for _, p := range cp.Parties {
						if p == party {
							return cp.Whitelisted
						}
					}
				}
				return false
			},
		}
	}
	if s.p.Signers != nil {
		ev.Signers = s.p.Signers
	}
	if s.p.Bridge != nil {
		for _, st := range s.p.Bridge.Settlements() {
			ev.Mints = append(ev.Mints, conform.AttestedMint{
				Attestation: st.Attestation, Signatures: st.Signatures,
			})
		}
	}
	// Lineage: how many gold rows there are, and how many of them can be
	// walked back to a bronze event. The counter is a closure rather than a
	// number so the invariant reads the lake at evaluation time and cannot be
	// handed a stale count.
	if s.p.Lake != nil {
		ev.Lineage = func() (gold, traced int) {
			snap, err := s.p.Lake.Snapshot(context.Background(), s.validAt(), s.knownAt())
			if err != nil {
				return 0, 0
			}
			landed := map[string]bool{}
			for _, e := range s.p.Lake.Seed.Events {
				if e.From != "" {
					landed[e.From] = true
				}
				if e.To != "" {
					landed[e.To] = true
				}
			}
			for _, pos := range snap.Positions {
				gold++
				if landed[pos.Counterparty] || pos.Ledger == domain.LedgerGolden {
					traced++
				}
			}
			return gold, traced
		}
	}
	if snap, err := s.snapshot(ctx); err == nil {
		q := snap.Quality
		ev.Quality = &q
	}
	return ev
}

func bundleDigest(e *policy.Engine) string {
	if e == nil {
		return ""
	}
	if b := e.Bundle(); b != nil {
		return b.Digest()
	}
	return ""
}

// ---------------------------------------------------------------------------
// operational endpoints
// ---------------------------------------------------------------------------

// handleMetrics serves the Prometheus text exposition.
func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(s.p.Metrics.Render()))
}

// handleHealthz reports that the process is alive.
func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.opts.Version,
		"now":     s.clock.Now(),
		"uptime":  s.clock.Since(s.started).String(),
	})
}

// handleReadyz reports whether the platform can answer.
//
// Readiness is a stronger claim than liveness and it is checked rather than
// asserted: the policy bundle must be installed and the curated snapshot must
// be buildable, because a process that is running but cannot render a decision
// should not be sent traffic.
func (s *server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{}
	ready := true
	if s.p.Policy != nil && s.p.Policy.Bundle() != nil {
		checks["policy"] = "ok"
	} else {
		checks["policy"] = "no bundle installed"
		ready = false
	}
	if _, err := s.snapshot(r.Context()); err != nil {
		checks["lake"] = err.Error()
		ready = false
	} else {
		checks["lake"] = "ok"
	}
	if s.p.Runtime != nil {
		checks["runtime"] = "ok"
	} else {
		checks["runtime"] = "not assembled"
		ready = false
	}
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ready": ready, "checks": checks})
}

// VersionInfo describes the build and the policy in force.
type VersionInfo struct {
	Version       string   `json:"version"`
	Commit        string   `json:"commit,omitempty"`
	BuiltAt       string   `json:"built_at,omitempty"`
	Environment   string   `json:"environment"`
	Model         string   `json:"model"`
	PolicyBundle  string   `json:"policy_bundle"`
	PolicyDigest  string   `json:"policy_digest"`
	Deterministic bool     `json:"deterministic"`
	Principals    []string `json:"principals"`
	Tools         []string `json:"tools"`
}

// handleVersion reports what is running.
func (s *server) handleVersion(w http.ResponseWriter, r *http.Request) {
	info := VersionInfo{
		Version: s.opts.Version, Commit: s.opts.Commit, BuiltAt: s.opts.BuiltAt,
		Environment: s.p.Opts.Environment, Deterministic: s.p.Opts.Deterministic,
		PolicyDigest: bundleDigest(s.p.Policy), Principals: platform.PrincipalNames(),
	}
	if b := s.p.Policy.Bundle(); b != nil {
		info.PolicyBundle = b.Version
	}
	if s.p.Model != nil {
		info.Model = s.p.Model.Name()
	}
	if s.p.Registry != nil {
		info.Tools = s.p.Registry.Names()
	}
	writeJSON(w, http.StatusOK, info)
}
