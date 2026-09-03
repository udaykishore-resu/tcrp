package risk

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/money"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Errors returned by the engine. They are sentinels because a caller that has
// to distinguish "this counterparty does not exist" from "we have no price for
// this bond" is making two different decisions, and matching on message text is
// not a decision anybody should have to defend.
var (
	// ErrUnknownCounterparty is returned when the snapshot has no such counterparty.
	ErrUnknownCounterparty = errors.New("risk: unknown counterparty")
	// ErrUnknownInstrument is returned when a position names an instrument the
	// snapshot does not describe.
	ErrUnknownInstrument = errors.New("risk: unknown instrument")
	// ErrNoPrice is returned when an instrument has no mark in the snapshot. It
	// is an error rather than a zero, because an unpriced pledge valued at zero
	// and an unpriced pledge valued at par are both wrong and only one of them
	// is obviously wrong.
	ErrNoPrice = errors.New("risk: no price for instrument")
	// ErrNoScenario is returned when a stress run names a scenario that is empty.
	ErrNoScenario = errors.New("risk: scenario has no id")
)

// Concentration dimensions. A dimension is the axis a portfolio is cut along;
// the bucket is the value on that axis.
const (
	DimensionIssuer = "issuer"
	DimensionClass  = "asset_class"
	DimensionSector = "sector"
	DimensionLedger = "ledger"
)

// CriticalAt is the utilisation at which a breach becomes critical: a limit
// exceeded by half as much again is a different conversation from a limit
// exceeded at all, and the desk escalates it differently.
var CriticalAt = money.Ratio{Num: 3, Den: 2}

// Terms are the collateral agreement's operational parameters. They belong to
// the legal agreement rather than to the market data, so they are configured on
// the engine and not read out of the snapshot.
type Terms struct {
	// Threshold is the unsecured exposure the agreement permits before a call
	// becomes due.
	Threshold money.Amount
	// MinimumTransfer is the smallest call worth settling; a shortfall below it
	// is not called.
	MinimumTransfer money.Amount
}

// DefaultTerms are the terms applied to a counterparty with no specific
// agreement configured.
func DefaultTerms(base money.Currency) Terms {
	return Terms{
		Threshold:       money.FromInt(base, 500_000),
		MinimumTransfer: money.FromInt(base, 250_000),
	}
}

// DefaultFX is the FX table the engine uses when none is configured. It carries
// only the currencies the seeded portfolio holds; anything else is a missing
// rate and therefore an error, which is the intended behaviour.
func DefaultFX() money.FXTable {
	return money.FXTable{
		Base: money.USD,
		Rates: map[money.Currency]money.Ratio{
			money.EUR: {Num: 108, Den: 100},
			money.GBP: {Num: 127, Den: 100},
		},
	}
}

// Engine computes collateral, exposure, concentration, limits and stress from a
// snapshot. It is safe for concurrent use and holds no per-calculation state.
type Engine struct {
	clock   obs.Clock
	tracer  *obs.Tracer
	metrics *Metrics
	audit   *audit.Log

	base    money.Currency
	fx      money.FXTable
	terms   Terms
	byCP    map[string]Terms
	workers int
	fund    string
}

// Option configures an Engine.
type Option func(*Engine)

// WithFX replaces the FX table. The table's base currency becomes the currency
// every aggregate figure is reported in.
func WithFX(fx money.FXTable) Option {
	return func(e *Engine) {
		e.fx = fx
		if fx.Base != "" {
			e.base = fx.Base
		}
	}
}

// WithTerms sets the default collateral agreement terms.
func WithTerms(t Terms) Option { return func(e *Engine) { e.terms = t } }

// WithCounterpartyTerms sets the terms for one counterparty, overriding the
// default.
func WithCounterpartyTerms(cp string, t Terms) Option {
	return func(e *Engine) {
		if e.byCP == nil {
			e.byCP = map[string]Terms{}
		}
		e.byCP[cp] = t
	}
}

// WithWorkers bounds the fan-out used by ExposureAll and Stress. A value below
// one restores the default of runtime.NumCPU.
func WithWorkers(n int) Option {
	return func(e *Engine) {
		if n < 1 {
			n = runtime.NumCPU()
		}
		e.workers = n
	}
}

// WithFundSymbol names the tokenised fund share that a redemption scenario
// redeems out of the pledge.
func WithFundSymbol(sym string) Option { return func(e *Engine) { e.fund = sym } }

// New builds an Engine. The clock, tracer, registry and audit log are all
// optional: a nil clock becomes obs.RealClock, a nil tracer produces no spans,
// a nil registry gets a private one and a nil audit log means the engine's
// breach register writes no records. That is deliberate — a calculation should
// never be impossible to run because the observability stack is not wired up.
func New(clock obs.Clock, tracer *obs.Tracer, reg *obs.Registry, log *audit.Log, opts ...Option) *Engine {
	if clock == nil {
		clock = obs.RealClock{}
	}
	fx := DefaultFX()
	e := &Engine{
		clock:   clock,
		tracer:  tracer,
		metrics: NewMetrics(reg),
		audit:   log,
		base:    fx.Base,
		fx:      fx,
		terms:   DefaultTerms(fx.Base),
		workers: runtime.NumCPU(),
		fund:    domain.FundSymbol,
	}
	for _, o := range opts {
		o(e)
	}
	if e.workers < 1 {
		e.workers = 1
	}
	return e
}

