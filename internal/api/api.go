// Package api is the platform's HTTP surface and the governance dashboard.
//
// No handler here makes an authorization decision. It resolves the acting
// principal, calls the runtime or an engine with it, and renders the answer;
// the policy gateway, the guardrail and the approval service decide what is
// allowed, exactly as they do when the same call arrives from the CLI. A
// control enforced at the edge is a control a second entry point routes around,
// so there is only one place any of them live and it is not this package.
//
// The route table below is the whole API. It is written out in one block rather
// than registered from a dozen files, because the set of things the platform
// exposes is a security-relevant fact and it should be readable in one screen.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/conform"
	"github.com/udaykishore-resu/tcrp/internal/obs"
	"github.com/udaykishore-resu/tcrp/internal/platform"
)

// HeaderDevPrincipal names a declared principal instead of presenting a signed
// assertion. It is honoured only when Options.DevPrincipalHeader is set, which
// a production build never does.
const HeaderDevPrincipal = "X-TCRP-As"

// Defaults for the server's own limits.
const (
	// DefaultMaxBodyBytes bounds a request body. The largest legitimate body
	// is a query and a note; a megabyte is already generous.
	DefaultMaxBodyBytes int64 = 1 << 20
	// DefaultRatePerMinute is the per-principal request allowance.
	DefaultRatePerMinute = 600
	// DefaultReadHeaderTimeout bounds the time a client may take to send its
	// headers, which is the cheapest defence against a slow-loris.
	DefaultReadHeaderTimeout = 5 * time.Second
)

// Options configure the server.
type Options struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// Version, Commit and BuiltAt are reported by /v1/version and appear in
	// the evidence bundle, so a verdict can be tied to a build.
	Version string
	Commit  string
	BuiltAt string
	// MaxBodyBytes bounds a request body; zero uses DefaultMaxBodyBytes.
	MaxBodyBytes int64
	// RatePerMinute is the per-principal allowance; zero uses the default and
	// a negative value disables the limiter.
	RatePerMinute int
	// DevPrincipalHeader enables HeaderDevPrincipal. It must be false in any
	// environment where the answers matter.
	DevPrincipalHeader bool
	// Logger receives the access log; nil discards it.
	Logger *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.Addr == "" {
		o.Addr = ":8080"
	}
	if o.Version == "" {
		o.Version = "dev"
	}
	if o.MaxBodyBytes == 0 {
		o.MaxBodyBytes = DefaultMaxBodyBytes
	}
	return o
}

// server holds everything a handler needs. It is unexported because the only
// supported way to obtain one is New: a server assembled field by field could
// be missing the middleware that authenticates its own requests.
type server struct {
	p       *platform.Platform
	opts    Options
	clock   obs.Clock
	log     *slog.Logger
	metrics *httpMetrics
	limiter *limiter
	started time.Time
}

// New returns the platform's HTTP handler.
func New(p *platform.Platform, opts Options) http.Handler {
	opts = opts.withDefaults()
	s := &server{
		p: p, opts: opts, clock: p.Clock,
		log:     opts.Logger,
		metrics: newHTTPMetrics(p.Metrics),
		limiter: newLimiter(p.Clock, opts.RatePerMinute),
		started: p.Clock.Now(),
	}
	if s.log == nil {
		s.log = obs.Discard()
	}
	return s.routes()
}

