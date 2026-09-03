# Architecture

This document covers four things: the components and how they fit; the request
path of one governed tool call, end to end; the data path from a ledger event to
a risk figure; and the trust boundaries.

The organising rule for the whole system is stated in
`internal/platform/platform.go`: there is exactly one constructor, and it is the
only place a component is allowed to be nil. Every binary — the server, the demo,
the eval harness, the CI gate — builds the same object graph through it. If the
guardrail could be skipped in one binary and not another, the platform's claims
would be true only in one binary.

## Components

```
                        ┌──────────────────────────────────────┐
   operator, analyst,   │  internal/api      HTTP + dashboard  │
   approver, auditor ──▶│  one governed path, no side doors    │
                        └───────────────┬──────────────────────┘
                                        │
                        ┌───────────────▼──────────────────────┐
                        │  internal/agent    the run loop      │
                        │   guard → policy → approval → audit  │
                        └───┬───────────┬──────────┬───────────┘
                            │           │          │
            ┌───────────────▼──┐  ┌─────▼──────┐  ┌▼───────────────┐
            │ internal/guard   │  │ policy     │  │ internal/tools │
            └──────────────────┘  └────────────┘  └───┬────────────┘
                                                      │
        ┌─────────────────────────────────────────────┴──────────┐
        │                                                        │
   ┌────▼──────────────┐   ┌──────────────────┐   ┌─────────────▼────┐
   │ internal/risk     │◀──│ internal/lake    │◀──│ internal/ledger  │
   └───────────────────┘   └──────────────────┘   └────────┬─────────┘
                                    ▲                      │
                           ┌────────┴─────────┐   ┌────────▼─────────┐
                           │ internal/stream  │   │ internal/bridge  │
                           └──────────────────┘   └──────────────────┘

   ┌──────────────────────────────────────────────────────────────────┐
   │ internal/audit · internal/obs · internal/identity  (cross-cutting)│
   └──────────────────────────────────────────────────────────────────┘
                                    ▲ evidence
   ┌────────────────────────────────┴─────────────────────────────────┐
   │ internal/conform (I=E gate)   ·   internal/eval (suite + drift)  │
   └──────────────────────────────────────────────────────────────────┘
```

**Cross-cutting, and depended on by everything.**

- `internal/obs` — one `Clock` interface every engine takes as a dependency, a
  tracer emitting OTLP/JSON spans on the OpenTelemetry GenAI conventions, and a
  metric registry with a Prometheus text exposition. Nothing on a decision path
  calls `time.Now` directly. That is what makes a trajectory replayable and a
  deterministic run byte-identical.
- `internal/identity` — the acting principal and the agent workload identity,
  travelling together in the context. The agent never becomes the system: every
  tool call, policy decision, audit record and span is attributed to the human
  *and* to the workload executing on their behalf. `AGENT-4` fails the build when
  a span is missing either.
- `internal/audit` — hash-chained, ed25519-signed, append-only. Each record
  carries `prev_hash` and `hash` over a canonical encoding, so a record cannot be
  altered, reordered or removed without breaking the chain. `Verify` reports the
  first sequence number at which it breaks.

**The domain layer.**

- `internal/money` — exact decimal arithmetic (ADR 0007).
- `internal/domain` — the shared vocabulary. Behaviour is limited to validation
  and rendering, and it depends on nothing but `money`, deliberately: the
  lakehouse, the risk engine, the ledgers and the agent's tools all speak these
  types, and if the vocabulary depended on any one of them the others would
  inherit it.

**Settlement.** `internal/ledger` (two simulators with genuinely different
semantics) and `internal/bridge` (lock-and-mint over M-of-N ed25519 attestations)
— ADR 0008.

**Data and calculation.** `internal/stream` (partitioned log), `internal/lake`
(bronze → bitemporal silver → gold, plus data-quality controls and
reconciliation) and `internal/risk` (a pure function of a snapshot).