// Clock returns the engine's time source, so components built around an engine
// share one clock rather than each reaching for the wall.
func (e *Engine) Clock() obs.Clock { return e.clock }

// Metrics returns the engine's instrumentation.
func (e *Engine) Metrics() *Metrics { return e.metrics }

// Base returns the reporting currency every aggregate is expressed in.
func (e *Engine) Base() money.Currency { return e.base }

// TermsFor returns the collateral agreement terms applying to a counterparty.
func (e *Engine) TermsFor(cp string) Terms {
	if t, ok := e.byCP[cp]; ok {
		return t
	}
	return e.terms
}

// start opens a span when a tracer is configured and returns a no-op closer
// otherwise, so every method can be written as though tracing is always on.
func (e *Engine) start(ctx context.Context, name string) (context.Context, *obs.Span) {
	if e.tracer == nil {
		return ctx, nil
	}
	return e.tracer.Start(ctx, name, obs.KindInternal)
}

// ---------------------------------------------------------------------------
// Collateral
// ---------------------------------------------------------------------------

// shocks describes the deformation a stress scenario applies to a valuation.
// The zero value is the unstressed base case, which is why CollateralValue and
// the stressed valuation share one implementation and cannot drift apart.
type shocks struct {
	// price is the per-instrument price multiplier, already 1+shock.
	price func(domain.Instrument) money.Ratio
	// haircutAddOn widens every haircut.
	haircutAddOn money.Ratio
	// quantity scales a holding, used to redeem fund shares out of a pledge.
	quantity func(domain.Instrument) money.Ratio
}

// CollateralValue values a counterparty's pledge: quantity times price for each
// line, then an advance rate of one minus the instrument's haircut. An
// ineligible instrument contributes nothing and is reported in IneligibleValue,
// so the difference between "we hold nothing" and "we hold something we cannot
// lend against" survives into the output.
func (e *Engine) CollateralValue(ctx context.Context, snap domain.Snapshot, cp string) (domain.CollateralValue, error) {
	_, span := e.start(ctx, "risk.collateral_value")
	defer span.End()
	span.SetAttr("tcrp.risk.counterparty", cp)
	started := e.clock.Now()

	out, err := e.collateral(snap, cp, shocks{})
	e.metrics.ObserveCalculation("collateral_value", e.clock.Since(started), err)
	if err != nil {
		span.RecordError(err)
		return domain.CollateralValue{}, err
	}
	span.SetAttrs(map[string]any{
		"tcrp.risk.collateral_value": out.CollateralValue.String(),
		"tcrp.risk.market_value":     out.MarketValue.String(),
		"tcrp.risk.lines":            len(out.Lines),
	})
	return out, nil
}

func (e *Engine) collateral(snap domain.Snapshot, cp string, sh shocks) (domain.CollateralValue, error) {
	if _, ok := snap.Counterparty(cp); !ok {
		return domain.CollateralValue{}, fmt.Errorf("%w: %s", ErrUnknownCounterparty, cp)
	}
	positions := snap.PositionsOf(cp) // already sorted deterministically

	market, collat, ineligible := money.NewBag(), money.NewBag(), money.NewBag()
	lines := make([]domain.CollateralLine, 0, len(positions))
	var priceAsOf time.Time

	for _, p := range positions {
		if !p.Pledged || !p.ValidAt(snap.ValidAt, snap.KnownAt) {
			continue
		}
		inst, ok := snap.Instrument(p.Symbol)
		if !ok {
			return domain.CollateralValue{}, fmt.Errorf("%w: %s held by %s", ErrUnknownInstrument, p.Symbol, cp)
		}
		px, ok := snap.Prices[p.Symbol]
		if !ok {
			return domain.CollateralValue{}, fmt.Errorf("%w: %s", ErrNoPrice, p.Symbol)
		}
		if priceAsOf.IsZero() || px.AsOf.Before(priceAsOf) {
			priceAsOf = px.AsOf
		}

		price := px.Px
		if sh.price != nil {
			shocked, err := price.Mul(sh.price(inst))
			if err != nil {
				return domain.CollateralValue{}, fmt.Errorf("risk: shocking %s: %w", p.Symbol, err)
			}
			price = shocked
		}

		qty := p.Quantity
		mv, err := price.Mul(money.Ratio{Num: qty, Den: 1})
		if err != nil {
			return domain.CollateralValue{}, fmt.Errorf("risk: valuing %s: %w", p.Symbol, err)
		}
		if sh.quantity != nil {
			scaled, err := mv.Mul(sh.quantity(inst))
			if err != nil {
				return domain.CollateralValue{}, fmt.Errorf("risk: scaling %s: %w", p.Symbol, err)
			}
			mv = scaled
		}

		haircut, eligible := snap.Haircuts.For(inst)
		if eligible {
			haircut = widen(haircut, sh.haircutAddOn)
		}
		advance := haircut.Complement()

		line := domain.CollateralLine{
			Symbol: p.Symbol, Class: inst.Class, Quantity: qty, Price: price,
			Market: mv, Haircut: haircut, AdvanceRate: advance,
			Collateral: money.Zero(inst.Currency), Eligible: eligible, Ledger: p.Ledger,
		}
		if eligible {
			cv, err := mv.Mul(advance)
			if err != nil {
				return domain.CollateralValue{}, fmt.Errorf("risk: haircutting %s: %w", p.Symbol, err)
			}
			line.Collateral = cv
			collat.Add(cv)
		} else {
			line.AdvanceRate = money.Ratio{Num: 0, Den: 1}
			ineligible.Add(mv)
		}
		market.Add(mv)
		lines = append(lines, line)
	}

	mvTotal, err := market.Collapse(e.base, e.fx)
	if err != nil {
		return domain.CollateralValue{}, fmt.Errorf("risk: market value of %s: %w", cp, err)
	}
	cvTotal, err := collat.Collapse(e.base, e.fx)
	if err != nil {
		return domain.CollateralValue{}, fmt.Errorf("risk: collateral value of %s: %w", cp, err)
	}
	inTotal, err := ineligible.Collapse(e.base, e.fx)
	if err != nil {
		return domain.CollateralValue{}, fmt.Errorf("risk: ineligible value of %s: %w", cp, err)
	}

	return domain.CollateralValue{
		Counterparty: cp, AsOf: snap.ValidAt, Lines: lines,
		MarketValue: mvTotal, CollateralValue: cvTotal, IneligibleValue: inTotal,
		Currency: e.base, HaircutVersion: snap.Haircuts.Version, PriceAsOf: priceAsOf,
	}, nil
}

