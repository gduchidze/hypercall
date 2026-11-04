# Control-plane development and recovery

Status: implementation complete for the bounded controlled-Python profile; its
real-host Phase 2A workflow passed on 2026-11-03. The full Phase 1B fan-out,
cross-host and live-GCS gate remains open. This result does not qualify arbitrary
agent snapshots, semantic checkpoints, live model routing or production operation.
Acceptance evidence is recorded in [local validation evidence](../validation/2A.md).

## Bounded implementation

The unprivileged Go API and controller own durable desired state in Postgres.
A separate privileged host service owns observed VM state. The controller has one
`Backend` boundary and is the single owner of sandbox lifecycle intent.
Task, Workspace, ModelConfig and Gateway definitions are immutable named resources.
A changed definition gets a new name and ID. Submission transactionally resolves
same-tenant/project ID+digest references and embeds those definitions in the Run.
Run, Branch, Checkpoint and Operation resources are system owned.

The first admitted task uses one model and one root branch. Matrix expansion,
agent barriers, semantic checkpoints and fork lineage are Phase 2B. Template,
agent, dataset and model digests supplied at admission are declared pins; the
host adapter must verify its supported artifacts and reject unsupported inputs
before allocating. They are not evidence of a downloaded artifact or live model.

Task limits are explicit: 1–8 CPU, 128–8192 MiB RAM, 1–1024 MiB workspace and
1–3600 seconds. Commands are bounded to 128 arguments/32 KiB; environment to 64
entries/64 KiB. Workspace content is bounded to 128 files/1 MiB, with checked
content hashes and confined, unique paths. Host capabilities may be narrower.
Pinned repository definitions are represented; an adapter must reject them if
it cannot securely materialize the pinned input. Secrets use references, not values.

## Durability and recovery

Migrations run in a transaction under a database advisory lock. Their checksums
must match on restart. Mutations have tenant/project-scoped idempotency keys and
normalized semantic request hashes. A changed request under an existing key fails.
Resource/event/outbox changes share transactions. Events are ordered per branch;
watch clients resume with the last committed branch sequence. Event IDs deduplicate
retries and reject conflicting content. Outbox delivery is at least once: consumers
deduplicate IDs, and delivery tokens/expiry fence acknowledgements. There is no
global sequence cursor that could skip a later-committing transaction.

A job claim has its own owner, claim epoch and expiry. The execution assignment
has a separate immutable ID, epoch and absolute lease. Reclaiming a job reuses
that assignment, avoiding a second VM on an ambiguous retry. An expired claim
cannot renew or commit. A late observation cannot overwrite a newer cancellation
intent. The host must reject expired epochs and tombstone stopped assignments,
including Stop-before-Ensure. Command/file access requires an authorized, currently
running assignment; clients receive no host or guest credentials.

Execution leases currently have a fixed maximum lifetime, rather than renewable
in-place host leases. Expiry records interruption and durable cleanup. Network or
backend errors leave work visibly pending with a canonical error; uncertainty is
not reported as confirmed termination. Controller restarts reclaim durable work.
An unavailable backend cannot manufacture a running VM or ready checkpoint.
Suspend admission fails explicitly when the configured backend lacks that
capability. The control plane implements resume for snapshot-capable backends;
the host adapter disables snapshots by default and only admits its explicitly
configured `controlled_python_v1` profile when snapshot storage is enabled.
Verified checkpoint metadata retains branch identity, committed event position
and manifest compatibility (template, snapshot format, architecture and protocol). No Firecracker snapshot implies rollback of external effects.

## Setup and operation

Use the pinned toolchains and `make verify`. Install PostgreSQL 17 and provision
a dedicated database and non-superuser database account. Supply its connection
URL through `HYPERCALL_DATABASE_URL`; require verified TLS for a remote database.
For local development only, use a loopback PostgreSQL endpoint with `sslmode=disable`.
Run `go run ./cmd/hypercall-server --migrate-only` before inspecting migration state.

