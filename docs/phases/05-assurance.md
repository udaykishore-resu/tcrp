# Phase 5 — Assurance

`internal/conform`, `internal/eval`, plus `bpmn/`.

5,626 lines of Go source, 3,810 of test. 286 lines of BPMN.

## What this phase delivers

The difference between "we designed a control", "the code implements it" and
"the system did it" — and machinery that produces the third.

### I=E: intent equals evidence

A process is declared **once**, as an annotated BPMN 2.0 model. The invariants
the model declares are evaluated against evidence collected from what the system
actually did — the hash-chained audit log, the OpenTelemetry spans, both ledgers,
the bridge's supply reconciliation, the lakehouse — and each evaluation yields a
verdict citing the specific records it read.

The claim this lets the platform make is not "we designed a control". It is:
**here is the control, here is what the system did, and here is the
machine-checked statement that the two agree.**

Two models ship:

| Model | Declares |
|---|---|
| `collateral-margin-call.bpmn` | The agent-assisted margin call: query exposure, guardrail, propose, policy gate, human approval, issue, settle, seal the chain. `AGENT-1` … `AGENT-7`. |
| `bridge-lock-and-mint.bpmn` | The cross-network transfer: authorize, lock on EVM, wait for finality, collect M-of-N attestations, mint on Canton, reconcile supply, check the registry, publish the tables. `AGENT-1`, `SETTLE-1` … `SETTLE-5`, `DATA-1`. |

Controls are carried on `textAnnotation` elements joined by `association`, using
four directives — `@invariant`, `@control`, `@evidence`, `@tier` — and **every
one of them is a hard error when malformed**. An `@invariant` id no registered
implementation answers to is `ErrUnknownInvariant` and fails the build, because
an intent with no evidence path is the exact failure this engine exists to catch,
and a model that declares a control nothing checks is worse than a model that
declares nothing, because it reads as assurance. An unrecognised `@control` name
is recorded but not interpreted, which makes the approval check **stricter**
rather than weaker — a typo cannot accidentally mark something as a gate.

### Approval dominance, proved by graph dominance

`Process.Validate` proves that **every element declared `@tier money_movement`
is dominated by an approval gate**: on every path from a start event to that
element, an approval `userTask` comes first.

It is not a search for an approval somewhere upstream. Delete every approval gate
from the flow graph and breadth-first search from the start events; anything
still reachable has a path that never passed through a gate, and the walk's
predecessor map is the **witness**, so the error names the path
(`Start_1 -> Gateway_1 -> Task_Pay`) rather than only the task.

A single unguarded branch out of a gateway fails the check even when the other
branch is approved. A loop back into a money task from downstream does not,
because a cycle cannot manufacture a path that avoids a gate the straight-line
path went through. Gateway conditions are never evaluated — every outgoing flow
is treated as takeable — so the check is conservative in the right direction.

### Thirteen invariants

Seven agent, five settlement, one data. Each returns `pass`, `fail` or
`inconclusive`, a severity, and the records it read.

### The gate, and the asymmetry

`Report.Gate` fails when any critical invariant **failed** and also when any
critical invariant was **inconclusive**.

> A failing critical invariant says a control did not hold. An inconclusive one
> says the engine could not tell — the audit log was empty, the spans never
> arrived, the ledger handle was nil. Those two are different facts about the
> world but they are the same fact about the release: in neither case does
> anybody have evidence that the control held.

Treating inconclusive as a pass means the build goes green **because** the
telemetry stopped, which is the one outcome an assurance system must not
produce. ADR 0009.

### The evidence bundle

`conform.Bundle` is signed and content-addressed: its id is the sha256 of the
canonical encoding of everything except the id and the signature, so two runs
over the same evidence produce the same id and any edit produces a different one.

It carries the verdicts, the **sha256 of the BPMN document** they are a statement
about, an artefact manifest of every input with its digest, the policy bundle
digest in force, and the audit chain head at the moment of the run.
`VerifyBundle` needs a file, a public key, and this package — none of the
platform. `ErrBundleTampered` and `ErrBundleSignature` are separate sentinels
because "this bundle was edited" and "this was signed by a key I do not trust"
are two different conversations, and an auditor should not have to read a string
to tell them apart.

The whole package is pure: nothing reads the wall clock, nothing has a side
effect on the system it is judging. Same evidence in, byte-identical verdicts and
bundle digest out.

### The evaluation harness

34 scenarios across 12 categories, deterministic scoring, an offline judge by
default. Trajectory (normalised Levenshtein), tool F1, answer grade, error
recovery and efficiency, weighted 0.30 / 0.25 / 0.20 / 0.15 / 0.10 — as constants,
because a benchmark whose weighting can be tuned per run is a benchmark whose
number cannot be compared across runs.

Three hard controls sit outside the weighted score and zero it:
`ForbiddenCalled`, `PolicyViolations`, and an unsatisfied `MustPause`.

**`RefusedAttempts` is scored separately and never fails the suite.** The model
reaching for a tool it may not have is what an attack looks like from inside the
loop, and the claim is that the reach is refused, not that it never happens.
Counting a refusal as a failure would reward a timid model over a correct
gateway, and would make the number worse every time the suite added a harder
adversarial prompt.

### The drift detector

Eight features — tool mix, steps per run, latency, answer length, and the
guardrail-trip / policy-denial / approval / refusal **rates** (counted per run,
not per event) — compared against a stored baseline using PSI at the conventional
0.10 and 0.25 cutoffs, plus a two-sample Kolmogorov-Smirnov test at 5% on the
continuous features. `no_data` is a first-class status, and every span-attribute
read has a zero-valued fallback so a renamed attribute is reported as an absence
rather than as a shift.

`docs/evaluation.md` covers the harness in full.

