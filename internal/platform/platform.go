// Package platform assembles the whole system in one place.
//
// Every binary — the server, the demo, the evaluation harness, the CI gate —
// builds the same object graph through this package rather than wiring the
// components itself. That is the point: if the guardrail can be skipped in one
// binary and not another, the platform's claims are only true in one binary.
// There is exactly one constructor, and it is the only place a component is
// allowed to be nil.
package platform

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/agent"
	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/bridge"
	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/guard"
	"github.com/udaykishore-resu/tcrp/internal/identity"
	"github.com/udaykishore-resu/tcrp/internal/lake"
	"github.com/udaykishore-resu/tcrp/internal/ledger"
	"github.com/udaykishore-resu/tcrp/internal/obs"
	"github.com/udaykishore-resu/tcrp/internal/policy"
	"github.com/udaykishore-resu/tcrp/internal/risk"
	"github.com/udaykishore-resu/tcrp/internal/tools"
)

// TreasuryShares is the fund's own unissued inventory, held by the operator
// address and used as the source of outbound collateral transfers. It is
// issuance, so it appears in the supply reconciliation as such rather than
// arriving from nowhere.
const TreasuryShares int64 = 100_000_000

// The bridge's attester set: five signers, three of which must agree.
const (
	BridgeSigners = 5
	BridgeQuorum  = 3
)

// Options configure the assembly.
type Options struct {
	// Dir is the state directory: lake tables, checkpoints, audit file.
	// Empty means a temporary directory that is removed with the process.
	Dir string
	// Environment is a policy input and appears on every decision.
	Environment string
	// PolicyPath overrides the embedded policy bundle.
	PolicyPath string
	// GuardURL, when set, routes guardrail inspection to a remote service
	// instead of the in-process engine.
	GuardURL string
	// Model is "planner" (deterministic, the default) or "anthropic".
	Model string
	// ModelID names the model when Model is "anthropic".
	ModelID string
	// Deterministic pins the clock to the seed instant and makes span ids
	// sequential, so a demo run is byte-identical each time.
	Deterministic bool
	// Logger receives structured logs; nil discards them.
	Logger *slog.Logger
	// TraceFile, when set, receives one JSON object per finished span.
	TraceFile string
	// SpanLimit bounds the in-memory span buffer the eval harness reads.
	SpanLimit int
}

// Platform is the assembled system.
type Platform struct {
	Opts     Options
	Clock    obs.Clock
	Tracer   *obs.Tracer
	Spans    *obs.MemoryExporter
	Metrics  *obs.Registry
	Log      *slog.Logger
	Audit    *audit.Log
	AuditKey ed25519.PublicKey
	Signer   *identity.Signer

	Policy *policy.Engine
	Guard  *guard.Engine
	// Inspector is what the runtime actually calls: the local engine, or a
	// client to a remote guardrail service when one is configured.
	Inspector guard.Inspector

	Seed  *domain.Seed
	Lake  *lake.Lake
	World *ledger.World
	Risk  *risk.Engine
	Tools *tools.Platform

	// Bridge relays locks on the EVM network into mints on Canton. It is
	// assembled here rather than in a test because SETTLE-3 is a statement
	// about attested mints, and an invariant with nothing to check is a
	// statement about nothing.
	Bridge  *bridge.Relayer
	Signers *bridge.SignerSet

	Registry  *agent.Registry
	Store     agent.Store
	Approvals *agent.ApprovalService
	Runtime   *agent.Runtime
	Model     agent.Model

	dir     string
	cleanup []func() error
}

