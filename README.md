# Hypercall

Hypercall is being built to run AI agents in isolated Firecracker microVMs,
fork runs from recorded checkpoints, and compare model, quantization, and
inference configurations for quality, cost, and latency.

A task and model matrix produce scored branches, trajectory comparisons,
a Pareto report, and a CI pass/fail result.

## Code grounding

- **google/ax:** control plane, resource manifests, reconciliation, and CLI.
- **e2b-dev/runtime** (formerly `infra`): Firecracker, guest agent, templates, and snapshots.
- **e2b-dev/E2B:** Python SDK lifecycle, commands, and files.
- **promptfoo/promptfoo:** evaluation semantics and fixtures for the Go port.

Selected code will be adapted into Hypercall, with upstream versions,
licenses, notices, and attribution preserved.

## Stack

Go services and CLI; Python SDK, agents, and scorers; shared protobuf/gRPC;
vLLM and LiteLLM; Postgres, ClickHouse, and object storage; Next.js dashboard.

## Status and development

The user marked Phase 0 ready; Phase 1A implementation is in progress. The first
wave establishes the private guest protocol, Go guest, Python conformance tests
and local host qualification tools. Phase 1A still requires real Linux/KVM
acceptance evidence before snapshots and forks begin.
Development targets internal use on cloud credits. Production operation begins
only after investment and release qualification.

- [Architecture](docs/ARCHITECTURE.md)
- [Development phases](docs/DEVELOPMENT_PHASES.md)
- [Phase 0 decisions](docs/PHASE_0.md)
- [Phase 1 plan and setup](docs/PHASE_1.md)
- [Phase 1A validation evidence](docs/validation/1A.md)
- [Contributor and agent guidance](AGENTS.md)

Hypercall's original code is licensed under [BSD-3-Clause](LICENSE).
Imported components retain their applicable licenses.