## Demonstrating it

```sh
go test ./internal/conform/... ./internal/eval/... -count=1 -v
```

Approval dominance, including the failure cases:

```sh
go test ./internal/conform/ -run 'Parse|Validate|Reachability|Association|UserTask' -count=1 -v
```

- `TestValidateRejectsAnUnreachableElement` — a control that never runs looks
  fine on the diagram, and fails validation precisely for that reason.
- `TestValidateRejectsAnInvariantNothingImplements` — `ErrUnknownInvariant`.
- `TestReachabilityRejectsAnUnapprovedPathToMoney` — `ErrUnapprovedPath`,
  **naming the path**.
- `TestReachabilityIsNotFooledByALoopBackPastTheGate`.
- `TestAUserTaskWithoutAnApprovalControlIsNotAGate`.
- `TestAssociationDirectionDoesNotMatter`.

The gate's asymmetry:

```sh
go test ./internal/conform/ -run 'TestGate|Inconclusive' -count=1 -v
```

- `TestGateFailsOnACriticalFailure`
- `TestGateFailsOnACriticalInconclusive`
- `TestANonCriticalInconclusiveDoesNotStopTheGate`
- `TestRunReportsAnUnimplementedInvariantRatherThanSkippingIt`

The evidence bundle's tamper-evidence:

```sh
go test ./internal/conform/ -run 'TestBundle|TestVerify|Tampered' -count=1 -v
```

- `TestBundleIsDeterministic` and `TestDifferentEvidenceProducesADifferentBundle`
  — the content address holds in both directions.
- `TestVerifyDetectsAnEditedVerdict`, `…AnEditedTally`, `…AnEditedAuditHead`.
- `TestVerifyRejectsTheWrongKey` and `TestVerifyRefusesToVouchWithoutAKey` — a
  third party has to say which key they trust; falling back to the key the
  bundle names would verify every bundle against itself.

The eval harness against the governed platform:

```sh
go test ./internal/eval/ -count=1 -v
```

The same two things through the binary. These are the commands
`.github/workflows/ci.yml` runs:

```sh
go run ./cmd/tcrpd conform \
  --process bpmn/collateral-margin-call.bpmn \
  --gate \
  --bundle evidence.json

go run ./cmd/tcrpd eval --strict=false --out eval-report.json --markdown eval-report.md
```

`--process` is singular and takes the last value given, so the second BPMN model
is a second invocation and not a repeated flag. `conform` exits non-zero under
`--gate` when a critical invariant failed *or* was inconclusive. `eval` exits
non-zero by default on any failing scenario, which is why the CI job passes
`--strict=false` — the suite is a report there, and the hard controls it checks
are gated by `conform` instead.

## The tests that prove it

| Package | Tests | What they establish |
|---|---:|---|
| `internal/conform` | 80 | The BPMN subset parses and everything outside it is rejected rather than ignored; associations in either direction; every element must be reachable from a start event; an unknown invariant is a hard error; approval dominance including the unguarded-branch and loop cases, with the path named; each of the thirteen invariants against constructed evidence, in all three outcomes; the gate blocks on critical fail **and** critical inconclusive; the bundle id is content-addressed and reproducible; `ErrBundleTampered` and `ErrBundleSignature` are distinguished; `VerifyBundle` refuses with no key |
| `internal/eval` | 53 | Trajectory Levenshtein and tool F1; a forbidden call zeroes the score and a refusal does not; policy-violation detection; error-recovery scoring; the rubric judge's soft and hard misses; grounding; scenario validation; per-scenario isolation; panic recovery; PSI, KS, `KSCritical`, quantile edges and bucketing; `no_data`; the OpenInference projection |

## What this phase deliberately does not do

- **The BPMN models are read, not executed.** There is no process engine in this
  repository and no plan for one. What makes the models load-bearing is that
  `Validate` fails the build when a model declares a control the engine cannot
  check, and that the document's sha256 is carried into every bundle so a verdict
  is a statement about one exact model text.
- **The parser is a small reader, not a BPMN implementation.** Seven flow element
  types, sequence flows, text annotations and associations. Gateways are plain
  branch points; no condition is evaluated; `conditionExpression` is ignored.
  Sub-processes, boundary events, lanes, pools, data objects, DI elements and
  every engine extension namespace are skipped — **except** that an element
  referenced by a sequence flow but not recognised is a hard error, because a
  flow into something the parser cannot see is a hole in the graph and the
  dominance check would be unsound over it.
- **There is no gate binary.** CI runs the package's tests, which is coverage of
  the mechanism and is not the mechanism running against a real platform's
  evidence.
- **The eval suite is thin on adversarial coverage.** Three direct-injection
  scenarios, two indirect, two approval-bypass. A real adversarial suite is
  hundreds, and nothing here generates new attacks — every scenario was written
  by hand.
- **The indirect-injection scenarios use synthetic poisoning.** The tools that
  would realistically carry attacker-controlled text read from the seeded
  dataset, which has no adversarial content.
- **The deterministic planner bounds what the suite can find.** It measures
  whether the platform refuses what it should, which is the stated goal, and it
  cannot measure whether a capable model finds a route nobody anticipated.
  Running against `--model anthropic` tests that and cannot gate a build,
  because those runs are not deterministic.
- **`Scenario.Weight` exists and every scenario uses the default.** The mechanism
  is there and is not being used to prioritise the controls that matter most.

## Why this phase is the point

Phases 1 through 4 build a system that behaves correctly. This phase produces the
artefact that says so to somebody who was not there, cannot read the source, and
has no reason to trust the operator: a signed, content-addressed bundle stating
which controls were declared, what evidence was collected, and whether they
agree — verifiable with a file, a public key, and one package.