// New assembles the platform.
func New(ctx context.Context, opts Options) (*Platform, error) {
	if opts.Environment == "" {
		opts.Environment = "development"
	}
	if opts.SpanLimit == 0 {
		opts.SpanLimit = 20_000
	}
	p := &Platform{Opts: opts, Log: opts.Logger}
	if p.Log == nil {
		p.Log = obs.Discard()
	}

	dir := opts.Dir
	if dir == "" {
		d, err := os.MkdirTemp("", "tcrp-")
		if err != nil {
			return nil, err
		}
		dir = d
		p.cleanup = append(p.cleanup, func() error { return os.RemoveAll(d) })
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	p.dir = dir

	// Time. A deterministic platform pins the clock to the seed's instant so
	// that every figure, span id and evidence digest reproduces exactly.
	seed := domain.NewSeed()
	p.Seed = seed
	if opts.Deterministic {
		p.Clock = obs.NewManualClock(seed.AsOf)
	} else {
		p.Clock = obs.RealClock{}
	}

	p.Metrics = obs.NewRegistry()
	p.Spans = obs.NewMemoryExporter(opts.SpanLimit)
	exporters := []obs.Exporter{p.Spans}
	if opts.TraceFile != "" {
		f, err := os.Create(opts.TraceFile)
		if err != nil {
			return nil, err
		}
		exporters = append(exporters, obs.NewWriterExporter(f))
		p.cleanup = append(p.cleanup, f.Close)
	}
	p.Tracer = obs.NewTracer("tcrp", p.Clock, exporters...)
	if opts.Deterministic {
		p.Tracer.Deterministic(true)
	}

	// The audit log signs every record. The key is generated per process here;
	// a deployment loads it from the HSM or KMS the institution already has,
	// and the verification path does not change.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	p.Audit = audit.New(p.Clock,
		audit.WithFile(filepath.Join(dir, "audit.jsonl")),
		audit.WithSigning(priv))
	p.AuditKey = priv.Public().(ed25519.PublicKey)
	p.cleanup = append(p.cleanup, p.Audit.Close)

	p.Signer = identity.NewSigner("tcrp-dev-signing-key", 15*time.Minute)

	// Policy.
	var bundle *policy.Bundle
	if opts.PolicyPath != "" {
		bundle, err = policy.LoadBundleFile(opts.PolicyPath)
	} else {
		bundle, err = policy.Default()
	}
	if err != nil {
		return nil, fmt.Errorf("platform: policy: %w", err)
	}
	p.Policy = policy.NewEngine(bundle, p.Clock, p.Tracer, p.Metrics)

	// Guardrail.
	p.Guard = guard.NewEngine(guard.DefaultConfig(), p.Clock, p.Tracer, p.Metrics)
	p.Inspector = p.Guard
	if opts.GuardURL != "" {
		c := guard.NewClient(opts.GuardURL, guard.DefaultConfig().Budget+50*time.Millisecond)
		c.Clock, c.Signer, c.Tracer = p.Clock, p.Signer, p.Tracer
		p.Inspector = c
	}

	// Data and engines.
	p.Lake, err = lake.NewFromSeed(ctx, filepath.Join(dir, "lake"), p.Clock, seed)
	if err != nil {
		return nil, fmt.Errorf("platform: lake: %w", err)
	}
	p.World, err = ledger.NewWorld(ledger.Options{Clock: p.Clock, Tracer: p.Tracer, Metrics: p.Metrics})
	if err != nil {
		return nil, fmt.Errorf("platform: ledgers: %w", err)
	}
	// The desk transacts from the fund's own unissued inventory. Without it the
	// operator holds nothing and every outbound transfer fails at the ledger
	// for a reason that has nothing to do with the control being demonstrated.
	// The operator has to be in the identity registry like anybody else: the
	// token does not exempt its own issuer from the transfer restrictions, and
	// a platform that granted itself an exemption would not be demonstrating
	// the control it claims to have.
	if err := p.World.EVM.Register(p.World.Operator, ledger.Identity{
		Address: p.World.Operator, Country: "US",
		Claims:       []string{ledger.ClaimKYC, ledger.ClaimAccredited},
		Counterparty: "TCRP-OPERATOR",
	}); err != nil {
		return nil, fmt.Errorf("platform: registering the operator: %w", err)
	}
	if _, err := p.World.EVM.Mint(ctx, p.World.Operator, p.World.Operator,
		domain.FundSymbol, TreasuryShares, "treasury inventory"); err != nil {
		return nil, fmt.Errorf("platform: treasury: %w", err)
	}

	// The bridge. Five attesters, three of which must agree: the quorum is a
	// protocol parameter, and making it visible here rather than defaulting it
	// is the difference between a threshold and an accident.
	signers := make([]*bridge.Signer, 0, BridgeSigners)
	keys := make(map[string]ed25519.PublicKey, BridgeSigners)
	for i := range BridgeSigners {
		pub, sk, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		id := fmt.Sprintf("attester-%d", i+1)
		signers = append(signers, bridge.NewSigner(id, sk))
		keys[id] = pub
	}
	p.Signers, err = bridge.NewSignerSet(BridgeQuorum, keys)
	if err != nil {
		return nil, err
	}
	p.Bridge, err = bridge.NewRelayer(bridge.Config{
		Source: p.World.EVM, Target: p.World.Canton, Symbols: []string{domain.FundSymbol},
		Signers: signers, SignerSet: p.Signers,
		SourceOperator: p.World.Operator, TargetOperator: p.World.Issuer,
		Clock: p.Clock, Tracer: p.Tracer, Metrics: p.Metrics, Audit: p.Audit,
		Actor: "svc/bridge-relayer", Tenant: "collateral-ops",
	})
	if err != nil {
		return nil, fmt.Errorf("platform: bridge: %w", err)
	}
	p.Bridge.SetTargetAddress(func(holder string) string {
		if cp, ok := p.World.Seed.Counterparty(labelOf(p.World, holder)); ok && len(cp.Parties) > 0 {
			return cp.Parties[0]
		}
		return p.World.Issuer
	})

	p.Risk = risk.New(p.Clock, p.Tracer, p.Metrics, p.Audit)
	p.Tools = tools.NewPlatform(p.Lake, p.Risk, p.World, p.Clock, p.Audit, p.Tracer)

	// Agent.
	p.Registry = agent.NewRegistry()
	if err := p.Tools.Register(p.Registry); err != nil {
		return nil, fmt.Errorf("platform: tools: %w", err)
	}
	p.Model, err = buildModel(opts)
	if err != nil {
		return nil, err
	}
	store, err := agent.NewMemoryStore(filepath.Join(dir, "runs"))
	if err != nil {
		return nil, err
	}
	p.Store = store
	cfg := agent.DefaultConfig()
	cfg.Environment = opts.Environment
	p.Approvals = agent.NewApprovalService(p.Store, p.Clock, p.Audit, p.Tracer, cfg.ApprovalWindow)
	p.Runtime, err = agent.New(cfg, agent.Deps{
		Tools: p.Registry, Model: p.Model, Guard: p.Inspector, Policy: p.Policy,
		Audit: p.Audit, Approvals: p.Approvals, Store: p.Store,
		Clock: p.Clock, Tracer: p.Tracer, Logger: p.Log, Registry: p.Metrics,
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

func buildModel(opts Options) (agent.Model, error) {
	switch opts.Model {
	case "", "planner", "deterministic":
		return agent.Planner{}, nil
	case "anthropic":
		m, err := agent.NewAnthropicModel(opts.ModelID)
		if err != nil {
			return nil, fmt.Errorf("platform: %w", err)
		}
		return m, nil
	default:
		return nil, fmt.Errorf("platform: unknown model %q", opts.Model)
	}
}

// Dir returns the state directory.
func (p *Platform) Dir() string { return p.dir }

// Advance moves a deterministic platform's clock. It is a no-op on a real
// clock, so callers do not have to branch.
func (p *Platform) Advance(d time.Duration) {
	if mc, ok := p.Clock.(*obs.ManualClock); ok {
		mc.Advance(d)
	}
}

// Close releases everything the platform opened.
func (p *Platform) Close() error {
	var errs []error
	for i := len(p.cleanup) - 1; i >= 0; i-- {
		if err := p.cleanup[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// principals
// ---------------------------------------------------------------------------

// Principals are the demo and test identities. They are declared here rather
// than invented at each call site so that the four-eyes rule can be checked:
// the agent acts for an operator, and the approver is somebody else entirely.
var (
	// Analyst can read and nothing else.
	Analyst = identity.Principal{
		Subject: "r.mehta", Kind: identity.KindHuman, Tenant: "collateral-ops", Desk: "risk",
		Roles:  []string{identity.RoleRiskAnalyst},
		Scopes: []string{identity.ScopeCollateralRead, identity.ScopeExposureRead},
	}
	// Operator runs the desk and can propose, but cannot move money alone.
	Operator = identity.Principal{
		Subject: "j.okafor", Kind: identity.KindHuman, Tenant: "collateral-ops", Desk: "collateral",
		Roles: []string{identity.RoleCollateralOps},
		Scopes: []string{identity.ScopeCollateralRead, identity.ScopeExposureRead,
			identity.ScopeMarginPropose, identity.ScopeTransferDraft},
	}
	// AgentPrincipal is the workload identity the runtime executes as, always
	// on behalf of a named human.
	AgentPrincipal = identity.Principal{
		Subject: "svc/collateral-agent", Kind: identity.KindAgent, Tenant: "collateral-ops",
		OnBehalf: "j.okafor", Desk: "collateral",
		Roles: []string{identity.RoleCollateralOps},
		Scopes: []string{identity.ScopeCollateralRead, identity.ScopeExposureRead,
			identity.ScopeMarginPropose, identity.ScopeTransferDraft,
			identity.ScopeTransferExecute, identity.ScopeMarginIssue, identity.ScopeRedeem},
	}
	// RiskManager approves. She holds no execution scopes, only the mandate to
	// authorise somebody else's.
	RiskManager = identity.Principal{
		Subject: "s.aldridge", Kind: identity.KindHuman, Tenant: "collateral-ops", Desk: "risk",
		Roles:  []string{identity.RoleRiskManager},
		Scopes: []string{identity.ScopeCollateralRead, identity.ScopeExposureRead, identity.ScopeApprove},
	}
	// Treasurer is the second approver for anything above the dual threshold.
	Treasurer = identity.Principal{
		Subject: "m.duarte", Kind: identity.KindHuman, Tenant: "collateral-ops", Desk: "treasury",
		Roles:  []string{identity.RoleTreasurer},
		Scopes: []string{identity.ScopeCollateralRead, identity.ScopeExposureRead, identity.ScopeApprove},
	}
	// Auditor may read the record and nothing else.
	Auditor = identity.Principal{
		Subject: "k.novak", Kind: identity.KindHuman, Tenant: "collateral-ops", Desk: "audit",
		Roles:  []string{identity.RoleAuditor},
		Scopes: []string{identity.ScopeCollateralRead},
	}
)

// PrincipalByName resolves one of the declared principals.
func PrincipalByName(name string) (identity.Principal, bool) {
	switch name {
	case "analyst", Analyst.Subject:
		return Analyst, true
	case "operator", Operator.Subject:
		return Operator, true
	case "agent", AgentPrincipal.Subject:
		return AgentPrincipal, true
	case "risk-manager", RiskManager.Subject:
		return RiskManager, true
	case "treasurer", Treasurer.Subject:
		return Treasurer, true
	case "auditor", Auditor.Subject:
		return Auditor, true
	}
	return identity.Principal{}, false
}

// PrincipalNames lists the selectable principals.
func PrincipalNames() []string {
	return []string{"analyst", "operator", "agent", "risk-manager", "treasurer", "auditor"}
}

// labelOf resolves a ledger address back to the counterparty identifier the
// rest of the platform uses. The bridge speaks addresses; everything above it
// speaks counterparties, and something has to hold the translation.
func labelOf(w *ledger.World, addr string) string {
	for _, cp := range w.Seed.Counterparties {
		if a, ok := w.Wallet(cp.ID); ok && a == addr {
			return cp.ID
		}
	}
	return addr
}

// BridgeFlow locks units on the EVM network and relays them to Canton.
//
// It is here rather than in the demo because the conformance gate needs the
// same flow: SETTLE-3 asks whether every mint on the target carried a quorum
// attestation and a matching lock on the source, and that question can only be
// answered about a bridge that has actually run.
func (p *Platform) BridgeFlow(ctx context.Context, counterparty string, qty int64) (bridge.PollResult, error) {
	holder, ok := p.World.Wallet(counterparty)
	if !ok {
		return bridge.PollResult{}, fmt.Errorf("platform: %s has no wallet", counterparty)
	}
	if _, _, err := p.Bridge.Lock(ctx, holder, domain.FundSymbol, qty); err != nil {
		return bridge.PollResult{}, err
	}
	// Finality is a number of confirmations, and confirmations are blocks. The
	// simulator only produces a block when something is written to it, so the
	// flow needs the chain to move on before the relayer will attest — which is
	// exactly the wait a real relayer performs and the window a reorg lives in.
	for range 3 {
		if _, err := p.World.EVM.Mint(ctx, p.World.Operator, p.World.Operator,
			domain.FundSymbol, 1, "block advance"); err != nil {
			return bridge.PollResult{}, err
		}
	}
	return p.Bridge.Poll(ctx)
}
