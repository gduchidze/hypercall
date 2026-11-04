# Cooperative agent checkpoints and continuation branches

Phase 2B adds a bounded instrumented-agent profile and deterministic routing.
Native Go/Python/Postgres tests qualify its contracts. A real-host report from
`make test-runtime-2b` is required for VM workflow evidence; writing the harness or
passing a conformance fixture does not execute that gate. Phase 1B simultaneous
fan-out and its other [remaining qualification cases](snapshots.md#qualification-status)
remain separate from the sequential continuation workflow.

The real ARM64/KVM workflow passed on 2026-11-03: a cooperative Python agent
committed and sealed step 1, the API/controller restarted, and two continuations
restored sequentially from that checkpoint and completed through distinct pinned
fixtures. Both children retained 14 ordered events, fresh physical/execution
identities and acknowledged routing sessions. Raw/normalized outputs diverged at
step 2, position 1; tool calls/observations diverged at step 3, position 1. The
paused parent was cancelled after both children stopped. All seven operator
checks passed. This qualifies that bounded continuation workflow, without a
simultaneous fan-out, cross-host, live-GCS or model-quality claim.

The run used template
`sha256:4db4d4d90590626c16dee7d7a4710665d817d69501d6c35464944fa717013baa`,
guest `sha256:2d5e1d66a6b50a4c1d0a9119e52fe28abdd193690a0b64475fc9eea4133c057f`,
and offline harness archive
`sha256:0ab5e54cf684a7ff57baf136118df4876eb6df2d1a56da59a559d897b76112de`.
The local redacted report and detailed acceptance record are retained separately
in `docs/validation/artifacts/2B/runtime-workflow.json` and `docs/validation/2B.md`.
The populated run database subsequently passed a ten-table backup/restore
fingerprint comparison. Authenticated inspection of the restored checkpoint,
14 ordered child events and the step-2 output comparison also passed.

## Supported agent and gateway

`cooperative_python_v1` runs the pinned built-in Python agent from the read-only
template. Task command/environment overrides are empty. The agent performs:

1. A registered `tool.echo` common prefix, state-file fsync, step-1 commit, and
   an acknowledged barrier before any suffix action.
2. After restore, a registered inference call through the selected immutable
   ModelConfig, then a step-2 state commit.
3. A registered echo observation, step-3 commit, and explicit finalization.

The Task embeds one or two immutable model references as an allowlist. The first
model produces the parent prefix. Fork selects candidates from those resolved
inputs, preserving their IDs and digests rather than looking up mutable names.
The supported offline ModelConfig has `provider: fixture`, `revision: text-v1`,
exactly one `fixture_response` parameter, and an artifact digest matching that
UTF-8 response. Gateway routes are `inference: fixture://text-v1` and
`tool.echo: fixture://echo-v1`; the optional `tool.unknown` route uses
`fixture://unknown-effect-v1`. No external provider, GPU, network tool, fallback,
provider secret or paid model call participates in this profile.

The synthetic unknown-effect adapter records an ambiguous mutating outcome.
Neither a step commit nor a checkpoint can silently pass that unresolved call.
Identical call retries reconcile the retained result; they do not replay the
mutating adapter. Payload hashes, request IDs, selected model, adapter version,
route digest, execution epoch and ordered registration/completion events are
retained in Postgres.

## Python hooks and the two acknowledgements

`hypercall.harness.AgentHarness` connects only to the guest-local Unix gRPC
socket. The Python process receives no ingress key or provider credential. The
authenticated controller/host assignment and provisioned guest session establish
authority. A sealed `HarnessSession` binds tenant, project, logical branch,
physical sandbox, execution ID/epoch, call namespace, route/model digests and
protocol. Its digest is SHA-256 over deterministic protobuf bytes with the
`session_hash` field empty; Go and Python verify the same representation.

Actions are sequential and restricted to the foreground owner thread. A
background attempt fails before waiting on the action lock or dispatching an RPC.
Arbitrary external threads, background side effects and independent SDK clients
are outside this instrumentation contract. Public Exec and WriteFile reject
mutations while the cooperative profile runs; ReadFile remains available for
inspection during a live assignment.

`commit_step()` registers the exact state bytes and returns their committed
cursor/digest. `await_resume()` closes the action gate. The controller checks that
the step is complete and no action has an unresolved outcome, appends the sealed
barrier event, and delivers that final marker. Its cursor can advance beyond the
original step-commit cursor. Native capture and restore require the **exact sealed
marker**, including source branch/execution/epoch/session, step, state digest and
final event sequence, in the durable Checkpoint and native artifact manifest.

On restore, Python verifies the refreshed session and inherited marker, closes
and recreates inherited Unix gRPC clients, invalidates cached model clients, and
reseeds the supported module-level Python RNG from the renewed kernel CSPRNG.
Independent `random.Random` instances and third-party RNG caches are unsupported.
Then two acknowledgements occur:

1. The Python hook acknowledges its local refreshed configuration. This allows
   native rebind to finish, while the guest action gate remains closed.
2. After durable controller state is committed, the controller records the exact
   session acknowledgement and delivers it through authenticated host/vsock RPCs.
   Only then does the Python acknowledgement return and the agent dispatch suffix
   actions. Old model-client objects fail locally before issuing a call.

Requests have a fresh execution-scoped call namespace after restore. A forked
child gets a new logical branch, physical sandbox and execution session. A resume
on the same branch also refreshes the physical sandbox/execution and epoch.
Checkpoint lineage and the remaining active execution allowance are durable;
restoring a snapshot does not grant a new unlimited task lifetime.

## Public API, CLI and SDK

The public v1 contract adds Fork, Calls, Steps and Compare, with generated REST
and OpenAPI mappings. Fork returns an aggregate Operation and independently
inspectable child Branch resources; replaying the same idempotency key preserves
their IDs. Child operations retain the parent operation and checkpoint IDs so
partial restoration remains visible and recoverable.

```sh
hypercall --key fork-example fork CHECKPOINT_ID MODEL_A_ID@sha256:DIGEST MODEL_B_ID@sha256:DIGEST
hypercall --limit 100 calls CHILD_BRANCH_ID
hypercall --after REGISTERED_SEQUENCE --limit 100 calls CHILD_BRANCH_ID
hypercall --limit 100 steps CHILD_BRANCH_ID
hypercall compare CHILD_A CHILD_B output_bytes_v1
```

Global connection/authentication flags follow [client setup](clients.md).
Calls pages contain inference/tool actions, excluding step-commit/finalize
control records. Steps expose committed state digests/cursors. Events retain the
complete ordered history. Calls use an exclusive **registration event sequence**
cursor; Steps use an exclusive step number. Every page returns its next cursor.
Limits are bounded to 1,000 and invalid oversized requests fail rather than
silently suggesting a complete trace.

```python
from hypercall import Client, types

with Client(endpoint, project=project, api_key=key) as client:
    run = client.inspect("Run", run_id)
    candidates = [types.Ref(id=m.metadata.id, digest=m.metadata.digest)
                  for m in run.run_spec.models]
    fork = client.fork(checkpoint_id, candidates, idempotency_key="fork-example")
    calls = list(client.iter_calls(fork.children[0].metadata.id, page_size=100))
    steps = list(client.iter_steps(fork.children[0].metadata.id, page_size=100))
    difference = client.compare(fork.children[0].metadata.id,
                                fork.children[1].metadata.id,
                                comparator="output_bytes_v1")
```

AsyncClient provides the same unary methods and async page iterators. Requests
are not retried automatically. Inspect the returned Operation after an ambiguous
mutation and reconcile with the identical idempotency key.

## Declared continuation comparators

| Version | Compares | Expected fixture difference |
| --- | --- | --- |
| `output_bytes_v1` | Inference response bytes | Step 2, position 1 |
| `output_normalized_v1` | Inference UTF-8 output with surrounding whitespace trimmed | Step 2, position 1 |
| `tool_calls_v1` | Tool name and registered argument bytes | Step 3, position 1 |
| `observations_v1` | Tool observation bytes | Step 3, position 1 |

Comparisons include the committed common prefix through checkpoint lineage.
Both branches must finalize complete instrumented traces. Missing instrumentation,
unresolved effects, incompatible lineage and incomplete outcomes return explicit
unknown/incomplete results. Declared ancestry/trace bounds fail with an error;
they do not silently omit later calls. A reported divergence is the first
**observed** difference at the versioned comparator's granularity. It makes no
causal or whole-task reliability claim. Replay and arbitrary historical
instructions are not substitutes for retained checkpoints.

## Offline Python image packaging

The SDK/harness supports Python 3.12–3.13. Development remains pinned to 3.13.9;
native hook tests also run under 3.12.14. The existing Ubuntu guest uses 3.12.3.
`deploy/runtime/harness-wheels.lock.json` pins exact CPython-3.12 Linux ARM64 and
AMD64 wheels, URLs, sizes and SHA-256 values for grpcio 1.76.0, protobuf 6.33.0 and
typing-extensions 4.16.0. The upstream distribution metadata declares support for
Python 3.12 ([grpcio](https://pypi.org/project/grpcio/1.76.0/),
[protobuf](https://pypi.org/project/protobuf/6.33.0/)).

```sh
uv run --frozen python scripts/build-harness-bundle.py --arch arm64 --output /absolute/new-harness.tar.gz
```

The archive contains a manifest, the locally built SDK wheel, all locked runtime
wheels and a fixed launcher. Complete wheel metadata and license material are
preserved. Tar/gzip metadata is deterministic and includes no developer-local
source paths or credentials. Online fetching happens only during this explicit
bundle-build step. Image construction accepts `--harness-bundle` and
`--harness-bundle-sha256`, validates the archive and wheel hashes/paths, installs
offline into the read-only template, and checks imports in the image. The fixed
launcher runs `/usr/bin/python3 -I -u /opt/hypercall/agent.py --socket SOCKET`.
No pip index access or dependency download occurs in the guest.

## Native and real-host acceptance

`make verify` covers generation, race tests, Python RPC/refusal contracts, offline
installation integrity, cross-builds and packaging. `make test-postgres` requires
an explicit real database and exercises durable calls/steps/barriers/forks,
gateway fencing, comparisons, plus composed public CLI and sync/async SDK access.
Byte-map or RPC conformance adapters remain explicitly labeled as such.

The opt-in real workflow requires a configured host with `--cooperative-agents`
and an explicit persistent snapshot store. The API uses
`--host-cooperative-agents` with its existing mTLS host configuration. Prepare
private operator inputs as in [the Phase 2A runtime procedure](phase2a-runtime.md),
using profile `cooperative_python_v1_firecracker`, a cooperative manifest with two
distinct fixture models, and the corresponding API/CLI binary hashes. Then:

```sh
export HYPERCALL_RUNTIME_2B_TEST=1
export HYPERCALL_2B_CONFIG=/absolute/private/operator.json
export HYPERCALL_2B_REPORT=/absolute/new-phase2b-report.json
make test-runtime-2b
```

`scripts/qualify-phase2b` verifies the real prefix/semantic checkpoint, restarts
its own API/controller, forks and completes each candidate sequentially from the
same retained checkpoint, validates fresh identity and durable route ACK before
calls, checks suffix results and all four declared divergences, and cancels the
paused parent. Its report states the sequential schedule explicitly. Native
batched-fan-out capacity remains unqualified. Only the operator starts the host;
the script does not start/reconfigure Lima or provision infrastructure.

Schema 004 adds authoritative call/step ledgers and semantic lineage state.
The real schema-3 upgrade preserved prior rows; repeated migration and restored
database migration checks passed. An older Phase 2A binary refuses the newer
schema instead of writing through an unsupported version. The backup drill now
compares all ten owned tables, including agent_calls/agent_steps; schema-3 backups
report those tables absent explicitly. Compatibility refusal is not an automatic
rollback mechanism. Restored execution databases still require host/credential
fencing before reopening admission as documented in [control-plane recovery](control-plane.md).