// widen adds a stress add-on to a haircut, clamped at one. A haircut above one
// would turn an advance rate negative and make a pledge subtract from
// collateral, which is arithmetic rather than risk.
func widen(h, addOn money.Ratio) money.Ratio {
	if addOn.Num == 0 || addOn.Den == 0 {
		return h
	}
	if h.Den == 0 {
		h = money.Ratio{Num: 0, Den: 1}
	}
	num := h.Num*addOn.Den + addOn.Num*h.Den
	den := h.Den * addOn.Den
	out := reduce(money.Ratio{Num: num, Den: den})
	if out.Num > out.Den {
		return money.Ratio{Num: 1, Den: 1}
	}
	if out.Num < 0 {
		return money.Ratio{Num: 0, Den: 1}
	}
	return out
}

// ---------------------------------------------------------------------------
// Exposure
// ---------------------------------------------------------------------------

// Exposure computes what the institution stands to lose to one counterparty.
// Netting is applied only where the counterparty has an enforceable agreement;
// where it does not, the netting sets are still reported, marked unenforceable,
// so the value of signing one is visible.
func (e *Engine) Exposure(ctx context.Context, snap domain.Snapshot, cp string) (domain.Exposure, error) {
	_, span := e.start(ctx, "risk.exposure")
	defer span.End()
	span.SetAttr("tcrp.risk.counterparty", cp)
	started := e.clock.Now()

	out, err := e.exposure(snap, cp, false)
	e.metrics.ObserveCalculation("exposure", e.clock.Since(started), err)
	if err != nil {
		span.RecordError(err)
		return domain.Exposure{}, err
	}
	span.SetAttrs(map[string]any{
		"tcrp.risk.net_exposure": out.NetExposure.String(),
		"tcrp.risk.uncovered":    out.Uncovered.String(),
		"tcrp.risk.shortfall":    out.Shortfall.String(),
	})
	return out, nil
}

// exposure is the shared implementation. defaulted suppresses netting, which is
// how a close-out challenged in an insolvency is modelled.
func (e *Engine) exposure(snap domain.Snapshot, cp string, defaulted bool) (domain.Exposure, error) {
	cv, err := e.collateral(snap, cp, shocks{})
	if err != nil {
		return domain.Exposure{}, err
	}
	return e.exposureFrom(snap, cp, cv.CollateralValue, defaulted)
}

