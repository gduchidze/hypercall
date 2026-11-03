# Phase 0 — Architecture decisions

Status: ready for Phase 1A; the user explicitly authorized implementation on 2026-11-03.
Date: 2026-11-03.

This document tracks the design interview for [ARCHITECTURE.md](ARCHITECTURE.md)
and phase 0 of [DEVELOPMENT_PHASES.md](DEVELOPMENT_PHASES.md). The subsequent
user instruction marked Phase 0 ready and requested implementation. That
authorization permits the bounded local Phase 1A work below; it does not answer
unresolved deployment, spending or evaluation choices. Earlier interview entries
remain the historical record.

## Established project constraints

The user has already specified these constraints; they are not interview defaults:

- Build a production-grade system using selected upstream code as grounding.
- Ground the sandbox in E2B runtime, the Python SDK in E2B, the control plane
  in AX, and the Go evaluation engine in Promptfoo semantics and fixtures.
- Preserve upstream licenses/notices, attribution, and exact imported revisions.
- Use Go server-side, Python agent-side, shared protobuf/gRPC, Firecracker/jailer,
  vsock guest communication, external vLLM/LiteLLM, and a Next.js dashboard.
- Use Postgres, ClickHouse, object storage, OpenTelemetry, Prometheus/Grafana,
  Kubernetes/Helm/Terraform, GCP sandbox hosts, and RunPod/Modal GPU options.
- Build sandbox first, then control plane, evaluation, inference and product operations.
- Complete architecture review before importing or implementing code.

Specific cloud topology, compatibility promises, numerical operating limits and
statistical guarantees remain bounded deferrals rather than assumed answers.

## Round 1 — Product boundaries and constraints

The user answered round 1. Accepted direction below is distinct from approval
of the complete architecture.

| ID | Decision | Recorded answer | Status |
| --- | --- | --- | --- |
| Q1 | First release audience | Internal team first; tenant boundaries included from the start | Accepted |
| Q2 | First supported workload | Python confirmed; coding-agent task, harness, and dataset remain unspecified | Partially resolved |
| Q3 | Evidence needed for model selection | Whole-task reliability comparisons and checkpoint continuations, reported separately | Accepted |
| Q4 | Initial benchmark data and provider access | Public/synthetic inputs; self-hosted and API models permitted; private-data egress requires a separate policy | Accepted |
| Q5 | Spending and release policy | Develop using cloud-service credits; no production operation before investment | Direction accepted; credit availability, eligibility, expiry, caps, and overage policy unresolved |

### Architectural consequences

- Build and validate in development/staging before investment; retain the
  production-quality engineering requirement.
- Production deployment is gated on investment and subsequent release qualification.
- Use whole-task runs for reliability claims and checkpoint continuations for
  diagnosis; preserve the distinction in reports.
- API-model access is permitted for the initial data policy, but funding and
  provider-specific credit coverage must be established before paid calls.
- The updated architecture proposes a small, disposable development environment.
  Its precise topology and operating envelope still require review.
- The user's follow-up requests an architecture based on these answers; it does
  not supply the unanswered workload, budget, or deployment details.

## Decision tree for subsequent rounds

Ask a decision when its prerequisites are settled. New answers can add branches;
this map is a coverage checklist, not an approval of the proposed defaults.