Provision API keys in a private JSON file (0600), mapping keys of at least 32 bytes
to explicit tenant/project grants:

```json
{"replace-with-a-random-32-byte-or-longer-key":{"tenant_id":"internal","projects":["development"]}}
```

Start with `go run ./cmd/hypercall-server --keys-file /absolute/private/keys.json
--insecure-loopback`. Plaintext is accepted only with that explicit flag and
literal loopback listeners. The defaults are gRPC `127.0.0.1:8081` and REST
`127.0.0.1:8080`. Remote listeners require `--tls-cert`, private `--tls-key`, and
`--tls-server-name` matching the certificate. The REST bridge uses the same
authenticated gRPC boundary. Credentials belong in environment/private config,
never committed manifests, command output or logs.

The server defaults to a fail-closed unavailable sandbox backend. Configure the
separate authenticated host adapter only after its artifact and capability checks
pass; see [host-service operations](host-service.md). CLI/SDK setup and examples are in
[CLI and Python clients](clients.md). API/schema/SDK are an
unreleased v1 contract; regenerate Go, Python, REST and OpenAPI together with
`make generate` and reject drift using `make generate-check`.

## Database backup and restore

Back up Postgres with PostgreSQL 17 `pg_dump --format=custom --schema=public
--no-owner --no-privileges`; retain artifact manifests with compatible retention.
Use a dedicated backup role/service and encrypted, access-controlled storage for
real data. The local test drill is not an approved production RPO/RTO.

Before restoring, stop API/controller admission and stop or fence every host and
execution credential. Restoring a DB can roll epochs backwards. Never restart
hosts with capabilities from the old timeline against the restored database;
revoke/reissue host and execution sessions or establish a fresh recovery generation.
Restore into an isolated empty database, verify contents, re-run migrations and
inspect resources/events through the authenticated API before reopening admission.
There is no automatic safe continuation of live VM state after a DB restore.

`scripts/postgres-backup-drill.sh` requires PostgreSQL 17 client tools and explicit
`HYPERCALL_DATABASE_URL`, `HYPERCALL_RESTORE_DATABASE_URL`, and absolute
`HYPERCALL_BACKUP_FILE`. The destination must contain no user tables. It refuses
an occupied destination, restores transactionally and compares row fingerprints.
Stop writers to obtain a stable comparison. The operator provisions the isolated
destination and reviews API/migration checks; the script does not provision cloud
resources or overwrite an existing user database.

## Remaining exit gates

Run `make test-postgres` with `HYPERCALL_TEST_POSTGRES` set to an isolated local
PostgreSQL 17 test DB. The tests use isolated schemas and remove only those schemas.
The target fails when the URL is missing; ordinary local checks can skip DB tests.
The bounded ARM64 controlled-profile real-KVM workflow, including native
suspend/resume and controller restart, now passes. Full Phase 1B fan-out,
cross-host/live-GCS qualification, broader workloads and production telemetry
export remain separate open gates.


## Authenticated sandbox host

Run the privileged host service as a separate Linux process, using a private,
durable bridge ledger and the independently verified runtime artifacts. The API
binary does not require root, KVM devices or host filesystem credentials. Configure
its client with `--host-address`, `--host-ca`, `--host-cert`, `--host-key`, and
`--host-server-name`. All five are required together; the client key must be a
private regular file. Host TLS verifies the server name and controller client
certificate; the host additionally authorizes its configured controller URI SAN.
The host endpoint is fixed to one trusted tenant/project scope; a different
scope is denied rather than rerouted implicitly.

The host verifies its supported template and workspace capabilities before
allocation, records assignment/tombstone state durably, and never renews an
execution past its absolute lease. Snapshot operations are disabled by default.
With explicit snapshot storage and opt-in on both ends, the host accepts only its
pinned `controlled_python_v1` profile; generic foreground snapshots remain
unsupported. This integrates the available Phase 1B implementation without
claiming its runtime gate passed. A configured host URI is connectivity intent,
not successful runtime qualification. Consult the host adapter's
[operating instructions](host-service.md) and real Linux/KVM evidence before
admitting workloads.

