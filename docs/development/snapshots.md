# Snapshot development and qualification

Phase 1B remains **IN PROGRESS and unqualified**. Capture/recovery and UTC renewal
have partial real KVM evidence; two-child continuity/retry currently fail after
health timeouts. Cross-host restore and live GCS have not run. The
[qualification status](#qualification-status) below separates passing checks from
remaining gates. The commands are acceptance procedures, not evidence that they
passed.

The implementation uses a fully materialized baseline using official
Firecracker/jailer 1.14.0. This is VM continuity at a controlled Python barrier;
semantic agent checkpoints and committed event histories belong to Phase 2B.
The host snapshot API is private. The sandbox backend owns lifecycle; the
snapshot store owns immutable publication, integrity and reference-aware retention.

## Qualification status

Final native `make verify` passed: generated-source drift, Go race tests, Python
contracts, artifact tests, three cross-builds, wheel packaging and `go vet`.
Real ARM64 KVM runs provide partial evidence for capture, capture expiry, staged
upload crash recovery, artifact rejection, a single restored child's ten-second
health and owner-crash cleanup, and guest UTC renewal. The capture-only profile
passed; the full Phase 1B profile did not.

Two-child continuity and partial fan-out retry fail after health timeouts.
Parent/sibling isolation and a sustained concurrency/fan-out envelope remain
unqualified. Compatible cross-host restore and live GCS have not run. The latest
recovery change also still needs a real two-live-orphan reference-release failure
and retry test. Native checks and configured hosts do not complete these gates.
Operator-local evidence is retained separately from the published repository.

## Run the real qualification

Use dedicated Linux/KVM hosts and freshly built Phase 1B guest/template artifacts.
A Phase 1A guest image cannot exercise the new barrier/rebind protocol. Follow
[host setup](host.md) for pinned tools, offline template builds, delegated cgroups
and the standalone driver build. Use a fresh, short, private **disk-backed** state
path per run; `/run` is unsuitable for materialized image staging. No test creates
cloud resources or provisions credentials.

```sh
sudo bin/hypercall-testdriver snapshot-qualify \
  --config /opt/hypercall-artifacts/template-arm64-1b3/runtime.json \
  --state-dir /var/lib/hcb1 --cgroup-dir /sys/fs/cgroup/hc-1b \
  --snapshot-store /var/lib/hco1 --output /var/lib/hc1b-local.json
```

A passing local report requires the Python process's inherited PID, memory
counter, disk sentinel and barrier; two fresh children; parent/sibling independent
memory and filesystem writes; fresh identity, credentials and RNG probes;
idempotent retry after partial fan-out; corrupt/missing object rejection before
child creation; bounded capture cancellation when a stalled upload crosses
allocation expiry; owner SIGKILL during a staged memory upload with repeated
intent recovery and collection after its lease/grace; recovery
after killing the restored VM's owning host process; cleanup and log redaction. Local and destination-restore cleanup delete unreferenced
checkpoint artifacts and verify they are unavailable; capture mode retains its
export for transfer.
The report records full capture/restore durations and materialized artifact bytes.
All artifacts are materialized and verified locally before loading. Official
Firecracker still maps the local memory file privately and faults pages on demand;
this is not a claim that all guest RAM is resident. Remote/layered lazy fetch,
incremental snapshots and deterministic generation remain unqualified.

The driver requests one vCPU, 256 MiB guest memory, an 8 MiB
writable disk and 32 workload PIDs per VM. The acceptance case uses parent plus
two children. Children are stopped between drills, bounding the driver to three
simultaneous VMs.
These are requested limits. Single-child checks have partial evidence; three
simultaneous VMs and fan-out two are not qualified. Failed continuity and the
remaining gates are summarized in [qualification status](#qualification-status).

## Compatible cross-host restore

Both hosts need identical pinned runtime/template artifacts at the configured
paths and the exact compatibility profile recorded in the manifest. Incompatible
CPU/host kernel/runtime/template/device/barrier/RNG/VM-size profiles are rejected
before starting a child. Cross-architecture portability is unsupported.

Capture on the source host:

```sh
sudo bin/hypercall-testdriver snapshot-capture \
  --config /opt/hypercall-artifacts/template-arm64-1b3/runtime.json \
  --state-dir /var/lib/hcbc --cgroup-dir /sys/fs/cgroup/hc-1b \
  --snapshot-store /var/lib/hcbo --handoff /var/lib/hcb-handoff.json \
  --output /var/lib/hc1b-capture.json
```

Transfer the complete private object directory and handoff to a distinct,
compatible Linux/KVM host using an operator-controlled private archive. Preserve
mode 0700 on the object directory and 0600 on the handoff. The handoff contains a
parent bearer only to verify rejection by the restored guest; keep it out of
source control, logs and shared evidence. The capture report is safe to retain
separately. Source state/VM disks are not required on the destination.

```sh
sudo bin/hypercall-testdriver snapshot-restore \
  --config /opt/hypercall-artifacts/template-arm64-1b3/runtime.json \
  --state-dir /var/lib/hcbr --cgroup-dir /sys/fs/cgroup/hc-1b \
  --snapshot-store /var/lib/hcbo --handoff /var/lib/hcb-handoff.json \
  --output /var/lib/hc1b-restore.json
```

Restore mode rejects a source host with the destination's `/etc/machine-id` hash.
It verifies inherited Python memory/disk state before continuation, then fresh
allocation identity and rejection of the parent's bearer. Every restored child
must also survive ten seconds of repeated public Inspect/InspectControlled calls
across multiple production health-monitor periods. Capture-only and
restore-only reports each require their own complete gate set; neither implies
the local fork/failure suite has passed.

The authenticated host renewal refreshes guest `CLOCK_REALTIME` from a current
host UTC sample before reopening the parent or clone gate. Firecracker restores
wall clock from the capture moment; UTC refresh is separate from monotonic-clock
or kernel-timer continuity and does not resolve the ARM64 timer regression under
investigation. Clock requests authenticate against the private session disk
before clock mutation; invalid timestamps fail closed.

The required `restored-clock` gate uses public guest execution of Python
`time.time_ns()`. It records host time immediately before and after the command
and requires child UTC within those bounds plus/minus two seconds. The private
handoff records completed capture UTC; qualification waits for at least five
seconds of checkpoint age **before restore** so inherited stale time can fail.
Fresh handoffs are required; older handoffs without that timestamp must be
recaptured. The gate also rejects old-parent renewal and malformed renewal on an
active child, then checks actual UTC again. It does not inject malformed samples
inside the host-owned capture gate, which would require an additional confirmed
test seam; that narrower negative remains a documented test gap.

## GCS

The same driver can exercise GCS by replacing `--snapshot-store` with:

```sh
--gcs-bucket YOUR_AUTHORIZED_BUCKET --gcs-prefix qualification/RUN_ID \
--gcs-token-file /private/refreshed-oauth-token
```

The bucket must already exist and be explicitly authorized by the operator.
The host rereads a mode-0600 regular token file for requests; refresh it outside
the driver. Tokens stay outside guest images, snapshot manifests and reports.
The CLI uses Google's HTTPS endpoint. It does not discover an account, create
buckets, install ADC or refresh credentials. Source and destination hosts can use
the same authorized GCS prefix; only the private handoff needs transferring.
A live GCS run is required to qualify account permissions, retention policy and
actual remote publication. Local HTTP protocol tests do not provide that evidence.

## Opt-in integration target

`make test-kvm-snapshot` runs the same operator through the integration suite.
Set absolute paths in `HYPERCALL_TESTDRIVER`, `HYPERCALL_HOST_CONFIG`,
`HYPERCALL_QUALIFICATION_STATE`, `HYPERCALL_CGROUP_DIR` and
`HYPERCALL_SNAPSHOT_STORE`. `HYPERCALL_SNAPSHOT_MODE` selects `local` (default),
`capture` or `restore`; cross-host modes also require
`HYPERCALL_SNAPSHOT_HANDOFF`. `HYPERCALL_QUALIFICATION_REPORT` optionally saves
JSON. For GCS, replace `HYPERCALL_SNAPSHOT_STORE` with
`HYPERCALL_GCS_BUCKET`, `HYPERCALL_GCS_PREFIX` and `HYPERCALL_GCS_TOKEN_FILE`.
`scripts/qualify-phase1b` accepts the same environment for direct execution.
Missing opted-in configuration fails; skipped KVM tests provide no qualification.

A report passing these runtime checks does not establish rollback of external
side effects, deterministic model output, semantic agent steps, AMD64 runtime
support or production availability. Retain exact source/binary/template hashes,
host profiles and reports alongside any acceptance claim.

For a minimal single-child stability probe, `snapshot-hold` accepts the same
capture handoff/storage flags and a fresh `--state-dir`. It restores one child,
checks public identity/barrier health every 500 ms for ten seconds, emits its
observation only after that succeeds, then waits for interruption. A failure
names the sandbox, elapsed time and failed operation; shutdown cleans its VM.
