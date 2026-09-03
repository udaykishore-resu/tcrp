package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/bridge"
	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/identity"
	"github.com/udaykishore-resu/tcrp/internal/money"
)

// snapshot returns the curated view every risk answer is computed from. It goes
// through the tool platform's accessor rather than the lake's, so the HTTP
// surface and the agent's tools answer from the same instant and a figure on
// the dashboard matches the figure in a run's transcript.
func (s *server) snapshot(ctx context.Context) (domain.Snapshot, error) {
	return s.p.Tools.Snapshot(ctx)
}

// counterparty resolves a path parameter against the snapshot, so an unknown id
// is a 404 rather than an engine error a client has to interpret.
func counterpartyOf(snap domain.Snapshot, raw string) (domain.Counterparty, error) {
	id := strings.ToUpper(strings.TrimSpace(raw))
	if id == "" {
		return domain.Counterparty{}, Error{Code: CodeBadRequest, Message: "counterparty is required"}
	}
	cp, ok := snap.Counterparty(id)
	if !ok {
		return domain.Counterparty{}, Error{Code: CodeNotFound, Message: "unknown counterparty " + id}
	}
	return cp, nil
}

// handleExposureAll returns every counterparty's exposure, worst first.
func (s *server) handleExposureAll(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	exposures, err := s.p.Risk.ExposureAll(r.Context(), snap)
	if err != nil {
		fail(w, r, err)
		return
	}
	names := map[string]string{}
	for _, cp := range snap.Counterparties {
		names[cp.ID] = cp.Name
	}
	out := make([]ExposureView, 0, len(exposures))
	totalUncovered := money.Zero(s.p.Risk.Base())
	totalShortfall := money.Zero(s.p.Risk.Base())
	for _, x := range exposures {
		out = append(out, exposureView(x, names[x.Counterparty]))
		totalUncovered = totalUncovered.MustAdd(x.Uncovered)
		totalShortfall = totalShortfall.MustAdd(x.Shortfall)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"as_of":           snap.ValidAt,
		"known_at":        snap.KnownAt,
		"exposures":       out,
		"count":           len(out),
		"total_uncovered": amt(totalUncovered),
		"total_shortfall": amt(totalShortfall),
		"quality":         snap.Quality.Status,
	})
}

// handleExposure returns one counterparty's exposure.
func (s *server) handleExposure(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	cp, err := counterpartyOf(snap, r.PathValue("cp"))
	if err != nil {
		fail(w, r, err)
		return
	}
	x, err := s.p.Risk.Exposure(r.Context(), snap, cp.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, exposureView(x, cp.Name))
}

// handleCollateral returns one counterparty's pledge, line by line.
func (s *server) handleCollateral(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	cp, err := counterpartyOf(snap, r.PathValue("cp"))
	if err != nil {
		fail(w, r, err)
		return
	}
	cv, err := s.p.Risk.CollateralValue(r.Context(), snap, cp.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, collateralView(cv))
}

// handleConcentration returns the portfolio cut along every dimension.
func (s *server) handleConcentration(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	conc, err := s.p.Risk.Concentration(r.Context(), snap)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := make([]ConcentrationView, 0, len(conc))
	for _, c := range conc {
		if c.Share.Num == 0 {
			continue
		}
		out = append(out, concentrationView(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"as_of": snap.ValidAt, "concentration": out, "count": len(out),
	})
}

// handleBreaches returns the limits currently exceeded, worst first.
func (s *server) handleBreaches(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	breaches, err := s.p.Risk.CheckLimits(r.Context(), snap)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := make([]BreachView, 0, len(breaches))
	counts := map[string]int{}
	for _, b := range breaches {
		out = append(out, breachView(b))
		counts[string(b.Severity)]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"as_of": snap.ValidAt, "breaches": out, "count": len(out), "by_severity": counts,
	})
}

