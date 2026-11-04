# Isolated scorer workers

`internal/scorer.Runner.Evaluate` executes one pinned Python artifact in one
fresh local Docker container. It implements the private versioned
`hypercall.scorer.v1.Scorer.Score` protobuf RPC. A clean Python child runs the scorer without importing the gRPC runtime,
allowing scorer subprocesses without gRPC fork-handler deadlocks. Its standard
output/error go to `/dev/null`; its bounded tmpfs result is validated by the
parent and host. The worker listens only on its
container-private Unix socket, authenticates a random per-invocation nonce, and
accepts one RPC. The Go client uses a fixed-argv `docker exec` byte relay to that
socket, supporting Docker Desktop without host IPC or listening ports.

## Build and qualify

Use the pinned Go toolchain from `.go-version`, generated contracts, a local
Linux Docker daemon (Docker Desktop is supported), and run:

```sh
scripts/qualify-scorer
```

The script builds `deploy/scorer/Dockerfile`, resolves its immutable local image
ID, and runs the real scorer integration suite under the Go race detector.
`HYPERCALL_SCORER_IMAGE=sha256:... go test -race ./internal/scorer -v` reuses an
already built image. An unset variable skips the real Docker tests; it does not
qualify isolation. Remote Docker daemons without the same local artifact
filesystem fail closed.

The base Python 3.12.12 image is pinned by digest. `requirements.lock` pins
Linux cp312 arm64/amd64 wheels by SHA256, matching the existing harness wheel
lock. Runtime execution accepts only an inspected exact local `sha256` image
ID, never a mutable image tag. Record the qualified image ID in release or
validation evidence after rebuilding changed worker/protobuf sources.

## Artifact and result contract

Register trusted artifact paths with a `scorer.Config`: `CodePath`,
`CodeDigest`, optional `RubricPath`/`RubricDigest`, `ImageDigest`, unique evaluator
lease-epoch `OwnerID`, CPU, memory, PID and timeout limits. Artifacts must be
regular files (no symlinks) no larger than 1 MiB. The supervisor copies verified bytes into a
private temporary directory, mounts individual files read-only, and the worker
verifies their hashes again before loading Python. Host Python never imports
the artifact. Request pins must match the registered artifact pins.

A scorer exports `score(request: dict) -> dict`. Input keys are `output`,
`variables`, `scorer_digest`, `rubric_digest`, `judge_model_digest`, `parameters`,
and `rubric`. Rubric instructions and untrusted output remain separate fields.
A result requires boolean `pass` and a finite `score` in `[0,1]`; optional
`reason` is at most 8 KiB. Optional `cost` requires nonnegative finite `amount`,
three-letter uppercase `currency`, `kind` (`measured`, `estimated` or `synthetic`), and pinned
measurement `source`. Judge costs remain distinct from agent inference costs. Synthetic costs are
fixture accounting inputs, including an explicit zero, and never measured billing.

```python
def score(request):
    approved = request["output"] == request["variables"]["expected"]
    return {"pass": approved, "score": 1.0 if approved else 0.0,
            "reason": "synthetic exact-output fixture"}
```

Judge requests additionally pin the rubric, judge-model digest and effective
parameters. Phase 3 qualifies deterministic judge artifacts only. The worker
has no egress or provider secrets; live provider judges require the Phase 4A
routing policy. Fixture judge scores/costs do not measure real model quality or
billing.

## Limits and recovery

Every container uses network `none`, read-only root filesystem and artifacts,
UID/GID 65532, dropped capabilities, no new privileges, bounded CPU, memory and
swap, PID count, open files, and a 16 MiB non-executable tmpfs. Docker logging is
disabled to prevent scorer stdout from filling host storage. Default Docker
seccomp remains enabled. A container is a separate scorer trust boundary; it is
not the Firecracker sandbox or a production multi-tenant qualification.

The overall deadline includes image inspection, startup, RPC and transport.
A separate trusted Python PID 1 supervises the worker with an independent
monotonic lifetime bounded by the configured timeout, starting at container
startup. It remains active after evaluator SIGKILL, lost transport or client
crash. On expiry it exits with status 124; Linux terminates every remaining
process in that PID namespace, including detached or signal-ignoring scorer
children. PID 1 disables dumpability to prevent same-UID scorer descendants
from rewriting its memory; unhandled stop signals cannot stop namespace PID 1.
Request/response messages are limited to 1 MiB. Missing, malformed, non-finite,
timeout, cancellation, artifact, image and worker-death results cannot pass.
Cleanup runs with a separate bounded context, force-removes the entire container
and its descendants, and preserves cleanup failures as nonpassing errors.

On evaluator restart, first fence the previous evaluator lease and then call
`Runner.Recover(ctx)` with that **previous** `OwnerID`. New lease epochs must
use new owner IDs. Recovery removes only containers labeled with the old owner, plus invocation
staging directories scoped by a hash of that owner. After evaluator death the
watchdog bounds active resource consumption; stopped container metadata and
small staged artifacts remain until this fenced recovery runs. Never recover the active owner or globally prune Docker. The durable evaluation
owner decides whether to retry an interrupted score; the runner owns only its
invocation container. Failed cleanup requires operator recovery and must not be
reported as a successful evaluation.

The evaluation adapter preserves scorer error codes and reasons, including
`deadline_exceeded`, `malformed_result`, `worker_error` and `cleanup_error`.
Known scoring costs survive cleanup failures; absent costs remain unknown,
including failed attempts, and are never replaced with a free zero-cost result.
