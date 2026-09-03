# Service level objectives

Every metric name in this document is registered somewhere in the source. Every
number presented as a measurement was produced by a command that is written next
to it. Where an objective is stated against a configured budget rather than
against a measurement, that is said explicitly — an SLO backed by a number
nobody measured is a number that will be wrong in the direction that flatters
the system.

## Measurement environment

All figures below were produced on:

```
goos: linux, goarch: amd64
cpu:  Intel(R) Xeon(R) Processor @ 2.80GHz
2 vCPU
go1.24.7
```

Two vCPUs is a small machine. Treat the absolute numbers as a floor and the
shape — how they scale with counterparty count — as the real information.

---

## What has been measured

### The risk engine

```sh
go test ./internal/risk/... -run XXX -bench . -benchtime 200ms -count=3
```

Median of three runs, `ns/op` converted:

| Benchmark | Latency | Allocated | Allocs |
|---|---:|---:|---:|
| `ExposureSingle` (one counterparty) | **6.52 µs** | 4.1 kB | 49 |
| `ExposureAll/counterparties=8` | **44.8 µs** | 25.4 kB | 200 |
| `ExposureAll/counterparties=64` | **306.8 µs** | 197.6 kB | 1,495 |
| `ExposureAll/counterparties=256` | **2,070 µs** | 788.8 kB | 5,936 |
| `Stress/counterparties=8` | **84.8 µs** | 49.8 kB | 479 |
| `Stress/counterparties=64` | **545.9 µs** | 385.7 kB | 3,600 |
| `Stress/counterparties=256` | **3,774 µs** | 1,538 kB | 14,306 |
| `StressAll` (4 scenarios, 8 counterparties) | **235.5 µs** | 195.7 kB | 1,766 |
| `CheckLimits` | **133.0 µs** | 69.7 kB | 542 |
| `FormatExposure` | **6.95 µs** | 3.7 kB | 80 |
| `Share` (one exact rational share) | **76.3 ns** | 0 B | 0 |

These figures were not re-measured when `cmd/tcrpd` landed, and a re-run of
three of them on a differently loaded two-vCPU box did not reproduce them
exactly: `ExposureSingle` came out at 8.5 µs (median of five, range 6.8–9.5)
against the 6.52 µs below, `ExposureAll/8` at 46.8 µs against 44.8, and `Share`
at 82 ns against 76.3. **The allocation counts matched to the byte on every
line**, which is the part that describes the code rather than the machine.
Treat the latency column as this-shape-of-box, not as a constant.

Run-to-run spread across the three runs was under 12% on every line and under 5%
on most; the 256-counterparty cases are the noisiest, which is what a 2-vCPU
machine does to a workload that fans out.

**The scaling is superlinear and it is a known cause.** 8 → 64 counterparties is
8× the work for 6.8× the time; 64 → 256 is 4× the work for 6.7× the time. The
engine is quadratic in counterparties at the top end because `domain.Snapshot`
exposes linear scans (`PositionsOf`, `Instrument`, `Counterparty`) rather than
indexes. That is a snapshot-shape problem, not an arithmetic one; the fix is an
index built once per snapshot and it belongs in `domain`.
`internal/risk/README.md` says the same.

**Zero allocations on `Share`** is worth noting because concentration shares are
exact rationals over `math/big` and the obvious implementation allocates on every
one of them.

### Platform assembly and an end-to-end governed run

```sh
go test ./internal/platform/ -run TestSmokeRun -count=1
```

**44–49 ms** across three runs (49, 44, 48). That covers: constructing the whole
platform (seeding the dataset, building and populating the lakehouse from the
seed's event stream, standing up both ledger simulators from the seeded
positions, compiling the policy bundle, registering all 16 tools, opening the
audit log) *and* driving four governed runs through the runtime, one of which
pauses on a dual-approval gate and resumes through two approvers, *and* verifying
the audit chain over the 45 records it produced.

That is a useful number for the wrong reason: it says the governed path is not
where time goes. It is **not** an end-to-end latency SLO, because the model is
the deterministic planner and does no I/O. A real model call dominates
everything above by three to four orders of magnitude, and that is the number
that would matter in production.

### The test suite

```sh
go test ./... -count=1          # 7.0 s cold cache, 3.0 s warm (3.0, 3.1, 3.0)
go test ./... -count=1 -race    # 19.7 s
```