**Governance.** `internal/guard` (ADR 0003), `internal/policy` (ADR 0004),
`internal/tools`, `internal/agent` (ADR 0002, ADR 0010), `internal/api`.

**Assurance.** `internal/conform` (ADR 0009) and `internal/eval`.

## The request path of one governed tool call

Trace one call — `issue_margin_call` against CP-DELTA — from HTTP to audit. This
is the path `TestSmokeRun` exercises and the numbers below are from that run.

### 1. HTTP, and the middleware order

`POST /v1/runs` with a body naming the query. The middleware order in
`internal/api/api.go` is a control rather than a convention, and the package
comment says why. Outermost first:

```
recover  →  limitBody  →  requestID  →  traceSpan  →  authenticate  →  mux
              (1 MiB)                                                    │
                                                       per route: accessLog
                                                                 →  observe
                                                                 →  rateLimit
                                                                 →  handler
```

- **Recovery is outermost** so a panic still produces a request id and a log
  line.
- **The request id and the trace span are established before authentication**, so
  a *rejected* request is still traceable.
- **Authentication precedes the access log**, so the log records who was refused.
- **Metrics sit innermost**, so the latency they record is the handler's rather
  than the middleware's.

Authentication is a signed principal assertion in `X-TCRP-Principal`, verified
by `identity.Signer`. `Options.DevPrincipalHeader`, off in a production build,
additionally accepts `X-TCRP-Dev-Principal` naming one of
`platform.PrincipalNames` — a development affordance for the dashboard.

The rate limiter refills from the platform's clock. Under a deterministic
platform that clock is pinned, so the limiter degrades to a fixed budget per
principal rather than a rate. That is stated in `Options.RatePerMinute` rather
than worked around, because a limiter that quietly consulted the wall clock
would make a deterministic run non-reproducible.

`/metrics`, `/healthz` and `/readyz` sit outside `/v1` and outside
authentication: a probe that needs a credential fails for the wrong reason, and
a scrape endpoint that needs one does not get scraped.

### 2. Guardrail on the prompt

Before the model sees anything. `guard.Request{Surface: prompt, Tier: <the
highest tier this principal could reach>}`. The tier is the principal's ceiling,
not the eventual call's, because at this point nobody knows what will be asked
for.

Detectors on the prompt surface: PII patterns, secret patterns, an injection
scorer, a domain detector. A blocking verdict ends the run before step one, with
`status=blocked` and an answer that says the request was stopped before it
reached the model.

An `audit` record `guard.verdict` is written either way, carrying the verdict,
the findings by category and rule, `latency_us`, `budget_exceeded`, the fail
mode, the guard policy version and whether retrieval is still trusted.

### 3. Model turn

`model.Next(state)` inside a 30-second step timeout. The state carries the query,
the message history, the principal, and — critically — only the tool specs at or
below the principal's tier ceiling (`Registry.SpecsForTiers`). A principal who
cannot execute a transfer is not shown `execute_transfer` at all. That is
attack-surface reduction, not authorization; the gateway still refuses it if the
model asks by name.

The default model is `agent.Planner`, deterministic. `agent.AnthropicModel`
swaps in behind the same one-method interface when `ANTHROPIC_API_KEY` is set.

### 4. Policy at propose stage

```go
dec := r.policy.Evaluate(ctx, r.policyInput(run, spec, call.Args, policy.StagePropose, nil))
```

`policy.Input` carries the principal (subject, roles, scopes, tenant, on-behalf),
the tool (name, tier, required scopes, reversibility), the raw args, the resource
(kind, counterparty, ledger, currency, amount in minor units, whether the
destination is whitelisted), and the run (id, stage, step number, environment,
whether retrieval is trusted, prior failures). Evaluation is deny-overrides over
the 16-rule bundle.

For `issue_margin_call` at 35,298,750.00 USD — above the 25,000,000 dual
threshold — the matching allow is `allow-money-movement-dual-approved`, carrying
`require_dual_approval` with roles `[risk-manager, treasurer]` and a 900-second
window, plus `simulate_first` and `notify`.

