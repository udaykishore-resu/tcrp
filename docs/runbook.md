# Runbook

Operating the platform. Every metric name, route and flag in this document was
taken from the source or from a run of the binary; nothing here is invented.
Where a procedure depends on something that does not exist yet — a standalone
guardrail, a database, real authentication — that is stated at the point it
matters rather than at the end.

## What exists, and what does not

`cmd/tcrpd` is the binary and it builds. Every subcommand assembles the object
graph through `internal/platform`, and `serve` turns that platform into an
`http.Handler` with `internal/api.NewServer`.

Three things this runbook refers to do **not** exist, and each is flagged where
it matters rather than only here:

- **No standalone guardrail process.** `guard.Handler` serves
  `POST /v1/inspect`, `GET /v1/healthz` and `GET /v1/config`, and
  `serve --guard-url` points the platform at it — but no binary serves it.
  `tcrpd` has no `guard` subcommand and there is no `cmd/guardd`. Port 8081
  below describes a workload that is not running.
- **No database.** `migrations/` is a real schema; no Go file in this repository
  imports `database/sql`. The run and approval store is JSON under `--dir` and
  the audit log is a JSONL file beside it.
- **No working authentication.** See "Authentication" below before putting this
  anywhere.

## Starting it

```sh
tcrpd serve \
  --addr        :8080 \
  --dir         /var/lib/tcrp \
  --env         production \
  --dev-auth=false \
  --policy      /etc/tcrp/policy/collateral-ops.json \
  --model       planner \
  --log         info \
  --trace-file  /var/log/tcrp/spans.jsonl
```

It prints the listen banner on stderr and the structured log on stdout:

```
  dashboard  http://localhost:8080/
  api        http://localhost:8080/v1/
  metrics    http://localhost:8080/metrics
```

The flags, and the `platform.Options` field each sets:

| Flag | Default | Field | Notes |
|---|---|---|---|
| `--addr` | `:8080` | — | Listen address, passed to `api.Options`. |
| `--dir` | temp dir | `Dir` | Lake tables, run and approval JSON, the audit log. The default is a temporary directory removed with the process — never in production. |
| `--env` | `development` | `Environment` | A policy input, on every decision. `deny-experimental-tools-in-production` reads it. |
| `--policy` | embedded | `PolicyPath` | Overrides the bundle compiled in with `go:embed`. A path that does not exist is a startup error. |
| `--guard-url` | empty | `GuardURL` | Routes inspection to a standalone guardrail. Empty runs it in-process, which is the only thing that works today. |
| `--model` | `planner` | `Model` | `planner` (deterministic, sends nothing anywhere) or `anthropic`. |
| `--model-id` | empty | `ModelID` | Names the model under `--model=anthropic`; falls back to `TCRP_MODEL_ID`, then `claude-sonnet-4-5`. |
| `--log` | `info` | — | `debug`, `info`, `warn`, `error`. |
| `--trace-file` | empty | `TraceFile` | One JSON object per finished span, appended. |
| `--dev-auth` | `true` | — | Accept the `X-TCRP-As` development principal header. |

There is no `--deterministic` flag on `serve`. `platform.Options.Deterministic`
exists and `ask` and `conform` set it; `serve` never does. It pins the clock to
the seed instant, which among other things degrades the API rate limiter to a
fixed budget per principal rather than a rate — the token bucket refills from
the platform clock and a pinned clock never refills it.

**Startup will fail, deliberately, if:** `--env=production` is combined with
`--dev-auth=true` (`serve` returns an error before building anything); the
policy bundle does not compile (`policy.MustDefault` panics — a binary that
cannot compile its own policy must not start); `--policy` names a file that is
not there; any of the tool registry, model, guardrail inspector, policy engine,
audit log or store is nil (`agent.New` returns an error, because a runtime
missing one of those is not degraded, it is ungoverned); or the state directory
cannot be created.

## The other subcommands

