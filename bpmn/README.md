# Process models

These are the platform's processes, declared once, in BPMN 2.0 XML. They are
the *intent* half of I=E: `internal/conform` parses them, derives the invariants
they declare, evaluates each invariant against evidence collected from what the
system actually did, and packages the verdicts into a signed evidence bundle.

Two models ship here:

| File | Process | What it declares |
|---|---|---|
| `collateral-margin-call.bpmn` | `tcrp-collateral-margin-call` | The agent-assisted margin call: query exposure, guardrail, propose, policy gate, human approval, issue, settle, seal the audit chain. Declares `AGENT-1` … `AGENT-7`. |
| `bridge-lock-and-mint.bpmn` | `tcrp-bridge-lock-and-mint` | The cross-network transfer: authorize, lock on EVM, wait for finality, collect M-of-N attestations, mint on Canton, reconcile supply, check the registry, publish the lakehouse tables. Declares `AGENT-1`, `SETTLE-1` … `SETTLE-5`, `DATA-1`. |

Neither model is executed. There is no BPMN engine in this repository and no
plan for one: the models are read, not run. What makes them load-bearing is that
`Process.Validate` fails the build when a model declares a control the engine
cannot check, and that the sha256 of the document is carried into every evidence
bundle, so a verdict is a statement about one exact model text.

## Annotation syntax

Controls are carried on `<bpmn:textAnnotation>` elements joined to a flow
element by a `<bpmn:association>`. Each line of the annotation body is either
prose (kept, ignored) or a directive beginning with `@`.

```
@invariant <ID> <statement in words>
@control   <control-name>
@evidence  <source>:<selector>
@tier      read_only | state_change | money_movement
```

- **`@invariant`** names an invariant id and restates it in words. The id has to
  resolve to a registered implementation in `internal/conform`; one that does
  not is `ErrUnknownInvariant` and fails the build. The statement is not
  optional — an id with no sentence behind it is a control nobody can review.
- **`@control`** names the control the element implements. `four-eyes`,
  `dual-approval` and `human-approval` are the three the parser interprets: a
  `userTask` carrying one of them is an approval gate for the reachability
  check below. Every other control name is recorded and reported but not
  interpreted, which means a mis-typed control name makes the check *stricter*,
  never weaker.
- **`@evidence`** names where the control's evidence comes from. The source must
  be one of `audit`, `span`, `ledger`, `bridge`, `lake`, `policy`; anything else
  is a hard error, because it means the model points at evidence nothing
  collects. The selector after the colon is free text — it documents the join
  for a human and the engine does not resolve it.
- **`@tier`** declares the risk tier of the work the element does, using the
  agent runtime's three tiers. `money_movement` is the one with teeth: it is
  what the approval-dominance check is written against.

An unknown directive is a hard error. So is an `@evidence` line without a
colon, an `@invariant` without an id or without a statement, and a `@tier` that
is not one of the three.

Associations may run in either direction — `sourceRef` the element and
`targetRef` the annotation, or the reverse. BPMN tools disagree about this and
the parser accepts both, taking whichever end names a `textAnnotation` as the
annotation.

## The approval reachability check

`Process.Validate` proves that **every element declared `@tier money_movement`
is dominated by an approval gate**: on every path from a start event to that
element, an approval `userTask` comes first.

It is not a search for an approval somewhere upstream. It is the standard
dominance test run as a graph walk: delete every approval gate from the flow
graph and breadth-first search from the start events. Anything still reachable
has a path that never passed through an approval, and the walk's predecessor
map is the witness — so the error names the path (`Start_1 -> Gateway_1 ->
Task_Pay`) rather than only the task. A single unguarded branch out of a
gateway fails the check even when the other branch is approved, and a loop back
into a money task from downstream does not, because a cycle cannot manufacture a
path that avoids a gate the straight-line path went through.

## The subset the parser understands

This is deliberately a small reader, not a BPMN implementation. It understands:

- `<bpmn:definitions>` in the namespace `http://www.omg.org/spec/BPMN/20100524/MODEL`,
  containing **exactly one** `<bpmn:process>`;
- the flow elements `startEvent`, `endEvent`, `task`, `userTask`, `serviceTask`,
  `exclusiveGateway`, `parallelGateway`, `intermediateCatchEvent`;
- `sequenceFlow` (by `sourceRef`/`targetRef` attributes);
- `textAnnotation` and `association`.

It does **not** understand, and will either ignore or reject:

- **Gateway semantics.** An `exclusiveGateway` and a `parallelGateway` are both
  read as plain branch points. Nothing evaluates a condition expression, and
  `conditionExpression` elements are ignored. The reachability check is
  therefore conservative in the right direction: it treats every outgoing flow
  as takeable, so a branch that could never fire in practice still has to be
  guarded.
- **`<bpmn:incoming>` / `<bpmn:outgoing>` child elements.** They are ignored.
  Sequence flows are the single source of truth for the graph, so a model whose
  `incoming` lists disagree with its flows is read from the flows.
- **Sub-processes, call activities, boundary events, event sub-processes,
  message and signal flows, lanes and pools, data objects, and BPMN DI
  (diagram) elements.** Any of these that appear are silently skipped, except
  that an element referenced by a sequence flow but not recognised is a hard
  error — a flow into something the parser cannot see is a hole in the graph
  and the dominance check would be unsound over it.
- **Multiple processes, collaborations and choreographies.** A document with
  more than one process is rejected rather than having one picked for it.
- **Execution.** No listeners, no expressions, no forms, no engine extensions
  (`camunda:`, `zeebe:`, `flowable:` namespaces and the rest) are read.

Every element in the process must be reachable from a start event. An annotated
element that nothing can reach is a control that never runs, and it fails
validation precisely because it looks fine on the diagram.
