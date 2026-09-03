# Phase 3 — Lakehouse and risk

`internal/stream`, `internal/lake`, `internal/risk`, plus `spark/` (Scala).

8,218 lines of Go source, 4,364 of test. 812 lines of Scala.

## What this phase delivers

The path from a ledger event to a number a desk is accountable for, and the
controls that make that number defensible three months later.

### A partitioned log

`internal/stream` is an append-only log with topics, partitions, consumer groups
and committed offsets, backed by files or by memory. Duplicates are suppressed;
consumer lag is a gauge. It is not a broker — no replication, no leader election
across brokers, no key compaction, no cross-process transport — and ADR 0001 says
what the absence of a real one costs.

### The lakehouse

Three layers, with the Iceberg metadata model implemented rather than mimed.

**Bronze** lands the ledger event as observed, with nothing derived and nothing
dropped, because the point of a bronze layer is that a bug in a downstream
transform is repairable by replaying it rather than by re-reading a chain.
Deduplication is on the natural key `ledger:tx_hash:log_index`, so a reorg
re-including a transaction lands once.

**Silver** is the bitemporal position store. A row is a version of one economic
slot — counterparty, symbol, ledger — over a half-open valid interval and a
half-open transaction interval. Applying an event never edits history: it closes
affected rows on the transaction axis at the event's `ObservedAt` and writes
replacements starting there. An event landing inside an already-published
interval splits it. Rows superseding a published belief are marked `corrected`.
**Nothing is ever deleted.** ADR 0005; the worked example is in
`docs/data-model.md`.

**Gold** flattens to one `(validAt, knownAt)` pair and joins to reference data —
counterparties, instruments, prices, haircuts, limits — which arrives from the
book of record rather than from a ledger. Materialised rather than computed on
every read, so a figure on a dashboard has a row behind it that can be pointed
at.

**The Iceberg metadata** underneath all three: schemas with stable field ids (so
a column can be renamed without breaking a reader), partition specs with identity
/ day / hour transforms, a snapshot per commit with a parent and a manifest list,
manifests whose entries carry row counts and per-column lower and upper bounds,
scans that use those bounds to skip files **and report how many they skipped**,
time travel by snapshot id or wall-clock time, snapshot expiry that removes only
unreferenced files, and compaction of small files into large ones.

**Six data-quality controls**, named as constants because a dashboard, a test and
an incident review all have to name the same thing: completeness of the ledger
sequence, uniqueness of the event id, timeliness of observation (15 minutes),
accuracy of the supply identity, referential integrity of the holder against the
registry, and freshness of prices (1 hour). A snapshot that failed a critical
control is still returned, but marked, so a caller can refuse to act on it rather
than never seeing it.

**Reconciliation** compares the ledger view against the book of record at a
`(validAt, knownAt)` pair, in the order a settlement operations team works:
instruction level first — `missing_in_book`, `missing_on_chain`,
`quantity_mismatch`, `stale_observation` — then a position sweep over only the
slots no instruction explains, because a position difference with a cause
attached is an operations task and the same difference with no cause is an
incident. Break ids are deterministic hashes of what the break is about, so a run
every five minutes re-detects the same break rather than opening a new one, and
an acknowledgement made between runs survives.

### The risk engine

A pure function of a `domain.Snapshot`. No clock of its own, no I/O, no state
between calls, so the same snapshot always produces byte-identical results.

It produces collateral values, gross and net exposures, netting-set detail,
uncovered and excess, callable shortfall after the agreement's threshold and
minimum transfer amount, concentration cut four ways (issuer, asset class,
sector, ledger), limit breaches with exact severity, and four stress scenarios.

Two design points worth naming:

**A default is modelled as close-out netting being challenged in the
insolvency** — the counterparty's exposure reverts to gross. That is the
conservative reading and it is stated in the package documentation rather than
buried in the code, because it is the assumption a regulator asks about first.

**Severity boundaries are exact.** Utilisation is `observed ÷ limit` compared as
a rational with `math/big`, so a value exactly on a boundary lands on the more
severe side deterministically rather than wobbling with float error.

It also carries the HA path: `Elector` over a `LeaseStore` with monotonic fencing
tokens, and an `EventLog` that rejects any write carrying a token below the
highest it has accepted. A stale leader that wakes from a pause is fenced on the
token alone — it does not need to know it was paused.

### The batch expression

`spark/` holds the same model as Scala 2.13 / Spark 3.5 jobs over Iceberg
tables: `GoldPositions` builds the bitemporal timeline with window functions and
applies the transaction axis with two `MERGE INTO` statements, so only rows that
actually moved are versioned; `ReconJob` does the two passes in one shuffle.

It is not built by anything in this repository. `spark/README.md` is explicit:
"Treat any claim beyond 'this is the code' as unverified."

## Demonstrating it

```sh
go test ./internal/stream/... ./internal/lake/... ./internal/risk/... -count=1 -v
```

The bitemporal property, which is the heart of the phase:

```sh
go test ./internal/lake/ -run 'AsOfDiffers|CorrectionSupersedes|OutOfOrder' -count=1 -v
```