// exposureFrom builds an exposure from an already-computed collateral figure,
// so a stress run does not value the same pledge twice.
func (e *Engine) exposureFrom(snap domain.Snapshot, cp string, collateral money.Amount, defaulted bool) (domain.Exposure, error) {
	party, ok := snap.Counterparty(cp)
	if !ok {
		return domain.Exposure{}, fmt.Errorf("%w: %s", ErrUnknownCounterparty, cp)
	}
	obligations := snap.ObligationsOf(cp)
	sort.Slice(obligations, func(i, j int) bool { return obligations[i].ID < obligations[j].ID })

	gross := money.Zero(e.base)
	setNet := map[string]money.Amount{}
	setGross := map[string]money.Amount{}
	setCount := map[string]int{}
	var setIDs []string

	for _, o := range obligations {
		amt, err := e.fx.Convert(o.Amount, e.base)
		if err != nil {
			return domain.Exposure{}, fmt.Errorf("risk: obligation %s: %w", o.ID, err)
		}
		if amt.Units > 0 {
			if gross, err = gross.Add(amt); err != nil {
				return domain.Exposure{}, err
			}
		}
		id := o.NettingSet
		if id == "" {
			// An obligation outside any agreement is its own set, so it can
			// never be netted against one that is inside an agreement.
			id = "unset:" + o.ID
		}
		if _, seen := setNet[id]; !seen {
			setIDs = append(setIDs, id)
			setNet[id] = money.Zero(e.base)
			setGross[id] = money.Zero(e.base)
		}
		if setNet[id], err = setNet[id].Add(amt); err != nil {
			return domain.Exposure{}, err
		}
		if amt.Units > 0 {
			if setGross[id], err = setGross[id].Add(amt); err != nil {
				return domain.Exposure{}, err
			}
		}
		setCount[id]++
	}
	sort.Strings(setIDs)

	enforced := party.Netting && !defaulted
	sets := make([]domain.NettingSet, 0, len(setIDs))
	net := money.Zero(e.base)
	for _, id := range setIDs {
		n := setNet[id]
		sets = append(sets, domain.NettingSet{
			ID: id, Gross: setGross[id], Net: n, Count: setCount[id], Enforced: enforced,
		})
		if enforced && n.Units > 0 {
			var err error
			if net, err = net.Add(n); err != nil {
				return domain.Exposure{}, err
			}
		}
	}
	if !enforced {
		net = gross
	}

	uncovered := money.Zero(e.base)
	excess := money.Zero(e.base)
	if net.Greater(collateral) {
		uncovered = net.MustSub(collateral)
	} else {
		excess = collateral.MustSub(net)
	}

	terms := e.TermsFor(cp)
	threshold, err := e.fx.Convert(terms.Threshold, e.base)
	if err != nil {
		return domain.Exposure{}, fmt.Errorf("risk: threshold for %s: %w", cp, err)
	}
	mta, err := e.fx.Convert(terms.MinimumTransfer, e.base)
	if err != nil {
		return domain.Exposure{}, fmt.Errorf("risk: minimum transfer for %s: %w", cp, err)
	}
	shortfall := money.Zero(e.base)
	if uncovered.Greater(threshold) {
		s := uncovered.MustSub(threshold)
		if !s.Less(mta) {
			shortfall = s
		}
	}

	return domain.Exposure{
		Counterparty: cp, AsOf: snap.ValidAt,
		GrossExposure: gross, NetExposure: net, Collateral: collateral,
		Uncovered: uncovered, Shortfall: shortfall, Excess: excess,
		NettingSets: sets, Currency: e.base, Covered: uncovered.IsZero(),
		Threshold: threshold, MinimumTransfer: mta,
	}, nil
}

// ExposureAll computes every counterparty's exposure across a bounded worker
// pool. Results are returned in counterparty-id order regardless of the order
// the workers finished in, because a report that reshuffles itself between runs
// cannot be diffed.
func (e *Engine) ExposureAll(ctx context.Context, snap domain.Snapshot) ([]domain.Exposure, error) {
	ctx, span := e.start(ctx, "risk.exposure_all")
	defer span.End()
	started := e.clock.Now()

	ids := counterpartyIDs(snap)
	out := make([]domain.Exposure, len(ids))
	errs := make([]error, len(ids))

	e.fanOut(ctx, len(ids), func(i int) {
		out[i], errs[i] = e.exposure(snap, ids[i], false)
	})

	err := firstError(errs)
	e.metrics.ObserveCalculation("exposure_all", e.clock.Since(started), err)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	span.SetAttr("tcrp.risk.counterparties", len(out))
	return out, nil
}

// fanOut runs fn for each index across at most e.workers goroutines. Each call
// writes into its own slot, so there is no shared mutable state and no
// ordering to recover afterwards.
func (e *Engine) fanOut(ctx context.Context, n int, fn func(i int)) {
	if n == 0 {
		return
	}
	workers := e.workers
	if workers > n {
		workers = n
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				if ctx != nil && ctx.Err() != nil {
					return
				}
				fn(i)
			}
		}()
	}
	wg.Wait()
}

