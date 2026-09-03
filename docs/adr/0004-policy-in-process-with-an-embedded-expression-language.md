# ADR 0004 — Policy in-process, over a small embedded expression language

## Status

Accepted. In force. `internal/policy/policy.go`, `internal/policy/expr/expr.go`.

## Context

Every tool call passes an authorization decision. The agent does not decide what
it may do; it proposes and the gateway decides. That happens twice per governed
step — propose and dispatch — so a four-call run evaluates eight times. It is on
the hot path.

The decision must also be **evidence**. "The model was denied" is not something
an auditor can act on; "rule `deny-money-movement-without-recorded-approval`
matched, at bundle version 2026.09.1 with digest …, against policy input hash …"
is. So the gateway returns the rules considered and how each evaluated, and can
reproduce a decision from a recorded input.

The default answer is OPA. ADR 0001 rules out the library; the sidecar is still
available and worth taking seriously.

## Decision

Evaluate **in-process**, over a versioned JSON bundle, using a small **total**
expression language.

**The bundle** (`internal/policy/bundles/collateral-ops.json`, embedded via
`go:embed`) has a package, a version, `default_effect: deny`, and 16 rules —
twelve deny, four allow. Each has an id, description, effect, priority, `when`
condition, reason, optional stages and optional obligations. Evaluation is
**deny-overrides**: any matching deny wins regardless of priority.

**The language** (`internal/policy/expr`) is a recursive-descent parser and tree
evaluator over boolean, comparison and membership operators, field and index
access, list literals, and four built-ins the rules actually need — `size`,
`subset`, `intersects`, `startsWith`. No loops, no user-defined functions, no
unbounded recursion; every expression terminates in time proportional to its own
size. A condition evaluating to a non-boolean is a policy authoring error and is
reported as such rather than coerced, because a silently truthy policy is worse
than a broken one.

**Obligations are checked, not advisory.** An allow can carry
`require_human_approval`, `require_dual_approval`, `redact_pii`, `notify`,
`rate_limit` or `simulate_first`. A type outside `policy.KnownObligations` is a
hard failure at load, so adding an obligation can never silently weaken
enforcement.

**A binary that cannot compile its own policy must not start.**
`policy.MustDefault` panics; CI compiles the embedded bundle and any override
before either is installed.

## Consequences

**Latency and availability.** Evaluation is a tree walk over a compiled program
with no I/O. There is no sidecar to be unreachable, so there is no "policy is
down, do we fail open" question to answer badly. `policy.Decision.Duration`
reaches every audit record as `latency_us`, so the real cost is measurable in
production. This repository ships no policy benchmark, so no latency figure is
stated here.

**The decision is evidence.** `RuleID`, `Reason`, `BundleVersion`,
`BundleDigest`, `InputHash` and the obligations all reach the audit record, and
`POST /v1/policy/evaluate` lets an operator ask what the bundle says about an
input without starting a run.

**What was given up against Rego.** Expressiveness — Rego has comprehensions,
partial rules, `some`/`every`, set arithmetic; several rules that would be one
Rego expression are two here. Partial evaluation. Tooling: `opa test`, coverage,
`--explain`, the playground, the editor extension. Distribution: bundle servers,
signed bundles, discovery, hot reload — here the bundle is embedded or loaded
from `--policy`, and changing it is a deploy. And hiring: someone who knows Rego
knows Rego; nobody knows this.

**What was gained:** a rule set a reviewer reads in full (150 lines), a decision
path with no network on it, a language that provably cannot hang the desk, and
an evaluator whose semantics — deny-overrides, default deny, unknown obligation
fatal — fit in a paragraph.

**The rules.** Denies: no authenticated principal; a guardrail block; a missing
scope; a state change on untrusted retrieved context; money movement at dispatch
with no recorded approval; an approval past the 900 s window; self-approval; an
approver without an authorising role; a transfer outside the identity registry;
a movement above 25,000,000 units on a single approval; experimental tools in
production; a run past its step or failure budget. Allows: read-only,
state-change proposals, single-approved and dual-approved money movement, each
with its obligations.

## Alternatives considered

**OPA as a sidecar.** Reintroduces the availability coupling the in-process
decision removes, and the answer to "unreachable" is either fail-closed (the
desk stops for an unrelated reason) or fail-open (the central claim becomes
conditional). ADR 0003 takes the opposite view for the *guardrail* on purpose:
its fail-open case is survivable for read-only traffic and the gateway's is not.

**OPA as a linked library.** The best technical answer and what a production
deployment should take. Rejected only on ADR 0001.

**CEL (`cel-go`).** Very close to what was written — total, fast, built for
this. Rejected on ADR 0001; if the rule were lifted, CEL rather than this
package is the replacement, and `expr.Program` is a small enough surface that
the swap is contained.

**Hard-code the rules in Go.** A rule set that is code cannot be versioned
independently of the binary, digested into an audit record as data, or shown to
a reviewer who does not read Go. The `BundleDigest` is the thing being bought.

**A DSL with loops.** A policy on the hot path of a trading system must be
total. A rule that can loop is a rule that can hang the desk.
