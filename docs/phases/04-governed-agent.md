# Phase 4 — The governed agent

`internal/guard`, `internal/policy` (+ `expr`), `internal/tools`,
`internal/agent`, `internal/api`, `web/`.

8,591 lines of Go source, 653 of test. **This phase has the repository's weakest
unit-test coverage and that is stated in full below rather than at the end.**

## What this phase delivers

An agent that assists a collateral desk, where every action it takes is
authorized before it happens, traced while it happens, and evidenced after it
happens.

### The run loop, and its ordering

The loop is hand-written. The ordering is the security property, not an
implementation detail, and a registration API cannot enforce an order. ADR 0002.

```
1. guard.Inspect(prompt)          before the model sees anything
2. model.Next(state)              only tools at or below the principal's ceiling
3. policy.Evaluate(propose)
4. approvals.Request  ──pause──▶  persisted; the process can exit
5. policy.Evaluate(dispatch)      with the approval; the propose-time allow is
                                  NOT carried forward
6. tool.Invoke
7. guard.Inspect(result)          as retrieved context, not as output
8. audit.Append                   at every step
```

`agent.New` returns an error when any of the tool registry, model, guardrail
inspector, policy engine, audit log or store is nil. A runtime missing one of
those is not a degraded runtime, it is an ungoverned one, so construction fails
rather than defaulting.

Budgets: 12 steps, 3 failures, a 30-second step timeout, a 15-minute approval
window.

### The guardrail

Three surfaces — prompt, retrieved context, model output — with deterministic
detectors: PII patterns (Luhn-checked cards, IBAN shape), secret patterns, an
injection scorer, an ungrounded-number detector and a domain detector. A
model-backed classifier is a pluggable extra stage that runs last inside whatever
budget remains; its unavailability never changes the verdict of the deterministic
stages that already ran, because a guardrail that cannot run without a network
round trip is a guardrail that is off during an incident.

A 100 ms pipeline budget, 30 ms per stage, and **asymmetric fail modes**:
read-only fails open and is flagged, state-change and money-movement fail closed.
Blocking severity is tiered the same way — critical for read-only, high for
state-change, medium for money-movement. ADR 0003.

The **grounding detector** is the highest-value output check in a risk setting: a
number in the answer that does not appear anywhere in what the tools returned is
flagged, because a model that invents a figure invents it with exactly the same
confidence as one it read.

### The policy gateway

Sixteen rules — twelve denies, four allows — over a versioned JSON bundle
embedded with `go:embed`, evaluated deny-overrides, with `default_effect: deny`.
Conditions are written in a small **total** expression language: no loops, no
user-defined functions, no unbounded recursion, so every expression terminates in
time proportional to its own size. A policy that can loop is a policy that can
hang the desk. ADR 0004.

The decision is evidence, not just a verdict: `RuleID`, `Reason`,
`BundleVersion`, `BundleDigest`, `InputHash`, the obligations and the evaluation
duration, all written into the audit record.

Obligations are checked, not advisory. An obligation type not in
`KnownObligations` is a **hard failure at bundle load**, so adding one can never
silently weaken enforcement.

### Sixteen tools

Each declares its risk tier, the scopes a principal must hold, and whether it is
reversible — as **data the gateway authorizes against**, not as documentation.

| Tier | Count | Tools |
|---|---:|---|
| `read_only` | 11 | `get_exposure`, `get_collateral_value`, `get_position`, `get_concentration`, `query_asof`, `list_recon_breaks`, `get_supply_reconciliation`, `run_stress_scenario`, `check_whitelist`, `get_haircut_schedule`, `get_counterparty_notes` |
| `state_change` | 2 | `propose_margin_call`, `draft_transfer` |
| `money_movement` | 3 | `issue_margin_call`, `execute_transfer`, `redeem_shares` |

The three money-movement tools are all `Reversible: false`.

Tools are thin. They validate arguments, call an engine, and render the answer.
**No tool computes a figure of its own**, and every figure a tool returns is
listed in the result's `Grounded` set — which is what lets the output guardrail
refuse any number the model did not get from here.

### Approvals as a persisted state machine

