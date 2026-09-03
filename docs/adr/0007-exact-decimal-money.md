# ADR 0007 — Exact decimal money

## Status

Accepted. In force. `internal/money/money.go`.

## Context

The platform's output is money. A collateral value, an uncovered exposure, a
margin call amount — each is a number somebody is asked to accept and may
dispute months later. Two properties follow and neither is negotiable.

**The number must be exactly reproducible.** The same snapshot must produce the
same figure on any machine, in any evaluation order, at any worker-pool width. A
figure that depends on the order a sum accumulated in cannot be defended.

**The rounding must be auditable.** There has to be an answer to "where did the
last digit come from", and it has to be one place.

`float64` fails both. It is binary, so neither 0.1 nor a 12% haircut has an
exact representation. Addition is not associative, so a parallel sum is not the
same number as a serial one — which directly breaks the risk engine's
determinism claim. And error accumulates silently: a haircut applied to a market
value, converted through an FX rate, summed across a book and compared to a
limit has been rounded four times in four places, none recorded.

The failure this prevents is not exotic. `TestBreachSeverityBoundaries` drives
CP-FOXTROT's uncovered exposure onto each severity boundary and one micro-unit
either side. That test is not writable against floats: a value mathematically on
the boundary lands on either side depending on summation order.

## Decision

**An `Amount` is an int64 count of 1e-6 units of a named currency.**

```go
const Scale = 6

type Amount struct {
    Ccy   Currency
    Units int64
}
```

The currency travels with the number. `Add` returns an error on a mismatch
rather than a value; the only exception is the untyped zero, so `Add` can be
folded over a slice starting from `Amount{}`.

**Every rate is an exact rational.** Haircuts, FX rates, stress shocks and
concentration shares are `money.Ratio` over `math/big` and are never rounded.
`money.Bps(50)` is exactly 1/200. Concentration shares are rationals over a
single denominator, which is why `TestConcentrationSharesSumToOne` compares
against `big.NewRat(1, 1)` rather than a tolerance.

**Rounding happens once, half-even, inside `internal/money`.** Applying a ratio
to an amount is the only rounding operation; it rounds at 1e-6 and lives in one
auditable package. Banker's rounding because it does not accumulate a
directional bias across a book of thousands of positions.

**`float64` is display only.** In `internal/risk` it appears in exactly three
functions in `format.go` — `pct`, `relative`, `utilisation` — plus metric values.

## Consequences

**Determinism holds by construction.** Integer addition is associative and
commutative, so a book's sum does not depend on the order or the goroutine
count. `TestDeterminism` and `TestDeterminismUnderVaryingWorkerCounts` assert
it; both would fail against floats.

**Boundary comparisons are exact.** Utilisation is `observed ÷ limit` compared
as a rational with `math/big`, so a value exactly at the limit lands on the more
severe side every time, not sometimes.

**Range.** int64 at 1e-6 gives about ±9.22 × 10¹² currency units — ample for a
collateral book and finite, so `Add` and `Mul` return `ErrOverflow` rather than
wrapping. An error at the boundary beats a silently negative exposure. Six
places is enough for a rate applied to a notional and for a per-share price, and
not enough for a currency with more than six minor digits; that is outside scope
and the constant is one place.

**Currency mismatch is loud.** Amounts accumulate in a `money.Bag` and collapse
through an explicit `money.FXTable`. A missing rate is an error, never an
implicit 1:1 — `TestMissingFXRateIsAnError` — because a silent 1:1 is how a
collateral shortfall goes unnoticed. Line-level values stay in the instrument's
own currency and only totals convert, so a line can be checked against a trade
ticket without unwinding a rate.

**It is more code and more friction.** Every operation returns an error, and the
risk engine threads them through paths that would be one expression with floats.

**JSON** marshals currency and integer units rather than a decimal string, so a
round trip is lossless. `FuzzParse` covers the text parser, which is where
untrusted input reaches the type.

## Alternatives considered

**`float64`.** Not exact, not associative, not auditable. The default answer,
and wrong for money in a way that does not show up until a dispute.

**`math/big.Rat` for amounts too.** Exact and unbounded, rejected on cost and on
shape: every amount becomes a heap allocation with two big-integer fields, and —
more importantly — an amount that is never rounded has no defined presentation.
A collateral value must become a specific number of cents at some point, and
deferring that to the display layer moves the rounding somewhere it cannot be
audited. Rationals are right for a *rate*, and are used for exactly that.

**A decimal library (`shopspring/decimal`, `cockroachdb/apd`).** The mainstream
answer and a good one; rejected on ADR 0001. `apd` is a reasonable replacement
for `Amount` if that rule were lifted — though the currency-carrying and the
FX-or-error behaviour would still be written on top, and those are the parts
that catch real mistakes.

**int64 cents (scale 2).** A 50bp haircut applied to a price is not
representable at two places, so intermediate values would round four times
instead of once.

**Per-currency scale.** Complexity with no payoff; a currency with three minor
digits changes a constant, not a design.