| Command | Use |
|---|---|
| `tcrpd demo` | Thirteen acts end to end with real numbers, including measured guardrail and policy latency percentiles. Exits non-zero if anything fails. The cheapest smoke test there is. |
| `tcrpd ask "<q>" --as agent --approve-as risk-manager` | One governed turn from the terminal, on the same runtime `serve` uses. `--approve-as` still cannot grant as the requester — four-eyes lives in the approval service, not the caller. |
| `tcrpd eval --out report.json --markdown report.md` | The 34-scenario suite. `--strict` defaults to **true** and exits 1 on any failing scenario; pass `--strict=false` for a report. |
| `tcrpd conform --process bpmn/collateral-margin-call.bpmn --gate --bundle evidence.json` | The CI gate. Exits non-zero on a critical invariant that failed *or* one that could not be evaluated. |
| `tcrpd version` | Build stamps. A subcommand, not a flag — `--version` exits 2. |

`--as` and `--approve-as` take `agent`, `analyst`, `operator`, `risk-manager`,
`treasurer` or `auditor`.

## Authentication

Read this before exposing the process to anything.

`--dev-auth=true`, the default, reads the acting principal from the `X-TCRP-As`
header **with nothing to prove it**. Any caller can be the treasurer.
`--env=production` refuses to start with it on, which is the right refusal.

`--dev-auth=false` leaves one credential: a signed assertion in
`X-TCRP-Principal`, verified by `identity.Signer`. Its HMAC secret is the
compile-time literal `tcrp-dev-signing-key` in `internal/platform/platform.go` —
no flag, environment variable or Secret overrides it — and no endpoint mints an
assertion. In practice `serve --env=production --dev-auth=false` answers `/`,
`/healthz`, `/readyz` and `/metrics`, and returns 401 for every `/v1` request.

The 401 body is the standard error envelope:

```json
{"error":{"code":"unauthenticated","message":"identity: no principal in context","request_id":"req_..."}}
```

## Ports

| Port | Service | Exposed |
|---|---|---|
| 8080 | Platform HTTP: `/v1`, `/metrics`, `/healthz`, `/readyz`, `/` (dashboard) | Yes, behind the ingress |
| 8081 | Standalone guardrail: `/v1/inspect`, `/v1/healthz`, `/v1/config`. **Nothing serves this today** — no binary implements it. | No — cluster-internal only |

There is no database port. `migrations/` is not applied by anything.

`/metrics`, `/healthz` and `/readyz` sit **outside `/v1` and outside
authentication**, by design: a probe that needs a credential fails for the wrong
reason, and a scrape endpoint that needs one does not get scraped. So does `/`,
the dashboard. They must not be exposed through the ingress — `/metrics` carries
`tcrp_risk_open_breaches` and `tcrp_lake_recon_breaks_open`, which describe the
institution's positions.

## Endpoints

The complete route table, from `internal/api/api.go`. Everything under `/v1`
requires a principal; the four above it do not.

| Route | What it does |
|---|---|
| `GET /` | The governance dashboard, served from `web/`. |
| `GET /healthz` | Liveness. |
| `GET /readyz` | Readiness. |
| `GET /metrics` | Prometheus text. |
| `GET /v1/version` | Build stamps, environment, model, policy bundle and digest, principal names, tool names. |
| `POST /v1/runs` | Start a governed run. Body `{"query": "..."}`. |
| `GET /v1/runs` | List runs. |
| `GET /v1/runs/{id}` | One run, with every step. |
| `GET /v1/runs/{id}/trace` | The run's spans. |
| `GET /v1/runs/{id}/audit` | The audit records the run produced. |
| `GET /v1/approvals` | Open and closed approval gates. |
| `GET /v1/approvals/{id}` | One gate. |
| `POST /v1/approvals/{id}/grant` | Grant. Refused for the requester. |
| `POST /v1/approvals/{id}/deny` | Deny. |
| `GET /v1/exposure` | Exposure for every counterparty. |
| `GET /v1/exposure/{counterparty}` | One counterparty. **Broken today** — see below. |
| `GET /v1/collateral/{counterparty}` | One counterparty's pledge. **Broken today** — see below. |
| `GET /v1/concentration` | Concentration by counterparty and issuer. |
| `GET /v1/breaches` | Open limit breaches. |
| `POST /v1/stress/{scenario}` | Run a stress scenario. **The path segment is ignored** — see below. |
| `GET /v1/snapshot` | The curated gold snapshot. |
| `GET /v1/breaks` | Reconciliation breaks. |
| `POST /v1/breaks/{id}/acknowledge` | Acknowledge a break. |
| `POST /v1/breaks/{id}/resolve` | Resolve a break. |
| `GET /v1/supply` | Supply across both ledgers. |
| `GET /v1/policy` | The installed bundle. |
| `POST /v1/policy/evaluate` | Evaluate a decision without taking it. |
| `GET /v1/audit` | The audit log. |
| `GET /v1/audit/verify` | Verify the chain. Returns `{"ok":…,"head":…,"records":…,"public_key":…}`. |
| `GET /v1/conformance` | The conformance report, live. |

