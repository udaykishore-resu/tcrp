# ADR 0001 — Standard library only

## Status

Accepted. In force. `go.mod` has no `require` block.

## Context

Five areas invite a dependency: policy evaluation (OPA), streaming (franz-go),
telemetry (the OpenTelemetry and Prometheus SDKs), columnar storage
(parquet-go), and the model API (the Anthropic SDK). Each is better than what
one engineer writes in a weekend.

Against that: this is a portfolio system whose argument is that a reader can
check its claims. Someone who must install a toolchain, resolve a module graph
and trust a transitive dependency is not checking anything. And "money movement
always pauses for a second human" is a claim about code — worth less when the
code enforcing it sits three layers of vendored indirection from the file that
says so.

There is a narrower argument too. A runtime that calls a policy sidecar has an
availability coupling: unreachable means fail closed (the desk stops) or fail
open (the control stops). In-process evaluation removes that failure mode for
the one decision that must never be skipped.

## Decision

Depend on nothing outside the standard library. No require block, no vendor
directory, no code generation, no build step but `go build`. The repository
builds and its full test suite runs from a clean clone with no network.

Where a library would have been used, write the smallest thing satisfying the
actual requirement and state in the package comment what was given up.

## Consequences

**What it costs, specifically.**

- **No OPA.** `internal/policy` is a hand-written deny-overrides evaluator;
  `internal/policy/expr` is a 1,000-line total expression language. Against Rego
  this loses partial evaluation, `opa test`, bundle distribution, and hireable
  familiarity. ADR 0004.
- **No Kafka client.** `internal/stream` has partitions, consumer groups and
  committed offsets, and no replication, no cross-broker leader election, no key
  compaction, no network transport.
- **No OpenTelemetry SDK.** `internal/obs` emits OTLP/JSON and Prometheus text
  by hand: no exporter negotiation, no batching processor with back-pressure, no
  resource detection, no semantic-convention package. The GenAI attribute keys
  are typed out.
- **No Parquet.** `internal/lake` writes gzipped NDJSON behind a `FileFormat`
  interface. The Iceberg *metadata* is real; the data files are not. ADR 0006.
- **No Anthropic SDK.** `net/http` directly; retries, streaming and error
  taxonomy are thinner than the SDK's.
- **No testify, no gomock.** Table-driven tests and hand-written fakes: more
  lines, fewer surprises.

**What it buys.** `git clone && go test ./...` works offline in seven seconds,
which is the property most likely to make someone actually run the code. Every
line governing a transfer, an approval or a policy decision is in this
repository. There is no supply-chain surface. And the failure modes are the
platform's own: an exceeded guardrail budget is a decision `internal/guard`
made, not a timeout in a library's connection pool.

**Where the line is.** The decision covers the *runtime*. `contracts/`, `daml/`
and `spark/` deliberately depend on Foundry, the Daml SDK and Spark — and are
deliberately not built by this repository's CI for exactly that reason. The
`foundry.yml` and `daml.yml` workflows are gated to environments where those
toolchains exist.

**What would change it.** A production deployment should take the dependencies.
`guard.Inspector`, `agent.Model`, `agent.Store`, `lake.FileFormat` and
`risk.LeaseStore` are the seams, and they are interfaces so the swap is wiring.
The argument is not that dependencies are bad; it is that a system whose point
is to be inspected should be inspectable.

## Alternatives considered

**Take the obvious dependencies.** Rejected: the clean-clone property is what
makes every other claim here checkable.

**Vendor them.** Preserves the offline build, loses the inspectability — the
wrong half to keep. A reader who will not install a module will not read 200,000
vendored lines either.

**Standard library plus `golang.org/x/...`.** An unprincipled boundary.
`x/sync/errgroup` is the thin end; the discipline is easier to hold at zero than
at "a few good ones".

**Dependencies on non-control paths only.** Costs the offline test run for the
least interesting half of the system, and "this is not on a control path" is an
argument that weakens every release.