func firstError(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func counterpartyIDs(snap domain.Snapshot) []string {
	ids := make([]string, 0, len(snap.Counterparties))
	for _, c := range snap.Counterparties {
		ids = append(ids, c.ID)
	}
	sort.Strings(ids)
	return ids
}

// ---------------------------------------------------------------------------
// Concentration
// ---------------------------------------------------------------------------

// Concentration measures how the total haircut-adjusted collateral pool is
// distributed across issuers, asset classes, sectors and ledgers. Shares are
// exact rationals over one denominator, so the shares within a dimension sum to
// exactly one.
func (e *Engine) Concentration(ctx context.Context, snap domain.Snapshot) ([]domain.Concentration, error) {
	ctx, span := e.start(ctx, "risk.concentration")
	defer span.End()
	started := e.clock.Now()

	out, err := e.concentration(ctx, snap)
	e.metrics.ObserveCalculation("concentration", e.clock.Since(started), err)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	span.SetAttr("tcrp.risk.buckets", len(out))
	return out, nil
}

func (e *Engine) concentration(ctx context.Context, snap domain.Snapshot) ([]domain.Concentration, error) {
	ids := counterpartyIDs(snap)
	values := make([]domain.CollateralValue, len(ids))
	errs := make([]error, len(ids))
	e.fanOut(ctx, len(ids), func(i int) {
		values[i], errs[i] = e.collateral(snap, ids[i], shocks{})
	})
	if err := firstError(errs); err != nil {
		return nil, err
	}

	totals := map[string]map[string]money.Amount{
		DimensionIssuer: {}, DimensionClass: {}, DimensionSector: {}, DimensionLedger: {},
	}
	total := money.Zero(e.base)
	for _, cv := range values {
		for _, line := range cv.Lines {
			if !line.Eligible {
				continue
			}
			v, err := e.fx.Convert(line.Collateral, e.base)
			if err != nil {
				return nil, fmt.Errorf("risk: concentration of %s: %w", line.Symbol, err)
			}
			inst, ok := snap.Instrument(line.Symbol)
			if !ok {
				return nil, fmt.Errorf("%w: %s", ErrUnknownInstrument, line.Symbol)
			}
			if total, err = total.Add(v); err != nil {
				return nil, err
			}
			// A fixed slice rather than a map literal: the accumulation would be
			// order-independent either way, but the package's rule is that no
			// map is ever ranged over, so there is nothing to argue about.
			for _, cut := range [4][2]string{
				{DimensionIssuer, inst.Issuer},
				{DimensionClass, string(inst.Class)},
				{DimensionSector, inst.Sector},
				{DimensionLedger, string(line.Ledger)},
			} {
				cur, ok := totals[cut[0]][cut[1]]
				if !ok {
					cur = money.Zero(e.base)
				}
				if cur, err = cur.Add(v); err != nil {
					return nil, err
				}
				totals[cut[0]][cut[1]] = cur
			}
		}
	}

	limits := e.concentrationLimits(snap)
	out := make([]domain.Concentration, 0, 32)
	for _, dim := range []string{DimensionIssuer, DimensionClass, DimensionSector, DimensionLedger} {
		buckets := make([]string, 0, len(totals[dim]))
		for b := range totals[dim] {
			buckets = append(buckets, b)
		}
		sort.Strings(buckets)
		for _, b := range buckets {
			v := totals[dim][b]
			c := domain.Concentration{Dimension: dim, Bucket: b, Value: v, Share: share(v, total)}
			if lim, ok := limits[dim][b]; ok {
				c.LimitPc = lim.Ratio
				c.Breached = cmpRatio(c.Share, lim.Ratio) > 0
			} else if lim, ok := limits[dim]["*"]; ok {
				c.LimitPc = lim.Ratio
				c.Breached = cmpRatio(c.Share, lim.Ratio) > 0
			}
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Dimension != out[j].Dimension {
			return out[i].Dimension < out[j].Dimension
		}
		if out[i].Value.Units != out[j].Value.Units {
			return out[i].Value.Units > out[j].Value.Units
		}
		return out[i].Bucket < out[j].Bucket
	})
	return out, nil
}

// concentrationLimits indexes the snapshot's concentration limits by dimension
// and scope.
func (e *Engine) concentrationLimits(snap domain.Snapshot) map[string]map[string]domain.Limit {
	out := map[string]map[string]domain.Limit{
		DimensionIssuer: {}, DimensionClass: {}, DimensionSector: {}, DimensionLedger: {},
	}
	for _, l := range snap.Limits {
		dim, ok := dimensionOf(l.Kind)
		if !ok {
			continue
		}
		scope := l.Scope
		if scope == "" {
			scope = "*"
		}
		out[dim][scope] = l
	}
	return out
}

func dimensionOf(k domain.LimitKind) (string, bool) {
	switch k {
	case domain.LimitIssuerConc:
		return DimensionIssuer, true
	case domain.LimitClassConc:
		return DimensionClass, true
	case domain.LimitSectorConc:
		return DimensionSector, true
	case domain.LimitLedgerConc:
		return DimensionLedger, true
	default:
		return "", false
	}
}

// share returns v/total as an exact reduced rational. A zero total yields zero
// rather than an invalid ratio, so a portfolio with no collateral renders
// instead of dividing by zero.
func share(v, total money.Amount) money.Ratio {
	if total.Units == 0 {
		return money.Ratio{Num: 0, Den: 1}
	}
	return reduce(money.Ratio{Num: v.Units, Den: total.Units})
}

// ---------------------------------------------------------------------------
// Limits
// ---------------------------------------------------------------------------

// CheckLimits evaluates every limit in the snapshot and returns the breaches,
// worst first. A limit that is comfortably observed produces nothing: the
// output is the exception list, not the report.
func (e *Engine) CheckLimits(ctx context.Context, snap domain.Snapshot) ([]domain.Breach, error) {
	ctx, span := e.start(ctx, "risk.check_limits")
	defer span.End()
	started := e.clock.Now()

	out, err := e.checkLimits(ctx, snap, span)
	e.metrics.ObserveCalculation("check_limits", e.clock.Since(started), err)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	span.SetAttr("tcrp.risk.breaches", len(out))
	return out, nil
}

func (e *Engine) checkLimits(ctx context.Context, snap domain.Snapshot, span *obs.Span) ([]domain.Breach, error) {
	exposures, err := e.ExposureAll(ctx, snap)
	if err != nil {
		return nil, err
	}
	byCP := make(map[string]domain.Exposure, len(exposures))
	aggregate := money.Zero(e.base)
	for _, x := range exposures {
		byCP[x.Counterparty] = x
		if aggregate, err = aggregate.Add(x.Uncovered); err != nil {
			return nil, err
		}
	}
	// Through the exported method, so the concentration pass inside a limit
	// check is spanned and timed like any other calculation.
	conc, err := e.Concentration(ctx, snap)
	if err != nil {
		return nil, err
	}
	byDim := map[string][]domain.Concentration{}
	for _, c := range conc {
		byDim[c.Dimension] = append(byDim[c.Dimension], c)
	}

	limits := append([]domain.Limit(nil), snap.Limits...)
	sort.Slice(limits, func(i, j int) bool { return limits[i].ID < limits[j].ID })

	now := e.clock.Now()
	traceID := ""
	if span != nil {
		traceID = span.Context().TraceID
	}

	var out []domain.Breach
	for _, l := range limits {
		if !l.Effective.IsZero() && snap.ValidAt.Before(l.Effective) {
			continue
		}
		switch l.Kind {
		case domain.LimitCounterparty:
			for _, x := range exposures {
				if l.Scope != "*" && l.Scope != "" && l.Scope != x.Counterparty {
					continue
				}
				if b, ok := amountBreach(l, x.Counterparty, x.Uncovered, now, snap.ValidAt, traceID); ok {
					out = append(out, b)
				}
			}
		case domain.LimitUncoveredAggr:
			scope := l.Scope
			if scope == "" {
				scope = "*"
			}
			if b, ok := amountBreach(l, scope, aggregate, now, snap.ValidAt, traceID); ok {
				out = append(out, b)
			}
		case domain.LimitIssuerConc, domain.LimitClassConc, domain.LimitSectorConc, domain.LimitLedgerConc:
			dim, _ := dimensionOf(l.Kind)
			for _, c := range byDim[dim] {
				if l.Scope != "*" && l.Scope != "" && l.Scope != c.Bucket {
					continue
				}
				if b, ok := ratioBreach(l, c.Bucket, c.Share, c.Value, now, snap.ValidAt, traceID); ok {
					out = append(out, b)
				}
			}
		}
	}
	sortBreaches(out)
	return out, nil
}

// breachID is deterministic so that the same condition recomputed produces the
// same identity, which is what lets the register deduplicate rather than raise
// the same breach every cycle.
func breachID(limitID, scope string) string {
	scope = strings.ReplaceAll(scope, " ", "_")
	if scope == "" {
		scope = "*"
	}
	return "BRC-" + limitID + "-" + scope
}

func amountBreach(l domain.Limit, scope string, observed money.Amount, now, asOf time.Time, traceID string) (domain.Breach, bool) {
	sev, ok := severityFor(big.NewInt(observed.Units), big.NewInt(l.Amount.Units), l.WarnAt)
	if !ok {
		return domain.Breach{}, false
	}
	return domain.Breach{
		ID: breachID(l.ID, scope), LimitID: l.ID, Kind: l.Kind, Scope: scope,
		Observed: observed, Limit: l.Amount, Severity: sev, Status: domain.BreachOpen,
		DetectedAt: now, AsOf: asOf, TraceID: traceID,
	}, true
}

func ratioBreach(l domain.Limit, scope string, observed money.Ratio, value money.Amount, now, asOf time.Time, traceID string) (domain.Breach, bool) {
	if observed.Den == 0 || l.Ratio.Den == 0 || l.Ratio.Num == 0 {
		return domain.Breach{}, false
	}
	// utilisation = observed / limit = (oN·lD) / (oD·lN)
	num := new(big.Int).Mul(big.NewInt(observed.Num), big.NewInt(l.Ratio.Den))
	den := new(big.Int).Mul(big.NewInt(observed.Den), big.NewInt(l.Ratio.Num))
	sev, ok := severityFor(num, den, l.WarnAt)
	if !ok {
		return domain.Breach{}, false
	}
	return domain.Breach{
		ID: breachID(l.ID, scope), LimitID: l.ID, Kind: l.Kind, Scope: scope,
		Observed: value, ObservedPc: observed, LimitPc: l.Ratio,
		Severity: sev, Status: domain.BreachOpen, DetectedAt: now, AsOf: asOf, TraceID: traceID,
	}, true
}

// severityFor grades utilisation num/den against the warning threshold. The
// comparisons are exact, so a value sitting precisely on a boundary is graded
// the same way every time.
func severityFor(num, den *big.Int, warnAt money.Ratio) (domain.BreachSeverity, bool) {
	if den.Sign() == 0 {
		return "", false
	}
	if num.Sign() <= 0 {
		return "", false
	}
	switch {
	case cmpBig(num, den, big.NewInt(CriticalAt.Num), big.NewInt(CriticalAt.Den)) >= 0:
		return domain.BreachCritical, true
	case cmpBig(num, den, big.NewInt(1), big.NewInt(1)) >= 0:
		return domain.BreachHard, true
	}
	if warnAt.Den == 0 || warnAt.Num == 0 {
		return "", false
	}
	if cmpBig(num, den, big.NewInt(warnAt.Num), big.NewInt(warnAt.Den)) >= 0 {
		return domain.BreachWarn, true
	}
	return "", false
}

var breachRank = map[domain.BreachSeverity]int{
	domain.BreachCritical: 3, domain.BreachHard: 2, domain.BreachWarn: 1,
}

func sortBreaches(bs []domain.Breach) {
	sort.SliceStable(bs, func(i, j int) bool {
		if breachRank[bs[i].Severity] != breachRank[bs[j].Severity] {
			return breachRank[bs[i].Severity] > breachRank[bs[j].Severity]
		}
		return bs[i].ID < bs[j].ID
	})
}

// ---------------------------------------------------------------------------
// Stress
// ---------------------------------------------------------------------------

// Stress runs one scenario across every counterparty, fanned out over the
// engine's worker pool. It reports the change in collateral value, the new
// uncovered exposure and the shortfall the scenario creates that did not exist
// in the base case.
func (e *Engine) Stress(ctx context.Context, snap domain.Snapshot, sc domain.StressScenario) (domain.StressSummary, error) {
	ctx, span := e.start(ctx, "risk.stress")
	defer span.End()
	span.SetAttr("tcrp.risk.scenario", sc.ID)
	started := e.clock.Now()

	out, err := e.stress(ctx, snap, sc)
	elapsed := e.clock.Since(started)
	e.metrics.ObserveCalculation("stress", elapsed, err)
	e.metrics.ObserveStress(sc.ID, elapsed)
	if err != nil {
		span.RecordError(err)
		return domain.StressSummary{}, err
	}
	span.SetAttrs(map[string]any{
		"tcrp.risk.total_new_shortfall": out.TotalShortfall.String(),
		"tcrp.risk.worst_counterparty":  out.WorstCP,
	})
	return out, nil
}

func (e *Engine) stress(ctx context.Context, snap domain.Snapshot, sc domain.StressScenario) (domain.StressSummary, error) {
	if sc.ID == "" {
		return domain.StressSummary{}, ErrNoScenario
	}
	started := e.clock.Now()
	ids := counterpartyIDs(snap)
	sh := e.shocksFor(sc)
	defaulted := make(map[string]bool, len(sc.DefaultedCounterparties))
	for _, d := range sc.DefaultedCounterparties {
		defaulted[d] = true
	}
	cpLimit, hasCPLimit := counterpartyLimit(snap)

	results := make([]domain.StressResult, len(ids))
	errs := make([]error, len(ids))

	e.fanOut(ctx, len(ids), func(i int) {
		cp := ids[i]
		base, err := e.exposure(snap, cp, false)
		if err != nil {
			errs[i] = err
			return
		}
		stressedCV, err := e.collateral(snap, cp, sh)
		if err != nil {
			errs[i] = err
			return
		}
		stressed, err := e.exposureFrom(snap, cp, stressedCV.CollateralValue, defaulted[cp])
		if err != nil {
			errs[i] = err
			return
		}
		delta, err := stressed.Collateral.Sub(base.Collateral)
		if err != nil {
			errs[i] = err
			return
		}
		newShort := money.Zero(e.base)
		if stressed.Uncovered.Greater(base.Uncovered) {
			newShort = stressed.Uncovered.MustSub(base.Uncovered)
		}
		results[i] = domain.StressResult{
			Scenario: sc.ID, Counterparty: cp,
			BaseCollateral: base.Collateral, StressedValue: stressed.Collateral,
			BaseUncovered: base.Uncovered, StressedUncov: stressed.Uncovered,
			Delta: delta, NewShortfall: newShort,
			BreachesLimit: hasCPLimit && stressed.Uncovered.Greater(cpLimit),
		}
	})
	if err := firstError(errs); err != nil {
		return domain.StressSummary{}, err
	}

	totalBase, totalStressed, totalShort := money.Zero(e.base), money.Zero(e.base), money.Zero(e.base)
	worst, worstAmt := "", money.Zero(e.base)
	for _, r := range results {
		var err error
		if totalBase, err = totalBase.Add(r.BaseCollateral); err != nil {
			return domain.StressSummary{}, err
		}
		if totalStressed, err = totalStressed.Add(r.StressedValue); err != nil {
			return domain.StressSummary{}, err
		}
		if totalShort, err = totalShort.Add(r.NewShortfall); err != nil {
			return domain.StressSummary{}, err
		}
		// Ties break on the lexicographically first counterparty; results are
		// already in id order, so the first one wins and the answer is stable.
		if r.NewShortfall.Greater(worstAmt) {
			worst, worstAmt = r.Counterparty, r.NewShortfall
		}
	}

	return domain.StressSummary{
		Scenario: sc.ID, AsOf: snap.ValidAt, Results: results,
		TotalBase: totalBase, TotalStressed: totalStressed, TotalShortfall: totalShort,
		WorstCP: worst, DurationMS: float64(e.clock.Since(started).Nanoseconds()) / 1e6,
		Counterparties: len(results),
	}, nil
}

// StressAll runs several scenarios concurrently and returns their summaries in
// the order the scenarios were given, so a report's rows do not depend on which
// scenario happened to finish first.
func (e *Engine) StressAll(ctx context.Context, snap domain.Snapshot, scenarios []domain.StressScenario) ([]domain.StressSummary, error) {
	ctx, span := e.start(ctx, "risk.stress_all")
	defer span.End()
	span.SetAttr("tcrp.risk.scenarios", len(scenarios))

	out := make([]domain.StressSummary, len(scenarios))
	errs := make([]error, len(scenarios))

	workers := e.workers
	if workers > len(scenarios) {
		workers = len(scenarios)
	}
	if workers < 1 {
		workers = 1
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(scenarios) {
					return
				}
				// Through the exported method so a scenario run is spanned and
				// timed the same way whether it was asked for alone or in a set.
				out[i], errs[i] = e.Stress(ctx, snap, scenarios[i])
			}
		}()
	}
	wg.Wait()

	if err := firstError(errs); err != nil {
		span.RecordError(err)
		return nil, err
	}
	return out, nil
}

