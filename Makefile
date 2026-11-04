SHELL := /bin/sh
GO ?= go
UV ?= uv
GO_VERSION := go1.27.3
UV_VERSION := 0.12.11

.PHONY: toolchains generate generate-check test test-postgres test-contract test-artifacts test-kvm build build-testdriver verify host-doctor test-kvm-snapshot test-runtime-2a test-runtime-2b test-eval

toolchains:
	@test "$$($(GO) version | cut -d ' ' -f 3)" = "$(GO_VERSION)" || { echo "Go 1.27.3 required (see .go-version)"; exit 1; }
	@test "$$($(UV) --version | cut -d ' ' -f 2)" = "$(UV_VERSION)" || { echo "uv $(UV_VERSION) required"; exit 1; }
	$(UV) sync --frozen

generate: toolchains
	$(UV) run --frozen python scripts/generate.py

generate-check: toolchains
	$(UV) run --frozen python scripts/generate.py --check

test: toolchains
	$(GO) test -race ./...

# Explicit opt-in: durable/public integration tests must not silently skip.
test-postgres: toolchains
	@test -n "$$HYPERCALL_TEST_POSTGRES" || { echo "HYPERCALL_TEST_POSTGRES is required (see docs/development/control-plane.md)"; exit 1; }
	$(GO) test -race -count=1 ./internal/controlplane ./internal/api ./internal/hostapi

test-contract: toolchains
	$(UV) run --frozen python -m unittest discover -s test/contract -v

test-artifacts: toolchains
	$(UV) run --frozen python -m unittest discover -s deploy/runtime -p 'test_*.py' -v

# Explicit opt-in: missing artifacts or host requirements fail this target.
test-kvm: toolchains
	HYPERCALL_KVM_TEST=1 $(GO) test -count=1 -tags=kvm -v -timeout=9m ./test/integration/kvm

# Separate opt-in preserves the cold-boot gate and supports capture/restore modes.
test-kvm-snapshot: toolchains
	HYPERCALL_SNAPSHOT_KVM_TEST=1 $(GO) test -count=1 -tags=kvm -run TestPhase1B -v -timeout=13m ./test/integration/kvm

# Starts only the configured API process; the operator supplies an existing host.
test-runtime-2a: toolchains
	@test "$$HYPERCALL_RUNTIME_2A_TEST" = "1" || { echo "HYPERCALL_RUNTIME_2A_TEST=1 is required"; exit 1; }
	@test -n "$$HYPERCALL_2A_CONFIG" -a -n "$$HYPERCALL_2A_REPORT" || { echo "HYPERCALL_2A_CONFIG and HYPERCALL_2A_REPORT are required"; exit 1; }
	$(UV) run --frozen python scripts/qualify-phase2a --config "$$HYPERCALL_2A_CONFIG" --report "$$HYPERCALL_2A_REPORT"

test-runtime-2b: toolchains
	@test "$$HYPERCALL_RUNTIME_2B_TEST" = "1" || { echo "HYPERCALL_RUNTIME_2B_TEST=1 is required"; exit 1; }
	@test -n "$$HYPERCALL_2B_CONFIG" -a -n "$$HYPERCALL_2B_REPORT" || { echo "HYPERCALL_2B_CONFIG and HYPERCALL_2B_REPORT are required"; exit 1; }
	$(UV) run --frozen python scripts/qualify-phase2b --config "$$HYPERCALL_2B_CONFIG" --report "$$HYPERCALL_2B_REPORT"

build: toolchains
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build ./...
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build ./...
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build ./...
	$(UV) build --no-build-isolation --wheel

build-testdriver: toolchains
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -o bin/hypercall-testdriver ./cmd/hypercall-testdriver

verify: generate-check test test-contract test-artifacts build
	$(GO) vet ./...

host-doctor:
	$(GO) run ./cmd/hypercall-host doctor --json

# Phase 3 explicit acceptance: every real external seam is required, not skipped.
test-eval: toolchains
	@test -n "$$HYPERCALL_TEST_POSTGRES" || { echo "HYPERCALL_TEST_POSTGRES is required"; exit 1; }
	@test -n "$$HYPERCALL_TEST_CLICKHOUSE_URL" -a -n "$$HYPERCALL_TEST_CLICKHOUSE_DATABASE" -a -n "$$HYPERCALL_TEST_CLICKHOUSE_USER" -a -n "$$HYPERCALL_TEST_CLICKHOUSE_PASSWORD" || { echo "HYPERCALL_TEST_CLICKHOUSE_URL/DATABASE/USER/PASSWORD are required"; exit 1; }
	@docker info >/dev/null || { echo "A local Docker daemon is required for scorer isolation acceptance"; exit 1; }
	$(GO) test -race -count=1 ./internal/eval ./internal/controlplane ./internal/api ./internal/analytics
	GO="$(GO)" scripts/qualify-scorer
