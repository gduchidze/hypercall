# Hypercall

A Go runtime for isolated agent workloads in Firecracker microVMs.

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
```

The host doctor returns nonzero when requirements or pinned artifacts are missing.
Native macOS cannot provide Linux KVM; use the [Lima development setup](docs/development/host.md#local-mac-alternative-nested-linux-with-lima).

## Documentation

- [Guest service and security model](internal/guest/README.md)
- [Protocol and code generation](proto/README.md)
- [Host setup and qualification](docs/development/host.md)
- [Snapshot/fork qualification](docs/development/snapshots.md)

## Status

The cold-boot profile is tested on Linux ARM64 with real nested KVM: authenticated
vsock, Go/Python commands and files, resource exhaustion, cancellation, network
denial, independent VMs, and cleanup after guest or host failure.

Workspaces are ephemeral and network access is disabled. Linux AMD64 is
cross-built but still needs runtime qualification. Snapshot capture and guest UTC renewal have partial real KVM evidence.
Two-child fork continuity currently fails after guest health timeouts; cross-host
restore and live GCS remain untested. Snapshot/fork support and its concurrency
envelope are unqualified; [qualification status](docs/development/snapshots.md#qualification-status)
records the results and remaining gates. Native Mac command tests use trusted processes; the
separate Linux KVM suite establishes VM behavior.

## License

[BSD-3-Clause](LICENSE).
