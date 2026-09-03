# internal/risk

The calculation engine. It reads a `domain.Snapshot` and produces collateral
values, exposures, concentrations, limit breaches and stress results — the
types already declared in `internal/domain/risk.go`.

`doc.go` carries the package documentation; this file is the operator's copy of
the same formula, plus the worked example the tests assert on.

## What the engine is

A pure function of a snapshot. No clock of its own (it takes an `obs.Clock`), no
I/O, no state between calls. Given identical input it produces byte-identical
output: every map is sorted before it is iterated, every worker pool writes into
an indexed slot rather than appending, and the fan-out width has no effect on
the answer. `TestDeterminism` and `TestDeterminismUnderVaryingWorkerCounts` are
the enforcement.

Money is `internal/money`. An amount is an integer count of 1e-6 units of a
named currency; a haircut, an FX rate, a stress shock and a concentration share
are all `money.Ratio`, exact rationals that are never rounded. The single
rounding — half-even — happens inside `money` when a ratio is applied to an
amount. `float64` appears in exactly three places, all of them display: `pct`,
`relative` and `utilisation` in `format.go`, plus the metric values.

## The exposure formula

For counterparty `c` against snapshot `s`, base currency `B`, FX table `F`.

### Collateral

Per position `p` — pledged, and valid at `s.ValidAt` as known at `s.KnownAt` —
holding instrument `i`:

```
market(p)     = price(i) × quantity(p)                [in i's own currency]
haircut(i)    = s.Haircuts.For(i)                     [exact ratio]
advance(i)    = 1 − haircut(i)
collateral(p) = market(p) × advance(i)   if i is eligible
              = 0                        if i is not

MarketValue     = F(Σ market(p), B)
CollateralValue = F(Σ collateral(p), B)
IneligibleValue = F(Σ market(p) over ineligible p, B)
```

An instrument with **no rule** in the schedule is ineligible, not unhaircut. An
ineligible line keeps its market value and lends nothing, so "we hold it but we
do not lend against it" is visible rather than absent.

Line-level `Market` and `Collateral` stay in the instrument's own currency; only
the totals are FX-converted. That way a line can be checked against a trade
ticket without unwinding a rate. A **missing FX rate is an error**, never an
implicit 1:1 — `TestMissingFXRateIsAnError`.

### Exposure

Over obligations `o` of `c`, each converted into `B`:

```
Gross = Σ max(o, 0)
```

Gross ignores every offset. A negative obligation is owed *by* the institution
*to* the counterparty, so it is not exposure and does not enter Gross.

Netting sets are the groups `o.NettingSet`. For each set `k`:

```
net(k) = Σ_{o ∈ k} o

Net = Σ_k max(net(k), 0)     if c.Netting is true
    = Gross                  otherwise
```

Netting applies **only** where the counterparty has an enforceable agreement.
Where it does not, each set is still reported with its arithmetic net and
`Enforced: false` — the desk should be able to see what an agreement would be
worth — but the exposure that counts is Gross. A set that nets negative
contributes zero; sets do not offset each other.

```
Uncovered = max(Net − CollateralValue, 0)
Excess    = max(CollateralValue − Net, 0)
Covered   = (Uncovered == 0)
```

### Threshold and minimum transfer amount

These are properties of the collateral agreement, not of the market data, so
they come from the engine's `Terms` (default `500,000.00` and `250,000.00`) and
not from the snapshot:

```
Shortfall = 0                       if Uncovered ≤ Threshold
          = 0                       if Uncovered − Threshold < MinimumTransfer
          = Uncovered − Threshold   otherwise
```

Threshold is the unsecured amount the agreement permits before a call is due.
MinimumTransfer suppresses a call too small to be worth settling. A shortfall
exactly equal to the MTA **is** called: the agreement says "at least".

## Worked example — CP-ALPHA from the seed

Everything below is asserted in `TestCollateralValueHandComputed` and
`TestExposureTable`.

| Line | Quantity | Price | Market | Rule | Haircut | Advance | Collateral |
|---|---:|---:|---:|---|---:|---:|---:|
| TMMF | 40,000,000 | 1.0000 | 40,000,000.00 | MMF / AAA | 50bp | 0.9950 | 39,800,000.00 |
| UST-10Y | 120,000 | 95.2500 | 11,430,000.00 | GOVT / AAA | 200bp | 0.9800 | 11,201,400.00 |
| CORP-JPM | 90,000 | 97.8750 | 8,808,750.00 | CORP / A | 700bp | 0.9300 | 8,192,137.50 |
| | | | **60,238,750.00** | | | | **59,193,537.50** |

Obligations, both in netting set `NS-ALPHA-1`, and CP-ALPHA has an agreement:

```
repo             +48,000,000.00
derivative MtM    −6,500,000.00
Gross = 48,000,000.00       Net = 41,500,000.00
Uncovered = 0    Excess = 17,693,537.50    Shortfall = none
```

Turning the netting agreement off leaves Gross unchanged at 48,000,000.00 and
raises Net to the same figure — the 6,500,000.00 the derivative would have
offset is exactly what the agreement is worth.

The undercollateralised name is CP-DELTA: collateral 11,464,600.00 against a
net exposure of 45,500,000.00, so 34,035,400.00 uncovered, less the
500,000.00 threshold, is a **33,535,400.00 margin call**.

