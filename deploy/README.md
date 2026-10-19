# deploy

Container image, Compose composition and Kubernetes manifests for the platform.

## Read this first

**The image builds and the composition runs.** `cmd/tcrpd` exists;
`deploy/Dockerfile` builds it into a distroless non-root static image and
`docker-compose.yml` starts it:

```sh
docker build -f deploy/Dockerfile -t tcrp:dev .
docker compose -f deploy/docker-compose.yml up --build -d
curl -s -o /dev/null -w '%{http_code}\n' 127.0.0.1:8080/healthz   # 200
open http://127.0.0.1:8080
```

**Two workloads described here still cannot start**, and both are visibly
disabled rather than quietly broken:

- **The standalone guardrail.** `guard.Handler` serves `POST /v1/inspect`,
  `GET /v1/healthz` and `GET /v1/config`, and `serve --guard-url` is the wiring
  that points the platform at it. No binary serves it: `tcrpd`'s subcommands are
  `demo`, `serve`, `ask`, `eval`, `conform`, `version`, and there is no
  `cmd/guardd`. `deployment.yaml` carries the workload at `replicas: 0`;
  `docker-compose.yml` carries it commented out.
- **Postgres.** `migrations/` is a real schema and its constraints are worth
  reading, but no Go file in this repository imports `database/sql`. The run and
  approval store is on disk under `--dir` and the audit log is a file. The
  service is commented out of the composition rather than started and left
  unused.

What is real everywhere else: the topology, the wiring, the settings, and the
reasoning. The health semantics are what `internal/api` implements, and every
`tcrp_*` series named in `prometheusrule.yaml` is registered somewhere in
`internal/` — though a freshly started `serve` exposes only the subset whose
code paths have run.

## What is here

| File | What it is |
|---|---|
| `Dockerfile` | Multi-stage; static `CGO_ENABLED=0` build of `./cmd/tcrpd`; distroless non-root final image; no `HEALTHCHECK`, because there is no shell and no `tcrpd healthcheck` subcommand. |
| `docker-compose.yml` | The platform, alone, configured by flags. The guardrail and Postgres services are commented out with what each would need. |
| `k8s/serviceaccount.yaml` | Namespace with the restricted Pod Security Standard enforced, plus two service accounts with no tokens and no RBAC. |
| `k8s/configmap.yaml` | The policy bundle override. The second ConfigMap, of `TCRP_*` names nothing reads, has been removed and replaced by a note. |
| `k8s/secret.yaml` | **Stub.** Four placeholders; only `ANTHROPIC_API_KEY` is projected into the container, because it is the only one any code reads. |
| `k8s/deployment.yaml` | Platform (3 replicas, real flags) and guardrail (**0 replicas** — no binary serves it). |
| `k8s/service.yaml` | Two ClusterIP services plus a headless one for per-pod scraping. |
| `k8s/hpa.yaml` | Autoscaling, with asymmetric scale-up and scale-down. |
| `k8s/pdb.yaml` | Disruption budgets sized around open approval gates and guardrail capacity. |
| `k8s/networkpolicy.yaml` | Default-deny, with the model API as the single egress exception. |
| `k8s/prometheusrule.yaml` | Alerts against metric names that are all registered in `internal/`. |

## How they are meant to be used

**Locally:**

```sh
docker compose -f deploy/docker-compose.yml up --build -d
open http://127.0.0.1:8080
```

No `.env` is required. `deploy/.env.example` carries the build stamps and
`ANTHROPIC_API_KEY`, and nothing else: the platform is configured entirely by
the flags on the `command:` in `docker-compose.yml`, because `tcrpd` reads its
configuration from its flag set and not from the environment. The only
environment variables any code here reads are `ANTHROPIC_API_KEY` and
`TCRP_MODEL_ID` (`internal/agent/anthropic.go`) and `TCRP_JUDGE_MODEL`
(`internal/eval/judge.go`), all three only under `--model=anthropic`.

The composition runs the guardrail **in process**, which is the default. The
separate-process form is `serve --guard-url http://guard:8081` against
`guard.Handler`, and it is a wiring change and not a code change — that is the
property ADR 0003 is about. It is not demonstrated here because nothing serves
`guard.Handler` yet.

**In a cluster:**

```sh
kubectl apply -f deploy/k8s/serviceaccount.yaml
kubectl apply -f deploy/k8s/configmap.yaml
# Do NOT apply secret.yaml. Project from a secret manager instead.
kubectl apply -f deploy/k8s/networkpolicy.yaml
kubectl apply -f deploy/k8s/deployment.yaml
kubectl apply -f deploy/k8s/service.yaml
kubectl apply -f deploy/k8s/pdb.yaml
kubectl apply -f deploy/k8s/hpa.yaml
kubectl apply -f deploy/k8s/prometheusrule.yaml
```

