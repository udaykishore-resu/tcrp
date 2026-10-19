# Contributing

## Running the gate locally

Everything below runs from a clean clone with a Go 1.24 or newer toolchain (CI uses the latest 1.26 patch release) and no
network. If any of it needs anything else, that is the bug.

```sh
go build ./...
go vet ./...
gofmt -l .              # must print nothing
go test ./... -count=1
go test ./... -count=1 -race
```

The benchmarks are in `internal/risk`:

```sh
go test ./internal/risk/... -run XXX -bench . -benchtime 200ms
```

The end-to-end path is `TestSmokeRun`. Read its output before and after a change
that touches the runtime, the policy bundle or the tool set — it prints every
step, the rule that decided it, and the approval that gated it:

```sh
go test ./internal/platform/ -run TestSmokeRun -v -count=1
```

The conformance gate is `conform.Report.Gate`, and `tcrpd conform` is how you
run it. This is the command CI runs, and it is worth running before you open a
pull request:

```sh
go run ./cmd/tcrpd conform \
  --process bpmn/collateral-margin-call.bpmn \
  --gate \
  --bundle evidence.json
```

It exercises a representative workload, checks the invariants against what the
platform actually did, writes a signed evidence bundle, and exits non-zero when
a critical invariant failed *or* could not be evaluated. Drop `--gate` to read
the report without the non-zero exit. `make gate` is the same thing.

CI also runs `internal/conform`'s own tests — `engine_test.go` and
`invariants_*_test.go` — which cover the mechanism against constructed evidence
including failing cases the real platform would never produce. Neither subsumes
the other.

## The invariants a change must not break

These are not style preferences. Each one is asserted by a test or a conformance
invariant, and a change that breaks one is a change to what the platform claims,
not a refactor.

**1. No third-party dependencies.** `go.mod` has no `require` block and must
keep none. If a change needs a library, the change is wrong or the library needs
to be written. ADR 0001 records what that costs.

**2. The clean-clone property.** `go build ./...` and `go test ./...` must
succeed with no network, no Docker, no database and no toolchain other than Go.
A test that reaches the network is a broken test.

**3. Nothing on a decision path reads the wall clock.** Every engine takes an
`obs.Clock`. A direct `time.Now()` in a package that produces a figure, a
decision, an audit record or a span makes replay impossible and makes a
deterministic test flaky. `time.Timer` for scheduling is not reading a clock;
`internal/risk/recompute.go` documents the one place that distinction matters.

**4. The ordering: guardrail → policy → approval → audit.** That order is the
security property, not an implementation detail. See ADR 0002. `agent.Runtime`
evaluates policy twice — once at propose, once at dispatch — and the dispatch
evaluation is what carries the approval. Removing either evaluation, or moving
the guardrail after the policy gate, breaks AGENT-1, AGENT-2 and AGENT-3.

**5. Money is never a `float64`.** `money.Amount` is an int64 count of 1e-6
units of a named currency. Haircuts, FX rates, stress shocks and concentration
shares are `money.Ratio`, exact rationals. The single rounding is half-even and
it happens inside `internal/money`. `float64` appears in `internal/risk` only in
`format.go` for display and in metric values. See ADR 0007.

**6. Amounts in different currencies do not add.** A missing FX rate is an
error, never an implicit 1:1. `TestMissingFXRateIsAnError`.

**7. Identical input produces byte-identical output.** Every map is sorted
before it is iterated; every worker pool writes into an indexed slot rather than
appending; the fan-out width has no effect on the answer. `TestDeterminism` and
`TestDeterminismUnderVaryingWorkerCounts`.

**8. The audit chain is append-only and verifies end to end.** Nothing rewrites
a record. `audit.Log.Verify` reports the first sequence number at which the
chain breaks, and AGENT-5 runs it.

**9. A requester cannot approve their own request, and an approver cannot grant
twice.** Enforced in `agent.ApprovalService.Grant`, restated as policy rules
`deny-self-approval` and `deny-approver-without-authority`, and restated again
as database constraints in `migrations/0001_agent.sql`. All three layers, not
one.

**10. A tool's tier, scopes and reversibility are data, not documentation.** The
policy gateway authorizes against them and the conformance engine checks them.
Adding a tool means declaring them; getting them wrong means the gateway
authorizes the wrong thing.

**11. Every invariant a BPMN model declares must have a registered
implementation.** `Process.Validate` fails otherwise, and it fails at parse time
rather than producing a report with a hole in it. Adding an `@invariant`
annotation without adding the invariant is a build failure, deliberately.

**12. Inconclusive is not a pass.** An invariant that cannot find its evidence
returns `StatusInconclusive`, and a critical inconclusive fails the gate. Do not
add a fallback that reports a pass when evidence is missing. See ADR 0009.

**13. A refusal is reported, never routed around.** `AgentDenialNotRetried`
(AGENT-7) fails if a policy denial is followed by the same tool succeeding in
the same run.

## Changing the policy bundle

`internal/policy/bundles/collateral-ops.json` is embedded at build time and
`policy.MustDefault` panics if it does not compile — a binary that cannot
compile its own policy must not start. When changing it:

- Bump `version`. It is recorded on every decision and every audit record, and a
  changed bundle with an unchanged version makes the record ambiguous.
- Keep `default_effect: deny`.
- A new obligation type must be added to `policy.KnownObligations` and handled
  by the runtime. An unknown obligation is a hard failure rather than a no-op,
  precisely so that adding one cannot silently weaken enforcement.
- Run the eval suite. Several scenarios in `internal/eval/suite.json` assert on
  a specific rule id in the denial reason.

## Changing a BPMN model

`bpmn/README.md` documents the annotation syntax and the subset of BPMN the
parser understands. The two things to remember: the sha256 of the document is
carried into every evidence bundle, so a verdict is a statement about one exact
model text; and `Process.Validate` proves approval dominance by graph walk, so a
new branch out of a gateway that reaches a `@tier money_movement` element
without passing an approval `userTask` fails the build and the error names the
path.

## Style

Match the package comments already in the tree. `internal/policy/policy.go`,
`internal/lake/lake.go` and `internal/risk/doc.go` are the reference. Plain,
declarative, first person plural avoided, no marketing adjectives. A package
comment states what the package is for, what it decided, and what it
deliberately does not do — the last one is not optional. Where something is
simulated or scoped down, say so at the top of the file rather than leaving a
reader to discover it.

Exported identifiers get doc comments. Sentinel errors are values, not strings,
because callers distinguish them. Constants are named where a conformance
engine, a dashboard or a test has to refer to the same thing.

## What is out of scope for a pull request

- Adding a dependency.
- Adding a build step that is not `go build`.
- Anything that makes a test require the network.
- Loosening a control "temporarily".
