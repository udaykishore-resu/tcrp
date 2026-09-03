# Spark batch path

Scala 2.13 / Spark 3.5 jobs that build the bitemporal silver timeline, the
curated gold positions and the reconciliation break register on Iceberg tables.

## This is not built by CI

There is no JVM toolchain in this repository's CI, nothing here is compiled by
`go build`, and there is no `make` target that pretends otherwise. These files
are source: they are the batch expression of the model, reviewable as code, and
they have not been run against a cluster from inside this repo. Treat any claim
beyond "this is the code" as unverified.

The runnable path is the Go implementation in `internal/lake`. It is complete,
tested and deterministic, and the demo runs it end to end. If you want to see
the model work, run that.

## Why both exist

The Go lakehouse holds the whole dataset in one process's memory: silver
rewrites its full row set on each commit and the gold join happens without a
shuffle. At the seeded dataset's size that is free, it keeps the code readable,
and it makes the demo a single binary with no cluster to stand up.

It stops being free somewhere around the point where the position store no
longer fits in memory or a single commit's rewrite takes longer than the
interval between commits. That is where these jobs earn their keep: the same
model, the same Iceberg tables, the same partitioning, expressed as window
functions and `MERGE INTO` over a cluster. The two are meant to produce the same
rows; nothing in this repository proves that they do.

## The jobs

### `GoldPositions`

Reads `bronze_ledger_events`, splits each event into the holdings it moves, and
builds the valid-time timeline for every `(counterparty, symbol, ledger)` slot
with a running sum over occurrence order and a `lead` for each interval's close.
Two `MERGE INTO` statements then apply the transaction axis: the first closes
the open rows whose content changed, the second inserts their replacements. Only
rows that actually moved are versioned, so a nightly rebuild does not write a new
version of every holding in the book.

The bitemporal read (`asOf`) is the same one the Go store implements: half-open
valid and transaction intervals, both supplied by the caller. `joinReference`
broadcasts the counterparty, instrument and price tables against the only side
that grows. `writeGold` overwrites partitions dynamically, so re-running one cut
is idempotent and does not touch the cuts either side of it.

```
spark-submit --class com.tcrp.lake.GoldPositions \
  --conf spark.sql.catalog.lake=org.apache.iceberg.spark.SparkCatalog \
  --conf spark.sql.catalog.lake.type=rest \
  --conf spark.sql.catalog.lake.uri=https://catalog.internal/iceberg \
  target/scala-2.13/tcrp-lake-spark_2.13-0.2.0.jar \
  --catalog lake --namespace tcrp \
  --valid-at 2026-09-03T16:00:00Z --known-at 2026-09-03T17:00:00Z
```

`--commit-at` defaults to `--known-at` rather than to wall-clock now, so
re-running a batch for a past cut reproduces that cut instead of inventing a new
one.

### `ReconJob`

Two passes, in the order a settlement operations team works. A full outer join
on the event's natural key yields all three instruction-level findings in one
shuffle — a null book side is `missing_in_book`, a null chain side is
`missing_on_chain`, a present pair with different sizes is `quantity_mismatch` —
and the observation lag gives `stale_observation` with no join at all. The
position sweep then runs only over the slots no instruction explains, because a
position difference with a cause attached is an operations task and the same
difference with no cause is an incident.

Breaks are merged into the register by a deterministic id hashed from what the
break is about, so a run every five minutes re-detects the same break rather than
opening a new one, and an acknowledgement made between two runs survives.

## Tables

Both jobs create what they need with `CREATE TABLE IF NOT EXISTS`. Partitioning
matches the Go implementation: silver by `(ledger, days(valid_from))`, gold by
`(ledger, hours(known_at))`, breaks by `(kind, days(detected_at))`.

The reference tables — `ref_counterparties`, `ref_instruments`, `ref_prices` —
and `book_instructions` are inputs from the book of record and are not created
here. In the Go implementation they come from `domain.NewSeed`; in a deployment
they are extracts from the systems that own them.

## Maintenance

`GoldPositions.maintain` calls `rewrite_data_files`, `rewrite_manifests` and
`expire_snapshots`. A per-batch commit leaves small files behind, and scan
planning is what degrades first. Expiring a snapshot ends time travel to it, so
the retention window is a policy decision; it is a named parameter here rather
than a number in a cron argument.

## Building it yourself

`sbt compile` with a JDK 17 toolchain. Spark and the Iceberg runtime are
`Provided` — shipping them in the assembly is how you get a `NoSuchMethodError`
at three in the morning.