A denial ends the step. The refusal is returned to the model as a tool error and
is added to the run's `Grounded` set — a refusal is a fact the platform produced,
so the figures inside it are grounded and the model may repeat them. An
`audit` record `policy.decision` carries the rule id, the reason, the bundle
version, the bundle digest, the policy input hash, the obligations and
`latency_us`.

### 5. Approval

The obligation is `require_dual_approval`, so `dec.RequiresApproval()` is true
and `dec.ApprovalsRequired()` is 2.

The runtime writes a `PendingCall` onto the run — approval id, tool call id,
tool, tier, the exact args, the decision, the summary — sets the run's status to
`awaiting_approval`, saves it, and returns. No goroutine blocks. The process can
exit. See ADR 0010.

`ApprovalService.Request` writes the approval with `ArgsHash` set to the
canonical JSON of the args and an `approval.required` audit record.

Two humans then call `POST /v1/approvals/{id}/grant`. `ApprovalService.Grant`
refuses a closed approval, an expired window, the requester or the human they act
for, an approver without `approvals:grant`, an approver without a required role,
and an approver who has already responded — each with a distinct sentinel error
and each writing an `approval.denied` audit record naming the reason.

The first grant leaves the approval `pending (1/2)`. The second satisfies it and
fires `OnResume` **synchronously**, so the approver's request does not return
until the run has moved. That makes the audit ordering unambiguous:
`approval.granted` is immediately followed by `agent.tool.invoked` with no gap in
which a reader has to infer causality from timestamps.

### 6. Policy again, at dispatch stage

`Runtime.Resume` re-evaluates with the approval attached. **The propose-time
allow is not carried forward.** A bundle change, a revoked scope or a market move
past a threshold during the approval window takes effect here.

`deny-money-movement-without-recorded-approval` fires only at
`input.run.stage == "dispatch"` and refuses money movement with no
`input.approval.recorded`. The runtime already would not reach dispatch without
one; the gateway refuses anyway. That is defence in depth, and it is the reason
the second evaluation runs for *every* tier rather than only for money movement.

### 7. Tool invocation

`tool.Invoke(ctx, args)`. Tools are thin: validate arguments, call an engine,
render the answer. No tool computes a figure of its own, and every figure a tool
returns is listed in the result's `Grounded` set.

A span `agent.tool.issue_margin_call` carries the operation name, the tool name,
the tool call id, the tier, the policy rule, the run id, the approval id and the
approvers.

### 8. Guardrail on the tool result

The result is **retrieved context**, not trusted output. `guard.Request{Surface:
context, Source: <tool name>}`. Anything inside it that looks like an instruction
sets `run.RetrievalTrusted = false` and quarantines the content with a prefix
saying so.

The policy rule `deny-state-change-on-untrusted-context` then refuses to let that
run drive any state change for the rest of its life. This is the control that
turns indirect prompt injection from an incident into a logged event.

### 9. Audit

`agent.tool.invoked` — or `collateral.margin_call.issued` for this tool
specifically — with the args, the tier, the effect, the policy rule, the policy
input hash, the bundle version, the approval id, the approvers and their roles.
Every record is hash-chained to its predecessor and ed25519-signed.

### 10. Output guardrail, and finish

When the model produces a final answer, `finalize` inspects it on the output
surface with the run's accumulated `Grounded` set attached. That enables
`GroundingDetector`, which flags numbers in the answer that do not appear
anywhere in what the tools returned — the single highest-value output check in a
risk setting, because a model that invents a figure invents it with the same
confidence as one it read.

A blocking verdict withholds the answer. A redact verdict substitutes the
redacted text. Either way `agent.run.completed` or `agent.run.failed` closes the
chain.

### What that call left behind

One trace with a root `agent.run` span and a child per step and per tool. A run
record with every step's tool, tier, args, reasoning, policy rule and reason,
obligations, guard verdict, approval id, approvers, outcome, duration and span
id. An approval record with both grants, their roles and their timestamps. And a
run of hash-chained audit records — the four-query smoke test produces 45 of
them — that `audit.Verify` accepts and that `internal/conform` reads as
evidence.