375 `Test` functions, 2 `Fuzz` targets, 7 benchmarks; 814 test cases including
subtests.

---

## What has not been measured, and is therefore not claimed

**Policy evaluation and guardrail inspection latency** used to be in this list
and are not any more. `tcrpd demo` act 12 measures both, 2,000 samples each, and
prints the distribution. One run:

```
  guardrail inspection     n=2000  mean    67.2µs  p50    71.4µs  p95   107.1µs  p99   172.2µs
  policy evaluation        n=2000  mean    19.0µs  p50    12.5µs  p95    49.9µs  p99    84.9µs
```

Four runs on the same machine — two vCPUs, Go 1.24.7, linux/amd64 — put the
guardrail p50 between 68.9 µs and 72.6 µs, p95 between 101.4 µs and 138.3 µs,
and p99 between 171.6 µs and 224.8 µs; policy p50 between 11.2 µs and 12.5 µs
and p99 between 76.5 µs and 95.7 µs. Treat the third significant figure as
noise.

**Read this measurement narrowly.** It is a tight loop in one process on an
idle single-tenant box, with no HTTP, no serialisation, no concurrency and no
contention, against the seeded corpus. It says the two inline controls cost
tens of microseconds against a 100 ms budget — three orders of magnitude of
headroom — and it says nothing about behaviour under load, on a throttled pod,
or against adversarial input longer than the seed's. The **configured** budget
is still what the code contracts on: 100 ms for the whole pipeline, 30 ms per
stage (`guard.DefaultConfig`), beyond which `BudgetExceeded` is set and the
per-tier fail mode applies. The SLO below stays written against the budget.

**HTTP request latency.** No load test has been run. `tcrp_http_request_seconds`
is registered and labelled by route; the objectives below are targets.

**Lake ingest, silver apply, gold snapshot build.** No benchmarks.

**Bridge settlement latency.** `tcrp_bridge_settlement_seconds` is registered.
The simulators have no real finality delay, so any figure measured here would
describe the simulator rather than a bridge.

**Availability, over any window.** Nothing has been deployed, so there is no
availability history. The objectives below are targets a deployment would adopt,
not measurements.

---

## Objectives

Each objective is marked **[measured]** or **[target]**.

### Availability

| Objective | Value | Basis |
|---|---|---|
| Platform HTTP availability (non-5xx on `/v1`) | 99.9% over 30 days | **[target]** |
| Guardrail service availability | 99.95% over 30 days | **[target]** — it is on the request path of everything |
| Readiness (`/readyz` passing) | 99.9% over 30 days | **[target]** |

99.9% over 30 days is 43 minutes 12 seconds of error budget.

### Latency

| Objective | Value | Basis |
|---|---|---|
| Read-only tool call, p99 | ≤ 50 ms | **[measured, indirectly]** — `ExposureSingle` is 6.52 µs and `ExposureAll/8` is 44.8 µs; the budget is dominated by serialisation and the guardrail, not by the calculation |
| Risk calculation for the full book, p99 | ≤ 10 ms at ≤ 64 counterparties | **[measured]** — `ExposureAll/64` is 306.8 µs, so this is 30× headroom, deliberately, because the scaling is superlinear |
| Guardrail inspection, p99 | ≤ 100 ms | **[target, equal to the configured budget]** — beyond it the per-tier fail mode applies. `tcrpd demo` measures 172–225 µs p99 in-process on two vCPUs, which is not a production measurement but does say the budget is not the binding constraint on an idle box |
| Policy evaluation, p99 | ≤ 5 ms | **[target]** — `tcrpd demo` measures 77–96 µs p99 in-process on two vCPUs; no benchmark and no load test exists |
| HTTP request, p99, read routes | ≤ 250 ms | **[target]** |
| Recompute lag (newest event → recompute complete), p99 | ≤ 30 s | **[target]** |

### Correctness

These are the ones that matter, and unlike the latency objectives they are
absolute rather than percentile. A correctness objective with a percentile
attached is not a correctness objective.

