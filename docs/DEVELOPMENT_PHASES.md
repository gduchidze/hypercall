# Hypercall development phases

Status: Phase 0 marked ready by the user; Phase 1A implementation in progress.
Date: 2026-11-03.

This roadmap expands the build order in [ARCHITECTURE.md](ARCHITECTURE.md).
That document owns design decisions and exact inspected upstream revisions.
Phase completion requires working code and recorded acceptance evidence.
The user explicitly authorized the bounded Phase 1A implementation on 2026-11-03;
unresolved operating choices remain recorded in PHASE_0.md.

## Delivery sequence

| Stage | Outcome | Depends on |
| --- | --- | --- |
| 0 | Reviewed architecture and first workload | User decisions |
| 1A | Reproducible sandbox runtime: boot, commands, files, stop | 0 |
| 1B | Durable snapshots and isolated VM forks | 1A |
| 2A | Durable control plane, API, CLI, Python SDK | 1B |
| 2B | Agent step checkpoints, branch lineage, routing and comparison | 2A |
| 3 | Evaluation engine, reports and CI decisions | 2B |
| 4A | Real model/quantization matrix and validated measurements | 3 |
| 4B | Dashboard and production deployment packaging | 4A |
| 4C | Qualified multi-tenant production release | 4B |

Development and validation before investment run in credit-funded development
or staging environments. Production operation requires investment and the phase
4C release gate. Credit availability, eligible services, expiry, and spending
limits must be established before provisioning; this roadmap assumes no grant
has already been awarded. The first workload is Python; coding tasks remain a proposal.

Stages 1A/1B, 2A/2B, and 4A/4B/4C subdivide architecture phases 1, 2, and 4.
Production quality applies throughout: typed contracts, meaningful tests,
observable behavior, reproducible builds, tenant boundaries, and failure recovery.
No calendar estimates are committed before the upstream extraction and host
qualification work establishes scope and available engineering capacity.

## Phase 0 — Agree on the first supported workflow

Track interview rounds and unresolved decisions in [PHASE_0.md](PHASE_0.md).

**Deliverables**

- Review the architecture, especially checkpoint semantics and upstream boundaries.
- Select the first Python agent/harness and a pinned task repository or dataset.
- Define success through executable tests/assertions and explicit failure rules.
- Record initial region, data residency, external-provider policy, and spending caps.
- Set an initial capacity envelope: VM sizes, concurrent runs, fork fan-out,
  checkpoint frequency, retention, and expected task duration.
- Record accepted decisions and open questions in architecture decision records.

**Recommended first workload:** an internal coding agent on a controlled repository,
with branch-private files and fixture-based external tools. Model comparisons
arrive after sandbox and harness correctness is established.

**Exit gate:** user reviews the architecture and agrees on the acceptance scenario,
environment, and limits. Unresolved choices that do not affect the current stage
can remain documented; implementation begins only after the review gate is met.

## Phase 1A — Establish the sandbox runtime

**Grounding:** E2B runtime `packages/orchestrator`, `packages/envd`, and supporting
packages; E2B Python SDK command/file behavior. Use architecture section 3 pins.

**Deliverables**

- Audit the selected source/dependency graph; choose the actual import revisions
  and record copied paths, licenses, notices, and modifications.
- Establish Go/Python toolchains, dependency locks, protobuf generation, build
  commands, CI checks, and a real Linux KVM integration environment.
- Implement the sandbox backend contract and a test driver without introducing
  a second product scheduler or public control plane.
- Adapt Firecracker/jailer lifecycle, guest communication over vsock, and isolated
  template building with pinned kernel, rootfs, guest agent, and image digests.
- Support boot/create, command execution, file transfer, cancellation, and stop.
- Enforce tenant scope, host/guest identity, resource limits, restricted networking,
  secret handling, lifecycle traces, and reliable cleanup.

**Exit gate:** a clean checkout builds reproducibly and passes real KVM tests for
commands/files, cancellation, unauthorized access, limits, guest/host failures,
and cleanup. Contract tests verify Go/Python interoperability. Record host
configuration, timings, and resource consumption alongside the test results.