## The data path

```
  ledger simulators                     book of record
  (EVM, Canton)                         (golden)
        │                                     │
        │ domain.LedgerEvent                  │
        ▼                                     │
  ┌───────────────────────────────┐           │
  │ BRONZE  bronze_ledger_events  │           │
  │ as observed; nothing derived, │           │
  │ nothing dropped               │           │
  │ dedup on ledger:tx:log_index  │           │
  └───────────────┬───────────────┘           │
                  │                           │
                  ▼                           │
  ┌───────────────────────────────┐           │
  │ SILVER  silver_positions      │           │
  │ bitemporal: [valid_from,to)   │           │
  │             [tx_from,to)      │           │
  │ apply never edits history     │           │
  │ late arrivals mark corrected  │           │
  │ partition (ledger, day(vf))   │           │
  └───────────────┬───────────────┘           │
                  │  AsOf(validAt, knownAt)   │
                  ▼                           │
  ┌───────────────────────────────┐           │
  │ GOLD  gold_positions          │           │
  │ flattened to one (valid,known)│           │
  │ joined to reference:          │           │
  │  counterparties, instruments, │           │
  │  prices, haircuts, limits     │           │
  │ partition (ledger, hour(ka))  │           │
  └───────────────┬───────────────┘           │
                  │                           │
                  │        ┌──────────────────▼──────────────────┐
                  │        │ RECONCILIATION                      │
                  ├───────▶│ instruction level: missing_in_book, │
                  │        │  missing_on_chain, quantity_mismatch│
                  │        │ then position sweep for slots no    │
                  │        │  instruction explains               │
                  │        │ deterministic break ids → dedup     │
                  │        └─────────────────────────────────────┘
                  ▼
  ┌───────────────────────────────┐
  │ domain.Snapshot               │
  │ ValidAt + KnownAt + Lineage   │
  │ + QualityReport               │
  └───────────────┬───────────────┘
                  ▼
  ┌───────────────────────────────┐
  │ internal/risk                 │
  │ pure function; no clock, no   │
  │ I/O, no state between calls   │
  │ collateral → exposure →       │
  │ concentration → limits →      │
  │ stress                        │
  └───────────────────────────────┘
```

**Bronze** lands the ledger event as observed, with nothing derived and nothing
dropped, because the point of a bronze layer is that a bug in a downstream
transform is repairable by replaying it rather than by re-reading a chain.
Duplicates are suppressed on the natural key `ledger:tx_hash:log_index`.

**Silver** folds events into the bitemporal history. Two axes, both half-open,
both supplied by the caller. Applying an event closes affected rows on the
transaction axis at `ObservedAt` and writes replacements starting there; an event
landing inside an already-published interval splits it. Rows superseding a
published belief are marked `corrected`. Nothing is deleted. ADR 0005.

**Gold** flattens to one `(validAt, knownAt)` pair and joins to reference data —
counterparties, instruments, prices, haircuts, limits — which arrives from the
book of record rather than from a ledger, which is why it is a parameter rather
than a table the pipeline populates. Materialised rather than computed on every
read, so a figure on a dashboard has a row behind it that can be pointed at.

**Data quality** runs six controls, named as constants because a dashboard, a
test and an incident review all have to name the same thing:
`dq.completeness.ledger_sequence`, `dq.uniqueness.event_id`,
`dq.timeliness.observation_lag` (15 minutes), `dq.accuracy.supply_identity`,
`dq.referential.known_holder`, `dq.freshness.prices` (1 hour). A snapshot that
failed a critical control is still returned, but marked, so a caller can refuse
to act on it rather than never seeing it.