### Three route defects to know about

These are in `internal/api` and are not configuration problems.

- `GET /v1/exposure/{counterparty}` and `GET /v1/collateral/{counterparty}`
  return `400 counterparty is required` for any path. The route patterns bind
  `{counterparty}`; the handlers read `r.PathValue("cp")`
  (`internal/api/risk.go`), which is always empty. Use `GET /v1/exposure`, which
  returns every counterparty, until the names agree.
- `POST /v1/stress/{scenario}` ignores the path segment. `handleStress` reads
  `scenario` from the JSON body and defaults to `rate_shock` when the body is
  empty, so `POST /v1/stress/haircut_widening` with no body runs `rate_shock`.
  Send `{"scenario":"haircut_widening"}` in the body.

## Health endpoints

### `GET /healthz` — liveness

```json
{"now":"2026-09-03T06:12:56.86225116Z","status":"ok","uptime":"1.989386541s","version":"dev"}
```

Asserts that the process is running and can serve. It checks nothing else, which
is correct for a liveness probe: a liveness probe that fails on a dependency
restarts the pod for something a restart cannot fix.

**If it fails:** the process is wedged. Restart. `deploy/k8s/deployment.yaml`
sets `failureThreshold: 3` at a 10-second period, so Kubernetes does it.

### `GET /readyz` — readiness

Readiness is a stronger claim than liveness and it is checked rather than
asserted. Two checks:

- `policy` — a bundle is installed (`Policy.Bundle() != nil`).
- `lake` — the curated snapshot is buildable.
- `runtime` — the agent runtime is assembled.

Returns the per-check detail either way:

```json
{"checks":{"lake":"ok","policy":"ok","runtime":"ok"},"ready":true}
```
 A process that is running but cannot
render a decision should not be sent traffic.

**If `policy` fails:** the bundle did not load. Check the `--policy` path and the
mounted ConfigMap. The process should not have started; if it did, that is a bug.

**If `lake` fails:** the lake cannot build a snapshot. Check `--dir` for disk
space and permissions, then the `dq.*` control verdicts on the last successful
snapshot. Do not restart first — a restart loses nothing but tells you nothing.

### `GET /v1/version`

The first thing to capture in an incident. Unlike the three probes it is under
`/v1`, so it needs a principal.

```json
{"version":"dev","commit":"none","built_at":"unknown","environment":"development",
 "model":"planner/deterministic-v1","policy_bundle":"2026.09.1",
 "policy_digest":"a8d124eb…","deterministic":false,
 "principals":["analyst","operator","agent","risk-manager","treasurer","auditor"],
 "tools":["check_whitelist","get_collateral_value", … 16 in all]}
```

`version`, `commit` and `built_at` are the `-ldflags -X` stamps from the build;
they read `dev`/`none`/`unknown` for a plain `go build`.

## Metrics

Every name below is registered in the source. `docs/slo.md` carries the alert
expressions.