A first-class stored object, not a channel or a callback, because the run may
outlive the process that started it. The run is checkpointed, its status becomes
`awaiting_approval`, and nothing about the call executes. An approver who
responds forty minutes later, after a deploy, resumes the same run against the
same arguments — pinned by `ArgsHash`. ADR 0010.

Four-eyes is enforced in the service with distinct sentinel errors, restated as
policy rules, and restated again as database constraints in
`migrations/0001_agent.sql`.

### The HTTP surface

Twenty-six routes under `/v1` — runs, approvals, risk, reconciliation and
settlement data, governance — plus `/metrics`, `/healthz`, `/readyz` and the
dashboard. The package owns no state and computes nothing: every answer comes
from a component the platform already assembled, and every write goes through the
same runtime, approval service and break register the CLI uses. **There is one
governed path and this package is a transport in front of it** — an HTTP layer
that could start a run without the policy gateway, or record an approval without
the four-eyes check, would make the platform's claims true only for the callers
that happen to use the other entry point.

The middleware order is a control: recovery outermost so a panic still produces a
request id; the request id and trace span before authentication so a rejected
request is still traceable; authentication before the access log so the log
records who was refused; metrics innermost so their latency is the handler's.

## Demonstrating it

The whole phase, end to end:

```sh
go test ./internal/platform/ -run TestSmokeRun -v -count=1
```

Four queries, one governed platform, a deterministic clock. What it shows:

```
=== What is our exposure to CP-DELTA?
status=completed steps=2 trajectory=[get_exposure]
  step 1 tool=get_exposure  outcome=invoked  rule=allow-read-only
  MARGIN CALL  35298750.00 USD

=== CP-DELTA looks undermargined, issue a margin call
status=awaiting_approval steps=4
  step 1 get_exposure          invoked            allow-read-only
  step 2 get_collateral_value  invoked            allow-read-only
  step 3 propose_margin_call   invoked            allow-state-change-proposal
  step 4 issue_margin_call     awaiting_approval  allow-money-movement-dual-approved
  approval after risk-manager: pending (1/2)
  approval after treasurer:    granted (2/2)
  after resume: status=completed

=== Transfer 5,000,000 shares to CP-HOTEL
  step 3 execute_transfer  policy_denied  deny-transfer-to-non-whitelisted

audit records: 45
```

Four controls, visible: the read query never touched an approval; the margin call
needed **two distinct approvers** because 35,298,750.00 is above the 25,000,000
dual threshold; the transfer to an unregistered counterparty was refused **at the
gateway**, before a ledger could revert it; and the session left 45 hash-chained
audit records that `Audit.Verify` accepts.

The expression language, including under fuzz:

```sh
go test ./internal/policy/... -count=1 -v
go test ./internal/policy/expr/ -run FuzzCompile -fuzz FuzzCompile -fuzztime 30s
```

The governed path against the adversarial scenarios — this is where the guardrail
and the tools are actually exercised:

```sh
go test ./internal/eval/... -count=1 -v
```

## The tests that prove it

**Directly:**

| Package | Tests |
|---|---:|
| `internal/policy/expr` | 15 + `FuzzCompile` |
| `internal/policy` | 1 (the shipped bundle compiles) |
| `internal/guard` | **0** |
| `internal/tools` | **0** |
| `internal/agent` | **0** |
| `internal/api` | **0** |

**Indirectly, and this is real coverage rather than an excuse:**

- `internal/platform/smoke_test.go` drives four queries through the complete
  governed path including a dual-approval gate, a policy denial and an audit
  chain verification.
- `internal/eval` — 34 scenarios across 12 categories, 53 tests in the package —
  exercises the guardrail on all three surfaces, the gateway on every rule
  category, the tools, the approval gates and the runtime's budgets.
- `internal/conform` — 80 tests — evaluates seven agent invariants against the
  audit log and the spans this phase produces:

| Invariant | Statement | Severity |
|---|---|---|
| `AGENT-1` | No money-movement invocation without a granted approval from someone other than the requester | critical |
| `AGENT-2` | Every tool invocation is preceded by an allowing policy decision for that tool | critical |
| `AGENT-3` | The guardrail ran on every prompt and every output of every run | high |
| `AGENT-4` | Every span in a run's trajectory carries the principal identity attributes | high |
| `AGENT-5` | The audit chain verifies end to end | critical |
| `AGENT-6` | No run exceeded its step budget without a terminal record | high |
| `AGENT-7` | A policy denial is never followed by the same tool succeeding in the same run | critical |