**Reconciliation** compares the ledger view against the book at a
`(validAt, knownAt)` pair, in the order a settlement operations team works:
instruction level first, then a position sweep over only the slots no
instruction explains — because a position difference with a cause attached is an
operations task and the same difference with no cause is an incident. Break ids
are deterministic hashes of what the break is about, so a run every five minutes
re-detects the same break rather than opening a new one, and an acknowledgement
made between two runs survives.

The seed plants three anomalies so the controls have something to find: an
on-chain transfer the book never received (CP-FOXTROT → CP-DELTA, 1,750,000), a
partial fill the book recorded at a different size (CP-GOLF → CP-BRAVO, 900,000
on chain against 1,000,000 in the book), and an event that occurred before the
cut but was observed after it. The first two are the two open breaks
`TestSmokeRun` reports.

**Risk** is a pure function of the snapshot. No clock of its own, no I/O, no
state between calls, so the same snapshot always produces byte-identical
results — which is what makes a figure defensible three months later when a
counterparty disputes it. The formula is in `internal/risk/doc.go` and
`internal/risk/README.md`.

## Trust boundaries

```
┌─ UNTRUSTED ────────────────────────────────────────────────────────┐
│  the user's prompt                                                 │
│  the model's output                                                │
│  anything a tool retrieved and handed back                         │
└────────────────────────────────────────────────────────────────────┘
        │ crossing requires: guard.Inspect on the relevant surface
        ▼
┌─ SEMI-TRUSTED ─────────────────────────────────────────────────────┐
│  a tool call the model proposed                                    │
│  arguments the model chose                                         │
└────────────────────────────────────────────────────────────────────┘
        │ crossing requires: policy.Evaluate at propose AND dispatch,
        │ plus an approval where the decision carries the obligation
        ▼
┌─ TRUSTED ──────────────────────────────────────────────────────────┐
│  a tool invocation that reached an engine                          │
│  a ledger write                                                    │
│  an audit record                                                   │
└────────────────────────────────────────────────────────────────────┘
```

**Boundary 1 — the model is never trusted.** Not its output, not its choice of
tool, not its arguments. The gateway authorizes the *action*, using the tool's
declared tier and scopes and the principal's, and it does so twice.

**Boundary 2 — retrieved content is not trusted either.** A tool result is
inspected on the context surface. Instruction-like text in a counterparty note
or a reconciliation memo marks `RetrievalTrusted = false` for the rest of the
run, and `deny-state-change-on-untrusted-context` then refuses anything above
read-only. The eval suite's `indirect_injection` category exists to keep that
honest.

**Boundary 3 — the agent is not the system.** The agent principal
(`svc/collateral-agent`) always carries `on_behalf_of: j.okafor`. Both identities
travel together and a decision made with only one of them is a bug. `AGENT-4`
fails the build when a span is missing either.

**Boundary 4 — the approver is not the requester.** Enforced in the approval
service, again as a policy rule, and again as a database `CHECK` in
`migrations/0001_agent.sql`. The demo principals make the separation structural:
`s.aldridge` and `m.duarte` hold `approvals:grant` and **no execution scope at
all**. An approver who could also execute is not a second pair of eyes.

**Boundary 5 — the platform does not trust its own operator.** The audit log is
append-only and hash-chained, `migrations/0002_audit.sql` puts an insert-only
trigger behind it at the database level, and `conform.Bundle` is signed and
content-addressed so a verdict can be checked by somebody who does not trust the
process that produced it. `VerifyBundle` needs a file, a public key, and the
package.

**Boundary 6 — egress.** The only outbound network call the platform makes is to
the model API. `deploy/k8s/networkpolicy.yaml` is default-deny with that as the
single exception. Tool arguments and prompts are the data that crosses it, which
is why `docs/threat-model.md` treats exfiltration through tool arguments as a
distinct threat from exfiltration through the answer.

**What is inside no boundary.** The ledger simulators are in-process and hold no
secrets. The bridge signers are in-process and hold keys in memory — in a real
deployment they are the most important boundary in the system and here they are
not a boundary at all. ADR 0008 says so under "what this model does not defend
against".