**Registration is lazy.** A series appears in `/metrics` only once its code path
has run. A freshly started `serve` exposes 38 of the names below; after one
agent run and one reconciliation query it exposes 42. The `tcrp_lake_*`,
`tcrp_bridge_supply_*` and `tcrp_stream_*` series appear when those pipelines
first run, which `serve` alone does not do. An alert on a series that has never
been emitted does not fire — that is a property of the exporter, not a missing
metric.

### Agent runtime — `internal/agent/runtime.go`

| Metric | Type | Means |
|---|---|---|
| `tcrp_agent_runs_total` | counter | Runs started. |
| `tcrp_agent_runs_completed_total` | counter | Runs that produced an answer. |
| `tcrp_agent_runs_blocked_total` | counter | Runs stopped by the guardrail — at the prompt or at the output. |
| `tcrp_agent_tool_denials_total` | counter | Tool calls refused by the policy gateway. |
| `tcrp_agent_approvals_requested_total` | counter | Human approval gates opened. |
| `tcrp_agent_tool_calls_total` | counter | Tool invocations that reached a tool. |
| `tcrp_agent_run_steps` | histogram | Steps per run. Buckets 1,2,3,4,5,6,8,10,12,20. |
| `tcrp_agent_run_seconds` | histogram | Wall-clock duration of a run. Buckets 0.01 … 30. |

A rising `runs_blocked_total` is either an attack or a guardrail regression, and
the two are distinguishable: check whether the blocking findings are on the
prompt surface (attack) or the output surface (regression, usually grounding).

`tool_denials_total` rising without `runs_total` rising means the same principals
are being refused more — a scope change, a policy change, or a counterparty that
has left the whitelist. `runs_total` and `tool_denials_total` rising together is
traffic.

### Guardrail — `internal/guard/pipeline.go`

| Metric | Type | Means |
|---|---|---|
| `tcrp_guard_inspections_total` | counter | Inspections performed, all three surfaces. |
| `tcrp_guard_blocks_total` | counter | Turns blocked. |
| `tcrp_guard_redactions_total` | counter | Turns redacted rather than blocked. |
| `tcrp_guard_budget_exceeded_total` | counter | Inspections that exceeded the 100ms budget. |
| `tcrp_guard_inspection_seconds` | histogram | End-to-end inspection latency. |
| `tcrp_guard_stage_seconds` | histogram | Per-detector latency, labelled by stage. |

### Policy — `internal/policy/policy.go`

| Metric | Type | Means |
|---|---|---|
| `tcrp_policy_decisions_total` | counter | Decisions rendered. |
| `tcrp_policy_denies_total` | counter | Decisions that denied. |
| `tcrp_policy_evaluation_seconds` | histogram | Evaluation latency. |
| `tcrp_policy_rule_errors_total` | counter | **Rule conditions that failed to evaluate.** |

`tcrp_policy_rule_errors_total` should be zero forever. A non-zero value means a
rule condition did not produce a boolean — a bad bundle edit, or an input shape
the rule did not expect. The rule contributed nothing to the decision, so the
gateway is running with fewer rules than it thinks. **Page on any increase.**

### Risk — `internal/risk/metrics.go`

| Metric | Type | Means |
|---|---|---|
| `tcrp_risk_calculations_total` | counter | Completed engine calls, labelled by operation. |
| `tcrp_risk_calculation_errors_total` | counter | Engine calls that returned an error. |
| `tcrp_risk_calculation_seconds` | histogram | Latency, labelled `op`. |
| `tcrp_risk_stress_scenario_seconds` | histogram | Latency, labelled `scenario`. |
| `tcrp_risk_open_breaches` | gauge | Limit breaches the register currently holds open. |
| `tcrp_risk_breaches_raised_total` | counter | **First** detections only. |
| `tcrp_risk_recomputes_total` | counter | Event-driven recompute cycles. |
| `tcrp_risk_recompute_lag_seconds` | histogram | Newest observed event → recompute completion. |