NetworkPolicy first, so no pod ever runs with unrestricted egress even briefly.

## What is deliberately not production-ready

Listed in full, because a deployment manifest that looks finished and is not is
worse than one that admits it.

### There is no working authentication

This is the largest gap and it is not the one the file used to lead with.

`serve --dev-auth=true` (the default, and what `docker-compose.yml` uses) reads
the acting principal from the `X-TCRP-As` header with **nothing to prove it**.
Any caller can be the treasurer. `serve --env=production` refuses to start
unless `--dev-auth=false`, which is the right refusal.

With `--dev-auth=false` — which is what `deployment.yaml` sets — the only
credential the server accepts is a signed `X-TCRP-Principal` assertion, verified
by `identity.Signer`. Its HMAC secret is the compile-time literal
`tcrp-dev-signing-key` in `internal/platform/platform.go`; no flag, environment
variable or Secret overrides it. There is no endpoint that mints an assertion
and no rotation. So a production-mode deployment refuses every `/v1` request
from anyone who has not implemented a minting client against a hard-coded key.

`/`, `/healthz`, `/readyz` and `/metrics` are outside `/v1` and outside
authentication by design, so those four answer regardless.

### There is no container healthcheck

The image has no shell and no `curl`, so `CMD curl -f .../healthz` cannot work,
and there is no `tcrpd healthcheck` subcommand to use instead (`tcrpd`'s
subcommands are `demo`, `serve`, `ask`, `eval`, `conform`, `version`). Rather
than ship a `HEALTHCHECK` that always fails, `Dockerfile` and
`docker-compose.yml` have none. Kubernetes is unaffected — `deployment.yaml`
probes `/healthz` and `/readyz` over HTTP with `httpGet`, which needs nothing
inside the container.

### State is on an `emptyDir`

`deployment.yaml` mounts an `emptyDir` at `/var/lib/tcrp`. That directory holds
the lake tables, the run and approval JSON, and the audit log — **all three of
which must survive a restart.** A run paused on an approval gate is exactly the
state that must outlive the process (ADR 0010), and with an `emptyDir` it does
not.

This is the single largest gap here. The fix is either a StatefulSet with a PVC
per replica, or — better — moving the store to Postgres using `migrations/`,
which additionally makes "which runs are waiting on me" answerable by a replica
that started none of them.

### The policy bundle is not signed

`configmap.yaml` mounts an override read-only, and `policy.LoadBundleFile` will
load any file it is given. The digest is recorded on every decision and in every
evidence bundle, so a change is **visible** in the audit stream — but visibility
requires somebody to look, and nothing refuses the load.

Treat write access to `tcrp-policy` as equivalent to write access to the
platform's authorization logic. `docs/threat-model.md` §6 calls this the most
significant gap in that document.

Also: a mounted ConfigMap updates in place without restarting the pod, so the
`checksum/policy` annotation on the pod template must be regenerated on every
bundle change or the running process keeps the old rules.

### No TLS anywhere inside

Every hop is plaintext HTTP: ingress → platform, and platform → guardrail if
that ever runs. Compose binds the platform to `127.0.0.1` so at least it is not
on the network. A cluster deployment needs TLS termination at the ingress and
should have mTLS between platform and guardrail, which is a mesh concern and is
not configured here.

### The secrets are placeholders, three of them are read by nothing, and one should not exist

`secret.yaml` must not be applied as written. `deployment.yaml` projects only
`ANTHROPIC_API_KEY` from it, because that is the only key any Go file looks up;
`TCRP_SIGNING_SECRET`, `TCRP_AUDIT_SIGNING_KEY` and `TCRP_DATABASE_URL` are
documentation of a shape, and filling them in changes nothing. Beyond that:

- `TCRP_SIGNING_SECRET` is the identity signer's HMAC key. Whoever holds it can
  assert **any** principal — the treasurer, the risk manager, the agent — and
  four-eyes, scope checks and tier ceilings all rest on the assertion being
  trustworthy. In this repository it is the literal string
  `tcrp-dev-signing-key`. There is no dual-key rotation, so rotating means a
  15-minute window (the assertion TTL) of failures.
- `TCRP_AUDIT_SIGNING_KEY` should not be a Kubernetes Secret at all. It should
  be a KMS or HSM reference with the private key never materialising in the pod;
  the signature is what makes an audit edit detectable by somebody who does not
  trust the operator, and that only holds if the operator cannot reach the key.

### No Ingress