// shocksFor turns a scenario into the valuation deformation the collateral
// walk applies.
func (e *Engine) shocksFor(sc domain.StressScenario) shocks {
	classes := make(map[domain.AssetClass]money.Ratio, len(sc.ClassShocks))
	for k, v := range sc.ClassShocks {
		classes[k] = v
	}
	base := sc.PriceShock
	fund := e.fund
	redemption := sc.RedemptionRate

	sh := shocks{haircutAddOn: sc.HaircutAddOn}
	if base.Num != 0 || len(classes) > 0 {
		sh.price = func(i domain.Instrument) money.Ratio {
			s := base
			if cs, ok := classes[i.Class]; ok {
				s = cs
			}
			if s.Den == 0 || s.Num == 0 {
				return money.One
			}
			return reduce(money.Ratio{Num: s.Den + s.Num, Den: s.Den})
		}
	}
	if redemption.Num != 0 && redemption.Den != 0 {
		remaining := reduce(money.Ratio{Num: redemption.Den - redemption.Num, Den: redemption.Den})
		sh.quantity = func(i domain.Instrument) money.Ratio {
			if i.Symbol == fund {
				return remaining
			}
			return money.One
		}
	}
	return sh
}

// counterpartyLimit returns the wildcard counterparty exposure limit, if one
// exists, so a stress result can say whether it would break it.
func counterpartyLimit(snap domain.Snapshot) (money.Amount, bool) {
	for _, l := range snap.Limits {
		if l.Kind == domain.LimitCounterparty && (l.Scope == "*" || l.Scope == "") {
			return l.Amount, true
		}
	}
	return money.Amount{}, false
}

