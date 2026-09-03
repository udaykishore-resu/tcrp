# ADR 0010 — Human-in-the-loop as a persisted state machine

## Status

Accepted. In force. `internal/agent/approval.go`, `internal/agent/run.go`.

## Context

A money-movement call pauses for a human. The obvious implementations model that
pause in memory — a channel the goroutine blocks on, a callback the approval UI
invokes, a context that is cancelled or not.

Every one assumes the process that started the run finishes it, and that is
wrong in the case that matters. The approver is in a meeting; the default window
is fifteen minutes and a real one is longer. In that time the pod is
rescheduled, a deploy rolls, an instance is autoscaled away. A blocked goroutine
survives none of it, and what the approver sees when they click approve is a
5xx — after which the options are to lose the run or to re-run it, and re-running
means re-proposing a movement a human has just authorised once and would now
authorise twice.

Second: a pending approval that exists only as a goroutine cannot be listed.
"What is waiting on me" is a query, and it must be answerable by a process that
started none of the runs.

## Decision

**An approval is a first-class persisted object**, and the run is checkpointed
alongside it. `agent.Approval` carries the run id, step, tool, tier, summary, the
exact args and their `ArgsHash`, the requester and `OnBehalfOf`, the required
roles and count, the grants and denials, the status, and the created, expiry and
decided times.

When a policy decision carries an approval obligation, the runtime writes a
`PendingCall` onto the run — approval id, tool call id, tool, tier, exact args,
decision, summary — sets the status to `awaiting_approval`, saves, and returns.
**Nothing about the call executes.** No goroutine blocks. The process can exit.
`Runtime.Resume(ctx, runID)` reloads from the store and continues; the whole
state needed is on disk.

**The four-eyes checks live in the service, not in callers.**
`ApprovalService.Grant` refuses, in order, with distinct sentinels:
`ErrApprovalClosed`, `ErrApprovalExpired` (marking it expired as it does),
`ErrSelfApproval` (the requester or the human they act for), `ErrApproverScope`,
`ErrApproverRole`, `ErrDuplicateGrant`. Each refusal writes an
`approval.denied` audit record naming the reason, so a rejected attempt is
evidence rather than a 403 nobody sees.

**`RequiredCount` comes from the policy obligation, not from code.**
`require_human_approval` is one, `require_dual_approval` two, attached by the
bundle above 25,000,000 units.

**Resume is synchronous with the grant.** `OnResume` fires inside `Grant`, so
the approver's request does not return until the run has moved. That makes the
audit ordering unambiguous: `approval.granted` is immediately followed by
`agent.tool.invoked` with no window in which a reader must infer causality from
timestamps.

**The dispatch-stage policy evaluation happens after the resume**, with the
approval attached; the propose-time allow is not carried forward. ADR 0002.

## Consequences

**Runs survive restarts.** `agent.MemoryStore` mirrors every write to disk as
JSON, atomically by write-to-temp-and-rename, and rehydrates on construction. An
approver responding forty minutes later, after a deploy, resumes the same run
against the same arguments.

**"What is waiting on me" is a query.** `ApprovalService.Pending` and
`GET /v1/approvals`; any process can answer it, including one that started none
of the runs.

**The args are pinned.** `ArgsHash` is the canonical JSON at request time,
recorded on the approval and on every audit record about it. What the approver
saw is what executes: the approval authorises *that* transfer, not "a transfer".

**The controls exist at three layers, deliberately.** Self-approval is refused
by the service, again by `deny-self-approval` at dispatch, and again by a
constraint in `migrations/0001_agent.sql`. Duplicate grants are refused by the
service and by a unique index on `(approval_id, approver)`, which additionally
closes the race the service's read-then-write check cannot. The failure defended
against is one person authorising their own money movement.

**Expiry is a state, not a timeout.** `ExpireDue` marks due approvals expired,
writes `approval.expired` and fires the resume callback; the run records a
denied step and continues. It does not hang and does not silently execute.
`Grant` also checks expiry on arrival, so a grant racing the sweep fails closed.

**Cost.** Every transition is a store write on the approval path. That is the
price of durability, paid on a path already waiting on a human.

**A gap.** `internal/agent` has no `_test.go` files. The state machine is
exercised by `internal/platform/smoke_test.go`, by the eval suite's
`money_movement` and `approval_bypass` scenarios, and by the conformance
invariants reading the resulting audit records — real coverage through other
packages, not unit tests.

## Alternatives considered

**Block a goroutine on a channel.** Simplest, and loses the run on any restart —
at the moment a human approved it, which is the worst time to lose one.

**A callback into the running process.** Same problem plus a worse one: the
approver's request must reach *the instance* holding the run, needing sticky
routing, and failing in a way that looks like a network error rather than a lost
approval.

**A workflow engine (Temporal).** The right tool for durable execution and a
well-known shape for this. Rejected on ADR 0001 and partly on fit: the run
record must exist and be queryable for audit reasons regardless, so making it
the resume point costs nothing extra.

**Store the approval, keep the run in memory.** The worst of both: resume must
reconstruct the run from the approval, re-running the model and possibly getting
a different trajectory for a call a human already authorised.

**Let callers enforce four-eyes.** A control implemented twice is a control
implemented once, incorrectly, eventually.

**Make `RequiredCount` a constant.** The dual threshold is a policy question and
belongs in the bundle, where it can change and be digested into an audit record,
not in a Go constant requiring a deploy.
