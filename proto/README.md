# Private guest protocol

`hypercall.guest.v1.Guest` is the private host-to-guest interface. Production uses vsock;
the explicitly enabled Unix socket is only for trusted local tests.

Each RPC carries exactly one `authorization: Bearer <session credential>` metadata
value. The guest validates the out-of-band configured credential and expiry,
which bind tenant, project, sandbox, execution and epoch. The response identity
is diagnostic information, not a substitute for authenticated transport/session
configuration. New allocations and forks need fresh credentials and sessions.

`Exec` takes explicit argv, workspace-relative cwd, bounded environment overrides
and a required positive timeout. It runs foreground work without an implicit
shell or PTY. Output is binary stdout/stderr events, followed by one result when
the RPC succeeds. Cancellation/deadline/disconnection ends foreground work and
may terminate with a gRPC error without a result. An exit code from the process
is a result, not a gRPC failure. A timeout is reported by `timed_out`. A bounded
output stream reports truncation explicitly. Detached jobs are unsupported.

File transfer is binary, unary, and limited to 1 MiB per request/response. Paths
are relative to the configured workspace; traversal and symlink escape are
rejected. File transfers do not create parent directories. Full filesystem, PTY,
stdin and generic command reconnection remain deferred.

The bounded Phase 1B protocol adds a guest-owned Python qualification process.
`StartControlled` starts it; `InspectControlled` reads its last acknowledged
barrier; `ContinueControlled` changes its in-memory counter and syncs its private
workspace disk. This process survives host/vsock disconnection. `PrepareCapture`
rejects in-flight guest work and closes ordinary RPC access before VM capture.
The source host reconnects and uses `ReleaseCapture`, which verifies its original
unmounted session disk and refreshes guest UTC too. Monotonic guest clocks are
retained; elapsed pauses and external effects are not rolled back. Restored guests require `RebindClone`: authenticate from
a private replacement session disk after invalidating its inherited block cache,
verify fresh scoped sandbox/execution identity and the new bearer, validate a fresh
host UTC sample against session expiry, set `CLOCK_REALTIME`, inject 256 fresh host entropy bits,
force a kernel CRNG reseed, and explicitly reseed the controlled Python RNG before
opening workload access. VM restore also resets vsock transports; no NIC exists.
This protocol does not renew arbitrary application RNG caches or credentials;
semantic agent checkpoint and provider routing protocols belong to Phase 2B.

Run `make generate` to regenerate and `make generate-check` to reject drift.
Go 1.27.3, Python 3.13.9 and uv 0.12.11 are pinned. `uv.lock` pins grpcio-tools
1.76.0 (bundled protoc 31.1), protobuf 6.33.0 and grpcio 1.76.0; the generation
script pins protoc-gen-go 1.36.12 and protoc-gen-go-grpc 1.5.1. Generated Python
imports use `hypercall_proto.hypercall.guest.v1`. Commit generated sources with
the schema. Removed proto fields must reserve their names and numbers; this is
the initial unreleased contract, so no released compatibility baseline exists.

`make test-contract` builds the real Go guest, starts its private test socket
inside a temporary directory, and exercises it with the Python gRPC client.
These tests verify interoperability and local process/file behavior. They do
not qualify vsock, Firecracker, jailer, KVM, tenant isolation or VM lifecycle.
`make build` checks darwin/arm64, linux/arm64 and linux/amd64. `make verify` also
runs generation drift, race tests and vet. CI executes the same checks on Linux
and macOS; real Linux/KVM runtime qualification is a separate requirement.
