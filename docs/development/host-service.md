# Authenticated sandbox host service

`hypercall-host-service` is a privileged Linux process, separate from the
unprivileged API/controller. It implements `hypercall.host.v1` over TLS 1.3 with
mutually verified certificates. The API's host adapter implements the existing
control-plane backend contract; it does not add another scheduler.

## Prerequisites and startup

Complete the [sandbox host setup](host.md) and qualify the pinned
Firecracker/jailer, template, guest agent, cgroups, and KVM configuration first.
The artifact config must be a regular file, at most 1 MiB, without group/other
write access. The service reads a private snapshot once; its template SHA-256
is both the runtime's verified input and the bridge's admission digest. Keep the
original artifact files protected as described in the sandbox setup.

Prepare a private CA and issue a server certificate with a DNS SAN for the host.
Issue the controller a client-auth certificate with an exact URI SAN, for example
`spiffe://hypercall/controller`. Configure that URI explicitly on the host. A
certificate signed by the CA with a different URI has no controller authority.
Keep private keys and runtime/ledger directories inaccessible to other users.
This example assumes certificates, artifacts, delegated cgroup, and a non-root
jailer UID/GID already exist; it does not provision resources.

```sh
go build -o /tmp/hypercall-host-service ./cmd/hypercall-host-service
sudo /tmp/hypercall-host-service \
  --listen 127.0.0.1:9444 \
  --client-ca /etc/hypercall/controller-ca.pem \
  --tls-cert /etc/hypercall/host.pem --tls-key /etc/hypercall/host.key \
  --controller-uri spiffe://hypercall/controller \
  --tenant team --project development \
  --artifacts /etc/hypercall/artifacts.json \
  --runtime-state /var/lib/hypercall/runtime \
  --ledger-state /var/lib/hypercall/host-ledger \
  --cgroup-dir /sys/fs/cgroup/hypercall \
  --jailer-uid 10001 --jailer-gid 10001
```

Use separate directories for runtime state and bridge ledger. Both must be clean
absolute paths without symlinks. The runtime directory must be root-owned 0700;
the bridge directory must be owned by the service user with mode 0700. One process
locks each directory. Use a private network path when host and API are separate
machines; mTLS remains required even on loopback. Bind only the internal endpoint
and restrict access at the host firewall.

Configure `hypercall-server` with `--host-address`, `--host-ca`, `--host-cert`,
`--host-key`, and `--host-server-name` using that client certificate and the
server's DNS SAN. The API must use the same tenant/project scope as the host.
See [control-plane setup](control-plane.md) for Postgres, admission, and clients.
No guest credential is returned through either public or internal host RPCs.

## Supported execution contract

The current bridge supports one configured tenant/project and one verified cold
boot template per host process. A task's `template_digest` must equal
`sha256:<template manifest SHA-256>` from the artifact configuration. Template
selection never accepts arbitrary paths or unverified template names.

Workspace materialization supports private writes and root-level files with
SHA-256 digests, at most 1 MiB total. Nested paths and repository cloning are
rejected before VM allocation. These are explicit compatibility limits, not
silently omitted workspace inputs. Tasks carry explicit argv, supported guest
environment overrides (`LANG`, `LC_ALL`, `TZ`, `PYTHONUNBUFFERED`), and a timeout
of 1–600 seconds within the runtime's CPU/RAM/workspace limits. Expired assignments are rejected before allocation. The native runtime caps its
relative TTL with the assignment’s absolute execution deadline. No shell is inserted by the bridge.

The workspace files are transferred before the task starts. The task runs once
under a host-owned deadline independent of controller RPC lifetime. Command/file
RPCs are available while that task is executing and carry the same immutable
scope, execution ID, epoch, and absolute execution lease. A normal task exit is
reported as `stopped` after VM termination; a nonzero exit or runtime interruption
is reported as `failed`. Failed or uncertain cleanup remains a durable pending
obligation and returns an error until termination is confirmed.

Declared agent/dataset digests and resolved ModelConfig/Gateway resources remain
immutable provenance metadata in Phase 2A. Only the verified template and inline
workspace file contents are materialized; the bridge does not resolve additional
agent or dataset artifacts from those declared digests. The host does not implement inference routing, model serving, external
network egress, or secret-reference resolution. A task that requires these
capabilities cannot execute successfully on this bridge. Task stdout/stderr are
currently consumed under the guest output limit; they are not retained as public
run events. Use the public Exec stream while running when command output is
needed. Foreground tasks return `failed_precondition` for snapshot suspend/resume. The
controlled profile below is the only implemented snapshot path; no semantic
agent checkpoint continuity is claimed.

## Controlled snapshot integration (opt-in, qualification incomplete)

