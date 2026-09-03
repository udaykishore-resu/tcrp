// Package risk turns a snapshot into the numbers a desk is accountable for:
// what a counterparty's pledge is worth after haircuts, what the institution
// stands to lose if that counterparty fails, and which of those figures has
// broken a limit.
//
// The engine is a pure function of a domain.Snapshot. It reads no clock of its
// own, opens no connection and holds no state between calls, so the same
// snapshot always produces byte-identical results — which is what makes a
// figure on a dashboard defensible three months later when a counterparty
// disputes it. Every map is sorted before it is iterated, every worker pool
// writes into an indexed slot rather than appending, and the only time the
// engine reads comes from the obs.Clock it was given.
//
// Money is never a float64. A market value is a quantity multiplied into a
// money.Amount, a haircut is an exact money.Ratio, and the single rounding —
// half-even, at 1e-6 — happens inside the money package where it can be
// audited. Positions in different currencies accumulate in a money.Bag and are
// collapsed through an explicit money.FXTable; a missing rate is an error,
// because a silent 1:1 is how a collateral shortfall goes unnoticed.
//
// # The exposure formula
//
// For a counterparty c against snapshot s, with base currency B and FX table F:
//
//	Per position p (pledged, and valid at s.ValidAt as known at s.KnownAt),
//	holding instrument i at price px(i):
//
//	    market(p)     = px(i) × quantity(p)                  [in i's currency]
//	    haircut(i)    = s.Haircuts.For(i)                    [exact ratio]
//	    advance(i)    = 1 − haircut(i)
//	    collateral(p) = market(p) × advance(i)  if i is eligible
//	                  = 0                       if i is not
//
//	An instrument with no rule in the schedule is ineligible, not unhaircut.
//	Ineligible market value is reported separately as IneligibleValue so that
//	"we hold it but we do not lend against it" is visible rather than absent.
//
//	    MarketValue     = F(Σ market(p), B)
//	    CollateralValue = F(Σ collateral(p), B)
//
//	Line-level Market and Collateral stay in the instrument's own currency;
//	only the totals are converted, so a reader can check a line against a
//	trade ticket without unwinding an FX rate.
//
//	Over obligations o of c, each converted into B:
//
//	    Gross = Σ max(o, 0)
//
//	Gross ignores every offset: it is what would be lost if nothing nets. A
//	negative obligation is owed by the institution to the counterparty, so it
//	is not exposure and does not enter Gross.
//
//	Netting sets are the groups o.NettingSet. For each set k:
//
//	    net(k) = Σ_{o ∈ k} o
//
//	    Net = Σ_k max(net(k), 0)    if c.Netting is true
//	        = Gross                 otherwise
//
//	Netting is applied only where the counterparty has an enforceable netting
//	agreement. Where it does not, the arithmetic net is still reported on each
//	NettingSet with Enforced false — the desk should be able to see what an
//	agreement would have been worth — but the exposure that counts is Gross.
//	A netting set that nets to a negative number contributes zero, not a
//	credit against another set: sets do not offset each other.
//
//	    Uncovered = max(Net − CollateralValue, 0)
//	    Excess    = max(CollateralValue − Net, 0)
//	    Covered   = Uncovered = 0
//
//	Finally the agreement's operational terms turn an uncovered number into a
//	callable one:
//
//	    Shortfall = 0                             if Uncovered ≤ Threshold
//	              = 0                             if Uncovered − Threshold < MinimumTransfer
//	              = Uncovered − Threshold         otherwise
//
//	Threshold is the unsecured amount the agreement permits before a call is
//	due; MinimumTransfer suppresses a call too small to be worth settling.
//	Both are properties of the agreement, not of the snapshot, so they come
//	from the engine's Terms rather than from the data.
//
// # Concentration and limits
//
// Concentration is a share of total haircut-adjusted collateral value across
// every counterparty, cut four ways: issuer, asset class, sector and ledger.
// Shares are exact rationals over the same denominator, so the shares within
// one dimension sum to exactly one rather than to 0.9999.
//
// A counterparty_exposure limit is measured against Uncovered, not against
// Gross or Net. A fully collateralised repo book consumes no credit appetite,
// and a limit that says otherwise makes the desk hold collateral it cannot
// count. Concentration limits are measured against the share; the aggregate
// limit against the sum of Uncovered over all counterparties.
//
// Severity is utilisation — observed ÷ limit — compared exactly:
//
//	utilisation ≥ CriticalAt (1.5) → critical
//	utilisation ≥ 1                → breach
//	utilisation ≥ limit.WarnAt     → warning
//	otherwise                      → not reported
//
// The comparisons are done on rationals with math/big, so a value exactly on
// a boundary lands on the more severe side deterministically instead of
// wobbling with float error.
//
// # Stress
//
// A scenario shocks prices (per asset class where ClassShocks names one, else
// PriceShock), widens every haircut by HaircutAddOn, redeems RedemptionRate of
// the tokenised fund share out of the pledge, and assumes the counterparties
// in DefaultedCounterparties have failed. A default is modelled as close-out
// netting being challenged in the insolvency: the counterparty's exposure
// reverts to Gross. That is the conservative reading, and it is stated here
// rather than buried in the code because it is the assumption a regulator
// will ask about first.
package risk