## Phase 1B — Make snapshot, restore, and fork correct

**Grounding:** E2B runtime snapshot storage, restore implementation, and
`packages/api/internal/handlers/sandbox_fork.go`.

**Deliverables**

- Capture consistent memory, VM state, and disks at a controlled test barrier.
- Publish checksummed immutable artifacts and manifests to GCS, with compatibility
  checks, staging cleanup, reference-aware retention, and private child disks.
- Implement restore and fan-out with idempotent requests and per-child results.
- Renew clone identity, credentials, transports, networking, and RNG policy.
- Measure a fully materialized baseline before qualifying lazy/incremental paths.

**Acceptance scenario:** a Python process holds a counter in memory and writes a
file, then waits at a barrier. Two restored children inherit the captured state,
continue under fresh identities, and independently modify memory and files.

**Exit gate:** demonstrate parent/sibling write isolation, compatible cross-host
restore, safe retry after partial fan-out, corrupt/missing artifact rejection,
interrupted upload recovery, and cleanup after crashes. Record a supported
concurrency/fan-out envelope. This stage proves VM continuity; semantic agent
step checkpoints are completed in phase 2B.

## Phase 2A — Add the durable control plane

**Grounding:** AX server, controller, schemas, store/lock interfaces, workspace
setup, and CLI; E2B Python SDK lifecycle and error handling.

**Deliverables**

- Replace AX's Agent Substrate dependency in reconciliation and status reads with
  the Hypercall sandbox backend. Keep one authoritative lifecycle owner.
- Introduce Task, Workspace, ModelConfig, Gateway, Run, Branch, Checkpoint, and
  Operation resources, scoped by tenant/project with immutable resolved inputs.
- Implement Postgres migrations, durable jobs, leases/epochs, event metadata,
  transactional outbox, idempotency, cancellation, and recovery reconciliation.
- Generate gRPC/REST contracts and Python clients; expose the Go CLI and SDK
  for submit, inspect, events, suspend/resume, cancel, commands, and files.
- Document setup, operation, database backup/restore, and compatibility rules.

**Exit gate:** submit YAML through the CLI and SDK, execute the sandbox, inspect
ordered events, and recover after controller restart. Verify duplicate requests,
stale-epoch rejection, lease expiry, network partitions, cancellation races,
authorization, migration behavior, and a Postgres backup/restore drill.

## Phase 2B — Add agent checkpoints and trajectory branching

**Grounding:** the phase-1 sandbox backend, AX-derived lifecycle logic, and
Hypercall's checkpoint protocol in architecture section 6.

**Deliverables**

- Build cooperative Python harness hooks for steps, events, barriers, and resume.
- Implement branch-aware inference/tool routing and a durable call ledger using
  deterministic inference fixtures initially.
- Drain calls, commit the final event position, snapshot at the resume gate, and
  release each child only after its new routing and identity are acknowledged.
- Retain checkpoint lineage and ordered subevents; implement declared comparators
  for output, tool calls, observations, and first observed divergence.
- Label replay, missing instrumentation, unknown effects, and incomplete traces.

**Exit gate:** checkpoint a real instrumented agent, restore two branches, route
them to distinct deterministic fixtures, and identify the expected divergence.
Verify that no child calls the parent's provider mapping, no unresolved tool call
is silently checkpointed/replayed, and recovery preserves committed lineage.

## Phase 3 — Add evaluation and CI reporting

**Grounding:** Promptfoo evaluator, assertions, configuration/types, and fixtures
at the recorded upstream revision. Publish the supported compatibility subset.

**Deliverables**

- Port selected matrix, case-variable, assertion, weight/threshold, and aggregation
  semantics to Go; reject unsupported expressions and configuration explicitly.
- Add isolated Python scorer workers and LLM-judge contracts with deterministic
  test doubles, pinned rubrics, deadlines, limits, and error results.
- Produce machine-readable and readable reports with scores, failures, latency,
  measured/estimated costs, trace links, repetition counts, and uncertainty.
- Add CI exit rules for regressions, budgets, execution/evaluation errors, and
  incomplete matrices; preserve distinct failure reasons.