## Concentration and limits

Concentration is a share of total haircut-adjusted collateral across every
counterparty, cut four ways — `issuer`, `asset_class`, `sector`, `ledger`.
Shares are exact rationals over one denominator, so the shares within a
dimension sum to *exactly* one (`TestConcentrationSharesSumToOne` compares
against `big.NewRat(1, 1)`, not against a tolerance).

A `counterparty_exposure` limit is measured against **Uncovered**, not Gross or
Net. A fully collateralised repo book consumes no credit appetite, and a limit
that says otherwise makes the desk hold collateral it cannot count.
Concentration limits are measured against the share; the aggregate limit against
the sum of Uncovered.

Severity is utilisation — observed ÷ limit — compared exactly with `math/big`:

| Utilisation | Severity |
|---|---|
| ≥ `CriticalAt` (1.5) | `critical` |
| ≥ 1 | `breach` |
| ≥ `limit.WarnAt` | `warning` |
| below that | not reported |

A value sitting precisely on a boundary lands on the more severe side, every
time. `TestBreachSeverityBoundaries` drives CP-FOXTROT's uncovered exposure onto
each boundary and one micro-unit either side of it.

Breach ids are deterministic (`BRC-<limit>-<scope>`), which is what lets
`BreachRegister` deduplicate: a limit still broken on the next recompute updates
the breach already raised rather than raising a second one, and only the first
detection writes an `audit.ActionLimitBreached` record.

## Stress

A scenario shocks prices (per asset class where `ClassShocks` names one, else
`PriceShock`), widens every haircut by `HaircutAddOn` (clamped at 1), redeems
`RedemptionRate` of the tokenised fund share out of the pledge, and assumes the
counterparties in `DefaultedCounterparties` have failed.

**A default is modelled as close-out netting being challenged in the
insolvency**: the defaulted counterparty's exposure reverts to Gross. That is
the conservative reading and it is stated in the docs rather than buried in the
code, because it is the assumption a regulator asks about first.
`TestStressDefaultSuppressesNetting` pins it: defaulting CP-ALPHA raises its
uncovered exposure by exactly the 6,500,000.00 the derivative was offsetting,
and moves nobody else.

## Failover

`Elector` over a `LeaseStore` (in-memory and file implementations, one shared
rule set so they cannot diverge). Fencing tokens increase monotonically and
never rewind, including across a restart of the file store. `EventLog.Append`
rejects any write carrying a token below the highest it has accepted, so a
leader that pauses, loses its lease and wakes up still believing it leads is
fenced on the token alone — it does not need to know it was paused.
`TestFailoverFencesTheStaleLeader` runs exactly that sequence against both
stores, and checks that the standby resumes at the offset it had reached rather
than replaying the log.

## Benchmarks

Two vCPUs (`Intel Xeon @ 2.80GHz`), `-benchtime 300ms`:

```
BenchmarkExposureAll/counterparties=8         54.8 µs/op     25 kB/op    200 allocs/op
BenchmarkExposureAll/counterparties=64       313.4 µs/op    198 kB/op  1,495 allocs/op
BenchmarkExposureAll/counterparties=256     2235.2 µs/op    789 kB/op  5,936 allocs/op
BenchmarkExposureSingle                        7.2 µs/op    4.1 kB/op     49 allocs/op
BenchmarkStress/counterparties=8              90.7 µs/op     50 kB/op    479 allocs/op
BenchmarkStress/counterparties=64            627.4 µs/op    386 kB/op  3,600 allocs/op
BenchmarkStress/counterparties=256          4520.2 µs/op  1,538 kB/op 14,307 allocs/op
BenchmarkStressAll (4 scenarios, 8 cps)      240.6 µs/op    196 kB/op  1,766 allocs/op
BenchmarkCheckLimits                         147.6 µs/op     70 kB/op    542 allocs/op
BenchmarkShare (exact rational)               248 ns/op        0 B/op      0 allocs/op
```

The engine is quadratic in counterparties at the top end because `Snapshot`
exposes linear scans (`PositionsOf`, `Instrument`, `Counterparty`) rather than
indexes. That is a snapshot-shape problem, not an arithmetic one; the fix is an
index built once per snapshot, and it belongs in `domain` rather than here.

## What is deliberately simple

- **FX comes from the engine, not the snapshot.** `domain.Snapshot` carries no
  rate table, and this package may not change `domain`. `DefaultFX` holds
  EUR and GBP against a USD base; `WithFX` replaces it. A currency the table
  does not name is an error at valuation time.
- **Agreement terms come from the engine too**, for the same reason, with
  `WithCounterpartyTerms` for per-name overrides.
- **The recomputer's debounce uses `time.Timer`.** Scheduling a wake-up is not
  reading a clock, and every timestamp that reaches a result still comes from
  the `obs.Clock`. It does mean the debounce window cannot be advanced by a
  `ManualClock`; the tests use short real windows instead.
- **`BreachRegister` is in-memory.** The audit log is the durable record; the
  register is the working set and is rebuilt by recomputing.
- **The concentration pass revalues every pledge.** `CheckLimits` therefore
  values the book twice — once for exposures, once for concentration. Caching
  it would be a straightforward change and was not made, because a cache is a
  correctness liability and this is not the bottleneck.
