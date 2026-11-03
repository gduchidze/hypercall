# Hypercall grounding

Canonical agent instructions: [AGENTS.md](AGENTS.md).
Matt Pocock skill workflows: [docs/agents/skills.md](docs/agents/skills.md).
Design and pinned upstream commits: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).
Wait for user architecture review before importing code or implementing.

Use these repositories to ground code and logic:

- `e2b-dev/runtime` (formerly `infra`): Firecracker orchestration, guest agent,
  template build, snapshot/restore and fork; inspect `packages/orchestrator` and `packages/envd`.
- `google/ax`: Go control plane, resources, reconciliation and CLI;
  inspect `DESIGN.md`, `internal/server` and `internal/controller`.
- `e2b-dev/E2B`: Python SDK commands, files and sandbox lifecycle;
  inspect `packages/python-sdk/e2b`.
- `promptfoo/promptfoo`: evaluation semantics and fixtures to port to Go;
  inspect `src/evaluator.ts`, `src/assertions` and related tests.

Adapt selected code to Hypercall contracts; preserve licenses and record exact upstream versions.
Build in order: sandbox → control plane → evaluation → inference and product operations.
