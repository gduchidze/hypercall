# Snapshot artifact storage

`internal/snapshot` owns artifact publication, integrity and storage lifetime pins.
It does not schedule sandboxes, create branch lineage, authenticate callers, or
replace Postgres checkpoint readiness. The host passes an authenticated
`Scope{TenantID, ProjectID, CheckpointID}`; strings alone are not authentication.

## Fully materialized baseline

`Publish` requires exactly three nonempty regular-file sources: `memory`,
`vmstate`, and `disk` (the private writable workspace disk). It hashes each whole
file with SHA-256, creates immutable objects, downloads and verifies each uploaded
object, and creates the manifest only after verification. The CAS lifetime record
changes to `ready` last. A manifest or partial upload without that state is
unavailable. The host/controller may declare a checkpoint ready only after this
operation succeeds. Source capture/quiescence remains the runtime's responsibility.
Sources are opened with `O_NOFOLLOW|O_NONBLOCK`, checked through descriptor stat,
and limited to nonempty regular files of at most 16 GiB each. FIFO and symlink
sources are rejected promptly. The same descriptor is retained through hashing
and upload, so replacing a pathname cannot change which file is published. Hashing
includes the whole logical file, including sparse zero ranges. Changes to a file's
bytes or length during publication fail verification rather than publishing a
mismatched manifest.

The captured session disk is excluded: a restored child needs a newly created
session disk. Memory and disks can still contain sensitive tenant state; all
objects, local materializations and operator credentials must stay private.

Object names are scoped beneath:

```
tenants/<tenant>/projects/<project>/checkpoints/<checkpoint>/
  lifetime
  manifest
  staging/<random-upload-id>/{disk,memory,vmstate}
```

`staging` object names remain immutable after successful publication; a ready
manifest's references make them retained artifacts. Do not configure bucket
lifecycle rules that delete this prefix by age alone. There is no cross-checkpoint
content deduplication, lazy loading, incremental layer chain, or live compression.
Each manifest records schema version, parent checkpoint (if supplied), upload ID,
artifact kind/key/size/SHA-256, and complete compatibility metadata. No source
paths or host credential values are serialized. Publication retries with the same
scope and content return the existing manifest; different content or an active
publication returns a conflict.

`OpenVerified` rejects unknown JSON fields, unsupported schemas, metadata
integrity failures, missing/corrupt artifacts and any compatibility difference.
Compatibility compares architecture, CPU identity/configuration hash, host kernel,
Firecracker/snapshot versions, template/rootfs/kernel/guest hashes, device layout,
barrier protocol/configuration hash, RNG policy, vCPU count and RAM. Materialized
files are private copies, verified before return and made read-only. `Verified.Close`
removes their temporary directory. The runtime owns crash cleanup of its local
materialization/cache directories; never remove files backing a running VM.
Use `OpenVerifiedIn` with an absolute existing owner-private directory (mode 0700)
to confine each backend's materializations beneath its own StateDir. It changes
only per-call options, so sharing a Store cannot redirect another backend's cache.
After acquiring its exclusive host lease and confirming old VMs are terminated,
the backend may clean its own abandoned `snapshot-*` directories. Never sweep a
shared/global temporary directory.

## Pins, retention and abandoned uploads

Acquire an idempotent `AcquireReference(scope, referenceID)` pin **before** opening
a snapshot. Retain it until the VM stops using the snapshot files; release it only
after process termination is confirmed. Pins do not expire, because an expired
controller lease does not prove that a VM has stopped using its memory backing.
Host allocation recovery must reconcile orphaned pins from its durable journal.
Releasing an unknown pin or a missing checkpoint is idempotent; acquisition still
requires a ready checkpoint.
Each checkpoint supports at most 1,024 pins in this bounded baseline.

`DeleteUnreferenced` checks pins and CAS-transitions the lifetime record to a
permanent `deleting` tombstone before removing objects. A concurrent acquire either
wins first and blocks deletion, or loses to the tombstone and cannot restore.
Deletion retries complete interrupted cleanup. The tombstone prevents checkpoint
ID reuse and is deliberately retained; storage is not the lifecycle authority.