## Local acceptance evidence (2026-11-03)

The final local `make verify` and required `make test-postgres` gates both passed
against PostgreSQL 17.6 (Docker ARM64 image digest
`sha256:f3bd19c606e442c3d7bdfa8002e03fe260a1023351e0ea4598032022b68dd6e3`).
The checks include generated-source drift, Go race tests, Python guest/artifact
contracts, three Go build platforms, SDK wheel build and Go vet. Fresh database
suite times were core 7.526 s, public API 4.510 s and host adapter 3.268 s.
`git diff --check` passed. The composed PostgreSQL/public API/controller/mTLS host/
real Go guest test passed with a host lifecycle fixture; it does not boot a VM.

The PostgreSQL 17 backup script was independently executed against a newly
provisioned empty destination. All eight table fingerprints/counts matched.
A separate restored-database check passed migration rerun and authenticated
CLI/REST/Python inspection with ordered events. There were no live host executions
to recover. Snapshot/restore, real KVM end-to-end execution, cross-host recovery,
production RPO/RTO and release qualification remain open; passing these local
checks does not mark Phase 2A complete.


A final host-ledger compatibility check also passed: future, unknown and malformed
ledger schemas fail closed. The independently repeated host race suite passed
(3.349 s), as did host Go vet and whitespace validation. Test servers and the
task-owned PostgreSQL container were stopped after validation; database data and
backup artifacts were retained.


## Phase 2A resume protocol and Phase 1B dependency

The durable control-plane resume protocol is implemented independently of the VM
snapshot engine. Snapshot-capable backend conformance tests exercise it through
Postgres, the public API, Go CLI and sync/async Python clients using an explicitly
synthetic state fixture. No cold sandbox creation substitutes for restore.

Successful suspension requires a verified compatible manifest and a locally
fenced predecessor. The transaction retains a scoped checkpoint and freezes the
remaining active budget using the database clock, including time spent suspending.
A completed suspended branch can outlive its old execution lease. A hung suspension
still expires and queues cleanup. Checkpoint cursor zero means the controller
assigns its committed branch cursor; contradictory nonzero cursors or branch IDs
are rejected. This cursor is not proof of a semantic agent step barrier.

Resume requires a completed suspension, retained same-branch compatible checkpoint
and positive remaining budget. Admission selects that checkpoint into the durable
Operation/job, creates a new execution ID and fencing epoch, and sets an absolute
lease from the frozen budget. Time suspended does not consume active budget, and
resume never resets the task's original full timeout. Controller restarts and lost
backend responses retry the exact checkpoint, execution, epoch and deadline.
The backend must refresh guest identity, credentials and transports before release.
Cancellation tombstones the new assignment even if restore is still in flight.
An accepted idempotency replay remains retrievable after a restart with a backend
that no longer advertises snapshots; only new intents require that capability.

Migration 002 adds scoped checkpoint references and nonnegative remaining-budget
state without changing migration 001. The migrator checks every retained checksum,
rejects gaps/future versions and upgrades an existing v1 database transactionally.
Backups from schema 1 can be restored and upgraded; an older binary must refuse a
schema 2 database. Keep an offline backup before upgrade and use a compatible
binary when restoring. The backup/restore fencing requirements above still apply.

| Capability | Can be implemented/qualified without Phase 1B VM snapshots? |
| --- | --- |
| Tenant resources, immutable resolution, auth and API/CLI/SDK | Yes; local contracts tested. |
| Postgres jobs, leases/epochs, idempotency, events/outbox and recovery | Yes; real PostgreSQL and fault fixtures tested. |
| Durable suspend/resume admission, checkpoint selection and fresh assignment | Yes; snapshot-capable synthetic backend conformance tested. |
| Migration and database backup/restore compatibility | Yes; real PostgreSQL upgrade and recovery drills passed. |
| Actual memory/disk continuity and fresh guest sessions | Controlled-profile single-continuation real-KVM workflow passed; fan-out/cross-host qualification remains open. |
| Agent step barriers, semantic lineage, forks and model-routing comparisons | Phase 2B; outside this Phase 2A increment. |