| Objective | Value | Basis |
|---|---|---|
| `tcrp_bridge_supply_delta` | **exactly 0**, always | **[measured in tests]** — `SETTLE-1`, `internal/bridge/supply_test.go` |
| `tcrp_policy_rule_errors_total` | **0**, always | A rule that failed to evaluate contributed nothing; the gateway is running with fewer rules than it believes |
| `tcrp_risk_calculation_errors_total` | **0** | An error is a missing FX rate or a malformed snapshot, both of which are bugs |
| Money movements without an approval | **0** | `AGENT-1` |
| Tool invocations without an allowing decision | **0** | `AGENT-2` |
| Audit chain verification failures | **0** | `AGENT-5` |
| Conformance gate pass rate | **100%** on every release | ADR 0009 — inconclusive counts as failing |

### Quality of governance

| Objective | Value | Basis |
|---|---|---|
| Approval gate response time, p95 | ≤ 10 minutes | **[target]** — the window is 15 minutes and an approval that routinely expires is a control that is being worked around |
| Approval expiry rate | ≤ 1% of gates opened | **[target]** |
| Guardrail budget exceeded | ≤ 0.1% of inspections | **[target]** |
| Runs blocked by the guardrail | no objective | Deliberately none. This is an attack-volume signal, not a quality signal, and giving it a target creates pressure to tune the guardrail down. |
| Policy denials | no objective | Same reason. A denial is the gateway working. |

---

## Error budgets

**Availability, 99.9% over 30 days — 43m 12s.**

Budget policy, in the order it should be applied:

- **Below 50% consumed.** Ship normally.
- **50–75% consumed.** Ship, and any change touching `internal/agent`,
  `internal/policy` or `internal/guard` needs a second reviewer.
- **75–100% consumed.** Reliability work only. No feature changes to the request
  path.
- **Exhausted.** Freeze until the window rolls, except for changes that reduce
  the burn.

**Latency budget.** A p99 breach on a read route for more than 1% of a rolling
hour consumes budget at the same rate as unavailability, because a collateral
query that takes ten seconds is not a slow answer, it is a desk that stops
asking.

**The correctness objectives have no budget.** `tcrp_bridge_supply_delta`
non-zero is not "we spent some budget", it is an incident. So is a broken audit
chain, so is money moving without an approval. A budget implies an acceptable
rate, and the acceptable rate for these is zero.

---

## Alerting rules

Written against metric names that exist in the source. `for` durations assume a
30-second scrape.

### Page immediately

```yaml
- alert: BridgeSupplyDelta
  # Units exist on one network that are not backed on the other.
  expr: abs(tcrp_bridge_supply_delta) > 0
  for: 1m
  labels: {severity: page}
  annotations:
    summary: "Bridge supply delta is non-zero on {{ $labels.direction }}"
    runbook: docs/runbook.md#metrics

- alert: BridgeSupplyBreaks
  expr: tcrp_bridge_supply_breaks > 0
  for: 5m
  labels: {severity: page}

- alert: PolicyRuleEvaluationErrors
  # A rule condition did not produce a boolean. The gateway is running with
  # fewer rules than it thinks it has.
  expr: increase(tcrp_policy_rule_errors_total[5m]) > 0
  for: 0m
  labels: {severity: page}

- alert: BridgeFinalityViolation
  # A post-finality reorg. The relayer will not mint again; a human decides.
  expr: increase(tcrp_bridge_alerts_total[10m]) > 0
  for: 0m
  labels: {severity: page}

- alert: RiskCalculationErrors
  expr: increase(tcrp_risk_calculation_errors_total[10m]) > 0
  for: 0m
  labels: {severity: page}

- alert: PlatformNotReady
  expr: up{job="tcrp"} == 0
  for: 2m
  labels: {severity: page}
```

### Warn