// StressRequest is the body of POST /v1/stress.
type StressRequest struct {
	// Scenario names a seeded scenario. Empty means rate_shock.
	Scenario string `json:"scenario,omitempty"`
	// ShockBps overrides the scenario's headline price shock, in basis points.
	ShockBps int64 `json:"shock_bps,omitempty"`
}

// handleStress runs a named scenario across every counterparty.
func (s *server) handleStress(w http.ResponseWriter, r *http.Request) {
	// The body is optional: the scenario is in the path and the only thing a
	// body adds is an override.
	var req StressRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			fail(w, r, err)
			return
		}
	}
	// The path segment names the scenario; a body may still carry an override
	// of the headline shock. A path that named one scenario while the body ran
	// another would be a dashboard showing a figure nobody asked for.
	id := strings.TrimSpace(r.PathValue("scenario"))
	if id == "" {
		id = strings.TrimSpace(req.Scenario)
	}
	if id == "" {
		id = "rate_shock"
	}
	sc, ok := s.p.Seed.Scenario(id)
	if !ok {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "unknown scenario "+id)
		return
	}
	// An override replaces the class shocks rather than layering on top of
	// them, exactly as the run_stress_scenario tool does. Two code paths that
	// interpret the same override differently would make the dashboard and the
	// agent disagree about what a scenario means.
	if req.ShockBps != 0 {
		sc.PriceShock = money.Bps(-req.ShockBps)
		sc.ClassShocks = nil
	}
	snap, err := s.snapshot(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	sum, err := s.p.Risk.Stress(r.Context(), snap, sc)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, stressView(sum, sc))
}

// ---------------------------------------------------------------------------
// reconciliation and settlement
// ---------------------------------------------------------------------------

// handleBreaks runs the reconciliation and returns the register's open items.
//
// It reconciles on every call rather than serving a cached result, because the
// register keeps whatever a human did to a break between runs: an acknowledged
// break stays acknowledged, and re-running is how the quantities stay current
// without erasing the investigation.
func (s *server) handleBreaks(w http.ResponseWriter, r *http.Request) {
	validAt := s.p.Tools.AsOf
	if validAt.IsZero() {
		validAt = s.clock.Now()
	}
	res, err := s.p.Lake.Reconcile(r.Context(), validAt, s.clock.Now())
	if err != nil {
		fail(w, r, err)
		return
	}
	register := s.p.Lake.Recon.Register()
	open := register.Unresolved()
	out := make([]BreakView, 0, len(open))
	counts := map[string]int{}
	for _, b := range open {
		out = append(out, breakView(b))
		counts[b.Severity]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"valid_at":    res.ValidAt,
		"known_at":    res.KnownAt,
		"compared":    res.Compared,
		"matched":     res.Matched,
		"suppressed":  res.Suppressed,
		"breaks":      out,
		"count":       len(out),
		"by_severity": counts,
		"ageing":      register.Ageing(),
		"policy": map[string]any{
			"materiality_qty": s.p.Lake.Recon.Policy().MaterialityQty,
			"high_qty":        s.p.Lake.Recon.Policy().HighQty,
			"critical_qty":    s.p.Lake.Recon.Policy().CriticalQty,
			"stale_after":     s.p.Lake.Recon.Policy().StaleAfter.String(),
		},
	})
}

// BreakDecisionRequest is the body of an acknowledge or a resolve.
type BreakDecisionRequest struct {
	// Note is the acknowledgement's reason or the resolution's statement.
	Note string `json:"note,omitempty"`
}

// handleAcknowledgeBreak assigns a break to the caller.
func (s *server) handleAcknowledgeBreak(w http.ResponseWriter, r *http.Request) {
	s.decideBreak(w, r, false)
}

// handleResolveBreak closes a break with a stated resolution.
func (s *server) handleResolveBreak(w http.ResponseWriter, r *http.Request) {
	s.decideBreak(w, r, true)
}