The host disables snapshots by default. Its controlled-Python opt-in integrates
the native Phase 1B code, whose runtime qualification remains incomplete.
Completing this protocol therefore removes an independent Phase 2A code gap;
it does not complete the Phase 1B runtime prerequisite or the full real-VM exit gate.

The resume follow-up independently passed the final full gates: `make verify`,
`make test-postgres` and `git diff --check`. Fresh PostgreSQL suite times were
core 12.485 s, public API 5.391 s and host adapter 3.233 s. A retained schema-1
fixture upgraded to schema 2 without changing its resource/event/job data, and a
fresh schema-2 backup/restore passed all eight table fingerprints, migration rerun
and authenticated CLI/REST/Python inspection. The host's snapshot
capability was disabled for that drill; these results qualify the durable protocol and synthetic
state conformance. The later controlled-profile VM workflow below also passed;
broader fan-out and cross-host continuity still require separate Phase 1B evidence.
The task-owned test servers and database container were stopped with data/backups
retained; no shared development VM was touched.


## Native Phase 1B implementation integration

The native snapshot implementation is now present from the first-party Hypercall
`sandbox-foundation` commit `746deaa1d88dac5e8bdf7111b0399ff5b27c5df7`.
Its code was selectively integrated while preserving the control-plane changes;
no third-party source or competing lifecycle scheduler was introduced. Availability
of that code does not establish its qualification. The owning snapshot guide
reports local fan-out continuity failures; compatible cross-host restore and live
GCS qualification have not run. Do not use a passing control-plane fixture as
VM-continuity evidence.

A Task may explicitly select `execution_mode: controlled_python_v1` and a
nonnegative `controlled_counter`. This profile runs the guest's pinned built-in
controlled Python program; arbitrary command/environment overrides are rejected.
The default empty mode and `foreground_v1` retain ordinary foreground commands,
which the real host rejects for snapshot operations. The controlled host verifies
the agent digest against its configured guest artifact before allocation.

Snapshot admission evaluates the resolved Task through `SupportsRun` after scoped
lookup and idempotency replay. Therefore a host that supports the controlled
profile cannot accidentally admit a generic foreground snapshot, and an accepted
operation remains retrievable if capability configuration later changes. Both
ends require opt-in: API/controller `--host-controlled-snapshots` with its mTLS host
configuration, and host `--controlled-snapshots` with an explicit local
`--snapshot-store` or authorized GCS configuration. These are unqualified
implementation capabilities, not permission to provision storage or run VM tests.

Migration 003 gives every new execution a physical `sandbox_id` distinct from the
logical Branch and execution IDs. Resume allocates another fresh physical ID,
required by the native guest clone/rebind protocol. Existing pre-v3 branches are
backfilled with their former physical identity (the Branch ID), preserving retained
host allocations rather than silently rebinding them. Claims, status, proxy calls
and completion fencing carry the durable physical ID. Migrations 001/002 remain
byte-for-byte unchanged; older binaries reject schema 3.

Native checkpoints retain their native checkpoint ID and exact manifest JSON bytes.
When native metadata is present, the core requires a bounded JSON object, valid
native ID and a SHA-256 manifest digest matching those bytes. The real host also
verifies native scope, compatibility and retained objects before restore. Legacy
synthetic conformance checkpoints can omit native fields; they remain fixtures.
Capture success is not suspension until the host confirms the predecessor stopped.
Native restore uses its journaled request and absolute execution expiry so retries
cannot reset TTL, mutate the request or resurrect an expired assignment.

