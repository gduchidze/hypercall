# Phase 3 validation evidence

Date: 2026-11-04. This record covers bounded local evaluation acceptance.
No cloud resource or paid provider call was used. The unresolved Phase 1B
simultaneous fan-out, cross-host and live-GCS gates remain unchanged.

The complete local evaluation, durable API, isolated-scorer and analytics checks
passed after the report-bound and nonfinite scorer-cost corrections. A clean
source export also passed the configured build and verification checks. These
results qualify synthetic evaluation and recorded branch-response assertions.
Remote CI status is tracked separately by the pull request checks.

## Environment and dependency pins

- macOS ARM64, Go 1.27.3, Python 3.13.9 and uv 0.12.11.
- Docker Desktop Linux engine 29.7.2, PostgreSQL 17.6 and ClickHouse 25.8.3.66.
- PostgreSQL image index:
  `sha256:f3bd19c606e442c3d7bdfa8002e03fe260a1023351e0ea4598032022b68dd6e3`.
- ClickHouse image index:
  `sha256:707dcb39a8314977492b9b17a718f2aefb7ce9f27b8098c8b703d2a9f919007b`.
- Scorer Python 3.12.12 image index:
  `sha256:593bd06efe90efa80dc4eee3948be7c0fde4134606dd40d8dd8dbcade98e669c`.
  Scorer wheels and hashes are pinned in `deploy/scorer/requirements.lock`.
  Worker/image changes require rebuilding and requalifying the scorer image.

## Recorded local checks

| Check | Result | Scope |
| --- | --- | --- |
| `make verify` | Passed in the working tree and a clean source export | Generated contracts, Go race tests, Python contracts/artifacts, cross-platform builds, SDK wheel and vet. Integration tests without required environment explicitly skip here. |
| `make test-eval` | Passed | Fixed conformance, real evaluation/durable/API/projection tests, OCI scorer runtime, isolated evaluation adapter and public API + Postgres + OCI grading. |
| `make test-postgres` | Passed | Real durable/control-plane, authenticated API and host API suites. |
| Fixed evaluation conformance | Passed | 56 assertion cases and four matrix scenarios checked against retained expected values. |
| Evaluation suite | Passed in 2.466s | Public Compile/Evaluate conformance, aggregation, accounting and CI policy. |
| Control-plane/API suites | Passed in 21.317s / 7.797s | Durable admission, recovery, tenant scope, authoritative reports and public clients. |
| ClickHouse projection | Passed in 2.105s | Scoped lag, outages, retry deduplication, catchup, rebuild and stale receipt fencing. |
| Scorer runtime / evaluation adapter | Passed in 22.100s / 6.182s | Isolation, watchdog deadlines after evaluator death, errors, costs, cleanup and nonpassing adapter results. |
| Public API + Postgres + OCI grader | Passed in 4.522s | Pinned synthetic scorer, durable evaluation, real isolated grading and authenticated report retrieval. |
| Report bounds / nonfinite costs | Passed | A 1,000-cell adversarial report and report/inspection responses stay below 4 MiB; nonfinite scorer costs fail closed. |
| Projector drain and status | Passed | Standalone startup, migration, drain and Postgres-only scoped status. |

The admission envelope permits 1,000 cells, 128 candidates and 4,000 assertion
applications. The payload regression uses escaped reasons and cost-source strings.
A 128 KiB escaped-JSON detail budget retains amounts, scores and error categories
while marking omissions and preserving cost-source digests.

Projection tests inject HTTP failure around an actual ClickHouse server. They
verify reports remain retrievable from Postgres during outages, backlog and age
remain visible, an ambiguously acknowledged committed insert retries to one
logical event, restarted projectors catch up, rebuild preserves results, lifecycle
consumer receipts remain independent, and superseded tokens cannot acknowledge
replacement receipts.

## Reproduce the composed gate

Create dedicated local test databases and install `deploy/clickhouse/schema.sql`
in the selected ClickHouse database. Set `HYPERCALL_TEST_POSTGRES`,
`HYPERCALL_TEST_CLICKHOUSE_URL`, `HYPERCALL_TEST_CLICKHOUSE_DATABASE`,
`HYPERCALL_TEST_CLICKHOUSE_USER`, and `HYPERCALL_TEST_CLICKHOUSE_PASSWORD` using
dedicated test credentials. Select the pinned Go and uv toolchains, then run:

```sh
make verify
make test-eval
```

`test-eval` fails when a required database or Docker seam is absent. It checks the
fixed evaluation corpus and real database suites, builds the pinned scorer image,
and qualifies its runtime, evaluation adapter and composed public grading path.
The `phase3-evaluation` CI job supplies pinned Postgres/ClickHouse services,
initializes ClickHouse and runs the same target.

## Qualification boundaries

The workload is synthetic. Real model quality, production serving latency,
provider billing, live LLM judges, held-out workloads, capacity, availability and
production multi-tenant scorer isolation require their own qualification. Real
inference and live judge qualification belongs to Phase 4A. Reports retain scope,
synthetic cost provenance, missing evidence and explicit CI reasons.
