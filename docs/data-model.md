# Data model

The vocabulary every package shares, the bitemporal model with a worked example,
and the contract a gold snapshot makes to the risk engine.

`internal/domain` holds the vocabulary. It has no behaviour beyond validation and
rendering, and it depends on nothing but `internal/money`. That is deliberate:
the lakehouse, the risk engine, the ledgers and the agent's tools all speak these
types, and if the vocabulary depended on any one of them the others would inherit
it.

## The vocabulary

### Money

An `Amount` is an int64 count of **1e-6 units of a named currency**. The currency
travels with the number. Amounts in different currencies do not add; the only
exception is the untyped zero, which lets `Add` be folded over a slice starting
from `Amount{}`.

A `Ratio` is an exact rational over `math/big` and is never rounded. Haircuts, FX
rates, stress shocks and concentration shares are all ratios. `money.Bps(50)` is
exactly 1/200; `money.Percent(35)` is exactly 7/20. The single rounding —
half-even, at 1e-6 — happens inside `internal/money` when a ratio is applied to
an amount. ADR 0007.

A `Bag` accumulates amounts in multiple currencies and is collapsed through an
explicit `FXTable`. A missing rate is an error, never an implicit 1:1.

### Ledger

Three values. The platform is dual-ledger by construction, and the book of record
is a third opinion rather than a tiebreaker.

| Value | What it is |
|---|---|
| `evm` | The permissioned-token network. ERC-3643-style, with an identity registry and a compliance module. |
| `canton` | The Daml contract-store network. Signatories, observers, contract keys, propose-accept, selective disclosure. |
| `golden` | The institution's off-chain book of record. |

No single ledger is authoritative. Reconciliation compares the chain view against
the book precisely because "the chain says X" and "our books say X" are different
claims.

### Counterparty

```
ID            CP-ALPHA
Name          Alpha Capital Markets
LEI           5493001KJTIIGC8Y1R12
Jurisdiction  US
Rating        AA
Sector        broker_dealer
Netting       true        — an enforceable netting agreement exists
Whitelisted   true        — present in the identity registry
Wallets       [0x…]       — its EVM addresses
Parties       [Alpha::1220] — its Daml parties
```

`Netting` and `Whitelisted` are the two fields with teeth. `Netting` decides
whether exposure is measured net or gross. `Whitelisted` is checked by the policy
rule `deny-transfer-to-non-whitelisted` **before** the ledger's own compliance
hook would refuse — the gateway refuses first so nothing reaches a chain that
would revert.

The seed's CP-HOTEL is deliberately un-whitelisted. Every transfer to it must be
refused, and `TestSmokeRun` shows it being refused at the gateway.

### Instrument, Price, Haircut

An `Instrument` has a symbol, issuer, sector, `AssetClass`, currency, rating and
a `Tokenised` flag. Asset classes: `money_market_fund`, `government_bond`,
`corporate_bond`, `equity`, `cash`, `stablecoin`.

A `Price` is a mark with a source and an `AsOf`. Prices are reference data from
the book of record; they do not belong on a ledger.

A `HaircutSchedule` is versioned and its rules match on class and, optionally,
rating. Most specific wins. **An instrument with no matching rule is ineligible,
not unhaircut** — an ineligible line keeps its market value and lends nothing, so
"we hold it but we do not lend against it" is visible rather than absent.

The seeded schedule, `haircuts-2026.09`:

| Class | Rating | Haircut | Advance |
|---|---|---:|---:|
| cash | any | 0bp | 100.00% |
| money_market_fund | AAA | 50bp | 99.50% |
| money_market_fund | any | 150bp | 98.50% |
| government_bond | AAA | 200bp | 98.00% |
| government_bond | any | 400bp | 96.00% |
| corporate_bond | AA | 500bp | 95.00% |
| corporate_bond | A | 700bp | 93.00% |
| corporate_bond | BBB | 1200bp | 88.00% |
| corporate_bond | any | 2500bp | 75.00% |
| stablecoin | any | 300bp | 97.00% |
| equity | any | 1500bp | 85.00% |

### Position

A holding of one instrument by one counterparty on one ledger, with a quantity, a
`Pledged` flag, a source, and **four timestamps**: `ValidFrom`, `ValidTo`,
`TxFrom`, `TxTo`. The four timestamps are the bitemporal model and are the next
section.

Only pledged positions count as collateral.

### Obligation

What a counterparty owes: an amount, a kind (`repo`, `derivative`, `loan`), a
`NettingSet`, an `AsOf` and a `DueAt`.

**Sign matters.** A negative obligation is owed *by* the institution *to* the
counterparty. It does not enter gross exposure — gross is `Σ max(o, 0)` — and it
does reduce the net within its netting set.

**Netting sets do not offset each other.** For each set `k`, `net(k) = Σ o`, and
`Net = Σ_k max(net(k), 0)`. A set that nets negative contributes zero, not a
credit against another set.

### Limit

Five kinds: `counterparty_exposure`, `issuer_concentration`,
`class_concentration`, `ledger_concentration`, `uncovered_aggregate`. Each has an
amount or a ratio, a warning level, an owner and an effective date.

