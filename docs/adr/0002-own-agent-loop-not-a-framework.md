# ADR 0002 — Write the agent loop, do not adopt a framework

## Status

Accepted. In force. `internal/agent/runtime.go`.

## Context

The loop itself is small: send state to a model, receive a tool call or a final
answer, execute, append, repeat until a budget is exhausted. Every framework
ships it.

What they do not agree on is where the controls go. Most treat authorization,
approval and audit as middleware or callbacks a user registers around the loop.
That is right for a general-purpose framework, which cannot know what the
controls are. It is wrong here, because on a collateral desk **the order of the
controls is the security property, and a registration API cannot enforce an
order.**

Concretely: if the guardrail runs after the policy gateway, an injected
instruction is authorized before anything inspects it. If policy is evaluated
once at propose time and carried through a forty-minute approval pause, the call
executes under a bundle nobody currently holds. If the audit record is written
before the tool returns, it says an action happened that may not have. Each is
plausible with a callback API and none is a bug the framework would report.

## Decision

Write the loop. Make the ordering explicit, in one function, with the
collaborators as required constructor arguments rather than optional
registrations.

The order is the same on every step for every tier:

1. **Guardrail on the prompt**, before the model sees anything. The threshold
   tier is the highest the *principal* could reach, because nobody yet knows
   what will be asked for.
2. **Model turn**, bounded by `StepTimeout`.
3. **Policy at propose stage.** A denial ends the step, records the rule id, and
   returns the refusal to the model as a tool error — the model is told, not
   silently given nothing.
4. **Approval, if the decision carries one.** The run is checkpointed to the
   store; nothing executes. ADR 0010.
5. **Policy again at dispatch stage**, with the approval attached. The
   propose-time allow is *not* carried forward. This runs for every tier, so the
   two-stage invariant is uniform.
6. **Tool invocation.**
7. **Guardrail on the result**, as retrieved context. Instruction-like text sets
   `run.RetrievalTrusted = false`, and `deny-state-change-on-untrusted-context`
   then refuses to let that run drive a state change.
8. **Audit**, after the fact, with args, tier, rule id, policy input hash,
   bundle version, and the approval id, approvers and roles where there were
   any.

`agent.New` errors when the tool registry, model, guardrail inspector, policy
engine, audit log or store is nil. A runtime missing one is not degraded, it is
ungoverned, so construction fails rather than defaulting.

## Consequences

**The ordering is checked against the evidence, not the source.**
`internal/conform` evaluates AGENT-1 (no money movement without a granted
approval from someone other than the requester), AGENT-2 (every invocation
preceded by an allowing decision for that tool), AGENT-3 (the guardrail ran on
every prompt and output) and AGENT-7 (a denial is never followed by the same
tool succeeding) against the audit log and the spans. That distinction matters: a
test reading the source can be satisfied by code that never executes.

**Re-evaluating at dispatch is not redundant.** It is what makes a policy change
during an approval window take effect, and what lets
`deny-money-movement-without-recorded-approval` exist as a rule — one that fires
only at dispatch and refuses money movement with no recorded approval. The
runtime would not reach dispatch without one; the gateway refuses anyway.

**The loop is ours to maintain.** Retries, the 12-step budget, the 3-failure
budget, the 30-second step timeout, unknown-tool handling and the resume path
are all hand-written, with no upstream fixing them.

**No ecosystem.** No integrations, no tool catalogue, no vendor dashboard that
already understands the traces. `internal/eval/openinference.go` projects the
spans into the OpenInference shape because nothing else produces it.

**The model is a plug-in at one interface.** `agent.Model` has one method;
`agent.Planner` is deterministic and `agent.AnthropicModel` calls the API. Every
control behaves identically either way, and the deterministic planner exists so
that claim is asserted in a test rather than in a document.

## Alternatives considered

**LangGraph or an equivalent graph runtime.** The strongest option: explicit
nodes suit a governed run and interrupt-and-resume is close to what the approval
gate needs. Rejected on ADR 0001, and on the ordering argument — a graph makes
the topology explicit and still leaves control ordering to whatever the author
wired. `conform.Process.Validate` exists because a graph that looks correct on a
diagram while a branch skips the gate is a real failure mode.

**Middleware hooks.** A registration API cannot enforce an order.

**Policy and guardrail as decorators around the tool.** Wrong level. A decorator
sees the call; it does not see the run, the prior failures, the retrieval-trust
flag, or that the same tool was already denied twice. `policy.Input` carries all
of that, and only can if the gateway is called by the loop.

**Evaluate once, carry the decision through the approval.** Forty minutes is
long enough for a bundle to change, a scope to be revoked, or a market to move
past a threshold.