`breaches_raised_total` counts first detections, so a flapping limit is
distinguishable from a limit that has been broken once and left open. If
`open_breaches` is high and `breaches_raised_total` is flat, nothing new is
wrong — somebody has not acknowledged the old ones.

### Lake — `internal/lake/`

| Metric | Type | Means |
|---|---|---|
| `tcrp_lake_bronze_events_total` | counter | Events landed. |
| `tcrp_lake_bronze_duplicates_total` | counter | Events suppressed by the natural key. |
| `tcrp_lake_silver_rows_total` | counter | Bitemporal position versions written. |
| `tcrp_lake_silver_corrections_total` | counter | Versions that superseded a published belief. |
| `tcrp_lake_gold_positions` | gauge | Positions in the most recent curated snapshot. |
| `tcrp_lake_pipeline_records_total` | counter | Records processed by the ingest pipeline. |
| `tcrp_lake_pipeline_lag` | gauge | Records between the pipeline and the end of its partitions. |
| `tcrp_lake_recon_breaks_total` | counter | Breaks detected, labelled `kind`. |
| `tcrp_lake_recon_breaks_open` | gauge | Breaks awaiting investigation. |

`silver_corrections_total` rising is the signal that data is arriving late. A few
is normal. A sustained rise means an indexer is behind, and every risk figure cut
during that period is a figure that will be restated.

### Ledger — `internal/ledger/ledger.go`, labelled by ledger

`tcrp_ledger_transactions_total`, `tcrp_ledger_reverts_total`,
`tcrp_ledger_reorgs_total`, `tcrp_ledger_orphaned_transactions_total`,
`tcrp_ledger_head_block` (gauge), `tcrp_ledger_finalised_block` (gauge).

`head_block − finalised_block` is the finality lag. The bridge will not mint
until a lock is final, so a growing gap shows up as pending bridge flows before
it shows up anywhere else.

### Bridge — `internal/bridge/metrics.go`, labelled by direction

| Metric | Type | Means |
|---|---|---|
| `tcrp_bridge_triggers_observed_total` | counter | Trigger events seen for the first time. |
| `tcrp_bridge_settlements_total` | counter | Completed flows. |
| `tcrp_bridge_abandoned_total` | counter | Flows abandoned after a **pre-finality** reorg. |
| `tcrp_bridge_alerts_total` | counter | Alerts raised. |
| `tcrp_bridge_submission_retries_total` | counter | Target submission retries. |
| `tcrp_bridge_pending` | gauge | Flows waiting on finality, quorum or retry. |
| `tcrp_bridge_quorum_signatures` | histogram | Signatures per settlement. Buckets 1,2,3,4,5,7,9,11. |
| `tcrp_bridge_settlement_seconds` | histogram | Observation → response. |
| `tcrp_bridge_supply_delta` | gauge | **Locked+minted vs burned+unlocked. Must be zero.** |
| `tcrp_bridge_supply_breaks` | gauge | Open supply breaks. |

`tcrp_bridge_quorum_signatures` is the early-warning metric nobody thinks to
watch: a quorum degrading from 5-of-7 to 3-of-7 still settles, and the histogram
shifting left is the only sign before it stops settling entirely.

`tcrp_bridge_supply_delta != 0` is a page at any hour. It means units exist on
one network that are not backed on the other.

### Stream — `internal/stream/`

`tcrp_stream_records_produced_total`, `tcrp_stream_records_consumed_total`,
`tcrp_stream_duplicates_suppressed_total`, `tcrp_stream_consumer_lag` (gauge).

### HTTP — `internal/api/middleware.go`

`tcrp_http_requests_total` (labels `route`, `status` as a class),
`tcrp_http_request_seconds` (label `route`), `tcrp_http_requests_in_flight`
(gauge).

---

## Procedures

### The guardrail budget is being exceeded

**Symptom.** `tcrp_guard_budget_exceeded_total` rising;
`tcrp_guard_inspection_seconds` p99 near or above 0.1.