See [snapshot implementation and qualification](snapshots.md) and
[host-service operations](host-service.md) for the supported controlled profile,
artifact/store setup and the unresolved runtime gates. The implementation phase did not access a shared development VM; the later
runtime qualification used the existing VM only after explicit user release.


## Coordinated checkpoint retention

Before reporting a native checkpoint suspended, the host acquires a persistent
snapshot-store reference named `retained-<native-checkpoint-id>` and confirms
predecessor termination. That pin survives a lost control-plane commit response,
controller restart and cancellation. An RPC timeout is not evidence that the
checkpoint is unreferenced, so it does not release the pin or delete artifacts.
The host ledger and Postgres may temporarily disagree after an ambiguous response;
recovery retains the manifest and artifacts until ownership is resolved.

Conservative orphan retention is intentional. There is no automatic coordinated
control-plane deletion/garbage collector in this increment. Operator cleanup must
quiesce writers and reconcile retained Checkpoint/Branch/Operation/job records,
host ledgers, native store references and backup retention before explicitly
releasing a retained reference or deleting objects. Preserve child/restore
references and active leases. A stopped parent, cancelled operation or expired
execution alone does not authorize artifact deletion. Follow the snapshot store's
reference-aware deletion procedure; do not delete blobs behind its reference pins.

The schema-3 upgrade was independently run against a retained schema-2 fixture
DB: all seven preexisting data tables were unchanged, and all 13 retained branches
kept their legacy physical identity. Migrations 001/002 checksums were unchanged.
A fresh schema-3 backup/restore matched all eight table fingerprints and passed
migration rerun. This is database recovery evidence, separate from the subsequent successful
controlled-profile real-VM Phase 2A workflow.

Final native-integration local gates also passed: `make test-postgres` (fresh core
13.822 s, public API 6.663 s, host adapter 5.406 s), `make verify` (generation,
all Go race tests, thirteen Python contract/operator tests, two artifact tests,
three cross-builds, wheel 0.2.0 and Go vet), and `git diff --check`. These results
include the native snapshot-store implementation and host conformance tests.
The [Phase 2A runtime harness](phase2a-runtime.md) subsequently passed the bounded
controlled-profile actual VM workflow. Full Phase 1B fan-out, cross-host and live
GCS qualification remain open; local tests do not substitute for those gates.


## Bounded Phase 2A real-host workflow passed

After explicit user authorization to use the existing development VM,
`make test-runtime-2a` passed the complete controlled-Python workflow on real
ARM64/KVM with official Firecracker 1.14.0, the existing pinned Phase 1B artifacts,
mTLS host RPC and local snapshot storage. YAML submission, commands/files, native
suspend, resume after controller restart, retained running assignment after another
restart, REST/ordered events and confirmed cancellation all passed. The full-memory
checkpoint retained counter 42 and a binary disk sentinel. Resume used a fresh
physical sandbox, execution ID and epoch; the predecessor was confirmed stopped.

This supplies actual VM evidence for the bounded Phase 2A controlled profile,
alongside the database/fencing/auth/client/backup tests above. It does not resolve
the separate Phase 1B fan-out continuity failures, cross-host/live GCS qualification,
general agents, semantic checkpoints or production-release gates.
See [runtime evidence and limits](phase2a-runtime.md).


The final controlled-profile result also passed SDK replay of the original CLI
submission (same Run, unchanged run count, no new VM) and an offline backup/restore
of its actual runtime database. All eight table fingerprints matched; migration
rerun and restored API/SDK inspection retained the native checkpoint, cancelled
branch and contiguous events. The final full verification suite passed after the
harness fix. These are additional durable-recovery checks, not broader Phase 1B
fan-out, cross-host or live-GCS qualification.

Cleanup is confirmed: the task's VM cgroup was empty, its host service and tunnel
stopped, the ephemeral host private key removed, and the development VM returned
to its original stopped state. The other stopped VM was untouched. All task-owned
API servers and the PostgreSQL container were stopped. Private checkpoint/ledger
state and database backups were retained for recovery, without copying secrets
into the repository.