- Project durable events into ClickHouse with deduplication and visible lag.

**Exit gate:** differential tests match the declared Promptfoo subset; scorer
timeouts and missing results cannot pass. Validate aggregation, shared-prefix
accounting, synthetic Pareto examples, and CI decisions against known fixtures.
Analytics outages must not lose authoritative results. Fixture measurements are
labeled synthetic and make no claims about real model quality or serving cost.

## Phase 4A — Qualify the inference and quantization matrix

**Deliverables**

- Connect authenticated vLLM deployments and LiteLLM API-provider routes.
- Pin available model/weight, tokenizer, quantization, serving, GPU, and sampling
  configuration; record undisclosed provider revisions and actual deployments.
- Prevent hidden model fallbacks and record every retry, cancellation, token
  measurement, pricing input, GPU allocation, and relevant network overhead.
- Enable real Python judges under the scorer policies established in phase 3.
- Run repeated whole-task and continuation experiments with declared cache/load
  conditions, balanced execution order, and held-out validation workloads.
- Produce Pareto reports with comparable quality/cost/latency scopes and qualified
  reliability thresholds; retain failed and missing cells under explicit policy.

**Exit gate:** reproduce reports from retained artifacts; verify model routing and
billing/estimate reconciliation. Each quantization candidate passes its declared
quality and serving-compatibility checks. Winners meet the agreed reliability
rule on held-out tasks; one successful run is insufficient evidence.

## Phase 4B — Deliver the dashboard and deployment packages

**Deliverables**

- Build the Next.js task submission, run timeline, branch diff, matrix, reports,
  and operational views against the public REST API.
- Package control-plane services, workers, and telemetry with Helm; provision GCP
  host pools and supporting infrastructure with Terraform.
- Automate host/guest artifact rollout, remote GPU endpoint configuration,
  secret references, capacity limits, and version compatibility checks.
- Provide dashboards, alerts, audit access, runbooks, and documented release,
  upgrade, rollback, retention, and disaster-recovery procedures.

**Exit gate:** deploy to a clean staging environment from documented inputs,
complete the supported workflow through UI/CLI/SDK, and prove upgrade/rollback
without breaking retained artifacts or active-version compatibility. User-facing
views clearly distinguish running, failed, incomplete, and evaluated work.

## Phase 4C — Qualify the production release

**Entry condition:** investment has been secured and production deployment is
authorized. Pre-production recovery/security exercises may happen earlier.

**Deliverables**

- Complete tenant onboarding, roles, scoped credentials, admission quotas, spend
  controls, and fair scheduling on the boundaries present since phase 1.
- Establish measured SLOs, capacity/fan-out limits, retention policies, and RPO/RTO.
- Execute load/soak, tenant isolation, partition/fencing, host-loss, dependency-loss,
  backup/restore, artifact deletion, and incident-response exercises.
- Ship signed/versioned artifacts, SDK releases, SBOMs, license attribution,
  operating documentation, and a tested compatibility matrix.

**Exit gate:** all agreed release criteria have linked evidence, recovery drills
meet the approved RPO/RTO, isolation tests pass, alerts are exercised, and remaining
limitations are explicit. Release first to the agreed internal users, observe the
declared workload envelope, then expand access under the same admission controls.

## Tracking and completion rules

- Start each stage with a bounded issue list, dependency map, and acceptance commands.
- Record completion evidence in `docs/validation/<stage>.md`: commit, environment,
  upstream revisions, checks/results, measured limits, and unresolved limitations.
- Update architecture/ADRs when evidence changes a design assumption; obtain user
  review for material scope changes before implementing the dependent work.
- Keep cost caps, compatibility, observability, and recovery checks in each stage.
- A passing unit suite alone does not qualify Firecracker, isolation, restore,
  inference reliability, or production readiness; run the relevant real integrations.
- Do not mark a stage complete when required evidence or acceptance criteria are missing.

The current action is the bounded [Phase 1A implementation](PHASE_1.md), including
source/dependency audit and Linux/KVM qualification. Phase 1B has not started.
Track actual exit-gate evidence in [validation/1A.md](validation/1A.md).
