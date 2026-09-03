# TCRP — multi-stage build to a distroless, non-root, static image.
#
#   docker build -f deploy/Dockerfile -t tcrp:dev .
#
# Build from the repository root, not from deploy/. The build context needs
# go.mod, cmd/, internal/ and web/ and nothing else.

# ---------------------------------------------------------------------------
# Stage 1 — build
# ---------------------------------------------------------------------------
# The tag is not a digest. A tag is mutable, so this build is reproducible in
# its inputs from the repository but not in its base image; pin
# `golang:1.24-bookworm@sha256:...` when that matters and update the pin
# deliberately when moving Go versions.
FROM golang:1.24-bookworm AS build

WORKDIR /src

# There are no dependencies to download — go.mod has no require block — so
# there is no separate dependency layer to cache. That is a consequence of ADR
# 0001 and it is why this build has no `go mod download` step and no
# --mount=type=cache for the module cache.
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web

ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG BUILT_AT=unknown

# CGO_ENABLED=0 gives a static binary with no libc dependency, which is what
# makes the distroless static base (and scratch) viable.
#
# -trimpath removes absolute build paths, so the binary does not carry the build
# machine's directory layout and two builds of the same source produce the same
# bytes.
#
# -buildvcs=false because the build context has no .git directory; leaving it on
# makes the build fail rather than silently omitting the stamp.
#
# -s -w strips the symbol table and DWARF. It costs readable stack traces in a
# core dump and saves several megabytes. The trade is worth it here because the
# platform's diagnostics are its spans and its audit log, not core dumps.
#
# The three -X targets are the package-level vars in cmd/tcrpd/main.go; `tcrpd
# version` is what prints them.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -buildvcs=false \
      -ldflags="-s -w \
        -X main.version=${VERSION} \
        -X main.commit=${COMMIT} \
        -X main.builtAt=${BUILT_AT}" \
      -o /out/tcrpd ./cmd/tcrpd

# Fail the build rather than shipping a dynamically linked binary into a base
# with no dynamic loader — the failure mode otherwise is a container that exits
# immediately with "no such file or directory" pointing at a file that exists.
#
# `version` is a subcommand, not a flag: `--version` is an unknown command and
# exits 2.
# The state directory has to be created here, owned by the runtime uid.
# distroless has no shell, so there is no `RUN mkdir` in the runtime stage, and
# a directory that Docker creates for a VOLUME is created root-owned and mode
# 0755 — under `USER 65532` the process then fails at startup with
# "lake: create root: mkdir /var/lib/tcrp/lake: permission denied". Creating it
# here and COPYing it with --chown is the only way to get the ownership right in
# a shell-less image.
RUN ["/out/tcrpd", "version"]
#
# Note the shape: the directory to be owned is a CHILD of the COPY source.
# `COPY --chown=... /out/state /var/lib/tcrp` copies the *contents* of
# /out/state and leaves the destination directory itself root-owned, which is
# the same failure. Copying /out/state/ onto /var/lib/ makes `tcrp` a copied
# entry, so --chown applies to it.
RUN mkdir -p /out/state/tcrp

# ---------------------------------------------------------------------------
# Stage 2 — runtime
# ---------------------------------------------------------------------------
# distroless static: no shell, no package manager, no libc, no busybox. The
# :nonroot variant runs as uid 65532.
#
# The alternative is FROM scratch, which is smaller still. distroless is chosen
# over scratch because it brings CA certificates (needed for the outbound TLS
# call to the model API), /etc/passwd with the nonroot user, and tzdata — all of
# which would otherwise have to be COPYed in by hand, and the hand-copied
# version is the one that silently ships with an empty CA bundle.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/tcrpd /tcrpd

# The state directory: lake tables, run and approval JSON, the audit log.
# Declared as a volume so the image's read-only root filesystem does not have to
# be relaxed for it. In Kubernetes this is a PVC; see deploy/k8s/deployment.yaml.
#
# COPYed with --chown rather than declared and left alone: see the note in the
# build stage. Order matters — the COPY must precede VOLUME, so that the
# ownership is baked into the image layer that Docker seeds a fresh volume from.
COPY --from=build --chown=65532:65532 /out/state/ /var/lib/
VOLUME ["/var/lib/tcrp"]

# uid 65532, from the :nonroot variant. Numeric rather than the name so that a
# Kubernetes runAsNonRoot check can verify it without resolving /etc/passwd.
USER 65532:65532

EXPOSE 8080

# There is no ENV block here on purpose.
#
# tcrpd reads its configuration from flags, not from the environment. The only
# environment variables any code in this repository reads are ANTHROPIC_API_KEY
# and TCRP_MODEL_ID (internal/agent/anthropic.go) and TCRP_JUDGE_MODEL
# (internal/eval/judge.go), and all three matter only when --model=anthropic.
# Setting TCRP_ADDR or TCRP_DIR here would be decoration: nothing reads them.
# Configure the container by overriding CMD, as deploy/k8s/deployment.yaml and
# deploy/docker-compose.yml both do.

# There is no HEALTHCHECK here, also on purpose.
#
# This image has no shell and no curl, so `CMD curl -f .../healthz` cannot work
# — it would fail with an exec error that reads like an application failure. The
# usual answer is a subcommand on the binary, and tcrpd has no `healthcheck`
# subcommand today (its subcommands are demo, serve, ask, eval, conform,
# version). Rather than ship a HEALTHCHECK that always fails, there is none:
# Kubernetes probes the endpoints over HTTP instead, which is what
# deploy/k8s/deployment.yaml does. GET /healthz and GET /readyz are served
# unauthenticated and return 200.

ENTRYPOINT ["/tcrpd"]
CMD ["serve", "--addr", ":8080", "--dir", "/var/lib/tcrp"]

# ---------------------------------------------------------------------------
# What this image deliberately does not contain
# ---------------------------------------------------------------------------
#
# - No shell. `docker exec` into it will not work, and that is the point. Debug
#   with the :debug variant of the same distroless tag, deliberately, for one
#   incident.
# - No policy bundle override. The default bundle is embedded with go:embed and
#   ships inside the binary. An override is mounted from a ConfigMap at
#   /etc/tcrp/policy and named with `--policy`; see deploy/k8s/configmap.yaml.
# - No bpmn/ directory. `tcrpd conform --process bpmn/...` reads the model from
#   disk, so the conformance gate runs in CI against a checkout, not in this
#   image. `tcrpd conform` without --process works here and checks the seven
#   AGENT invariants that do not come from a process model.
# - No secrets, no keys, no credentials. The audit signing key and the identity
#   signer's secret are generated in-process at startup; ANTHROPIC_API_KEY
#   arrives from the environment at runtime and is only read under
#   --model=anthropic.
# - No init process. The platform handles SIGTERM itself; there is no zombie
#   reaping problem because there are no child processes.
