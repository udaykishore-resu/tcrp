# ADR 0006 — Iceberg metadata, with the file format behind an interface

## Status

Accepted. In force, and the least comfortable record here.
`internal/lake/table.go`, `internal/lake/values.go`.

## Context

The lakehouse needs table semantics: atomic commits, time travel, schema
evolution that does not break a reader, partition pruning, file skipping,
maintenance. Iceberg is the specification, and implementing it splits into two
halves of very different size.

The **metadata** half is a tree of JSON documents — table metadata pointing at a
current snapshot; snapshots with parents, a manifest list and a summary;
manifests whose entries carry a path, a record count and per-column bounds. The
semantics are the interesting part and none of it is much code.

The **data file** half is Parquet: a page index, dictionary and run-length
encoding, definition and repetition levels, per-page statistics, and a
specification long enough that a correct writer is a project. ADR 0001 rules out
`parquet-go`.

## Decision

Implement the metadata properly. Put the data file format behind an interface
and ship the wrong implementation, clearly labelled.

**Implemented and working:** schemas with **stable field ids**, so a column can
be renamed without breaking a reader; partition specs with `identity`, `day` and
`hour` transforms; a **snapshot** per commit with a parent, a manifest list and
manifests carrying row counts and per-column bounds; scans that use those bounds
to **skip files and report how many they skipped**; **time travel** by snapshot
id or wall-clock time; **snapshot expiry** removing only unreferenced files; and
**compaction** of small files into large ones.

**Not implemented:**

```go
// Format is the data file format. Nil means gzipped NDJSON.
Format FileFormat
```

`NDJSONGzip` is the default and the only implementation.

## Consequences

**What NDJSON costs, concretely.**

- **No column pruning.** A scan needing `quantity` decodes every one of the
  14 silver fields — roughly an order of magnitude more decoding than the query
  needs.
- **No page statistics.** File-level bounds exist and are used; within a file
  there is no finer granularity, so a matching file is read whole.
- **No dictionary or run-length encoding.** `ledger` takes three distinct values
  across the whole table and is stored as a repeated string on every row. Gzip
  recovers some of this and is not the same thing.
- **No predicate pushdown into the reader.** Filtering happens after decode.
- **Row-oriented I/O.** Every read is a full-row read.

The metadata layer is unaffected by all of it. The manifests, the bounds, the
snapshot lineage, the bitemporal history, the partition specs, the expiry and
compaction logic are the same regardless of what is inside the files — which is
the point of the interface rather than an inlined JSON encoder.

**The honest framing.** This is an Iceberg-shaped metadata implementation with a
toy storage layer, not an Iceberg implementation. Calling it "an Iceberg
lakehouse" without that sentence would be an overclaim, and
`internal/lake/lake.go` puts the sentence in the package comment where a reader
meets it first rather than in a document they might not open.

**The production swap.** Implement `FileFormat` over Parquet and change one
field in `lake.Options`; nothing above the interface moves. Pointing the catalog
at a REST Iceberg catalog rather than a directory is a larger change and is not
behind an interface today. `spark/README.md` documents the same tables read by
real Spark against a real catalog.

**A second limitation, easy to conflate with this one and not the same.**
`Silver.Apply` rewrites its whole row set on each commit and the gold join
happens in one process's memory. That is a *volume* limit, not a *format* limit
— Parquet would not fix it; the `MERGE INTO` in `GoldPositions.scala` would.
Both are stated in `internal/lake/lake.go`, as two separate sentences on
purpose.

**What is honestly good.** The parts of Iceberg most implementations get wrong —
bounds actually used for skipping and actually reported, expiry that does not
orphan files, compaction observably a no-op to a reader, field ids surviving a
rename — are implemented and tested (31 tests in the package). What is missing
is the part with a published specification and a well-known correct
implementation, which is the better half to be missing.

## Alternatives considered

**Take `parquet-go`.** The right answer for a production system. Rejected on
ADR 0001 and nothing else; if the rule were lifted this is the first change.

**Write a Parquet writer.** Seriously considered. A subset — flat schema, plain
encoding, no dictionary, no page index — is achievable and would produce files
other tools can read. It would also produce files with none of the properties
that make Parquet worth using, so the honest description would be
"Parquet-shaped files with NDJSON's performance characteristics", which is a
worse sentence than the current one.

**Plain partitioned directories, no Iceberg.** Loses atomic commits, time travel
and snapshot lineage, all load-bearing here: the bitemporal store must say what
a table looked like at a past instant, and `DATA-1` is a lineage claim.

**SQLite or DuckDB as the storage layer.** Replaces the interesting part with
something that already works while leaving the file format question exactly
where it was.

**Say nothing and let the metadata speak.** A reader who finds an overclaim
stops trusting everything else, and this is the most findable one available.
