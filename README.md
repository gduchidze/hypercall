# Hypercall

A Go guest runtime and host qualification toolkit for agent sandboxes.

![Go 1.27.3](https://img.shields.io/badge/Go-1.27.3-00ADD8?logo=go&logoColor=white)
![Python 3.13.9](https://img.shields.io/badge/Python-3.13.9-3776AB?logo=python&logoColor=white)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD--3--Clause-blue)](LICENSE)

## Features

- **Guest execution:** authenticated gRPC commands with binary output, deadlines,
  cancellation, environment restrictions, and a non-root workload identity.
- **File transfer:** bounded binary reads and atomic writes within a workspace.
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
macOS/Linux cross-builds, Python wheel packaging, and `go vet`.

```sh
make generate       # Regenerate Go and Python bindings
make test-contract  # Run Python against the real Go guest
make build          # Check target builds and package the Python bindings
make host-doctor    # Report host prerequisites as JSON
```

The host doctor returns nonzero when requirements or pinned artifacts are missing.
Native macOS cannot provide Linux KVM; use the [Lima development setup](docs/development/host.md#local-mac-alternative-nested-linux-with-lima).

## Documentation

- [Guest service and security model](internal/guest/README.md)
- [Protocol and code generation](proto/README.md)
- [Host setup and qualification](docs/development/host.md)

## Status

Guest RPC behavior and host prerequisites are tested. Firecracker VM lifecycle,
end-to-end vsock communication, isolation under failure, and snapshot/fork support
are still under development. Local command tests execute trusted processes and
do not establish VM isolation.

## License

[BSD-3-Clause](LICENSE).