// NewServer returns an *http.Server wired to the platform, with the timeouts a
// public listener needs set rather than left at Go's zero values.
func NewServer(p *platform.Platform, opts Options) *http.Server {
	opts = opts.withDefaults()
	return &http.Server{
		Addr:              opts.Addr,
		Handler:           New(p, opts),
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
}

// route is one entry in the table.
type route struct {
	pattern string
	handler http.HandlerFunc
	// scope, when set, is the scope a principal must hold. It is a
	// defence-in-depth check on top of whatever the engine behind the handler
	// enforces, not a substitute for it.
	scope string
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	table := []route{
		// The page and the operational endpoints.
		{"GET /", s.handleDashboard, ""},
		{"GET /healthz", s.handleHealthz, ""},
		{"GET /readyz", s.handleReadyz, ""},
		{"GET /metrics", s.handleMetrics, ""},
		{"GET /v1/version", s.handleVersion, ""},

		// The agent.
		{"POST /v1/runs", s.handleStartRun, ""},
		{"GET /v1/runs", s.handleListRuns, ""},
		{"GET /v1/runs/{id}", s.handleGetRun, ""},
		{"GET /v1/runs/{id}/trace", s.handleRunTrace, ""},
		{"GET /v1/runs/{id}/audit", s.handleRunAudit, ""},

		// Human approval.
		{"GET /v1/approvals", s.handleListApprovals, ""},
		{"GET /v1/approvals/{id}", s.handleGetApproval, ""},
		{"POST /v1/approvals/{id}/grant", s.handleGrantApproval, ""},
		{"POST /v1/approvals/{id}/deny", s.handleDenyApproval, ""},

		// Risk.
		{"GET /v1/exposure", s.handleExposureAll, ""},
		{"GET /v1/exposure/{cp}", s.handleExposure, ""},
		{"GET /v1/collateral/{cp}", s.handleCollateral, ""},
		{"GET /v1/concentration", s.handleConcentration, ""},
		{"GET /v1/breaches", s.handleBreaches, ""},
		{"POST /v1/stress/{scenario}", s.handleStress, ""},

		// Lakehouse and ledgers.
		{"GET /v1/snapshot", s.handleSnapshot, ""},
		{"GET /v1/breaks", s.handleBreaks, ""},
		{"POST /v1/breaks/{id}/acknowledge", s.handleAcknowledgeBreak, ""},
		{"POST /v1/breaks/{id}/resolve", s.handleResolveBreak, ""},
		{"GET /v1/supply", s.handleSupply, ""},

		// Governance.
		{"GET /v1/policy", s.handlePolicy, ""},
		{"POST /v1/policy/evaluate", s.handlePolicyEvaluate, ""},
		{"GET /v1/audit", s.handleAudit, ""},
		{"GET /v1/audit/verify", s.handleAuditVerify, ""},
		{"GET /v1/conformance", s.handleConformance, ""},
	}

	for _, rt := range table {
		mux.Handle(rt.pattern, s.observe(rt.pattern, s.accessLog(rt.pattern, rt.handler)))
	}

	// The chain runs outside in. Recovery is outermost so a panic anywhere
	// below still produces one error shape; the request id is next so every
	// later layer can name the request; the trace span wraps authentication so
	// the principal lands on a span that already exists; and the rate limiter
	// runs after authentication because it counts per principal, not per
	// socket.
	var h http.Handler = mux
	h = s.rateLimit(h)
	h = s.authenticate(h)
	h = s.traceSpan(h)
	h = s.limitBody(h)
	h = s.requestID(h)
	h = s.recover(h)
	return h
}

// validAt is the business time the read endpoints answer for. The seeded world
// has a fixed as-of instant so that a figure on the dashboard is the same
// figure the tests assert; a deployment has no seed and uses the clock.
func (s *server) validAt() time.Time {
	if s.p.Seed != nil && !s.p.Seed.AsOf.IsZero() {
		return s.p.Seed.AsOf
	}
	return s.clock.Now()
}

// knownAt is the transaction time: everything the platform has learned by now.
func (s *server) knownAt() time.Time { return s.clock.Now() }

// HeaderRequestID carries the request id back to the caller, so a support
// conversation, an access-log line and a trace all name the same request.
const HeaderRequestID = "X-Request-ID"

// Conformance runs every registered invariant against this process's own
// evidence. It is exported so the CI gate binary can reach it without standing
// up a listener.
func Conformance(ctx context.Context, p *platform.Platform) conform.Report {
	s := &server{p: p, clock: p.Clock, log: obs.Discard(), started: p.Clock.Now()}
	return conform.NewEngine(nil, s.clock).Run(ctx, nil, s.evidence(ctx))
}

// ConformanceAgainst runs the invariants over a process model as well, so the
// structural checks the BPMN carries are evaluated alongside the behavioural
// ones.
func ConformanceAgainst(ctx context.Context, p *platform.Platform, proc *conform.Process) conform.Report {
	s := &server{p: p, clock: p.Clock, log: obs.Discard(), started: p.Clock.Now()}
	return conform.NewEngine(nil, s.clock).Run(ctx, proc, s.evidence(ctx))
}

// Evidence assembles what the invariants are allowed to read from a platform.
// It is exported so the CI gate can package a bundle from the same evidence the
// dashboard shows, rather than from a second, slightly different collection.
func Evidence(ctx context.Context, p *platform.Platform) conform.Evidence {
	s := &server{p: p, clock: p.Clock, log: obs.Discard(), started: p.Clock.Now()}
	return s.evidence(ctx)
}