func (s *server) decideBreak(w http.ResponseWriter, r *http.Request, resolve bool) {
	var req BreakDecisionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, r, err)
		return
	}
	// As with an approval, who acted is the authenticated principal. A break's
	// assignee is part of the investigation record and a caller-supplied name
	// would make that record worthless.
	who, err := identity.MustFromContext(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	note := req.Note
	if strings.TrimSpace(note) == "" {
		note = "no note given"
	}
	register := s.p.Lake.Recon.Register()
	var b domain.Break
	if resolve {
		b, err = register.Resolve(r.PathValue("id"), who.Subject, note)
	} else {
		b, err = register.Acknowledge(r.PathValue("id"), who.Subject, note)
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, breakView(b))
}

// handleSupply returns the cross-network supply reconciliation.
func (s *server) handleSupply(w http.ResponseWriter, r *http.Request) {
	if s.p.World == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeInternal, "no ledgers are configured")
		return
	}
	rep := bridge.Reconcile(s.p.World.EVM, s.p.World.Canton, []string{domain.FundSymbol}, s.clock.Now())
	breaks := make([]BreakView, 0, len(rep.Breaks))
	for _, b := range rep.Breaks {
		breaks = append(breaks, breakView(b))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"as_of":         rep.AsOf,
		"source_ledger": rep.SourceLedger,
		"target_ledger": rep.TargetLedger,
		"symbols":       rep.Symbols,
		"balanced":      rep.Balanced,
		"breaks":        breaks,
	})
}

// SnapshotSummary is the bitemporal gold view, summarised.
//
// It is a summary and not a dump on purpose: the point of the endpoint is to
// show that the platform can answer "as it was understood then" as easily as
// "as it is understood now", and a client that wants the rows asks the risk
// endpoints for the figures rather than re-deriving them from positions.
type SnapshotSummary struct {
	ValidAt        time.Time            `json:"valid_at"`
	KnownAt        time.Time            `json:"known_at"`
	Counterparties int                  `json:"counterparties"`
	Instruments    int                  `json:"instruments"`
	Positions      int                  `json:"positions"`
	Obligations    int                  `json:"obligations"`
	Prices         int                  `json:"prices"`
	Limits         int                  `json:"limits"`
	HaircutVersion string               `json:"haircut_version"`
	FundShares     int64                `json:"fund_shares"`
	ByLedger       map[string]int64     `json:"fund_shares_by_ledger"`
	Quality        domain.QualityReport `json:"quality"`
	Lineage        []string             `json:"lineage,omitempty"`
}

// handleSnapshot answers for a (valid_at, known_at) pair.
func (s *server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	base := s.p.Tools.AsOf
	if base.IsZero() {
		base = s.clock.Now()
	}
	validAt, err := timeQuery(r, "valid_at", base)
	if err != nil {
		fail(w, r, err)
		return
	}
	knownAt, err := timeQuery(r, "known_at", s.clock.Now())
	if err != nil {
		fail(w, r, err)
		return
	}
	snap, err := s.p.Lake.Snapshot(r.Context(), validAt, knownAt)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := SnapshotSummary{
		ValidAt: snap.ValidAt, KnownAt: snap.KnownAt,
		Counterparties: len(snap.Counterparties), Instruments: len(snap.Instruments),
		Positions: len(snap.Positions), Obligations: len(snap.Obligations),
		Prices: len(snap.Prices), Limits: len(snap.Limits),
		HaircutVersion: snap.Haircuts.Version, ByLedger: map[string]int64{},
		Quality: snap.Quality, Lineage: snap.Lineage,
	}
	for _, p := range snap.Positions {
		if p.Symbol != domain.FundSymbol {
			continue
		}
		out.FundShares += p.Quantity
		out.ByLedger[string(p.Ledger)] += p.Quantity
	}
	writeJSON(w, http.StatusOK, out)
}

// timeQuery reads an RFC3339 or YYYY-MM-DD query parameter.
func timeQuery(r *http.Request, name string, def time.Time) (time.Time, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, Error{
		Code:    CodeBadRequest,
		Message: name + " must be RFC3339 or YYYY-MM-DD, got " + raw,
	}
}