**What is happening.** The 100ms pipeline budget or the 30ms per-stage budget is
being exhausted. The consequence is **not uniform**, and this is the thing to
establish first:

- `read_only` turns **fail open**, flagged. Answers are going out uninspected.
- `state_change` and `money_movement` turns **fail closed**. Those calls are
  being blocked.

So the same metric means "we have lost coverage" for reads and "we have an
availability incident" for writes.

**Triage.**

1. Which stage? `tcrp_guard_stage_seconds` is labelled by stage. In practice it
   is `injection` (scoring is the most work) or `grounding` (proportional to the
   number of grounded strings a long run has accumulated).
2. Which surface? A slow `output` surface on long runs points at grounding; a
   slow `prompt` surface points at input size or the injection scorer.
3. Is a classifier installed? `Engine.SetClassifier` adds a stage that runs last
   inside whatever budget remains, and its unavailability should not change a
   verdict — but a classifier that is slow rather than absent eats the budget the
   deterministic stages needed. Remove it first.
4. Check input sizes. `tcrp.guard.bytes` is a span attribute on
   `guard.inspect`.

**Actions, in order.**

- If a classifier is installed, remove it. It is by design the disposable stage.
- If the guardrail is remote (`--guard-url`), check whether the *service* is slow
  or the *network* is. `guard.Client`'s timeout is the budget plus 50ms, and a
  client-side timeout is a different failure from a server-side budget
  exhaustion — the client's own `FailOpen` map applies, which defaults to
  read-only only.
- Scale the guardrail horizontally if it is standalone. It is stateless.
- Raise `Config.Budget` only as a deliberate, recorded decision. It is the user's
  latency and it is the control's coverage. Do not do this during an incident to
  make an alert stop.

**Do not** disable the guardrail. `AGENT-3` — the guardrail ran on every prompt
and every output — will go inconclusive, and a critical inconclusive fails the
conformance gate, so the next release is blocked anyway. Fix the cause.

### A reconciliation break opens

**Symptom.** `tcrp_lake_recon_breaks_open` rises, or
`tcrp_lake_recon_breaks_total{kind=...}` increments.

**Read the kind first.** The kinds are not equally urgent:

| Kind | Means | Urgency |
|---|---|---|
| `missing_in_book` | On chain, not in the book of record. | **Highest.** Value moved and the institution's books do not know. |
| `missing_on_chain` | In the book, not on chain. | High. A settlement instruction did not execute. |
| `quantity_mismatch` | Both sides have it, at different sizes. | High. A partial fill, or a bug. |
| `stale_observation` | The observation lag exceeded the threshold. | Medium. Usually an indexer, not a break. |
| Position sweep breaks | A position difference **no instruction explains**. | **Highest.** A difference with a cause is an operations task; the same difference with no cause is an incident. |

**Procedure.**

1. `GET /v1/breaks`. Break ids are deterministic hashes of what the break is
   about, so the same break re-detected every five minutes is the same id and an
   acknowledgement made between runs survives.
2. Pull the underlying event. Every break names a counterparty, a symbol, a
   ledger and the two quantities.
3. **Check the transaction axis before escalating.** A late arrival looks
   exactly like a break at `knownAt = now` and is not one. Query
   `GET /v1/snapshot?valid_at=…&known_at=…` at a later `known_at` — if the
   break resolves, it was observation lag, and `tcrp_lake_silver_corrections_total`
   should show the correction.
4. If it is real: `POST /v1/breaks/{id}/acknowledge` with a note, so the next
   run does not re-page. Acknowledging is not resolving.
5. Resolve with `POST /v1/breaks/{id}/resolve` only once the underlying
   difference is gone, not once it is understood.

**Note for this repository specifically.** The seeded dataset plants two
permanent breaks — `missing_in_book` for CP-DELTA on `evm` (1,750,000) and
`quantity_mismatch` for CP-BRAVO on `canton` (book 1,000,000 against chain
900,000). They are supposed to be there. A demo showing zero breaks means the
reconciler is not running.