| Area | Decisions to resolve | Prerequisites |
| --- | --- | --- |
| Agent contract | First harness, task repository/dataset, step definition, supported adapters, background work | Q2 |
| Tool effects | Fixture/live policy, branch-private services, idempotency, ambiguous effects, network access | Q2, Q4 |
| Experiment design | Baseline, candidate matrix, whole-task vs continuation protocol, repetitions, judge/scorer role, CI rule | Q2, Q3, Q4 |
| Checkpoint semantics | Retained step boundaries, cadence, parent continuation, artifact contents, restore compatibility, replay labeling | Agent contract, tool effects |
| Branching | Nested forks, partial fan-out, lineage retention, identity renewal, model swap acknowledgment, cancellation | Checkpoint semantics |
| Control-plane ownership | AX adaptation boundaries, sandbox contract, authority, reconciliation, leases/fencing, event ordering | Agent contract, checkpoint semantics |
| Data model and APIs | Resource scope, immutable revisions, protobuf versions, idempotency, streaming, SDK support surface | Ownership, branching |
| Security and tenancy | Trust model, auth/roles, tenant isolation, secret delivery, quotas, audit and deletion | Q1, Q4, tool effects |
| Deployment | Initial region, KVM host pool, GPU provider/location, networking, store adapters, deployment roles | Q4, Q5, security requirements |
| Capacity and cost | Concurrent runs, fan-out, VM size, task duration, GPU capacity, snapshot cost, retention | Q2, Q5, deployment, checkpoint semantics |
| Recovery | Interruption/retry policy, data-loss tolerance, RPO/RTO, host partitions, corrupt artifacts, restore drills | Ownership, checkpoint semantics, deployment |
| Release qualification | Quality/reliability thresholds, latency targets, supported envelope, upgrade/rollback, SLO evidence | Experiment design, capacity, recovery, Q1 |
| Upstream integration | Import strategy, compatibility subset, version updates, conformance tests, provenance and licenses | Relevant component contracts |

## Recording decisions

- Record the answer, rationale, constraints, and dependent questions after each round.
- Use root `GLOSSARY.md` for settled domain terms, with no implementation details.
- Use concise `docs/adr/NNNN-*.md` records for consequential, non-obvious trade-offs.
- Keep numerical requirements separate from measured results. Estimates do not
  establish capacity or performance guarantees.
- If a required fact is unknown, investigate it; ask the user for preferences and
  constraints rather than for facts available from code, tooling, or primary sources.
- Leave a decision pending when an answer is incomplete or its prerequisite changes.

## Completion criteria

- Every architectural area above has an explicit decision, or a bounded deferral
  with a named stage, trigger, and safe interim contract.
- The first acceptance workload, data policy, spending caps, capacity envelope,
  and evaluation/release requirements are specified sufficiently to guide delivery.
- Architecture, roadmap, glossary, and justified ADRs agree with the user's answers.
- Remaining implementation experiments have acceptance criteria and do not hide
  unresolved product decisions.
- The user confirms the shared understanding and approves the architecture before
  phase 1 implementation begins.

## Implementation authorization and bounded deferrals

On 2026-11-03 the user explicitly marked Phase 0 ready and requested Phase 1
implementation. The first acceptance workload is local synthetic command/file
fixtures through the shared guest gRPC interface, including a real Python client
against a Go guest. These fixtures make no model-quality or isolation claims.
The sandbox driver and guest RPC are the agreed test seams. Phase 1A must still
pass its real Linux/KVM exit gate before Phase 1B starts.

| Deferred choice | Required stage/trigger | Safe interim contract |
| --- | --- | --- |
| Cloud provider credits, region, residency, numerical caps and overage | Before any cloud provisioning or paid model call | Local development only; no authorized spend |
| Production deployment and SLOs | Phase 4C, investment and release authorization | No production operation or availability claim |
| VM capacity, concurrency, lifetime and resource envelope | Phase 1A KVM qualification | Explicit local test resources; no measured capacity claim |
| Harness, task dataset and step definition | Before Phase 2B semantic checkpoints | Synthetic foreground commands/files only |
| Snapshot retention, fan-out, continuation and recovery limits | Before dependent Phase 1B work | No snapshot/fork correctness claim |
| API/GPU provider, grading, repetition and CI quality thresholds | Before Phase 3/4A dependent work | Deterministic fixtures; no model comparison claims |
| Private inputs and external tool effects | Before private data or live tool access | Public/synthetic inputs; no external effects |

The narrow next steps and their acceptance commands are in [PHASE_1.md](PHASE_1.md).
Actual results and outstanding exit-gate evidence belong in [validation/1A.md](validation/1A.md).
