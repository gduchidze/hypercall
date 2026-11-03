# Hypercall agent instructions

Applies repository-wide; follow more specific nested `AGENTS.md` files.

## Product and review gate

- Build a production system for isolated agent runs, checkpoint forks, and model evaluation.
- Read `docs/ARCHITECTURE.md` before changing design or writing implementation code.
- Follow `docs/DEVELOPMENT_PHASES.md` for delivery stages and acceptance gates.
- Architecture is proposed: wait for user review before imports, scaffolding, or implementation.
- Build order: E2B sandbox layer → AX control plane → Promptfoo eval → inference/dashboard/deployment.

## Code and logic grounding

- `google/ax`: Go server/controller, Task/Workspace/Model resources, CLI, workspace setup.
  Read `DESIGN.md`, `docs/manifests.md`, `internal/server`, `internal/controller`, `runner`.
  Replace its Agent Substrate dependency; verify Gateway/event/branch capabilities individually.
- `e2b-dev/runtime` (formerly `e2b-dev/infra`): Firecracker, templates, snapshots, restore/fork.
  Read `docs/ARCHITECTURE.md`, `packages/orchestrator`, `packages/envd`, `packages/shared`.
  Ground fork semantics in `packages/api/internal/handlers/sandbox_fork.go`.
- `e2b-dev/E2B`: Python SDK lifecycle, commands, files, sync/async behavior and errors.
  Read `packages/python-sdk/e2b`; adapt transport and identity to Hypercall.
- `promptfoo/promptfoo`: eval configuration, matrix, assertions, aggregation, CI/report logic.
  Read `src/evaluator.ts`, `src/assertions`, `src/types`, `src/util/config`, and related tests.
  Port the declared subset to Go; verify it against pinned upstream fixtures.
- Use upstream code and tests as grounding; adapt selected components to Hypercall contracts.
- Exact inspected commits and source links are in `docs/ARCHITECTURE.md` section 3.

## Agent skills

- Use `mattpocock-skills` for matching tasks; read the selected `SKILL.md` before applying it.
- Read `docs/agents/skills.md` when choosing a workflow or configuring its repo integration.
- Architecture review: `grill-with-docs` uses `grilling` and `domain-modeling`.
- Features/fixes: `tdd`; change review: `code-review`; agent instructions: `writing-for-agents`.
- Specs/tickets: `to-spec` / `to-tickets`; publish only to a user-authorized destination.
- Apply workflows within the user's scope and architecture gate; production work stays production quality.

## Implementation rules

- Go: server, host orchestrator, guest agent, eval engine, API and CLI.
- Python: SDK/harnesses, agents, scorers/judges, quantization and benchmark scripts.
- Share protobuf contracts; use gRPC for Hypercall Go/Python RPCs and vsock to the guest.
- Use external vLLM/LiteLLM serving; Next.js for the dashboard.
- Postgres owns durable state; object storage owns artifacts; ClickHouse holds projections.
- Keep one lifecycle owner; do not combine competing AX and E2B control planes.
- Checkpoints require an agent barrier, committed events, consistent disks and VM state.
- Refresh identity, credentials, transports and model routing before a fork continues.
- Do not claim rollback of external effects or determinism from a VM snapshot alone.
- Preserve LICENSE/NOTICE, copyright and attribution for every copied or translated component.
- Record repository, commit, source/destination paths and changes in `third_party/upstreams.yaml`.
- Check package/file licenses: the E2B Python SDK has its own license.
- Pin toolchains, dependencies, images and artifact revisions; document reproducible commands.
- Add meaningful tests, telemetry, migrations and failure recovery with each behavior change.
- Preserve unrelated edits; keep secrets and local `.omc/` state out of changes.
- Run relevant configured checks; report actual results and gaps. None are configured yet.
- Do not commit, push or perform destructive Git actions unless requested.
- Keep guidance under 60 lines and update concrete commands when implementation establishes them.