### The conformance gate fails

**Symptom.** The CI gate job fails. `tcrpd` exits 1 with
`tcrpd: conform: conformance gate failed: AGENT-1 inconclusive, AGENT-2 inconclusive, AGENT-5 inconclusive, AGENT-7 inconclusive`
(that exact message is what `--exercise=false` produces)
— the invariant ids and their statuses are in the message.

**Reproduce it locally.** The gate job runs exactly this, and it takes a couple
of seconds:

```sh
go run ./cmd/tcrpd conform \
  --process bpmn/collateral-margin-call.bpmn \
  --gate \
  --bundle evidence.json
```

Drop `--gate` to read the full report without the non-zero exit. Add
`--markdown` for the report CI puts in the step summary. Download the
`evidence-bundle` artefact from the failed run to see the evidence the gate
actually had, rather than the evidence a local run happens to produce.

`--exercise` defaults to true and is what puts evidence in the audit log for
the invariants to read; `--exercise=false` makes almost everything inconclusive,
which is a useful way to see what inconclusive looks like and is never what you
want in CI.

**First: distinguish `fail` from `inconclusive`.** They look the same in the
gate's error and they are completely different problems.

- **`fail`** — a control did not hold. Something the platform claims is untrue.
  Do not merge, do not override.
- **`inconclusive`** — the engine could not tell. The audit log was empty, the
  spans never arrived, a handle was nil. Absence of evidence is not evidence of
  conformance, so it blocks (ADR 0009) — but the fix is usually in the
  evidence-collection path, not in the control.

**By invariant.**

| Invariant | If it fails | If it is inconclusive |
|---|---|---|
| `AGENT-1` money needs approval | Stop. Money moved without a second human. Pull the run id from the verdict's refs and read the audit chain around it. | No money-movement records in the evidence window. Usually the run set was read-only. |
| `AGENT-2` every tool call has an allowing decision | A tool executed without an authorization record. Either the runtime was bypassed or the audit write failed. | No tool invocations in the window. |
| `AGENT-3` guardrail ran | The guardrail was skipped on a prompt or an output. | Guard verdict records missing — check the audit writer. |
| `AGENT-4` spans carry identity | A span is missing the principal attributes. Usually a new span created without `SetAttrs`. | Spans never arrived; check the exporter and `--trace-file`. |
| `AGENT-5` audit chain verifies | **The chain is broken.** See below. | The log was empty. |
| `AGENT-7` denial not retried | A denied tool later succeeded in the same run. A real control failure. | No denials in the window. |
| `SETTLE-1` supply conserved | Units exist that are not backed. Page. | Bridge handle nil or no bridge activity. |
| `SETTLE-4` no nonce minted twice | A double mint. Page. | No mints in the window. |
| `DATA-1` gold traces to bronze | A gold row with no bronze event behind it. A lineage bug. | Lake handle nil. |

**If `AGENT-5` fails — the audit chain is broken.** `audit.Log.Verify` returns
the **first sequence number at which it breaks**. Everything before that
sequence is intact and remains admissible; everything after it is suspect. Do
not truncate, do not rewrite, do not restart to "clear" it. Take a copy of the
file, record the sequence number, and treat it as an integrity incident. The
most common benign cause is a partially written final record after an unclean
shutdown, which shows as a break at the last sequence.

**Do not override the gate.** The one legitimate reason to see a critical
inconclusive is that the evidence window genuinely contained no activity of that
kind, and the fix is to make CI exercise it — not to reclassify the invariant.

### An approval expires

**Symptom.** `approval.expired` in the audit log; a run recording a denied step
with reason "the approval window closed before anyone responded".

**What happens automatically.** `ApprovalService.ExpireDue` marks the approval
expired, writes the audit record, and fires the resume callback. `Runtime.Resume`
then appends a denied step, sets the run back to `running`, increments the
failure count, and lets the loop continue. **The run does not hang and the call
does not execute.** That is the designed behaviour and no operator action is
required.