Every publication has a bounded total operation context and an upload lease.
The lease starts at `Publish` entry, before source hashing.
Defaults are five minutes per operation and thirty minutes per publication lease;
`Options` can change both, but the lease must be at least the operation timeout.
For interrupted publication, enumerate the admitted checkpoint IDs in the host's
journal and call `RecoverExpired` per scope. It CAS-fences an expired upload and
waits until its lease expiry plus the configured operation timeout before deleting
objects, allowing an in-flight request time to finish. Repeat recovery after
crashes or delayed provider completion. It never promotes partial uploads.
Persist a host-owned capture intent before calling `Publish`. If a crash leaves
an ambiguously missing lifetime record, retain the intent until `RecoveryGrace()`
(`StagingTTL + OperationTimeout`) from intent creation and retry recovery; an
initial request may have been in flight when the host disappeared. Retry
an aborted checkpoint with a fresh checkpoint ID. Recovery does not sweep ready
checkpoints, active leases or another scope. Do not reduce recovery timeouts below
the timeouts used by the publishing hosts.

## Local and GCS adapters

`NewFileObjects` creates/restricts a dedicated local root to mode 0700. Cooperating
processes serialize CAS operations using an advisory file lock. An object write
uses a private temporary file, fsync, atomic rename, and directory fsync through the
root. Startup removes abandoned adapter upload files while holding the same lock.
Open readers hold an immutable file descriptor across subsequent replacements.
The local administrator and filesystem are trusted; this is not a network filesystem
CAS service. Local stores must be copied intact for cross-host handoff.

`NewGCSObjects(GCSConfig{Bucket, Prefix, TokenSource, TempDir})` uses the actual
Google Cloud Storage JSON API. `Prefix` is mandatory. Inject a host-only OAuth token
source scoped by IAM to the authorized bucket/prefix; the adapter does not discover
accounts, assume application-default credentials, issue signed URLs, or give cloud
credentials to guests. Token-source errors and provider response bodies are not
included in returned errors. Redirects are refused. Default endpoint is
`https://storage.googleapis.com`; an HTTP loopback endpoint exists for protocol
tests. Constructor calls do not make requests.

Writes spool to a private temporary file to calculate CRC32C without holding the
VM memory snapshot in RAM, then send a single JSON multipart upload with the
expected checksum and `ifGenerationMatch=0` (or the exact CAS generation). Upload
response size/checksum must match. Reads select a specific generation after fetching
metadata, then verify CRC32C while streaming; store-level SHA-256 checks remain
mandatory. Deletes require a generation precondition. Listing is prefix-scoped and
paginated, and rejects objects outside its requested scope. Token refresh, IAM,
bucket encryption/location, storage charges and cloud provisioning belong to the
operator. No cloud account or destination was used to validate this implementation.

Google API references: [object insert](https://docs.cloud.google.com/storage/docs/json_api/v1/objects/insert),
[object get](https://docs.cloud.google.com/storage/docs/json_api/v1/objects/get),
[request preconditions](https://docs.cloud.google.com/storage/docs/request-preconditions),
and [checksum validation](https://docs.cloud.google.com/storage/docs/data-validation).

## Telemetry

Set `Options.Logger` to a host-owned structured `slog.Logger` to observe publication,
materialization, pin acquisition/release, deletion and expired-upload recovery.
The default logger discards events. Records contain operation outcome class,
duration, validated tenant/project/checkpoint IDs and returned artifact bytes.
They omit source paths, credential values, compatibility configuration and raw
errors. GCS response bodies and token-source errors remain excluded from returned
adapter errors as well. Operator log sinks own access control and retention.

## Verification evidence

With the pinned Go toolchain:

```sh
PATH=/Users/gduchidze/.cache/hypercall/toolchains/go1.27.3/go/bin:/Users/gduchidze/.local/bin:$PATH GOTOOLCHAIN=local go test -race ./internal/snapshot
```

The storage seam tests cover immutable publication/materialization, exact-content
retry/conflict, empty/corrupt/missing artifact rejection, compatibility rejection,
interrupted-upload unavailability/recovery, permanent pins and competing retention,
prompt unsafe-source rejection, source-descriptor stability, sparse size limits and
safe structured telemetry.
GCS `httptest` fixtures verify multipart CRC32C metadata, create preconditions,
generation-pinned reads, corrupt-stream rejection, scoped listing and conditional
deletion. These are protocol tests, not live GCS qualification, VM continuity,
KVM, cloud crash-consistency or cross-host qualification evidence.

Runtime acceptance remains incomplete: two-child continuity/retry fail, and live
GCS and compatible cross-host restore have not run. See the public
[qualification status](snapshots.md#qualification-status) for the remaining gates.
