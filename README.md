# Hypercall

A Go control plane and runtime for isolated agent workloads in Firecracker microVMs.

![Go 1.27.3](https://img.shields.io/badge/Go-1.27.3-00ADD8?logo=go&logoColor=white)
![Python 3.13.9](https://img.shields.io/badge/Python-3.13.9-3776AB?logo=python&logoColor=white)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD--3--Clause-blue)](LICENSE)

## Features

- **MicroVM lifecycle:** jailed cold boots, scoped allocation identities, expiry,
  monotonic epochs, stop, and recovery after a host process crashes.
- **Guest execution:** authenticated gRPC commands with binary output, deadlines,
  cancellation, environment restrictions, and a non-root workload identity.
- **File transfer:** bounded binary reads and atomic writes within a workspace.
- **Containment:** host and guest cgroup limits, read-only templates, private
  workspaces, and a network profile that denies all egress.
- **Reproducible images:** pinned runtime inputs, isolated builds, immutable
  manifests, and dependency notices packaged with generated artifacts.
- **Host diagnostics:** real Linux KVM, cgroup, namespace, and artifact checks.
- **Shared contracts:** generated Go and Python bindings with live interoperability
  tests.
- **Mac development:** a pinned Lima configuration for nested ARM64 Linux/KVM on
  supported Apple Silicon Macs.
- **Durable control plane:** tenant-scoped resources, Postgres jobs/events/outbox,
  authenticated gRPC/REST, a Go CLI, and sync/async Python clients.
- **Cooperative checkpoints:** committed agent steps, retained fork lineage,
  deterministic branch routing, and declared trajectory comparisons.

## Development

Use Go **1.27.3**, Python **3.13.9**, and uv **0.12.11**. Put Go and uv on your PATH:

```sh
uv python install 3.13.9
uv sync --frozen
make verify
```

`make verify` checks generated-source drift, Go race tests, Python interoperability,
artifact integrity tests, macOS/Linux cross-builds, Python wheel packaging, and
`go vet`.

```sh
make generate       # Regenerate Go and Python bindings
make test-contract  # Run Python against the real Go guest
make build          # Check target builds and package the Python bindings
make host-doctor    # Report host prerequisites as JSON
make build-testdriver # Build the real KVM acceptance operator
make test-postgres  # Run durable/public contracts with HYPERCALL_TEST_POSTGRES set
```

The host doctor returns nonzero when requirements or pinned artifacts are missing.
Native macOS cannot provide Linux KVM; use the [Lima development setup](docs/development/host.md#local-mac-alternative-nested-linux-with-lima).

## Documentation

- [Guest service and security model](internal/guest/README.md)
- [Protocol and code generation](proto/README.md)
- [Host setup and qualification](docs/development/host.md)
- [Snapshot/fork qualification](docs/development/snapshots.md)
- [Control-plane setup and limits](docs/development/control-plane.md)
- [CLI and Python SDK](docs/development/clients.md)
- [Cooperative agent checkpoints](docs/development/agent-checkpoints.md)

## Status

The cold-boot profile is tested on Linux ARM64 with real nested KVM: authenticated
vsock, Go/Python commands and files, resource exhaustion, cancellation, network
denial, independent VMs, and cleanup after guest or host failure.

Workspaces are ephemeral and network access is disabled. Linux AMD64 is
cross-built but still needs runtime qualification. Native Mac command tests use
trusted processes; the separate Linux KVM suite establishes VM behavior.

The bounded Phase 2A controlled-Python workflow passed on real ARM64/KVM,
including full-memory suspend/restore across controller restarts, fresh execution
identity, retained memory/files, active budgets, and confirmed cancellation.
See the [controlled-profile runtime procedure](docs/development/phase2a-runtime.md).
The default sandbox backend fails closed unless a configured host is supplied.

The bounded Phase 2B cooperative-Python workflow also passed on ARM64/KVM:
step-1 checkpointing, controller restart, and two sequential continuations from
one retained checkpoint through distinct deterministic fixtures, with fresh
acknowledged routing and expected trajectory divergence. See the
[supported profile and limits](docs/development/agent-checkpoints.md).

Simultaneous two-child fork continuity remains unqualified after guest health
timeouts; cross-host restore and live GCS remain untested. These bounded workflows
do not qualify arbitrary agents, real model serving, production operation, or the
full snapshot/fork concurrency envelope. The
[qualification status](docs/development/snapshots.md#qualification-status)
records the remaining gates.

## License

[BSD-3-Clause](LICENSE).
