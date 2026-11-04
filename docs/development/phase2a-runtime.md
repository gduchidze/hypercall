# Phase 2A real-host workflow qualification

Status: **PASSED for the bounded `controlled_python_v1` ARM64 Firecracker
workflow on 2026-11-03**. This does not pass the full Phase 1B gate.
The procedure is opt-in. It does not start, stop or reconfigure Lima, provision
infrastructure, or launch the host service.
It requires an operator-provided existing Firecracker host service and an isolated
Postgres database. Native contract/conformance tests do not satisfy this gate.

The workflow qualifies the composed API/controller, host bridge, CLI and SDK on
the bounded `controlled_python_v1` profile. It does not qualify Phase 1B sibling
fan-out, arbitrary Python programs, semantic agent checkpoints or external-effect
rollback. The [Phase 1B qualification status](snapshots.md#qualification-status)
remains separate, including its failed and untested cases.

## Prepare the existing host and operator inputs

Follow [host service setup](host-service.md) with verified Phase 1B guest/template
artifacts, its authorized controller URI certificate, private runtime/ledger state,
and a persistent snapshot store. Enable `--controlled-snapshots` and either
`--snapshot-store /absolute/private/objects` or the explicitly authorized GCS
configuration described there. This harness does not enable or start that service.

Build the known API and CLI binaries with the repository's pinned tools. Choose
an unused operator-owned directory; the example paths below are placeholders:

```sh
mkdir -p /absolute/qualification/bin
go build -trimpath -o /absolute/qualification/bin/hypercall-server ./cmd/hypercall-server
go build -trimpath -o /absolute/qualification/bin/hypercall ./cmd/hypercall
shasum -a 256 /absolute/qualification/bin/hypercall-server /absolute/qualification/bin/hypercall
```

Prepare a private API key JSON file as in [control-plane setup](control-plane.md),
with exactly the tenant/project intended for this run. Its selected key is supplied
through `HYPERCALL_API_KEY`; the database URL is supplied through
`HYPERCALL_DATABASE_URL`. Neither value is written to the qualification report.
Use an isolated database and a fresh project/resource naming convention. Existing
database/host state is not erased or replaced by this harness.

Prepare a multi-document [Task manifest](clients.md#manifest-contract) containing
Workspace, ModelConfig, Gateway and one final Task. All artifact/reference pins
must be real and supported by the configured host. The task needs:

```yaml
execution_mode: controlled_python_v1
controlled_counter: 42
command: []
env: {}
limits:
  cpu: 1
  memory_mb: 256
  disk_mb: 8
  timeout_seconds: 600
```

`template_digest` must identify the host's verified template manifest;
`agent_digest` must identify its verified guest binary containing the built-in
controlled workload. The host validates that supported profile rather than
executing arbitrary code from the declared agent pin. Keep the synthetic fixture
model/gateway offline; this procedure makes no provider calls or model claims.
The checked-in sample with illustrative digests is insufficient for a real run.

Create `/absolute/qualification/operator.json` with mode 0600. Every field below
is required; replace example values and hashes with the prepared inputs:

```json
{
  "profile": "controlled_python_v1_firecracker",
  "server_binary": "/absolute/qualification/bin/hypercall-server",
  "server_sha256": "REPLACE_WITH_64_LOWERCASE_HEX_CHARACTERS",
  "cli_binary": "/absolute/qualification/bin/hypercall",
  "cli_sha256": "REPLACE_WITH_64_LOWERCASE_HEX_CHARACTERS",
  "keys_file": "/absolute/private/api-keys.json",
  "manifest_file": "/absolute/qualification/controlled.yaml",
  "tenant_id": "internal",
  "project_id": "runtime-qualification",
  "grpc_address": "127.0.0.1:18081",
  "http_address": "127.0.0.1:18080",
  "host_address": "existing-host.example:9444",
  "host_ca": "/absolute/private/host-ca.pem",
  "host_cert": "/absolute/private/controller.pem",
  "host_key": "/absolute/private/controller.key",
  "host_server_name": "existing-host.example",
  "architecture": "aarch64"
}
```

Use `x86_64` only for that matching host profile. API listeners must be unused
literal loopback addresses. The controller-to-host path always uses mTLS, even
when that host endpoint is local. All files require absolute, regular paths;
configuration and private keys must be inaccessible to group/other users. Binary
SHA-256 values are checked before execution. Unknown configuration fields and
test-only profiles are rejected.

## Explicit operator invocation

Keep the existing host reserved for this run and set its authorized database and
API key in the operator's environment. Then run:

```sh
export HYPERCALL_RUNTIME_2A_TEST=1
export HYPERCALL_2A_CONFIG=/absolute/qualification/operator.json
export HYPERCALL_2A_REPORT=/absolute/qualification/phase2a-runtime-report.json
make test-runtime-2a
```

The report path must be new. The harness starts only the supplied API binary with
fixed flags, submits YAML through the compiled CLI, and verifies SDK command/file
I/O. It suspends the controlled VM, validates the native checkpoint manifest,
restarts its own API/controller process while suspended, resumes through the CLI,
and checks fresh physical sandbox/execution IDs, a higher epoch, refreshed guest
identity, the controlled counter and a private disk sentinel. It then restarts
its API/controller while the resumed assignment runs, verifies that assignment
is retained, checks REST inspection and ordered/resumable events, and confirms
cancellation. It never accepts an arbitrary shell restart command.

Native checkpoint validation requires the correct tenant/project/native checkpoint
scope, protocol, template/guest pins, real architecture/runtime profile, all three
memory/VM-state/disk artifacts, and a full-memory artifact whose size equals the
configured guest RAM. Tiny byte-map conformance fixtures cannot pass this mode.
The procedure trusts the explicitly configured mTLS host and known binaries; it
does not claim independent hardware attestation or prove untested snapshot paths.

The mode-0600 report contains stage/check results, binary/manifest digests, run,
branch, checkpoint and operation IDs, and bounded native snapshot size metadata.
It excludes credentials, connection URLs, snapshot manifest bytes, VM memory and
guest file contents. On failure it attempts durable cancellation. If cleanup is
unconfirmed, preserve host state and reconcile the recorded branch and execution
lease; do not delete fencing records to retry. The report's idempotency prefix
supports reconciliation of an ambiguous submission using the identical input.

Only an exit-zero report with every check passed supplies this particular runtime
evidence. Missing inputs, unavailable services, incompatible artifacts, timeouts
and unconfirmed lifecycle transitions fail. Merely installing this harness or
passing its native refusal tests is **not** a real-host qualification result.


## Recorded bounded real-host result

The user explicitly released the existing development VM for this run. Root ran
`make test-runtime-2a` against Linux ARM64/KVM, official Firecracker 1.14.0,
the existing pinned `template-arm64-1b3` artifacts, a private local snapshot store,
and a dedicated PostgreSQL database. The controller-to-host path used mTLS over
an SSH tunnel. No live model/provider, GCS or cloud provisioning was used.

The successful second attempt exited 0 with all five checks passed:
`start_command_files`, `native_suspend`,
`native_resume_after_controller_restart`, `running_controller_restart`, and
`rest_events_cancel`. The real full-memory snapshot retained counter 42 and a
binary disk sentinel. Resume used fresh physical sandbox/execution identities
and epoch 2, after the source VM was confirmed stopped. The resumed assignment
survived another controller restart, and final cancellation confirmed child stop.

The redacted [workflow report](../validation/artifacts/2A/runtime-workflow.json)
contains only IDs, digests, bounded snapshot metadata and check results. Native
memory was 268,435,456 bytes; total snapshot artifacts were 276,831,610 bytes.
Observed cold boot was 6,765 ms and restored boot 1,809 ms; publication took
540 ms and materialization 271 ms. These are one local run's observations, not
performance targets or a concurrency qualification.

The first attempt stopped at a harness version-format check: Firecracker reported
`Firecracker v1.14.0` while the check expected a bare version. That check was fixed,
six focused harness tests passed, and the failed attempt's VM cleanup was confirmed
before the successful retry. The runtime result therefore applies to the corrected
harness and recorded known binaries.

This passes the composed Phase 2A controlled-profile workflow. Phase 1B two-child
fan-out continuity/isolation, compatible cross-host restore, live GCS and a
supported concurrency envelope remain open. Arbitrary agent snapshots, semantic
step checkpoints, model routing, external-effect rollback and production release
qualification are outside this result.


The final post-fix verification suite passed. The SDK replayed the original CLI
submission after VM shutdown and returned the identical Run without increasing
the run count. An offline backup/restore of the actual runtime database matched
all eight tables, then retained the native checkpoint, cancelled branch and
contiguous events through the restored API/SDK.

Final cleanup is confirmed: task cgroup empty, host service/tunnel stopped,
ephemeral host private key removed, all task API servers/database container
stopped, and `hypercall-dev` restored to its original stopped state. The separate
stopped restore VM was untouched. Retained private snapshot objects, ledgers and
backups remain available for recovery. This does not change the narrower scope
of the passing controlled-profile result.
