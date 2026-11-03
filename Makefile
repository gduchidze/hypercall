SHELL := /bin/sh
GO ?= go
UV ?= uv
GO_VERSION := go1.27.3
UV_VERSION := 0.12.11

.PHONY: toolchains generate generate-check test test-contract test-artifacts test-kvm build build-testdriver verify host-doctor

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

test-contract: toolchains
	$(UV) run --frozen python -m unittest discover -s test/contract -v

test-artifacts: toolchains
	$(UV) run --frozen python -m unittest discover -s deploy/runtime -p 'test_*.py' -v

# Explicit opt-in: missing artifacts or host requirements fail this target.
test-kvm: toolchains
	HYPERCALL_KVM_TEST=1 $(GO) test -count=1 -tags=kvm -v -timeout=9m ./test/integration/kvm

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