- `TestAsOfDiffersEitherSideOfTheLateArrival` — the two views differ by exactly
  the late transfer's quantity on exactly the two slots it touched, and on
  nothing else.
- `TestCorrectionSupersedesWithoutErasing` — the earlier belief is still there,
  closed on the transaction axis.
- `TestOutOfOrderArrivalRewritesTheValidTimeline`.

The worked exposure example, hand-computed and asserted:

```sh
go test ./internal/risk/ -run 'TestCollateralValueHandComputed|TestExposureTable' -count=1 -v
```

`internal/risk/README.md` carries the table those tests assert on: CP-ALPHA's
three lines summing to 60,238,750.00 market and 59,193,537.50 collateral, and
CP-DELTA as the undercollateralised name.

Determinism:

```sh
go test ./internal/risk/ -run TestDeterminism -count=1 -v
```

Failover:

```sh
go test ./internal/risk/ -run TestFailoverFencesTheStaleLeader -count=1 -v
```

The benchmarks:

```sh
go test ./internal/risk/... -run XXX -bench . -benchtime 200ms -count=3
```

Measured on two vCPUs (Intel Xeon @ 2.80GHz), median of three:
`ExposureSingle` 6.52 µs; `ExposureAll` 44.8 µs at 8 counterparties, 306.8 µs at
64, 2,070 µs at 256; `Stress` 84.8 / 545.9 / 3,774 µs at the same widths;
`CheckLimits` 133.0 µs; one exact concentration share 76.3 ns with zero
allocations. `docs/slo.md` carries the full table and the scaling discussion.

Reconciliation against the seeded anomalies:

```sh
go test ./internal/lake/ -run TestRecon -count=1 -v
```

## The tests that prove it

| Package | Tests | What they establish |
|---|---:|---|
| `internal/stream` | 9 | Ordering within a partition; the partitioner is deterministic; an idempotent producer suppresses a retry; group rebalance is range-based and deterministic; a rebalance hands over at the committed offset; a restart resumes from the committed offset and restores the idempotence table; concurrent producers |
| `internal/lake` | 31 | Table snapshots, parents, manifests and bounds; file skipping is reported; time travel by id and by time; expiry removes only unreferenced files; compaction is a no-op to a reader; the three bitemporal properties; `Apply` is idempotent; the store reopens from its table; all six DQ controls; the three planted anomalies are found and classified correctly |
| `internal/risk` | 61 + 7 benchmarks | The hand-computed worked example; the exposure table across all 8 counterparties; determinism, including under varying worker counts; concentration shares summing to exactly 1 (`big.NewRat(1,1)`, not a tolerance); a missing FX rate is an error; severity boundaries and one micro-unit either side; stress default suppresses netting by exactly the derivative offset and moves nobody else; fencing a stale leader across both lease stores |

Conformance: `DATA-1` — every gold row traces to a bronze event.

## What this phase deliberately does not do

- **The data file format is gzipped NDJSON, not Parquet.** No column pruning, no
  page statistics, no dictionary or run-length encoding; a scan decodes every
  column of every row it opens. `FileFormat` is the interface Parquet goes
  through and everything above it is unaffected. **ADR 0006 is the honest
  version of this and it should be read before the lakehouse is described to
  anyone.**
- **Silver rewrites its whole row set on each commit**, and the gold join happens
  in one process's memory. That is a volume limit, not a format limit — Parquet
  would not fix it. The `MERGE INTO` in `spark/` is the fix.
- **The catalog is a directory**, not a REST Iceberg catalog. That is the one
  part of the metadata model not behind an interface.
- **The Spark jobs have never been run against a cluster from inside this
  repository.** They are reviewable code and nothing more.
- **FX rates and agreement terms come from the risk engine, not the snapshot.**
  `domain.Snapshot` carries no rate table and this package may not change
  `domain`. `DefaultFX` holds EUR and GBP against a USD base; `WithFX` and
  `WithCounterpartyTerms` override.
- **`BreachRegister` is in-memory.** The audit log is the durable record; the
  register is the working set and is rebuilt by recomputing.
- **The recomputer's debounce uses `time.Timer`.** Scheduling a wake-up is not
  reading a clock, and every timestamp that reaches a result still comes from the
  `obs.Clock` — but it does mean the debounce window cannot be advanced by a
  `ManualClock`, and the tests use short real windows instead. That distinction
  is documented at the call site.
- **The engine is quadratic in counterparties at the top end**, because
  `domain.Snapshot` exposes linear scans rather than indexes. The fix is an index
  built once per snapshot and it belongs in `domain`.

## What the next phase needs from this one

Phase 4 needs `Lake.Snapshot(ctx, validAt, knownAt)` and the risk engine's
figures, because every tool the agent is given is a thin wrapper that validates
arguments, calls one of these engines, and renders the answer. No tool computes a
figure of its own, and every figure a tool returns is listed in its result's
`Grounded` set — which is what lets the output guardrail refuse any number the
model did not get from here.
