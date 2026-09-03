# Phase 1 — Foundations

`internal/money`, `internal/domain`, `internal/obs`, `internal/identity`,
`internal/audit`.

3,004 lines of source, 2,035 of test (`wc -l` over the non-test and test Go
files in those five packages).

## What this phase delivers

Five packages that everything above depends on and that depend on almost nothing.
Each exists because a decision made here is expensive to reverse later, and each
was written first for that reason.

### Exact decimal money

An `Amount` is an int64 count of 1e-6 units of a named currency. A `Ratio` is an
exact rational over `math/big` and is never rounded. The single rounding —
half-even, at 1e-6 — happens in one place, inside the money package, where it can
be audited.

Amounts in different currencies do not add. A `Bag` accumulates across
currencies and collapses through an explicit `FXTable`; a missing rate is an
error, never an implicit 1:1, because a silent 1:1 is how a collateral shortfall
goes unnoticed.

Why it matters here rather than in the risk engine: float arithmetic is not
associative, so a parallel sum is not the same number as a serial sum. Every
determinism claim the platform makes downstream rests on integer addition being
associative. ADR 0007.

### The shared vocabulary

`internal/domain` is what a counterparty, an instrument, a price, a haircut
schedule, a position, an obligation, a limit, a stress scenario and a ledger
event are. It has no behaviour beyond validation and rendering and it depends on
nothing but `money` — deliberately, because the lakehouse, the risk engine, the
ledgers and the agent's tools all speak these types, and if the vocabulary
depended on any one of them the others would inherit it.

It also holds `NewSeed`: a complete deterministic dataset at a pinned instant.
8 counterparties, 10 instruments, 19 positions, 12 obligations, 5 limits, 11
haircut rules, 4 stress scenarios, and an event stream with **three deliberate
anomalies** — an on-chain transfer the book never received, a partial fill the
book recorded at a different size, and an event that occurred before the cut but
was observed after it. A reconciliation control with nothing to find proves
nothing, and a bitemporal store with nothing late in its input proves nothing.

### The observability kernel

One `Clock` interface. **Nothing on a decision path calls `time.Now`.** Every
engine takes a clock, which is what makes a trajectory replayable and a test
deterministic. `ManualClock` is what the demo and the tests use.

A tracer emitting spans in OTLP/JSON on the OpenTelemetry GenAI attribute
conventions, with W3C `traceparent` propagation, a memory exporter for the eval
harness and a writer exporter for a file. Under `Deterministic(true)` span ids
are sequential, so a run's span ids and trace ids repeat exactly. `tcrpd demo`
is not byte-identical for a different reason: it prints run ids, approval ids
and measured guardrail and policy latency percentiles, none of which are pinned
by the clock.

A metric registry with a Prometheus text exposition, counters, gauges and
histograms with labels.

### Identity that travels

The agent never becomes the system. Every tool call, policy decision, audit
record and span is attributed to the human or service principal that started the
run **plus** the agent workload identity executing it. Both travel together in
the context; a decision made with only one of them is a bug, and the conformance
invariant `AGENT-4` fails the build when a span is missing either.

A `Principal` carries subject, kind (human or agent), tenant, desk, `on_behalf`,
roles and scopes. `Signer` mints and verifies HMAC-SHA256 assertions carried in
`X-TCRP-Principal`, with expiry. Nine scopes and five roles, named as constants
because the policy bundle, the tool specs and the conformance engine all have to
agree on the strings.

### The immutable record

Every audit record is hash-chained to its predecessor over a canonical encoding
and ed25519-signed. A record cannot be altered, reordered or removed without
breaking the chain. `Verify` walks the chain and **reports the first sequence
number at which it breaks** — which is the useful answer, because everything
before that sequence remains admissible.

Twenty-two audited actions, named as constants because the conformance engine
matches on them and a typo would silently produce an unmatched invariant.

## Demonstrating it

```sh
# The whole phase.
go test ./internal/money/... ./internal/domain/... ./internal/obs/... \
        ./internal/identity/... ./internal/audit/... -count=1 -v
```

Money — exact arithmetic, half-even rounding, currency mismatch, overflow, and
the text parser under fuzz:

```sh
go test ./internal/money/ -count=1 -v
go test ./internal/money/ -run FuzzParse -fuzz FuzzParse -fuzztime 30s
```

The audit chain, including breaking it deliberately and checking that `Verify`
names the right sequence:

```sh
go test ./internal/audit/ -count=1 -v
```

The determinism property, end to end, is easiest to see in the risk engine's
tests — but it is this phase that makes it possible:

```sh
go test ./internal/risk/ -run 'TestDeterminism' -count=1 -v
```

## The tests that prove it

| Package | Tests | What they establish |
|---|---:|---|
| `internal/money` | 16 + `FuzzParse` | Exact arithmetic; half-even rounding; currency mismatch is an error; overflow is an error, not a wrap; the parser survives arbitrary input |
| `internal/obs` | 19 | Clock substitution; span parent/child structure; `traceparent` round trip; the Prometheus exposition; histogram bucketing |
| `internal/identity` | 7 | Assertion signing and verification; `ErrBadSignature`, `ErrExpired`, `ErrBadToken`; scope and role checks |
| `internal/audit` | 9 | The chain verifies from genesis to head; a mutated record breaks it at the right sequence; signatures verify; the file round-trips |
| `internal/domain` | **0** | — |

**`internal/domain` has no tests of its own.** It is validation and rendering
over value types with no behaviour, and it is exercised very heavily indirectly —
every one of the 375 tests in the repository constructs domain types, and
`internal/risk`'s 61 tests assert on the seeded dataset's exact values. That is a
defensible position for a package with no logic and it is still a gap, because
the seed's own construction — the three planted anomalies, the netting-set
grouping, the haircut schedule's most-specific-wins matching — is logic and is
only asserted from downstream.

The determinism claims that this phase makes possible are enforced downstream:
`TestDeterminism` and `TestDeterminismUnderVaryingWorkerCounts` in
`internal/risk`, and the byte-identical evidence bundle digest in
`internal/conform`.

## What this phase deliberately does not do

- **No persistence.** The audit log writes a file; there is no database. That is
  `migrations/`.
- **No transport.** `identity.Signer` mints assertions; nothing serves them.
- **No collector.** The tracer writes OTLP/JSON to a file or a buffer; there is
  no exporter negotiation, no batching processor, no resource detection. ADR
  0001 lists what the absence of the OpenTelemetry SDK costs.
- **The audit signing key is generated per process** in `platform.New`. A
  deployment loads it from a KMS or HSM; the verification path does not change.
- **The identity signer's secret is the literal string `tcrp-dev-signing-key`.**
  That is fine for a demo and is not a credential.

## What the next phase needs from this one

Phase 2 needs `domain.LedgerEvent` and the `Ledger` values to model two networks
against one vocabulary; `money.Amount` so a fund share quantity and a collateral
value are the same kind of number everywhere; `obs.Clock` so a reorg can be
replayed deterministically; and `audit.Log` so a bridge settlement is a record
rather than a log line.