**What to do anyway.**

1. The action was not taken. If it was a margin call, the desk still needs to
   issue it. The run's answer says so; it does not silently succeed.
2. Restarting the run re-proposes the call and opens a **new** approval with a
   new id. It does not reuse the expired one, and it cannot — `Grant` refuses a
   non-pending approval with `ErrApprovalClosed`.
3. If expiries are recurring, the window is wrong for the desk's actual response
   time. `Config.ApprovalWindow` defaults to 15 minutes and the policy bundle's
   `deny-stale-approval` independently refuses at 900 seconds. **Change both or
   neither** — raising the config window alone means an approval is granted and
   then refused at dispatch as stale, which is a confusing failure.
4. If a single approver is the bottleneck, that is approval fatigue forming. See
   `docs/threat-model.md` §4.

### Failover

The risk engine is the component with a leader, because a recompute cycle that
runs twice writes two breach detections. Everything else is stateless.

**The mechanism** (`internal/risk/ha.go`). `Elector` over a `LeaseStore`, with
in-memory and file-backed implementations sharing one rule set so they cannot
diverge. A `Lease` carries a holder, an **expiry**, and a **monotonically
increasing fencing token**. `EventLog.Append` rejects any write carrying a token
below the highest it has accepted, returning `ErrFenced`.

The token is the point. A lease alone cannot make a paused leader safe: it can
wake up believing it still holds a lease that has in fact expired and been
taken. Correctness comes from the token, not from clocks agreeing across nodes.

**Planned failover.**

1. On the standby, `Acquire` blocks until the lease is free or expired.
2. On the leader, `Release`. The standby acquires with a strictly greater token.
3. The standby resumes at the offset it had reached; it does not replay the log.

**Unplanned failover.** Do nothing. The lease expires and the standby acquires.
If the old leader returns, its writes are refused with `ErrFenced` — it does not
need to know it was paused.

**What to check afterwards:**

- `tcrp_risk_recompute_lag_seconds` — did the standby catch up.
- `tcrp_risk_open_breaches` — should be identical across the transition. The
  register is in-memory and is rebuilt by recomputing; the audit log is the
  durable record.
- The audit chain across the transition. Both nodes append to their own log; a
  deployment must have them appending to one store, and
  `migrations/0002_audit.sql` is where that becomes a database sequence rather
  than a file.

**Do not** run two leaders "briefly" to smooth a transition. Two leaders is two
sets of breach detections and two sets of `risk.limit.breached` audit records for
one condition, which is exactly what the fencing token exists to prevent.

`TestFailoverFencesTheStaleLeader` runs the pause-lose-lease-wake sequence
against both store implementations and asserts the stale leader is fenced and
the standby resumes at its offset rather than replaying.

---

## Things that are not procedures, and should be known

**A denial is not an incident.** `tcrp_agent_tool_denials_total` incrementing
means the gateway worked. The eval suite counts a refused forbidden tool
separately from an executed one for exactly this reason, and only the second
fails anything.

**Two reconciliation breaks are permanent in the seeded dataset.** See above.

**A deterministic platform's rate limiter is not a rate limiter.** With
`platform.Options.Deterministic` the clock is pinned, so the token bucket never
refills and degrades to a fixed budget per principal. `Options.RatePerMinute`
documents it. `serve` never sets it — there is no `--deterministic` flag — and
`ask` and `conform` do.

**The audit signing key is generated per process.** Records signed by one process
cannot be verified with another process's key until the deployment loads the key
from a KMS or HSM. `platform.New` says so at the call site. The bridge signer
keys are generated the same way.

**There is no `tcrpd healthcheck`.** The image has no shell and no `curl`, so
neither `deploy/Dockerfile` nor `deploy/docker-compose.yml` declares a
healthcheck. Kubernetes probes `/healthz` and `/readyz` over HTTP with
`httpGet`, which needs nothing inside the container. Probe from outside, or use
the `:debug` variant of the distroless tag for one incident.
