# ADR 0005 — Bitemporal positions

## Status

Accepted. In force. `internal/lake/silver.go`.

## Context

A counterparty disputes a margin call issued at 16:00 against an uncovered
exposure. They produce a settlement confirmation for a transfer at 14:30 — ninety
minutes before the cut — which would have reduced it.

Both parties are right. The transfer happened at 14:30. The platform did not
know at 16:00; the indexer surfaced it at 16:35. The question that decides the
dispute is not "what was the position at 16:00" and not "what is it now". It is
**"what did we know at 16:00 about what was true at 16:00"**, and a store with
one timestamp per row cannot answer it.

This is not hypothetical: `domain.NewSeed` plants exactly that event —
CP-ALPHA to CP-ECHO, 400,000 TMMF, occurring at `SeedTime − 90m` and observed at
`SeedTime + 35m` — because a bitemporal store with nothing late in its input
proves nothing.

The alternatives each fail it specifically. Overwrite-in-place loses the earlier
belief, so the desk's decision is unreproducible. Append-only with one timestamp
conflates "the world changed" with "we found out", so a correction is
indistinguishable from a genuine later movement. Event sourcing with replay can
reconstruct a past belief only by folding the whole log with an observation
filter on every query — a full scan per question, with the reconstruction logic
in every caller.

## Decision

Every silver row is a version of one economic slot — `(counterparty, symbol,
ledger)` — over **two half-open intervals**: valid time `[valid_from, valid_to)`
for when the holding was true, driven by the event's `OccurredAt`; and
transaction time `[tx_from, tx_to)` for when the platform believed it, driven by
`ObservedAt`.

A query supplies both: `Silver.AsOf(validAt, knownAt)`. `domain.Snapshot`
carries both as first-class fields and `Lake.Snapshot(ctx, validAt, knownAt)` is
the only way to build one.

**Applying an event never edits history.** It closes affected rows on the
transaction axis at `ObservedAt` and writes replacements starting there. An
event landing inside an already-published interval splits it: the part before
keeps the old quantity, the part from the event onwards takes the new one. The
superseded rows stay exactly where they were, closed on the transaction axis.
**Nothing is ever deleted.**

Rows superseding a published belief are marked `corrected`. The trigger is
`DefaultLateArrival` — five minutes, well clear of an indexer's ordinary few
seconds and well inside a risk cut — or arrival out of order in valid time.
`corrected` carries through to gold, so a snapshot can report how much of itself
is a restatement.

Partitioning follows the query shape: silver by `(ledger, days(valid_from))`,
gold by `(ledger, hours(known_at))` — hours because a desk asks for several cuts
a day and each is a separate answer.

## Consequences

**The dispute question is one call.** Three readings, all available, all cheap:

| Query | What it is |
|---|---|
| `AsOf(16:00, 16:00)` | what the desk saw; what the call was issued against |
| `AsOf(16:00, 17:00)` | the restatement; what was actually true at 16:00 |
| `AsOf(17:00, 17:00)` | the current position |

The call is defensible on the first reading and wrong on the second, and the
platform produces both from one code path and says which is which.
`docs/data-model.md` works it through against the seed.

**A restatement and a live recompute are the same code path.** The recomputer
threads both clocks through its `SnapshotProvider` signature for that reason; a
nightly restatement is not a special mode.

**Row growth.** Corrections write versions rather than updating. Free at the
seeded size; at volume it is the thing that grows, and the transaction axis is
the compaction axis because rows closed on it are only read by an explicitly
historical query.

**A separate implementation cost.** `Silver.Apply` rewrites its whole row set on
each commit, in one process's memory. That is not a limit of the model:
`spark/…/GoldPositions.scala` expresses the same model as window functions and
two `MERGE INTO` statements, so only rows that moved are versioned. That job is
not built or run by this repository.

**Every caller supplies two timestamps.** No default and no `Now()` shortcut: a
caller that does not know which question it is asking should not get an answer.
The tools default `known_at` to `as_of`, giving "what the desk saw", and
`query_asof` documents that in its parameter description rather than hiding it.

**Reconciliation depends on it.** Without the transaction axis, every late
arrival would open a break that closes itself an hour later, and an operations
team would learn to ignore the break register.

## Alternatives considered

**One timestamp, overwrite in place.** The earlier belief is gone, so no
decision made against it can be reproduced. This is the design that loses the
dispute above by being unable to describe what happened.

**Append-only, valid time only.** The 14:30 transfer observed at 16:35 would
either be recorded at 16:35 — wrong, it changes the 15:00 position too — or at
14:30 with no record that the 16:00 belief differed.

**Event sourcing, replay with an observation filter.** Every point-in-time query
becomes a full replay, and the reconstruction logic duplicates into the risk
engine, the reconciler and the tools. Bronze *is* the event log and does support
replay; silver exists so the common query is a partition scan.

**Snapshot tables, one per cut.** Answers only for the cuts somebody
materialised, and the dispute always asks about the one nobody did.

**System-versioned temporal tables.** Right if the store were Postgres — but
`SYSTEM VERSIONING` gives only the transaction axis; valid time still needs
modelling. This is what a temporal database gives plus the axis it would not.
