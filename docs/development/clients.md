# CLI and Python SDK

The public contract is `hypercall.v1.ControlPlane`. Generated Go, Python,
grpc-gateway REST and OpenAPI artifacts come from
`proto/hypercall/v1/control.proto` and `http.yaml`; regenerate with `make generate`.
The private guest and host protocols are separate trust boundaries.

## Local workflow

Run the server as described in [control-plane.md](control-plane.md). Set a key
granted to the selected project in the server's private key configuration:

```sh
export HYPERCALL_ENDPOINT=127.0.0.1:8081
export HYPERCALL_PROJECT=development
export HYPERCALL_API_KEY='your-operator-provisioned-key'
go build -o bin/hypercall ./cmd/hypercall
bin/hypercall --plaintext --key first-task submit test/fixtures/phase2a/task.yaml
bin/hypercall --plaintext inspect Run RUN_ID
bin/hypercall --plaintext events BRANCH_ID
bin/hypercall --plaintext --after 1 events BRANCH_ID
bin/hypercall --plaintext --key cancel-first-task cancel BRANCH_ID
```

Flags precede the command. `--plaintext` is an explicit local-development option
limited to loopback addresses. TLS is the default; private certificate authorities
use `--ca /path/ca.pem` and optionally `--server-name expected-dns-name`.
`--timeout 30s` bounds an RPC, including foreground commands. Process interruption
cancels the RPC. `exec BRANCH_ID python agent.py` streams stdout/stderr and returns
nonzero for command failure. `read-file BRANCH_ID PATH` writes raw bytes to stdout;
`write-file BRANCH_ID PATH LOCAL_FILE` transfers up to 1 MiB.

The checked-in fixture is synthetic and uses illustrative digests and an invalid
fixture endpoint. It demonstrates admission and durable submission, and is not a
bootable workload. A valid template, agent, dataset and authenticated host adapter
are required for actual execution. The public suspend/resume protocol can be
validated independently of VM qualification using the explicit test state backend.
Snapshot capability is enabled explicitly for a configured host and its supported
task profile. Ordinary foreground tasks remain ineligible for snapshots. Accepting
an Operation records intent and does not establish runtime completion or qualify
a VM snapshot.

## Manifest contract

`apiVersion: hypercall.io/v1`, `kind`, `metadata.name`, and typed `spec` are the
only envelope fields. Unknown fields and duplicate documents fail admission.
Use at most 64 documents and 1 MiB of YAML. Put Workspace, ModelConfig and Gateway
definitions first and exactly one Task last. The fixture shows the complete shape.

Task reference `id` may name an earlier definition of the corresponding kind in
the same manifest. The server replaces that name with its immutable resource ID
and digest before admitting the Task. References to resources outside the manifest
must carry both their ID and digest. Run resources retain the resolved definitions.
Definition names are immutable: create a new name for changed inputs.

The manifest is syntax-checked before application. Definition application proceeds
in order and is not atomic across documents; a later semantic validation failure
can leave earlier definitions stored. A submitted Run and its initial Branch,
Operation, event and job are created transactionally. Reuse the same idempotency
key and identical input after a timeout to retrieve the original submission.
Reusing a key for different inputs fails. Tenant identity and project grants are
derived from authentication; YAML cannot supply authority or output-only metadata.

## Python

`uv sync --frozen` installs the local `hypercall-sdk` distribution. Wheels contain
the SDK and generated `hypercall_proto` namespace. Both sync and async clients use
the same protobuf contract and canonical gRPC status codes:

```python
import os
from pathlib import Path
from hypercall import Client, HypercallError

with Client(os.environ["HYPERCALL_ENDPOINT"],
            project=os.environ["HYPERCALL_PROJECT"],
            api_key=os.environ["HYPERCALL_API_KEY"],
            plaintext=True, timeout=30) as client:
    result = client.submit_yaml(Path("test/fixtures/phase2a/task.yaml").read_text(),
                                idempotency_key="first-task")
    run = client.inspect("Run", result.run.metadata.id)
    page = client.events(result.branch.metadata.id, after=0)
    next_page = client.events(result.branch.metadata.id, after=page.next_sequence)
```