// ---------------------------------------------------------------------------
// Exact rational helpers
// ---------------------------------------------------------------------------

// reduce divides a ratio by the greatest common divisor of its terms and
// normalises the sign onto the numerator, which keeps the numbers small enough
// that a subsequent multiplication does not need big.Int.
func reduce(r money.Ratio) money.Ratio {
	if r.Den == 0 {
		return r
	}
	if r.Den < 0 {
		r.Num, r.Den = -r.Num, -r.Den
	}
	g := gcd(abs64(r.Num), r.Den)
	if g > 1 {
		r.Num /= g
		r.Den /= g
	}
	return r
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	if a == 0 {
		return 1
	}
	return a
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// cmpRatio compares two ratios exactly, using big.Int so that a share whose
// denominator is a portfolio's total micro-units cannot overflow the comparison.
func cmpRatio(a, b money.Ratio) int {
	if a.Den == 0 || b.Den == 0 {
		return 0
	}
	return cmpBig(big.NewInt(a.Num), big.NewInt(a.Den), big.NewInt(b.Num), big.NewInt(b.Den))
}

// cmpBig compares an/ad with bn/bd. Denominators are assumed positive, which
// every caller guarantees by construction.
func cmpBig(an, ad, bn, bd *big.Int) int {
	l := new(big.Int).Mul(an, bd)
	r := new(big.Int).Mul(bn, ad)
	return l.Cmp(r)
}
