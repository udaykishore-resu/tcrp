# Tokenized Collateral Risk Platform.
#
# Everything here works from a clean clone with no network: the module has no
# dependencies, so there is nothing to download, and every target below runs
# against the standard library alone.
#
#   make demo     the whole platform, narrated, with real numbers
#   make check    what CI runs: fmt, vet, tests with the race detector, the gate
#   make gate     the conformance gate on its own

SHELL       := /bin/bash
GO          ?= go
BIN         := bin
PKG         := ./...
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILT_AT    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.builtAt=$(BUILT_AT)
BINARIES    := tcrpd guardd tcrpctl

.DEFAULT_GOAL := help

## help: list the targets
help:
	@echo "Tokenized Collateral Risk Platform"
	@echo
	@sed -n 's/^## //p' $(MAKEFILE_LIST) | awk -F': ' '{printf "  %-14s %s\n", $$1, $$2}'

## build: compile every binary into bin/
build:
	@mkdir -p $(BIN)
	@for b in $(BINARIES); do \
		echo "  building $$b"; \
		CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/$$b ./cmd/$$b || exit 1; \
	done

## demo: narrate the whole platform end to end, with real numbers
demo:
	@$(GO) run ./cmd/tcrpd demo

## run: serve the API and the governance dashboard on :8080
run:
	@$(GO) run ./cmd/tcrpd serve --addr :8080

## ask: one governed agent turn; Q="..." [AS=agent] [APPROVE=risk-manager]
ask:
	@$(GO) run ./cmd/tcrpd ask --as $(or $(AS),agent) \
		$(if $(APPROVE),--approve-as $(APPROVE),) "$(or $(Q),what is our exposure to CP-DELTA?)"

## test: the full suite
test:
	@$(GO) test $(PKG) -count=1

## test-race: the full suite under the race detector
test-race:
	@$(GO) test $(PKG) -race -count=1

## vet: go vet
vet:
	@$(GO) vet $(PKG)

## fmt: format every file in place
fmt:
	@gofmt -w $(shell find . -name '*.go' -not -path './bin/*')

## lint: fail when anything is unformatted
lint:
	@out=$$(gofmt -l . | grep -v '^bin/' || true); \
	if [ -n "$$out" ]; then echo "not gofmt'd:"; echo "$$out"; exit 1; fi
	@echo "  gofmt clean"

## gate: run the conformance gate; non-zero when an invariant fails
gate:
	@$(GO) run ./cmd/tcrpd conform \
		--process bpmn/collateral-margin-call.bpmn \
		--bundle evidence.json --gate

## eval: run the evaluation suite against the runtime
eval:
	@$(GO) run ./cmd/tcrpd eval --strict=false --out eval-report.json --markdown eval-report.md

## bench: the risk engine's benchmarks
bench:
	@$(GO) test ./internal/risk/... -run XXX -bench . -benchmem -benchtime 300ms

## cover: coverage across the whole module
cover:
	@$(GO) test $(PKG) -coverpkg=$(PKG) -coverprofile=coverage.out -covermode=atomic >/dev/null
	@$(GO) tool cover -func=coverage.out | tail -1

## check: fmt check, vet, race tests and the gate — what CI runs
check: lint vet test-race gate

## ci: check plus the evaluation suite
ci: check eval

## clean: remove build output and generated reports
clean:
	@rm -rf $(BIN) coverage.out evidence.json eval-report.json eval-report.md

.PHONY: help build demo run ask test test-race vet fmt lint gate eval bench cover check ci clean