A `counterparty_exposure` limit is measured against **Uncovered**, not gross or
net. A fully collateralised repo book consumes no credit appetite, and a limit
that says otherwise makes the desk hold collateral it cannot count.

### LedgerEvent

The unit of ingestion, and the seam a production deployment replaces the
simulators at.

```
Seq         monotonic within the stream
Ledger      evm | canton | golden
Kind        mint | burn | transfer | lock | unlock | …
Symbol      TMMF
From, To    counterparty ids
Quantity    int64
Block       height
TxHash      hash
LogIndex    index within the transaction
OccurredAt  block time            ← drives valid time
ObservedAt  when the platform saw it ← drives transaction time
Finalised   bool
Memo        free text
```

The natural key is `ledger:tx_hash:log_index`, and bronze deduplicates on it — so
a reorg re-including a transaction lands once.

**`OccurredAt` and `ObservedAt` are the whole design.** One drives valid time and
the other drives transaction time, and an event carrying both is what makes the
next section possible.

## The bitemporal model

### Two axes

- **Valid time**, `[valid_from, valid_to)` — when the holding was true in the
  world.
- **Transaction time**, `[tx_from, tx_to)` — when the platform believed it.

Both half-open. Both supplied by the caller:
`Silver.AsOf(validAt, knownAt)`. There is no default and no `Now()` shortcut,
because a caller that does not know which question it is asking should not get an
answer.

### The rules

1. **Applying an event never edits history.** It closes the affected rows on the
   transaction axis at the event's `ObservedAt` and writes replacements starting
   there.
2. **An event landing inside an already-published interval splits it.** The part
   before the event keeps the old quantity; the part from the event onwards takes
   the new one.
3. **Nothing is ever deleted.** Superseded rows stay exactly where they are,
   closed on the transaction axis. A query with the earlier `knownAt` still
   returns the earlier belief.
4. **Rows that supersede a published belief are marked `corrected`.** The trigger
   is `DefaultLateArrival` — five minutes of observation lag, well clear of an
   indexer's ordinary few seconds and well inside a risk cut — or arrival out of
   order in valid time. `corrected` is carried through to the gold row.

### Worked example — the seed's late-arriving event

`domain.NewSeed` plants exactly one late arrival, because a bitemporal store with
nothing late in its input proves nothing:

```go
late := LedgerEvent{
    Ledger:     LedgerEVM,
    Kind:       EventTransfer,
    Symbol:     "TMMF",
    From:       "CP-ALPHA",
    To:         "CP-ECHO",
    Quantity:   400_000,
    OccurredAt: SeedTime.Add(-90 * time.Minute),  // 2026-09-03T14:30:00Z
    ObservedAt: SeedTime.Add(+35 * time.Minute),  // 2026-09-03T16:35:00Z
    Memo:       "late-arriving; observed after the risk cut",
}
```

`SeedTime` is `2026-09-03T16:00:00Z` — the risk cut. So: the transfer **happened
ninety minutes before the cut** and **was seen thirty-five minutes after it**.

Now the three readings. Write `Q` for CP-ALPHA's TMMF quantity on `evm` as the
event stream leaves it just before the late transfer applies.

| Query | CP-ALPHA | CP-ECHO | What it is |
|---|---|---|---|
| `AsOf(16:00, 16:00)` | `Q` | `E` | **What the desk actually saw.** The event exists in the world but the platform did not know it. |
| `AsOf(16:00, 17:00)` | `Q − 400,000` | `E + 400,000` | **The restatement.** What was really true at 16:00, as understood an hour later. |
| `AsOf(17:00, 17:00)` | `Q − 400,000` | `E + 400,000` | The current position. |

`TestAsOfDiffersEitherSideOfTheLateArrival` asserts precisely this, and it
asserts it as a *difference* rather than as two absolute numbers:

```
fromBefore − fromAfter == late.Quantity      (CP-ALPHA is 400,000 lighter)
toAfter    − toBefore  == late.Quantity      (CP-ECHO is 400,000 heavier)
```

and then walks every other slot to confirm the two views **differ only on the
two the event touched**. That second assertion is the one that matters: a
bitemporal store that gets the touched slots right and quietly restates
everything else is worse than one that does not work at all.

**Why this settles a dispute.** A counterparty disputes a margin call issued at
16:00 and produces a settlement confirmation for a 14:30 transfer. Both parties
are right. The transfer did happen at 14:30; the platform did not know at 16:00.
The margin call is **defensible on the first reading and wrong on the second**,
and the platform can produce both from one code path and say which is which. A
store with one timestamp per row can produce neither. ADR 0005.

**What the other rows look like.** `TestCorrectionSupersedesWithoutErasing`
inspects `Silver.History(key)` for the CP-ALPHA slot and finds the earlier belief
still there, closed on the transaction axis at `16:35`, alongside the replacement
rows opening at `16:35` and marked `corrected: true`.

