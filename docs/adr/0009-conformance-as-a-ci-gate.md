# ADR 0009 — Conformance as a CI gate, and why inconclusive fails it

## Status

Accepted. In force. `internal/conform/engine.go`, `bpmn/`.

## Context

A platform can say three different things about a control, usually conflated:
**we designed it** (a document), **the code implements it** (a unit test), and
**the system did it** (evidence). The third is the only one an auditor can use,
and nobody produces it, because doing so needs a machine-readable statement of
intent, a mechanism for collecting evidence, and something that compares them.

The second is weaker than it looks. A unit test over `Runtime.step` can be
satisfied by code that is never reached — a branch, a flag, an alternative entry
point. The claim is "no money moved without an approval", and only the audit log
can make it.

The motivating gap is one an operator will recognise. Telemetry stops arriving;
the invariant reading the spans finds nothing. If "found nothing" is a pass, the
build goes green **because** the monitoring broke — silent, safe-looking, and
exactly when it matters.

## Decision

**Intent equals evidence, and the comparison is a CI gate.**

**Intent is declared once, as annotated BPMN 2.0.** Controls ride on
`textAnnotation` elements joined by `association`, using four directives —
`@invariant`, `@control`, `@evidence`, `@tier` — and every one is a hard error
when malformed. An `@invariant` id no registered implementation answers to is
`ErrUnknownInvariant` and fails the build: a model declaring a control nothing
checks is worse than one declaring nothing, because it reads as assurance. An
unrecognised `@control` name is recorded but **not interpreted**, which makes
the approval check stricter rather than weaker — a typo cannot accidentally mark
something as a gate.

**`Process.Validate` proves approval dominance by graph dominance.** Every
element declared `@tier money_movement` must be dominated by an approval gate on
every path from a start event. Not a search for an approval upstream: delete
every approval `userTask` from the flow graph and breadth-first search from the
start events; anything still reachable has a path that never passed a gate, and
the walk's predecessor map is the **witness**, so the error names the path
(`Start_1 -> Gateway_1 -> Task_Pay`) rather than only the task. Gateway
conditions are never evaluated, so the check is conservative in the right
direction.

**Thirteen invariants** — seven agent, five settlement, one data — each
returning `pass`, `fail` or `inconclusive`, a severity, and the records it read.

**The gate.** `Report.Gate` fails when any critical invariant **failed**, and
also when any critical invariant was **inconclusive**.

## Consequences

**The asymmetry is the most load-bearing decision in the package.** A failing
critical invariant says a control did not hold. An inconclusive one says the
engine could not tell — the audit log was empty, the spans never arrived, a
handle was nil. Two different facts about the world, the same fact about the
release: in neither case does anybody have evidence the control held.
`StatusInconclusive` is a first-class result rather than a shade of pass, and it
is what "I could not find the records I need" returns everywhere.

**The critical set is deliberately small**, because a gate that blocks on
everything gets bypassed. Critical: AGENT-1 (money needs approval), AGENT-2
(every call has an allowing decision), AGENT-5 (the audit chain verifies),
AGENT-7 (a denial is not retried into a success). High: AGENT-3, AGENT-4,
AGENT-6. Only critical blocks.

**The report is portable evidence.** `conform.Bundle` is signed and
content-addressed: its id is the sha256 of the canonical encoding of everything
but the id and signature, so two runs over the same evidence produce the same id
and any edit produces a different one. It carries the verdicts, the sha256 of
the BPMN document, an artefact manifest of every input with its digest, the
policy bundle digest in force, and the audit chain head. `VerifyBundle` needs a
file, a public key and this package. `ErrBundleTampered` and
`ErrBundleSignature` are separate sentinels because "this was edited" and "this
was signed by a key I do not trust" are two different conversations.

**The engine is pure.** It stamps `CheckedAt` from its own clock rather than
letting an invariant read one, and has no side effect on the system it judges.
Same evidence in, byte-identical verdicts and bundle digest out.

**Cost.** Every new control needs three things: the implementation, an invariant
that can find evidence of it, and an annotation on the model. Forgetting any is
a build failure rather than a control nobody notices is missing.

**What is there now.** `tcrpd conform --process bpmn/collateral-margin-call.bpmn
--gate --bundle evidence.json` is the gate, and `.github/workflows/ci.yml` runs
it — once per BPMN model — alongside the package's 80 tests. The two are not the
same coverage: the tests exercise the mechanism against constructed evidence,
including failing and inconclusive cases a real platform would not produce, and
the gate exercises the mechanism against a real platform's evidence. Neither
subsumes the other.

**What is still not there.** Nothing binds the ed25519 key the bundle is signed
with to an identity: `tcrpd conform` generates a keypair per invocation and
discards it, so a bundle's signature proves the bundle has not been edited since
it was written and proves nothing about who wrote it.

## Alternatives considered

**Unit tests only.** They prove the code *can* behave correctly, not that it
did. A branch never taken passes every test over it.

**Runtime assertions.** The same component asserting about itself, producing no
artefact anybody outside the process can check.

**Treat inconclusive as a pass, alert separately on evidence gaps.** The obvious
compromise and the one most systems make. It splits the signal: the release
decision and the monitoring alert reach different people at different times, and
the release ships. Blocking is the only version of "we have no evidence" that
changes an outcome.

**Treat inconclusive as failing at every severity.** Too brittle. A gate that
fires on everything acquires a `--force` flag.

**Write the intent in Go.** BPMN exists so a process owner draws it and a
compliance reviewer reads it. Intent in the implementation language is intent
only implementers can review, and the model's sha256 in the bundle is what makes
a verdict a statement about one document a non-engineer approved.

**Execute the BPMN.** The models are read, not run. What makes them load-bearing
is that `Validate` fails the build when a model declares a control the engine
cannot check, and that the digest travels into every bundle.
