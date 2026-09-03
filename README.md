# TCRP — Tokenized Collateral Risk Platform

An institution issues a tokenised money-market fund share. Counterparties pledge
it as collateral. The share exists on two settlement networks at once — as an
ERC-3643-style permissioned token on an EVM chain and as a Daml contract on a
Canton network — and neither network is authoritative on its own. An AI agent
assists the collateral desk: it answers exposure questions, proposes margin
calls, and can move value. Every action it takes is authorized before it
happens, traced while it happens, and evidenced after it happens, and no path
through the system reaches money without a human who is not the requester.

**Go 1.24, standard library only, no third-party dependencies, no Docker
required. It builds and runs its full test suite from a clean clone with no
network access.** `go.mod` has no `require` block. There is no vendor
directory, no code generation step, and nothing to install before `go test`.

```
git clone <repo> && cd tcrp
go build ./...
go test ./... -count=1
```

## This is a portfolio project

It is a project, not production experience. Nothing here has run against a real
chain, a real Canton domain, a real counterparty or real money. It is labelled
as such deliberately, and the same labelling runs through every document in
`docs/` and every package comment in the source: where something is simulated,
the code says so at the top of the file rather than leaving a reader to find
out. The section [What is real and what is simulated](#what-is-real-and-what-is-simulated)
is the summary; it is not buried.

## Running it

`cmd/tcrpd` is the binary. One package, one object graph: every subcommand
builds the platform through `internal/platform`, so a control cannot be present
in one entry point and absent from another.

```sh
go build ./...                  # compiles every package
go build -o tcrpd ./cmd/tcrpd
./tcrpd demo                    # the whole platform, end to end, in one screen
```

### The commands

| Command | What it does |
|---|---|
| `tcrpd demo` | Narrates the platform in thirteen acts with real numbers and exits 0. No flags. |
| `tcrpd serve --addr :8080` | The HTTP API and the governance dashboard. Also `--dir`, `--env`, `--policy`, `--guard-url`, `--model`, `--model-id`, `--log`, `--trace-file`, `--dev-auth`. |
| `tcrpd ask "<question>"` | One governed turn from the terminal. `--as`, `--approve-as`, `--env`, `--json`, `--dir`, `--model`. |
| `tcrpd eval` | The 34-scenario suite against the real runtime. `--suite`, `--out`, `--markdown`, `--concurrency`, `--category`, `--strict`. |
| `tcrpd conform` | The conformance gate and the signed evidence bundle. `--process`, `--bundle`, `--gate`, `--markdown`, `--exercise`. |
| `tcrpd version` | Build stamps. A subcommand, not a flag — `--version` exits 2. |

`--as` and `--approve-as` take one of `agent`, `analyst`, `operator`,
`risk-manager`, `treasurer`, `auditor`. Every subcommand takes `-h`.

The `Makefile` wraps the common ones: `make build test test-race vet fmt lint
demo run ask eval gate bench cover clean check ci`.

### Two things worth running first

`tcrpd demo` builds the object graph, drives thirteen scenarios through it and
evaluates all thirteen invariants at the end. It is the fastest way to see the
system work, and it is also the cheapest end-to-end smoke test in the
repository — it exits non-zero if any of that fails.

`tcrpd conform --process bpmn/collateral-margin-call.bpmn --gate --bundle
evidence.json` is the CI gate. It exercises a representative workload, checks
the seven `AGENT-*` invariants against what the platform actually did, writes a
signed evidence bundle, and exits non-zero when the gate does not pass. The same
command against `bpmn/bridge-lock-and-mint.bpmn` checks the six `DATA-*` and
`SETTLE-*` invariants.

### An excerpt from `tcrpd demo`

Act 3 of thirteen, verbatim. Run ids and approval ids differ per run; everything
else is fixed by the seed instant, `2026-09-03T16:00:00Z`.

```
  ────────────────────────────────────────────────────────────────────────────
  3. MONEY MOVEMENT STOPS FOR A HUMAN, AND ONE APPROVER IS NOT ENOUGH
  ────────────────────────────────────────────────────────────────────────────
  principal        agent/svc/collateral-agent@collateral-ops obo:j.okafor
  query            "CP-DELTA looks undermargined, issue a margin call"
  run              run_ad2fdb1334e1  status=awaiting_approval  trace=…00000000001e
    step 1         get_exposure             read_only          invoked (0.00ms)
    step 2         get_collateral_value     read_only          invoked (0.00ms)
    step 3         propose_margin_call      state_change       invoked (0.00ms)
    step 4         issue_margin_call        money_movement     PAUSED for apr_54bfd0f82c32

  gate             issue_margin_call(counterparty=CP-DELTA, amount=35298750, currency=USD, proposal_id=MC-CP-DELTA-0)
                   2 approver(s) required, from risk-manager or treasurer, window closes 2026-09-03T16:15:00Z
    s.aldridge     granted  (1 of 2) → pending
    m.duarte       granted  (2 of 2) → granted

  run              run_ad2fdb1334e1  status=completed  trace=…00000000001e
    step 1         get_exposure             read_only          invoked (0.00ms)
    step 2         get_collateral_value     read_only          invoked (0.00ms)
    step 3         propose_margin_call      state_change       invoked (0.00ms)
    step 4         issue_margin_call        money_movement     PAUSED for apr_54bfd0f82c32
    step 5         issue_margin_call        money_movement     invoked (0.00ms)
    step 6         answered                 guard=allow
  issued           approved by s.aldridge and m.duarte, policy rule allow-money-movement-dual-approved, approval apr_54bfd0f82c32
```

Act 4 is the same gate refusing the requester:

```
  run              run_eb8e0837d9f5 paused on execute_transfer
    j.okafor       holds approvals:grant and risk-manager, and is still refused:
                   agent: the requester may not approve their own action
    s.aldridge     granted → run completed
    execute_transfer invoked, approved by s.aldridge
```

Act 12 measures what the two inline controls cost, on whatever machine you run
it on. From a run on the machine this paragraph was written on — two vCPUs, Go
1.24.7, linux/amd64 — with the caveat that these are process-local figures from
a single-tenant box and not a production measurement:

```
  guardrail inspection     n=2000  mean    67.2µs  p50    71.4µs  p95   107.1µs  p99   172.2µs
  policy evaluation        n=2000  mean    19.0µs  p50    12.5µs  p95    49.9µs  p99    84.9µs
```

Repeat runs on the same machine moved the guardrail p50 between 68.9µs and
72.6µs and its p99 between 171.6µs and 224.8µs, so treat the third significant
figure as noise. `docs/slo.md` carries the objectives.

### The test suite

```sh
go vet ./...                    # clean
gofmt -l .                      # prints nothing
go test ./... -count=1          # 5s with a warm build cache, 32s from `go clean -cache`
go test ./... -count=1 -race    # 17s warm, 43s cold
```

Measured on this machine: two vCPUs, Go 1.24.7, linux/amd64. `-count=1` defeats
the test cache, so "warm" and "cold" here mean the build cache and nothing else.

The end-to-end behaviour is also exercised by `TestSmokeRun` in
`internal/platform/smoke_test.go`, which assembles the whole platform on a
deterministic clock and drives four queries through the governed runtime:

```sh
go test ./internal/platform/ -run TestSmokeRun -v -count=1
```

That run produces the following, abridged — the real output carries the full
exposure and collateral tables at each step. The clock is pinned to the seed
instant, 2026-09-03T16:00:00Z, so the figures are the same on every run:

```
=== What is our exposure to CP-DELTA?
status=completed steps=2 trajectory=[get_exposure]
  Gross exposure    45500000.00 USD
  Net exposure      45500000.00 USD
  Collateral         9701250.00 USD
  Uncovered         35798750.00 USD
  Threshold           500000.00 USD
  MARGIN CALL       35298750.00 USD

=== CP-DELTA looks undermargined, issue a margin call
status=awaiting_approval steps=4
  step 1 tool=get_exposure          outcome=invoked           rule=allow-read-only
  step 2 tool=get_collateral_value  outcome=invoked           rule=allow-read-only
  step 3 tool=propose_margin_call   outcome=invoked           rule=allow-state-change-proposal
  step 4 tool=issue_margin_call     outcome=awaiting_approval rule=allow-money-movement-dual-approved
  approval after risk-manager: pending (1/2)
  approval after treasurer:    granted (2/2)
  after resume: status=completed

=== Transfer 5,000,000 shares to CP-HOTEL
status=completed steps=4 trajectory=[check_whitelist draft_transfer execute_transfer]
  step 3 tool=execute_transfer  outcome=policy_denied  rule=deny-transfer-to-non-whitelisted

=== Are there any reconciliation breaks?
  [critical] missing_in_book      CP-DELTA  evm     book=0 ledger=1,750,000
  [high]     quantity_mismatch    CP-BRAVO  canton  book=1,000,000 ledger=900,000

audit records: 45
```

Four things are visible there and each is a control rather than a behaviour of
the model: the read query never touched an approval; the margin call stopped at
a gate and needed two distinct approvers because the amount was above the dual
threshold; the transfer to an unregistered counterparty was refused by rule
`deny-transfer-to-non-whitelisted` before it reached a ledger; and the whole
session left 45 hash-chained audit records that `Audit.Verify` accepts.

## The five phases

Each phase is a coherent slice that could ship and be written up on its own. The
line counts are `wc -l` over the non-test and test Go files in each package,
measured on this tree.

### Phase 1 — Foundations

`internal/money`, `internal/domain`, `internal/obs`, `internal/identity`,
`internal/audit`. 3,004 lines of source, 2,035 of test.

Exact decimal money (int64 minor units at 1e-6, exact rationals for every rate,
one half-even rounding inside the money package). The shared domain vocabulary.
An observability kernel where nothing on a decision path calls `time.Now` — every
engine takes an `obs.Clock`, which is what makes a trajectory replayable. A
principal type that carries the acting human and the agent workload identity
together, end to end. A hash-chained, ed25519-signed audit log whose `Verify`
reports the first sequence number at which the chain breaks.

See `docs/phases/01-foundations.md`.

### Phase 2 — Dual-ledger settlement

`internal/ledger`, `internal/bridge`, plus `contracts/` (Solidity) and `daml/`.
3,605 lines of Go source, 2,382 of test; 1,420 lines of Solidity, 584 of Daml.

Two in-process ledger simulators with genuinely different semantics: an
ERC-3643-style permissioned token with an identity registry, a compliance module
and a bridge vault; and a Daml contract store with signatories, observers,
contract keys, propose-accept transfers and selective disclosure. A lock-and-mint
bridge between them, authorised by an M-of-N set of ed25519 attestations, built
around four failure modes — pre-finality reorg, post-finality reorg, replayed
lock, unavailable signer — each of which has a test.

The Solidity and Daml sources are the specification the Go simulators mirror.
Neither side is generated from the other and nothing enforces the
correspondence automatically. `contracts/README.md` and `daml/README.md` say so.

See `docs/phases/02-dual-ledger-settlement.md`.

### Phase 3 — Lakehouse and risk

`internal/stream`, `internal/lake`, `internal/risk`, plus `spark/` (Scala).
8,218 lines of Go source, 4,364 of test; 812 lines of Scala.

An Iceberg-shaped lakehouse: schemas with stable field ids, partition specs,
snapshots with parents and manifest lists, per-column bounds used to skip files,
time travel by snapshot id or wall clock, snapshot expiry and compaction. A
bitemporal silver store where valid time and transaction time are two arguments
to one query. Gold snapshots joined to reference data. Six data-quality controls
and a reconciliation engine against the book of record. A pure risk engine that
turns a snapshot into collateral values, exposures, concentrations, limit
breaches and stress results, with byte-identical output for identical input.

The data file format is gzipped NDJSON behind a `FileFormat` interface, not
Parquet. That is a real limitation and `internal/lake/lake.go` states it in the
package comment. See ADR 0006.

See `docs/phases/03-lakehouse-and-risk.md`.

### Phase 4 — The governed agent

`internal/guard`, `internal/policy` (+ `expr`), `internal/tools`,
`internal/agent`, `internal/api`, `web/`. 8,591 lines of Go source, 653 of test.

A hand-written agent loop whose ordering is the security property: guardrail,
then policy, then approval, then audit — in that order, on every step, with no
path around it. An inline guardrail with deterministic detectors, a declared
latency budget and a per-tier fail mode. A policy gateway evaluating a versioned
bundle of 16 rules in-process, deny-overrides, over a small total expression
language. Sixteen tools, each declaring its risk tier, required scopes and
reversibility as data the gateway authorizes against. Human approval as a
persisted state machine that survives a restart. An HTTP surface and a
governance dashboard over exactly the same governed path.

Test coverage in this phase is the repository's weakest: `internal/guard`,
`internal/tools`, `internal/agent` and `internal/api` have no `_test.go` files
of their own. They are exercised end to end by `internal/platform/smoke_test.go`
and by the eval suite in `internal/eval`, and every one of their controls has a
conformance invariant in `internal/conform` — but that is coverage through other
packages, not unit tests, and it is a gap rather than a design choice. Measured
with `-coverpkg=./...` so cross-package exercise is credited: `internal/agent`
55.6%, `internal/tools` 41.2%, `internal/guard` 36.5%, and `internal/api`
**0.0%** — nothing in the repository touches the HTTP surface at all.

See `docs/phases/04-governed-agent.md`.

### Phase 5 — Assurance

`internal/conform`, `internal/eval`, plus `bpmn/`. 5,626 lines of Go source,
3,810 of test; 286 lines of BPMN.

Intent equals evidence. Two processes are declared once, as annotated BPMN 2.0
models. Thirteen invariants are registered against those declarations; a model
that names an invariant nothing implements fails at parse time. `Process.Validate`
proves by graph dominance that every element tagged `@tier money_movement` is
preceded by an approval gate on every path from a start event. The engine
evaluates each invariant against evidence collected from the audit log, the
spans, both ledgers, the bridge and the lake, and packages the verdicts into a
signed, content-addressed bundle. An inconclusive critical invariant fails the
gate, because absence of evidence is not evidence of conformance.

Alongside it, an evaluation harness: 34 scenarios across 12 categories, a
deterministic offline judge, trajectory and tool-F1 scoring, and a drift
detector using PSI and Kolmogorov-Smirnov against a stored baseline.

See `docs/phases/05-assurance.md`.

## Architecture

```
                        ┌──────────────────────────────────────┐
   operator, analyst,   │  internal/api      HTTP + dashboard  │
   approver, auditor ──▶│  one governed path, no side doors    │
                        └───────────────┬──────────────────────┘
                                        │
                        ┌───────────────▼──────────────────────┐
                        │  internal/agent    the run loop      │
                        │                                      │
                        │   1. guard.Inspect(prompt)           │
                        │   2. model.Next(state)               │
                        │   3. policy.Evaluate(propose)        │
                        │   4. approvals.Request  ─── pause ───┼─▶ human
                        │   5. policy.Evaluate(dispatch)       │   (persisted,
                        │   6. tool.Invoke                     │    survives
                        │   7. guard.Inspect(tool output)      │    restart)
                        │   8. audit.Append  (every step)      │
                        └───┬───────────┬──────────┬───────────┘
                            │           │          │
            ┌───────────────▼──┐  ┌─────▼──────┐  ┌▼───────────────┐
            │ internal/guard   │  │ policy     │  │ internal/tools │
            │ pii, secrets,    │  │ 16 rules,  │  │ 16 tools:      │
            │ injection,       │  │ deny-      │  │ 11 read_only   │
            │ grounding,       │  │ overrides, │  │  2 state_change│
            │ domain           │  │ in-process │  │  3 money_move  │
            │ 100ms budget     │  │ expr lang  │  └───┬────────────┘
            │ fail-open: read  │  └────────────┘      │
            │ fail-closed: $$  │                      │
            └──────────────────┘                      │
                                                      │
        ┌─────────────────────────────────────────────┴──────────┐
        │                                                        │
   ┌────▼──────────────┐   ┌──────────────────┐   ┌─────────────▼────┐
   │ internal/risk     │◀──│ internal/lake    │◀──│ internal/ledger  │
   │ pure function of  │   │ bronze → silver  │   │ EVM simulator    │
   │ a snapshot;       │   │ (bitemporal)     │   │ Canton simulator │
   │ exposures,        │   │ → gold           │   │ reorgs, finality │
   │ concentration,    │   │ Iceberg metadata │   └────────┬─────────┘
   │ limits, stress    │   │ 6 DQ controls    │            │
   └───────────────────┘   │ reconciliation   │   ┌────────▼─────────┐
                           └──────────────────┘   │ internal/bridge  │
                                    ▲             │ lock-and-mint,   │
                           ┌────────┴─────────┐   │ M-of-N ed25519   │
                           │ internal/stream  │   │ attestations     │
                           │ partitioned log, │   └──────────────────┘
                           │ groups, offsets  │
                           └──────────────────┘

   ┌──────────────────────────────────────────────────────────────────┐
   │ internal/audit    hash-chained, ed25519-signed, append-only      │
   │ internal/obs      one Clock, OTLP/JSON spans, Prometheus metrics │
   │ internal/identity principal + workload identity on every hop     │
   └──────────────────────────────────────────────────────────────────┘
                                    ▲
                                    │ evidence
   ┌────────────────────────────────┴─────────────────────────────────┐
   │ internal/conform   BPMN intent  ⟷  collected evidence  = verdict │
   │ 13 invariants, signed content-addressed bundle, CI gate          │
   │ internal/eval      34 scenarios, 12 categories, drift detection  │
   └──────────────────────────────────────────────────────────────────┘
```

`docs/architecture.md` walks one governed tool call end to end, the data path
from ledger event to risk figure, and the trust boundaries.

## The properties the system asserts, and where each is tested

Every row names a test that exists in this tree and passes.

| Property | Enforced in | Tested by |
|---|---|---|
| Money movement never happens without a granted approval from someone other than the requester | `agent.Runtime.step`, `policy` rule `deny-money-movement-without-recorded-approval` | `conform.AgentMoneyNeedsApproval` (AGENT-1), `internal/conform/invariants_agent_test.go` |
| A requester cannot approve their own request | `agent.ApprovalService.Grant` → `ErrSelfApproval`; policy rule `deny-self-approval` | `internal/conform/invariants_agent_test.go`; `internal/platform/smoke_test.go` |
| An approver cannot grant twice | `agent.ApprovalService.Grant` → `ErrDuplicateGrant` | `internal/conform/invariants_agent_test.go` |
| Every tool invocation is preceded by an allowing policy decision | `agent.Runtime.step` two-stage evaluate | `conform.AgentToolNeedsDecision` (AGENT-2) |
| The guardrail ran on every prompt and every output | `agent.Runtime.Start` and `finalize` | `conform.AgentGuardrailRan` (AGENT-3) |
| Every span carries the principal identity | `agent`, `obs.Tracer` | `conform.AgentSpansCarryIdentity` (AGENT-4) |
| The audit chain verifies from genesis to head | `audit.Log.Verify` | `conform.AgentAuditChainVerifies` (AGENT-5), `internal/audit/audit_test.go` |
| A policy denial is never followed by the same tool succeeding in the same run | run step records | `conform.AgentDenialNotRetried` (AGENT-7) |
| Bridge supply is conserved: nothing is created or destroyed crossing networks | `bridge.Relayer`, `bridge/supply.go` | `conform.SettleSupplyConserved` (SETTLE-1), `internal/bridge/supply_test.go` |
| No token holder sits outside the identity registry on either ledger | `ledger/evm.go` `_beforeTokenTransfer` analogue | `conform.SettleHoldersRegistered` (SETTLE-2), `internal/ledger/evm_test.go` |
| Every mint carries a quorum attestation and a matching source lock | `bridge.Relayer` | `conform.SettleMintIsAttested` (SETTLE-3), `internal/bridge/attestation_test.go` |
| No bridge nonce is minted twice | replay protection keyed on the bridge nonce | `conform.SettleNonceNotReplayed` (SETTLE-4), `internal/bridge/relayer_test.go` |
| Every gold row traces back to a bronze event | `lake` lineage | `conform.DataGoldTracesToBronze` (DATA-1) |
| Identical snapshot in, byte-identical risk figures out | `risk.Engine` | `TestDeterminism`, `TestDeterminismUnderVaryingWorkerCounts` |
| Concentration shares within one dimension sum to exactly 1 | exact rationals | `TestConcentrationSharesSumToOne` (compares to `big.NewRat(1,1)`, not a tolerance) |
| A missing FX rate is an error, never an implicit 1:1 | `risk` | `TestMissingFXRateIsAnError` |
| A stale leader is fenced by its token, not by a clock | `risk.Elector`, `EventLog.Append` | `TestFailoverFencesTheStaleLeader` |
| Every money-movement task in a process model is dominated by an approval gate on every path | `conform.Process.Validate` graph dominance | `internal/conform/bpmn_test.go` |
| A BPMN model declaring an unimplemented invariant fails the build | `ErrUnknownInvariant` | `internal/conform/bpmn_test.go` |

The full suite is 375 `Test` functions, 2 `Fuzz` targets and 7 benchmarks; a
`-v` run reports 814 test cases including subtests. All pass under `-race`.
Module-wide statement coverage measured with `-coverpkg=./...` is **74.4%**, and
the distribution is uneven in a documented way — see the phase 4 note above.

## What is real and what is simulated

Real, in the sense that the code does the thing and the tests prove it:

- The exact-decimal money arithmetic, the half-even rounding, the exact
  rationals for haircuts and FX.
- The bitemporal store's semantics. A late-arriving event splits an interval on
  the valid axis and closes rows on the transaction axis; nothing is deleted;
  an earlier `knownAt` still returns the earlier belief.
- The Iceberg *metadata* model: field ids, partition specs, snapshot parents,
  manifest lists, per-column bounds and file skipping, time travel, expiry,
  compaction.
- The policy language and evaluator, the guardrail pipeline, the approval state
  machine, the hash chain, the M-of-N attestation arithmetic, the BPMN
  dominance check, the drift statistics.

Simulated, and stated as such in the source:

- **The two ledgers are in-process simulators.** `internal/ledger/evm.go` and
  `internal/ledger/canton.go` model the rules — the whitelist, the compliance
  ceilings, the pause and freeze, signatories and observers, contract keys,
  propose-accept, selective disclosure, reorgs and finality — but there is no
  chain, no consensus, no gas and no network.
- **The bridge signers are in-process** and hold keys in memory. A real
  deployment runs them in separate failure domains with keys in separate HSMs.
  The quorum arithmetic is the same either way; the failure domains are not.
- **The lake writes gzipped NDJSON, not Parquet.** No column pruning, no page
  statistics, no dictionary or run-length encoding; a scan decodes every column
  of every row it opens. `FileFormat` is the seam Parquet goes through. See ADR
  0006.
- **Silver rewrites its whole row set on each commit** and the gold join happens
  in one process's memory. That is free at the seeded dataset's size and stops
  being free well before production volumes.
- **The Solidity in `contracts/`, the Daml in `daml/` and the Scala in `spark/`
  are not compiled, tested or run by anything in this repository.** There is no
  `solc`, no Daml SDK and no JVM in the toolchain this repository assumes, and
  adding one would cost the clean-clone property. Treat any claim about them
  beyond "this is the code" as unverified. The workflows in
  `.github/workflows/foundry.yml` and `daml.yml` exist for an environment where
  those toolchains are present and are gated so they do not run where they are
  not.
- **The BPMN models are read, not executed.** There is no process engine.
- **The default model is a deterministic planner**, not a language model.
  `agent.Planner` produces a fixed trajectory per query shape, which is what
  makes the eval suite's scores identical run to run. `AnthropicModel`
  swaps in behind the same interface when `ANTHROPIC_API_KEY` is set; every
  control behaves the same either way, and the deterministic planner exists
  precisely so that claim can be asserted rather than asserted about.
- **The audit signing key is generated per process** in `platform.New`. A
  deployment loads it from a KMS or HSM; the verification path does not change.
- **The dataset is seeded and fixed.** `domain.NewSeed` builds 8 counterparties,
  10 instruments, 19 positions, 12 obligations, 5 limits and 4 stress scenarios
  at a pinned instant, with three deliberate anomalies planted so the
  reconciliation controls have something to find.

- **The eval suite's scores are stable, not its report.** Identical verdicts and
  identical scores run to run — 15 of 34 scenarios pass, overall 0.6512 — but
  the JSON report carries a timestamp, per-run ids and wall-clock durations, so
  it is not byte-identical.

Not present at all, and not claimed: a database, a message broker outside the
in-process one, a real identity provider, TLS termination, working
authentication of any kind (`--dev-auth` trusts a header; with it off the only
credential is an assertion signed with a compile-time literal), multi-tenancy
beyond a tenant field on the principal, a standalone guardrail process, and any
deployment that has actually been applied.

## Repository layout

```
internal/money       exact decimal arithmetic; int64 minor units at 1e-6
internal/domain      the shared vocabulary; validation and rendering only
internal/obs         Clock, OTLP/JSON tracer, Prometheus registry
internal/identity    principal + workload identity, signed assertions, scopes
internal/audit       hash-chained ed25519-signed append-only log
internal/ledger      EVM and Canton simulators; reorgs, finality, disclosure
internal/bridge      lock-and-mint relayer; M-of-N ed25519 attestations
internal/stream      partitioned append-only log with consumer groups
internal/lake        bronze / bitemporal silver / gold; Iceberg metadata; DQ; recon
internal/risk        the calculation engine; exposures, limits, stress; HA
internal/guard       inline guardrail; deterministic detectors; tiered fail modes
internal/policy      authorization gateway; versioned bundle; deny-overrides
internal/policy/expr small total expression language for rule conditions
internal/tools       the 16 collateral-operations tools
internal/agent       the run loop, the model interface, approvals, the store
internal/api         HTTP surface and the governance dashboard
internal/conform     I=E: BPMN intent, evidence, verdicts, signed bundle, gate
internal/eval        scenario suite, scoring, offline judge, drift detection
internal/platform    the single constructor; assembles everything
cmd/tcrpd            the binary: demo, serve, ask, eval, conform, version

contracts/           Solidity: ERC-3643-style token, registry, compliance, vault
daml/                Daml: FundShare, TransferProposal, Pledge, FundIssuance
spark/               Scala: the batch expression of the lakehouse model
bpmn/                the two process models, annotated with controls
web/                 the dashboard, served by internal/api

docs/                architecture, threat model, runbook, SLOs, data model
docs/adr/            10 architecture decision records
docs/phases/         one page per phase
deploy/              Dockerfile (builds), compose (runs), Kubernetes manifests
migrations/          Postgres DDL for a persistence path nothing uses yet
.github/workflows/   CI, and the gated Foundry and Daml workflows
```

## The demo principals

`internal/platform/platform.go` declares them in one place so the four-eyes rule
can actually be checked: the agent acts for an operator, and the approver is
somebody else entirely.

| Name | Subject | Roles | What they can do |
|---|---|---|---|
| analyst | `r.mehta` | risk-analyst | read exposure and collateral, nothing else |
| operator | `j.okafor` | collateral-ops | propose a margin call, draft a transfer; cannot execute |
| agent | `svc/collateral-agent` | collateral-ops | the workload identity, always `on_behalf_of: j.okafor` |
| risk-manager | `s.aldridge` | risk-manager | approve; holds no execution scope at all |
| treasurer | `m.duarte` | treasurer | approve; the second approver above the dual threshold |
| auditor | `k.novak` | auditor | read the record |

The risk manager and the treasurer hold `approvals:grant` and no execution
scope. That separation is the point: an approver who could also execute is not a
second pair of eyes.

## What would change in a production deployment

Listed here rather than left implicit, because the gap between this and a
deployable system is the part a reader should be able to size.

**Persistence.** `agent.MemoryStore` writes JSON files. Production is Postgres:
`migrations/0001_agent.sql` carries the schema, and it puts the controls in the
database as constraints rather than leaving them in application code — a unique
index preventing one approver granting twice, a check preventing a requester
approving their own request, an append-only trigger on the audit table.
`migrations/0002_audit.sql` does the same for the hash chain.

**The lake.** Replace `NDJSONGzip` with a Parquet writer behind the existing
`FileFormat` interface, and point the catalog at a REST Iceberg catalog rather
than a directory. Move silver's full-set rewrite to the `MERGE INTO` in
`spark/src/main/scala/com/tcrp/lake/GoldPositions.scala`. Nothing above the file
format changes.

**The ledgers.** Replace the simulators with an indexer against a real chain and
a real Canton participant. The `domain.LedgerEvent` shape is the seam. The
reconciliation controls, the bitemporal store and the risk engine do not know
which one they are reading from.

**The bridge signers.** Separate processes, separate failure domains, keys in
separate HSMs, and a threshold-signature verifier in front of `BridgeVault.unlock`
so the chain itself checks the quorum rather than trusting the relayer.
`contracts/README.md` names that as the single most important contract change.

**Keys.** The audit signing key, the identity signer's HMAC key and the bridge
signer keys are all generated or hard-coded in-process. All three become KMS or
HSM references. The verification paths do not change.

**Authentication.** `Options.DevPrincipalHeader` accepts a principal by name for
the dashboard and the demo — `serve --dev-auth`, on by default, and `serve
--env=production` refuses to start with it on. Turning it off leaves the signed
principal assertion in `X-TCRP-Principal`, which is already the other path in
`internal/api/middleware.go`. That is not yet authentication either: the
signer's HMAC secret is the compile-time literal `tcrp-dev-signing-key` in
`platform.New`, nothing mints an assertion, and nothing rotates the key. So
`serve --env=production --dev-auth=false` today answers `/`, `/healthz`,
`/readyz` and `/metrics` and returns 401 for every `/v1` request. Production
needs an issuer, a real key from a KMS, and dual-key rotation.

**The guardrail.** Deploy it as a sidecar or a separate service rather than
in-process. `guard.Inspector` is already the seam and `guard.Client` already
implements it over HTTP; production would move that to gRPC over a unix socket.
See ADR 0003.

**Telemetry.** The tracer emits OTLP/JSON to a file or an in-memory buffer.
Production points it at a collector. The metric registry exposes Prometheus text
at `/metrics` already.

**Egress.** The only outbound call the platform makes is to the model API, and
`deploy/k8s/networkpolicy.yaml` is default-deny with that as the single
exception. A deployment should keep it that way.

**What does not change.** The policy bundle, the tool tier declarations, the
approval state machine, the audit chain format, the BPMN models, the invariant
set and the evidence bundle are all deployment-independent. That is the
intended shape: the things that make the claims are data and pure functions, and
the things that would be replaced are I/O.

## Documents

| Document | What it covers |
|---|---|
| `docs/architecture.md` | Components, the request path of one governed tool call, the data path, trust boundaries |
| `docs/threat-model.md` | Nine threats against an agent with money-movement tools, the control for each, where it lives, how it is tested, and what is out of scope |
| `docs/runbook.md` | Starting it, ports, health endpoints, every metric, and what to do when each thing goes wrong |
| `docs/slo.md` | Objectives, the measured benchmark numbers, error budgets, alert rules against the real metric names |
| `docs/evaluation.md` | The 34-scenario suite, trajectory scoring, the drift detector, how to read a report |
| `docs/data-model.md` | The vocabulary, the bitemporal model with the seed's late-arriving event worked through, the gold snapshot contract |
| `docs/adr/` | Ten decision records, each stating what was decided in this code and what lost |
| `docs/phases/` | One page per phase: what it delivers, how to demo it, which tests prove it |
| `internal/risk/README.md` | The operator's copy of the exposure formula, with the hand-computed worked example |
| `contracts/README.md`, `daml/README.md`, `spark/README.md`, `bpmn/README.md` | Each subsystem, including exactly what this repository does not build |

## Licence

MIT. See `LICENSE`.