The imported Phase 1B implementation provides native Capture/Restore/Fork and
fresh guest identity, credentials, transports, RNG renewal, and UTC refresh. Its
published qualification remains incomplete: two-child continuity/retry failed,
and live GCS and compatible cross-host restore have not run. See the
[snapshot status](snapshots.md#qualification-status). Integrating these operations
does not turn that evidence into a passing runtime gate.

Snapshots remain disabled unless the host starts with `--controlled-snapshots`
and one explicit object-store configuration:

```sh
# Add to the host startup command; use a dedicated private directory.
--controlled-snapshots --snapshot-store /var/lib/hypercall/snapshots
# Or use an already authorized bucket/prefix and externally refreshed token:
--controlled-snapshots --gcs-bucket YOUR_BUCKET --gcs-prefix YOUR_PREFIX \
  --gcs-token-file /private/refreshed-oauth-token
```

The API separately requires `--host-controlled-snapshots`. Neither configuration
permits arbitrary task snapshots. A supported Task has
`execution_mode: controlled_python_v1`, an optional int64 `controlled_counter`,
and empty `command` and `env`. The embedded controlled Python program belongs
to the verified guest artifact. `agent_digest` must equal that artifact's SHA-256
from the verified template manifest; `template_digest` must equal the template
manifest digest. Generic agents cannot opt into checkpoint support merely by
supplying ordinary argv. Workspace filenames `controlled-origin.txt` and
`controlled-state.json` are reserved for the controlled program. This profile is
a sandbox continuity workload, not the semantic agent barrier planned for 2B.

Suspend journals a fixed native checkpoint ID before Capture. A lost Capture
reply recovers only a ready store manifest; it never repeats an ambiguously
started capture. The host binds scope, source sandbox/execution/epoch, verified
template/guest, and canonical manifest digest, acquires a permanent retention
pin, and confirms parent termination before returning `suspended`. Capture's
native parent reopening is therefore followed by an explicit confirmed Stop.
An absent or incomplete publication stays pending until reconciliation or lease
expiry and durable cancellation; it cannot become a verified checkpoint.

Postgres retains native checkpoint ID and canonical native manifest bytes,
alongside its branch, event cursor, digest and compatibility metadata. Resume
verifies those bytes against the ready scoped store manifest and uses fresh
sandbox/execution identities with a higher epoch. It journals the exact native
Restore request, including operation ID, fixed TTL and absolute expiry, before
side effects. Retries reuse that request. Native Restore independently validates
full artifact bytes and host compatibility, rebinds the captured controlled
process to the fresh session and returns authenticated readiness. The adapter
never replays Task argv or starts another controlled process after Resume.

A permanent `retained-<native checkpoint ID>` pin remains even if a controller
commit acknowledgement is lost or a later resume is canceled. It protects any
possibly retained Postgres checkpoint. There is no automatic checkpoint deletion
or orphan-pin reclamation in this stage. Operators must reconcile Postgres
references and journals before any manual retention action.

## Fencing, recovery, and backups

The bridge fsyncs a versioned local assignment record before allocation. Records
retain the resolved-run hash and identity metadata, not workspace file contents
or guest credentials. Repeated Ensure requests inspect the same assignment;
they never blindly repeat Create or replay an ambiguously started task. Resolved
inputs, execution identity, epoch, and execution lease cannot change on a retry.
The runtime’s fixed TTL is capped by the controller’s absolute lease, with
an additional lease-bound context for foreground tasks. Snapshot Restore journals
include the same absolute cap, so a late retry cannot extend the lease. There is no in-place lease renewal.

Stop first persists a closing tombstone, including when no VM exists. Only
confirmed runtime termination advances it to closed. Delayed Ensure calls for
that epoch cannot create a VM. A higher epoch requires the prior assignment to be closed or suspended; a
suspended predecessor can only continue through Resume with its retained manifest. Older epochs cannot stop or mutate the newer assignment. Tombstones
survive process restarts and are required safety state.

On host-process restart, the runtime reaps its previous VMs. An admitted or foreground task without its original worker is conservatively
stopped and failed when reconciled. Controlled assignments use native authenticated
barrier/identity observations. Pending captures recover their ready manifest, and
pending restores reuse their exact journaled request; native host recovery fences
interrupted VMs and never silently cold-starts a restored identity. The controller can restart independently and continue querying
the existing host worker. A snapshot of a VM is not used to replay external effects.

Back up the ledger together with runtime fences using a quiesced host (stop
admission, confirm termination, then stop the service), and retain its directory
ownership and permissions. Postgres backups alone do not replace host fencing
state. Never restore an older ledger onto a live host or delete tombstones to
retry an assignment. A lost ledger/fence set requires fencing and terminating the
old host, then explicit reconciliation with new execution identities on a new
host; it cannot be treated as an empty safe state. Tombstone garbage collection
and multi-host placement are not implemented in this stage.

## Validation and remaining gate

```sh
go test -race ./internal/hostapi
# Set HYPERCALL_TEST_POSTGRES to an isolated local test database first.
go test -race ./internal/hostapi -run TestPostgresPublicAPIControllerAuthenticatedHostAndGuestRecoverTogether -count=1
go vet ./internal/hostapi
GOOS=linux GOARCH=amd64 go build -o /tmp/hypercall-host-service-linux ./cmd/hypercall-host-service
```

The host tests exercise mutually authenticated internal gRPC, unauthorized
certificate URI/scope rejection, actual guest commands and files through an
explicit trusted-local guest runtime, once-only task execution, lease fencing,
absent Stop across restart, legacy physical-ID alias fencing, and cleanup
uncertainty/retry. Controlled tests use a real immutable local store with synthetic
artifact bytes to exercise manifest binding, retention pins before parent Stop,
lost capture/restore replies, exact retry requests, cancellation before child
allocation and generic task rejection. An actual trusted-local guest Python test
checks the controlled startup/barrier path without simulating VM continuity. With
`HYPERCALL_TEST_POSTGRES` set, a separate composed test submits YAML through the
authenticated public API, persists real Postgres resources, starts an actual
Python process through the mTLS host bridge and trusted-local guest, reopens
Postgres/API/controller while the host worker continues, verifies once-only
execution, proxies commands/files, and records confirmed cancellation in ordered
events. A second composed Postgres/public-API/mTLS-host test persists a controlled
checkpoint and resumes under fresh physical identities after reopening the
controller/database, using the explicitly synthetic snapshot runtime. It requires Python 3 and creates/drops a temporary database schema.
These local test seams do not qualify Firecracker isolation, cross-host recovery,
or a real Postgres→controller→host→KVM execution. Those remain required Phase 2A integration
and release gates. See [Phase 2A evidence](../validation/2A.md).