**What reconciliation makes of it.** At `knownAt = 16:00` the event is invisible,
so nothing is out. At a `knownAt` past `16:35`, the observation lag of two hours
and five minutes exceeds the 15-minute timeliness threshold and a
`stale_observation` break opens against **CP-ECHO — the receiving side** —
which `internal/lake/recon_test.go` asserts. Note the classification: it is a
*timeliness* finding, not a `missing_in_book` or a `quantity_mismatch`. A late
arrival the bitemporal store handles correctly is a data-quality warning, not a
break, and `TestSeededSnapshotPassesEveryControlBeforeTheLateArrival` asserts
that the snapshot after the late arrival is `warn` and still `Usable()` —
timeliness never blocks a snapshot.

**The two anomalies that are real breaks**, for contrast, are the other two
planted in the seed:

- CP-FOXTROT → CP-DELTA, 1,750,000 TMMF on `evm`, with memo `settlement
  instruction SI-88421` — on chain, never received by the book.
  `missing_in_book`, **critical**.
- CP-GOLF → CP-BRAVO, 900,000 TMMF on `canton`, where the book shows 1,000,000.
  `quantity_mismatch`, **high**.

Those two are what `TestSmokeRun` reports when the agent is asked "are there any
reconciliation breaks?".

## The gold snapshot contract

`domain.Snapshot` is what the risk engine calculates from. It is the only input
the engine takes, and the engine is a pure function of it.

```go
type Snapshot struct {
    ValidAt time.Time
    KnownAt time.Time

    Counterparties []Counterparty
    Instruments    []Instrument
    Prices         map[string]Price
    Positions      []Position
    Obligations    []Obligation
    Haircuts       HaircutSchedule
    Limits         []Limit

    Lineage []string
    Quality QualityReport
}
```

### What the contract guarantees

**Both clocks are present and explicit.** `ValidAt` with `KnownAt` also in the
past is what the desk actually saw; `ValidAt` in the past with `KnownAt` now is a
restatement. The snapshot says which it is rather than leaving a reader to
infer it.

**Positions are already filtered.** Every position in the snapshot was valid at
`ValidAt` as known at `KnownAt`. The engine does no temporal filtering of its
own — deciding what was true and when it was known is the lakehouse's job.

**Reference data is joined in.** Counterparties, instruments, prices, haircuts
and limits arrive from the book of record, not from a ledger, which is why they
are a `Reference` parameter to the gold layer rather than a table the pipeline
populates.

**`Lineage` names the gold tables and snapshot ids the view was built from**, so
a figure on a dashboard can be walked back to the rows behind it. `DATA-1` —
every gold row traces to a bronze event — is the conformance invariant over it.

**`Quality` carries the data-quality verdict.** Six controls run, named as
constants because a dashboard, a test and an incident review all have to name the
same thing:

| Check | Threshold | What it finds |
|---|---|---|
| `dq.completeness.ledger_sequence` | — | A gap in the sequence |
| `dq.uniqueness.event_id` | — | A duplicate natural key |
| `dq.timeliness.observation_lag` | 15 min | Observations lagging occurrence |
| `dq.accuracy.supply_identity` | — | Minted less burned not matching circulating |
| `dq.referential.known_holder` | — | A holder not in the registry |
| `dq.freshness.prices` | 1 hour | A mark older than the valid time allows |

**A snapshot that failed a critical control is still returned, but marked.** A
caller can refuse to act on it; a caller that never sees it cannot. `Usable()`
is the accessor, and the API surfaces the report.

### What the engine does with it

The full formula is `internal/risk/doc.go` and `internal/risk/README.md`. In
outline, per counterparty:

```
market(p)       = price(i) × quantity(p)              [instrument's currency]
collateral(p)   = market(p) × (1 − haircut(i))        if eligible, else 0
MarketValue     = FX(Σ market(p) over pledged p, base)
CollateralValue = FX(Σ collateral(p), base)

Gross = Σ max(obligation, 0)
Net   = Σ_k max(Σ_{o∈k} o, 0)   if the counterparty has an enforceable agreement
      = Gross                    otherwise

Uncovered = max(Net − CollateralValue, 0)
Shortfall = 0                       if Uncovered ≤ Threshold
          = 0                       if Uncovered − Threshold < MinimumTransfer
          = Uncovered − Threshold   otherwise
```

`Threshold` (500,000.00) and `MinimumTransfer` (250,000.00) are properties of the
collateral **agreement**, not of the market data, so they come from the engine's
`Terms` rather than from the snapshot.

Line-level market and collateral values stay in the instrument's own currency;
only the totals are FX-converted, so a line can be checked against a trade ticket
without unwinding a rate.

### The contract the engine makes back

Given the same `Snapshot`, the engine produces **byte-identical output**. No
clock of its own, no I/O, no state between calls. Every map is sorted before it
is iterated, every worker pool writes into an indexed slot rather than appending,
and the fan-out width has no effect on the answer. `TestDeterminism` and
`TestDeterminismUnderVaryingWorkerCounts` are the enforcement.

That is what makes a figure on a dashboard defensible three months later when a
counterparty disputes it — which is the same reason the bitemporal model exists,
arriving from the other direction.
