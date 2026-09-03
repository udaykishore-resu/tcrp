# ADR 0003 — Guardrail transport, and asymmetric fail modes

## Status

Accepted. In force. `internal/guard/pipeline.go`, `internal/guard/service.go`.

> `internal/guard/service.go` refers to this record as
> `docs/adr/0003-guardrail-transport.md`. This file is the record it means.

## Context

The guardrail inspects three surfaces on every turn — the prompt, the retrieved
context, the model output. It sits on the request path, so its latency is the
user's, and it is a control, so its availability is the platform's control
coverage. Those two facts pull opposite ways on topology and again on fail mode.

**Topology.** In-process is fastest and cannot be unreachable, but its release
cadence is the platform's and a badly backtracking regex takes the trading
system down. A sidecar buys independent release and a blast-radius boundary for
a round trip and a second failure mode. A remote service buys one policy version
across every consumer for a network hop on the hot path.

**Fail mode.** A guardrail with a latency budget will exceed it, and there is no
single right answer: failing open on a read query costs an unfiltered answer to
someone already allowed to ask; failing open on a wire costs the wire.

## Decision

**Transport is wiring behind one interface.** `guard.Inspector` has a single
method. `guard.Engine` implements it in-process; `guard.Client` implements it
against a remote service. `platform.New` selects on `Options.GuardURL`, and the
agent runtime cannot tell the difference.

The repository defaults to **in-process** and ships the HTTP service
(`guard.Handler`: `POST /v1/inspect`, `GET /v1/healthz`, `GET /v1/config`) so
the standalone topology is code rather than a claim. `deploy/docker-compose.yml`
runs it that way. Requests carry a signed principal assertion and a W3C
`traceparent`, so a guardrail decision is attributable and lands in the same
trace as the run.

**Fail modes are per risk tier, and asymmetric.** From `guard.DefaultConfig`:

| Tier | Blocks at | Budget exceeded |
|---|---|---|
| `read_only` | critical | **fail open**, flagged |
| `state_change` | high | **fail closed** |
| `money_movement` | medium | **fail closed** |

Budget 100 ms for the pipeline, 30 ms per stage. `guard.Client`'s HTTP timeout
comes from the same budget, and its own `FailOpen` map defaults to read-only for
the case where the *service* is unreachable rather than merely slow.

## Consequences

**The asymmetry is the point.** A uniformly fail-open guardrail is off exactly
when the system is loaded, which is when an attack is likeliest. A uniformly
fail-closed one is an availability incident every time a detector is slow. The
tiering puts the trade where the cost is asymmetric: an unfiltered answer is
recoverable, a transfer is not. The severity thresholds tier the same way — a
money-movement turn blocks on a medium finding a read-only turn tolerates.

**Every result says which mode it took.** `guard.Result` carries
`BudgetExceeded`, `FailMode` and a per-stage breakdown with each detector's
duration and whether it was skipped. Those reach the `guard.verdict` audit
record with `latency_us`, so "the guardrail was degraded during this incident"
is a query rather than a guess. `docs/runbook.md` has the procedure.

**Detectors are deterministic and cheap** — PII and secret patterns, an
injection scorer, an ungrounded-number detector, a domain detector, all pure
functions of text. A model-backed `Classifier` is a pluggable extra stage that
runs last inside whatever budget remains, and its unavailability never changes
the verdict of the deterministic stages that already ran. A guardrail that
cannot run without a network round trip is off during an incident.

**In-process means shared fate.** A pathological regex is a platform outage. The
mitigation is the per-stage budget and short reviewable patterns, not absence of
risk.

**The remote path is lightly exercised.** `guard.Client` has no unit tests;
`internal/guard` has none at all. That is a gap. The in-process path is covered
by `internal/platform/smoke_test.go` and every eval scenario.

## Alternatives considered

**In-process only.** Simpler and honest about what ships. Rejected because the
interface and the service are what make "this could be a sidecar" verifiable.

**Sidecar only.** Costs the clean-clone property — `go test ./...` would need a
second process — and makes every runtime test an integration test.

**Uniform fail-open.** The control disappears under exactly the load that
matters.

**Uniform fail-closed.** An exposure query fails because a regex was slow, which
trains operators to route around the guardrail. A control people work around is
worse than one tuned to be survivable.

**A global fail-mode flag.** The cost of failing open genuinely differs per
tier; one flag forces the wrong answer for one of them.

**One blocking severity for every tier.** A medium injection signal heading for
`execute_transfer` and the same signal heading for `get_exposure` are not the
same risk, and a single threshold covering both is wrong for both.