That is coverage of the *controls*, which is the load-bearing part, and it is
coverage through other packages rather than unit tests.

## The gap, stated plainly

**Four packages in this phase have no `_test.go` files: `internal/guard`,
`internal/tools`, `internal/agent`, `internal/api`.** That is 6,925 lines of
source, including the run loop, the guardrail detectors and the approval state
machine — the three things this repository's central claims rest on.

The indirect coverage is real but partial. Measured with `-coverpkg` so that
tests in one package are credited to the code they exercise in another:

```sh
go test ./... -count=1 -coverpkg=./... -coverprofile=coverage.out -covermode=atomic
go tool cover -func=coverage.out | tail -1
```

| Package | Statement coverage |
|---|---:|
| `internal/agent` | 55.6% (489/879) |
| `internal/tools` | 41.2% (175/425) |
| `internal/guard` | 36.5% (133/364) |
| `internal/api` | **0.0%** (0/754) |
| `internal/policy` | 73.0% (135/185) |
| `internal/policy/expr` | 92.0% (462/502) |
| Module total | 74.4% (7,940/10,673) |

`internal/api` at exactly zero is the sharpest statement available: **nothing in
the repository exercises the HTTP surface at all.** Not one handler, not the
middleware chain, not the rate limiter, not the authentication path. Every claim
in this document about the API is a claim about code that has never run.

What is missing that indirect coverage does not provide:

- **Detector unit tests.** Each pattern's true and false positives. A Luhn check
  that accepts a non-card, an IBAN shape that misses a real one, an injection
  scorer's threshold behaviour at 49 and 51 — none of that is asserted.
- **Guardrail budget and fail-mode tests.** The per-tier fail-open/fail-closed
  asymmetry is the design's most important property and there is no test that
  forces a budget exhaustion and checks the verdict per tier.
- **`guard.Client`.** The remote transport has no test at all.
- **Approval service edge cases.** Each of the six sentinel errors, the expiry
  sweep racing a grant, the resume-after-denial path.
- **Runtime budget tests.** Step budget exhaustion, failure budget exhaustion,
  unknown tool handling, model error retry.
- **HTTP handler tests.** Status codes, the middleware order, the rate limiter,
  body limits, the auth paths.

It is a gap, not a design choice, and it is the first thing to fix.

## What this phase deliberately does not do

- **`cmd/tcrpd` has no tests of its own.** The binary exists and builds; the
  package is 0.0% covered under `-coverpkg=./...` and its landing moved
  module-wide statement coverage from 74.4% to 72.3%. What the subcommands do is
  covered — by `internal/platform`'s smoke test, by the conformance gate and by
  the eval suite — but not through this package's statements.
- **The default model is a deterministic planner**, not a language model.
  `AnthropicModel` swaps in behind the same one-method interface when
  `ANTHROPIC_API_KEY` is set, and every control behaves identically — which is
  assertable precisely because the planner path is deterministic.
- **The policy bundle is digested but not signed.** `LoadBundleFile` loads any
  file it is given. `docs/threat-model.md` §6 calls this the most significant
  gap in the document.
- **The guardrail ships in-process by default.** The HTTP service and client
  exist; the sidecar deployment is a wiring change and is not the default.
- **`agent.MemoryStore` writes JSON files.** Production is Postgres;
  `migrations/0001_agent.sql` is the schema, and nothing applies it — no Go file
  in this repository imports `database/sql`.
- **`DevPrincipalHeader` accepts a principal by name.** A development affordance
  for the dashboard; a production build leaves it false.
- **No streaming, no partial results, no cancellation of an in-flight tool.**

## What the next phase needs from this one

Phase 5 needs the audit chain and the spans this phase produces. The conformance
invariants read them as **evidence** rather than reading the source — which is
the whole distinction, because a test that reads the source can be satisfied by
code that is never executed, and an invariant that reads the audit chain is
satisfied only by what the system actually did.
