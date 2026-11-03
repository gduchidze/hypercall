# Phase 1 — Sandbox foundation

Status: Phase 1A in progress; Phase 1B not started. User authorized implementation
on 2026-11-03. Scope follows [DEVELOPMENT_PHASES.md](DEVELOPMENT_PHASES.md); bounded
product and operating deferrals remain in [PHASE_0.md](PHASE_0.md).

## Delivery waves and dependencies

| Wave | Deliverable | Depends on | Acceptance |
| --- | --- | --- | --- |
| 1 | Pinned toolchains, shared guest protobuf, real Go guest, Python interoperability, executable host qualification command and local Linux preparation | Phase 0 authorization | Generation drift, Go race tests, Python live RPC, cross-builds and an honest host capability report |
| 2 | Pinned Firecracker/jailer, kernel/rootfs/guest artifacts, isolated template preparation, the lifecycle adapter and its sandbox backend contract, vsock and boot/create/stop | Wave 1 and usable Linux/KVM | Boot a real microVM through the sandbox driver and verify commands/files through vsock |
| 3 | Auth, time/resource bounds, cancellation, denied networking, lifecycle telemetry and failure cleanup | Wave 2 | Real KVM unauthorized-access, limits, guest/host-failure and cleanup evidence |
| 1B | Consistent durable snapshots, private child disks, restore/fork and renewed identities/transports | Complete Phase 1A exit gate | Phase 1B acceptance scenario and failure/isolation tests |

Wave 1 local process tests do not qualify a VM or its isolation. A host capability
report establishes prerequisites; it does not boot a sandbox. There is one future
Hypercall lifecycle owner and no imported upstream scheduler/control plane.

## Wave 1 acceptance status

The final integrated root `make verify` passed after the universal guest FD
helper and host FIFO/path-reopen fixes: no generation drift, native Go race tests,
eight live Python contract tests, three target cross-builds, wheel and vet.
The Linux ARM64 guest RPC suite passed all 12 top-level tests with no skips,
including root-daemon/non-root-workload behavior; it used Unix RPC test transport
and was cross-compiled without race instrumentation.

The local Linux host passed actual KVM API, cgroup and namespace prerequisites;
its doctor still exits 1 because real pinned runtime artifacts are absent.
[Validation evidence and exact commands](validation/1A.md),
[host prerequisite report](validation/artifacts/1A/linux-arm64-host-prerequisites.json),
[host environment/source hashes](validation/artifacts/1A/linux-arm64-host-environment.json)
and [CLI regression fixtures](validation/artifacts/1A/linux-arm64-cli-regressions.json)
record the results and limits. Fresh Standards and Spec reviews each reported
0 actionable findings; Spec independently reran the Go race suite successfully. Firecracker/jailer cold boot, authenticated vsock,
the lifecycle adapter and full VM isolation/failure/cleanup qualification remain
outstanding. Phase 1A is in progress and Phase 1B has not started.

## Selected dependency extraction audit

At runtime commit `0446bafb471fe3a12ec961e35f58df36e9bfbbb9`, the inspected
`packages/envd/go.mod` requires the sibling `packages/shared` module through a
local replacement. Its selected command/file paths are mapped in
[third_party/upstreams.yaml](../third_party/upstreams.yaml). `packages/shared/go.mod`
declares GCP/AWS/Azure storage and registry clients, Redis/redislock, Nomad,
Kubernetes clients, LaunchDarkly feature flags, Postgres/queues and telemetry.
This is the upstream module dependency surface, not proof that every dependency
is linked by every guest operation or that each service must run for it.

The first wave extracts the narrow behavior through an original Go guest and
Hypercall protobuf contract. It imports none of those E2B modules or their
control-plane/scheduler code. The actual production Go graph starts from
`google.golang.org/grpc`, `google.golang.org/protobuf` and `github.com/mdlayher/vsock`;
its pinned transitive graph is recorded in `go.mod`/`go.sum`. Python uses the
locked grpcio/protobuf runtime plus explicit generation/build tools in `uv.lock`.
No upstream implementation or fixture was copied or translated, and no upstream
test suite was run. Later lifecycle/storage extraction needs its own package-level
audit rather than assuming this wave qualifies the full E2B graph.

The E2B custom Firecracker repository/revision remains inaccessible. The official
Firecracker source reference is recorded separately and is not a qualified
substitution or a verified runtime artifact. Wave 2 must resolve exact executable,
kernel/rootfs and guest artifact provenance before boot qualification. Wave 1
delivers a host qualification command; a sandbox lifecycle adapter and backend
contract begin with actual lifecycle behavior in Wave 2.

## Local setup and acceptance commands

Install the exact versions in `.go-version` (Go 1.27.3), `.python-version`
(Python 3.13.9), and uv 0.12.11. Put the selected Go toolchain on PATH explicitly;
no shell profile or global environment edit is required. Python can be installed
with `uv python install 3.13.9`. Dependencies are locked in `go.sum` and `uv.lock`.

```sh
uv sync --frozen
make generate-check
make test
make test-contract
make build
make verify
make host-doctor
```

`make generate` regenerates Go and Python bindings with pinned compiler/plugins;
`make generate-check` compares regenerated files without overwriting the checkout.
`make test-contract` builds the actual Go guest, starts its explicit private test
Unix socket, and calls it using Python gRPC. It does not use a mock guest.
`make build` checks darwin/arm64, linux/arm64 and linux/amd64 and builds the Python
wheel with locked build dependencies. `make verify` also runs `go vet`.

The production guest is Linux/vsock. Its test Unix transport executes commands
on the development machine and is only for trusted tests. It needs an explicit
workspace and a 0600 session configuration with credential, allocation identity
and expiry; use the conformance fixture rather than manually embedding secrets.

`make host-doctor` reports missing requirements and returns nonzero when the host
is not ready. macOS cannot supply native Linux KVM. An optional pinned Lima ARM64
VM provides a local development route if nested virtualization exposes `/dev/kvm`;
Lima startup alone does not establish this. See [development/host.md](development/host.md) and `deploy/dev/lima-arm64.yaml`
for its exact configuration and prerequisites. No cloud provisioning is part
of this wave.

## Completion recording

[validation/1A.md](validation/1A.md) owns commands, environment and actual results.
Record the tested source tree/commit, artifact hashes, timings, resource envelope,
cleanup observations and gaps there. A passing local suite does not complete
Phase 1A. Phase 1B begins only after the real Linux/KVM exit gate is evidenced.