Cluster-specific, so there is none. Two things any Ingress in front of this must
do: terminate TLS, and **not expose `/metrics`, `/healthz` or `/readyz`**. Those
sit outside `/v1` and outside authentication by design — a probe that needs a
credential fails for the wrong reason — which is correct inside a cluster and is
an information disclosure on the public internet. `/metrics` carries
`tcrp_risk_open_breaches`, `tcrp_lake_recon_breaks_open` and
`tcrp_bridge_supply_delta`, which describe the institution's positions.

### Egress is a CIDR, not a hostname

`networkpolicy.yaml` permits `0.0.0.0/8` minus the private ranges and the cloud
metadata address, on port 443. That is far broader than "the model API".
NetworkPolicy cannot express a hostname; the right answers are an egress gateway
with FQDN policy (Cilium `toFQDNs`, an Istio `ServiceEntry`) or a forward proxy
holding the allowlist. Both are cluster-level dependencies and neither is
assumed.

The rule is also unnecessary under `--model=planner`, which is the default and
which makes no outbound call at all. Delete it in that case.

### The images are unpinned by digest

`tcrp:0.0.0-dev` in the manifests, `golang:1.26-bookworm` and
`gcr.io/distroless/static-debian12:nonroot` in the Dockerfile. A tag is mutable
and a reproducible build that follows a moving tag is not reproducible. Pin by
digest before this goes anywhere real.

### No CPU limits, deliberately

Neither container sets a CPU limit. That is a considered choice, not an omission:
CFS throttling on a latency-sensitive path turns a p50 in milliseconds into a
p99 in hundreds of milliseconds, and the guardrail has a 100 ms budget beyond
which **read-only turns fail open and go out uninspected**. A throttled guardrail
silently loses coverage. Memory limits are set; capacity is managed by requests
plus the HPA.

If your cluster policy requires CPU limits, set them generously and watch
`tcrp_guard_budget_exceeded_total`.

### The HPA scales on the wrong signal

CPU and memory are proxies. The metric that predicts a guardrail budget breach is
the inspection latency itself, and the right rule is a custom metric on
`tcrp_guard_inspection_seconds` p99 targeting half the budget. That needs
`prometheus-adapter`. `hpa.yaml` carries the rule to use, commented, and it is
the first change to make.

### The audit signing key does not survive a restart

`platform.New` generates it with `ed25519.GenerateKey` per process, so a chain
signed by one process cannot be verified with the next one's key. The bridge
signer keys are generated the same way. `platform.New` says so at the call site.
This is what makes the `emptyDir` above a compounding problem rather than a
merely inconvenient one.

### The single-node story

Compose runs one replica, so the risk engine's leader election
(`risk.Elector`, fencing tokens, `ErrFenced`) is never exercised. In Kubernetes
there are three platform replicas and nothing configures the lease store, so the
recompute path would run on every replica. That must be resolved before the
recomputer is enabled in a multi-replica deployment — two leaders is two sets of
breach detections and two sets of `risk.limit.breached` audit records for one
condition, which is exactly what the fencing token exists to prevent.
`docs/runbook.md#failover` has the mechanism.

### There is no database

`migrations/` holds a real schema and its constraints put four-eyes and the
append-only audit chain in the database rather than only in application code.
Nothing uses it: no Go file in this repository imports `database/sql`. The run
and approval store is on disk under `--dir`, the audit log is a JSONL file
beside it, and both are per-process.

The commented Postgres service in `docker-compose.yml` would apply the
migrations through `initdb`, which runs them in lexical order **on first start
only, on an empty volume** — fine for a demo composition and not a migration
strategy.

### No debug access

The image has no shell, which is intentional and has a consequence: `kubectl
exec` into it will not work, and the commands in `docs/runbook.md` that suggest
otherwise assume the `:debug` variant of the distroless tag. Debug through the
Service, or deliberately deploy the debug variant for one incident.

### Not addressed at all

No backup or restore. No log aggregation. No trace collector — the
platform writes one JSON span object per line to the path given by
`--trace-file` and something has to tail it, and nothing here does. No `ServiceMonitor` (the `prometheus.io/scrape` annotations assume
annotation-based discovery). No canary or blue-green. No cost controls. No
multi-region. No disaster recovery plan.

## Related

- `docs/runbook.md` — ports, health endpoints, every metric, and the procedures.
- `docs/slo.md` — objectives, the measured numbers, and the alerting reasoning.
- `docs/architecture.md` — trust boundaries, which is what the NetworkPolicy is
  an implementation of.
- `docs/adr/0003-guardrail-transport-and-fail-modes.md` — why the guardrail is
  in-process by default and deployable as a service.
- `migrations/` — the schema that would back the store, and the constraints that
  would put four-eyes and the append-only audit chain in the database rather than
  only in application code. Nothing applies it today.