```yaml
- alert: GuardrailBudgetExceeded
  # Read-only turns are now failing open and going out uninspected; state-change
  # and money-movement turns are failing closed and being blocked.
  expr: |
    rate(tcrp_guard_budget_exceeded_total[5m])
      / rate(tcrp_guard_inspections_total[5m]) > 0.001
  for: 10m
  labels: {severity: warn}
  annotations:
    runbook: docs/runbook.md#the-guardrail-budget-is-being-exceeded

- alert: GuardrailLatencyHigh
  expr: |
    histogram_quantile(0.99, rate(tcrp_guard_inspection_seconds_bucket[5m])) > 0.1
  for: 10m
  labels: {severity: warn}

- alert: PolicyEvaluationSlow
  expr: |
    histogram_quantile(0.99, rate(tcrp_policy_evaluation_seconds_bucket[5m])) > 0.005
  for: 10m
  labels: {severity: warn}

- alert: ReconciliationBreaksOpen
  expr: tcrp_lake_recon_breaks_open > 5
  for: 30m
  labels: {severity: warn}
  annotations:
    note: "The seeded dataset carries two permanent breaks; the threshold accounts for them."
    runbook: docs/runbook.md#a-reconciliation-break-opens

- alert: MissingInBookBreak
  # Value moved on chain and the institution's books do not know.
  expr: increase(tcrp_lake_recon_breaks_total{kind="missing_in_book"}[15m]) > 0
  for: 0m
  labels: {severity: warn}

- alert: BridgeQuorumDegrading
  # A quorum falling from 5-of-7 to 3-of-7 still settles. The histogram shifting
  # left is the only signal before it stops settling entirely.
  expr: |
    histogram_quantile(0.5, rate(tcrp_bridge_quorum_signatures_bucket[15m])) < 3
  for: 15m
  labels: {severity: warn}

- alert: BridgeFlowsStuck
  expr: tcrp_bridge_pending > 0
  for: 30m
  labels: {severity: warn}

- alert: RiskRecomputeLag
  expr: |
    histogram_quantile(0.99, rate(tcrp_risk_recompute_lag_seconds_bucket[10m])) > 30
  for: 10m
  labels: {severity: warn}

- alert: LakeIngestLag
  expr: tcrp_lake_pipeline_lag > 1000
  for: 15m
  labels: {severity: warn}

- alert: LateArrivalsElevated
  # Data is arriving late. Every risk cut taken during this period will be
  # restated.
  expr: rate(tcrp_lake_silver_corrections_total[15m]) > 0.1
  for: 15m
  labels: {severity: warn}

- alert: HTTPErrorRate
  expr: |
    sum(rate(tcrp_http_requests_total{status="5xx"}[5m]))
      / sum(rate(tcrp_http_requests_total[5m])) > 0.001
  for: 10m
  labels: {severity: warn}

- alert: HTTPLatencyHigh
  expr: |
    histogram_quantile(0.99, sum by (le, route) (rate(tcrp_http_request_seconds_bucket[5m]))) > 0.25
  for: 15m
  labels: {severity: warn}

- alert: LedgerFinalityLagGrowing
  expr: (tcrp_ledger_head_block - tcrp_ledger_finalised_block) > 64
  for: 15m
  labels: {severity: warn}

- alert: StreamConsumerLag
  expr: tcrp_stream_consumer_lag > 10000
  for: 15m
  labels: {severity: warn}
```

### Inform

```yaml
- alert: ApprovalGateVolumeHigh
  # Approval fatigue forming. Not an outage; a control degrading.
  expr: rate(tcrp_agent_approvals_requested_total[1h]) * 3600 > 50
  for: 2h
  labels: {severity: info}
  annotations:
    note: "See docs/threat-model.md section 4."

- alert: GuardrailBlockRateShift
  # An attack, or a guardrail regression. Distinguish by which surface the
  # blocking findings are on: prompt is the former, output is usually grounding.
  expr: |
    rate(tcrp_agent_runs_blocked_total[1h])
      / rate(tcrp_agent_runs_total[1h]) > 0.05
  for: 1h
  labels: {severity: info}

- alert: RiskBreachesRaised
  # First detections only, so a flapping limit is distinguishable from one that
  # has been broken once and left open.
  expr: increase(tcrp_risk_breaches_raised_total[1h]) > 0
  for: 0m
  labels: {severity: info}

- alert: LedgerReorgObserved
  expr: increase(tcrp_ledger_reorgs_total[15m]) > 0
  for: 0m
  labels: {severity: info}
```

`deploy/k8s/prometheusrule.yaml` carries these as a deployable resource.

## What is deliberately not alerted on

- **`tcrp_agent_tool_denials_total`.** A denial is the gateway working. Alerting
  on it trains an operator to treat a successful control as a problem, and
  creates pressure to tune it down. The eval suite scores refusals separately
  from executions for exactly the same reason.
- **`tcrp_guard_blocks_total` in absolute terms.** Only the *rate relative to
  runs* is informational, and even that is `severity: info`.
- **`tcrp_ledger_reverts_total`.** A revert is the ledger's compliance rule
  working. A transfer to an unregistered address should revert.
- **`tcrp_lake_bronze_duplicates_total`.** Deduplication on the natural key is
  the design. A reorg re-including a transaction produces duplicates and that is
  correct behaviour.