Use `async with AsyncClient(...)` and `await` for unary methods. `exec` yields
protobuf output/result events (an async iterator on AsyncClient); `run_command`
collects them into `CommandResult` with raw stdout/stderr, exit code, timeout and
truncation flags. Nonzero command exit is a result, whereas transport failures
raise `HypercallError`. A stream ending without a terminal result is `data_loss`.
Close a partially consumed command iterator explicitly to cancel its RPC.

`HypercallError` exposes `code`, `retryable`, `ambiguous`, and optional
`operation_id`. No method automatically retries. `retryable` describes a transient
status, not permission to replay a mutation. Reconcile lifecycle/submission
timeouts with the original key and inspect the returned Operation. Do not replay
commands automatically after an ambiguous timeout. Set per-call `timeout=` to
override the default deadline. TLS uses system roots by default; private CAs use
`root_certificates=Path("ca.pem").read_bytes()`.

## Suspend and resume

CLI `suspend` / `resume`, SDK `suspend()` / `resume()`, and the REST action endpoint
use the same durable operation contract. Unsupported backend capabilities fail
with `failed_precondition` before an intent is stored. For supported backends,
inspect the returned Operation until its status reports completion, and inspect
the Branch to distinguish desired state from observed state.

Completed suspension retains a verified, immutable Checkpoint resource. Its
`branch_id`, protocol and compatibility metadata describe which execution inputs
it can restore. `branch_status.checkpoint_id` identifies the retained checkpoint;
`budget_remaining_ms` records the remaining active execution allowance at the
last lifecycle transition. It is not a live countdown. Suspension freezes that
allowance; resume consumes the remaining allowance rather than granting a fresh
task timeout.

Resume selects the Branch's retained checkpoint on the server. The returned
Operation includes `operation_spec.checkpoint_id`, so clients can inspect exactly
which immutable checkpoint was chosen. Successful resume uses a fresh execution
ID, a fresh physical `sandbox_id`, and a higher epoch on the same logical Branch;
ordered event sequence numbers continue.
Retry an ambiguous request with the identical idempotency key to reconcile the
same Operation and assignment. A new key is a separate intent, and may fail while
the first resume is in progress. A terminal branch cannot resume.

The client and durable orchestration tests cover this protocol before Phase 1B
VM snapshot qualification. Their snapshot-capable conformance fixture copies Go
byte maps only; it proves no VM memory continuity, disk consistency, transport or
credential refresh, isolation, external-effect rollback, or fork behavior. Real
host snapshot support must be enabled and qualified separately on its supported
Phase 1B profile; local protocol tests do not complete the Linux/KVM acceptance gate.

For that bounded profile, replace the Task's ordinary command with:

```yaml
execution_mode: controlled_python_v1
controlled_counter: 42
command: []
env: {}
```

This selects the built-in controlled Python workload with a nonnegative initial
counter. It does not make arbitrary Python agents snapshot-compatible. The host
must verify the pinned template/guest and reject unsupported inputs before
starting a VM. `foreground_v1` (or an omitted execution mode) requires explicit
command arguments and does not accept a controlled counter. The public native
checkpoint fields are output-only: `native_checkpoint_id` locates the scoped
snapshot object, while `snapshot_manifest` carries canonical manifest JSON as
bytes (base64 in protobuf JSON), retaining the runtime compatibility information.

## Compatibility and validation

SDK version 0.2.0 and server/public schema `hypercall.v1` are developed together.
Generated source drift is checked by `make generate-check`; regenerating requires
the pinned Go/Python tools and plugin versions in `scripts/generate.py`. Changes
to field numbers, types, or semantics require an explicit compatibility decision;
the initial version does not promise compatibility with E2B or AX clients.

`make test-postgres` requires `HYPERCALL_TEST_POSTGRES` and runs the real Postgres
store plus authenticated gRPC/REST, sync/async Python and compiled Go CLI scenarios.
Tests use private temporary schemas. Ordinary `make verify` clearly skips database
scenarios when that variable is absent. These tests prove API and persistence
behavior; actual VM execution, checkpoint/fork and recovery gates need separate
Linux/KVM acceptance evidence.

The separately opt-in `make test-runtime-2a` exercises the real configured host
and API/controller restarts. Its [operator setup and concrete invocation](phase2a-runtime.md)
require private configuration, pinned binary hashes, mTLS, real controlled-profile
artifacts, and an unused report path. Native refusal/conformance checks do not
count as executing that runtime gate.
