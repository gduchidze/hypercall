# Host development and qualification

`hypercall-host doctor` checks prerequisites only. A ready report does not prove
microVM boot, guest communication, isolation, cleanup under failure, or completion
of Phase 1A. There is no runtime backend, fake lifecycle, snapshot, or fork in
this command. Run it with the same identity, capabilities, and delegated cgroup
directory intended for the host process.

## Build and inspect

Use the repository-pinned Go toolchain (`.go-version`). From the repository root:

```sh
go build -trimpath -buildvcs=false -o bin/hypercall-host ./cmd/hypercall-host
bin/hypercall-host doctor --json
```

Exit statuses are 0 for all prerequisites passed, 1 for a structured blocked
report, and 2 for command usage or output failure. Missing `--config` is a blocker;
the command never downloads artifacts or treats missing files as successful work.
The report lists OS, Go architecture names (`amd64` or `arm64`), individual checks,
evidence, and blocker IDs. Keep reports with the tested kernel/host configuration.

On Linux it opens `/dev/kvm` read/write and issues `KVM_GET_API_VERSION` (requires
12). It checks cgroups v2 and enabled/delegated `cpu`, `memory`, and `pids`
controllers. It creates an empty temporary child, opens its control files for
writing, and removes it. It never moves processes or enables parent controllers.
Cleanup failures are blockers and identify the residual directory. Supply a
properly delegated directory with `--cgroup-dir`; the default is `/sys/fs/cgroup`.
The namespace probe starts and reaps `/bin/true` in new mount, network, PID, IPC,
and UTS namespaces. It does not change the invoking process's namespaces.

## Explicit artifacts

Supply operator-controlled, nonempty regular files with no group/world write
permission. All artifact paths are absolute. Special files are opened without
blocking and rejected using the opened descriptor's type; final path-component
symlinks are rejected. JSON schemas reject unknown fields
and additional JSON values; schema version 1 is the only supported version.
Replace every placeholder below with the selected, reviewed artifact value.
The SHA-256 fields contain 64 hexadecimal characters, without a `sha256:` prefix.

```json
{
  "schema_version": 1,
  "firecracker": {
    "path": "/srv/hypercall/artifacts/firecracker",
    "sha256": "<reviewed binary SHA-256>",
    "version": "Firecracker v<selected version>"
  },
  "jailer": {
    "path": "/srv/hypercall/artifacts/jailer",
    "sha256": "<reviewed binary SHA-256>",
    "version": "Jailer v<selected version>"
  },
  "template": {
    "path": "/srv/hypercall/artifacts/template.json",
    "sha256": "<reviewed manifest SHA-256>"
  }
}
```

The pinned template manifest describes the exact kernel, root filesystem, and
guest-agent artifacts. Integrity does not establish that the agent is installed
in the root filesystem, protocol compatibility, kernel capability, or bootability.

```json
{
  "schema_version": 1,
  "architecture": "arm64",
  "kernel": {"path": "/srv/hypercall/artifacts/vmlinux", "sha256": "<SHA-256>"},
  "rootfs": {"path": "/srv/hypercall/artifacts/rootfs.ext4", "sha256": "<SHA-256>"},
  "guest_agent": {"path": "/srv/hypercall/artifacts/hypercall-guest", "sha256": "<SHA-256>"}
}
```

The manifest architecture must equal the host architecture. After a binary's
digest matches, doctor runs its `--version` with a five-second deadline, a minimal
environment, and bounded output. The configured version is the exact trimmed
output. On Linux it executes the verified open descriptor through `/proc/self/fd`,
so replacing the configured pathname cannot redirect execution. Darwin does not
support that execution path; it copies the descriptor to a private directory
owned by the effective UID with mode 0700, rechecks the copied bytes' digest, and
executes the private copy with mode 0500. Temporary-copy cleanup failures block
qualification. Manifest parsing also uses its verified descriptor.

Keep artifacts immutable while qualification runs. Mode checks do not authenticate
the original files' owner or ancestor directories; doctor does not prevent a
privileged or same-owner process from modifying the same inode or the diagnostic's
private files. It is a diagnostic for trusted operator-controlled artifacts, not
an enforcement mechanism against their owner. No original artifact pathname is
reopened for version execution or verified manifest parsing.
No Firecracker build is implicitly substituted for E2B's custom fork. The E2B
`v1.14-0.2.0` artifact/capability gap requires resolution before lifecycle reuse;
an official Firecracker prerequisite probe does not qualify those custom features.

```sh
bin/hypercall-host doctor --json --config /srv/hypercall/artifacts/runtime.json \
  --cgroup-dir /sys/fs/cgroup/hypercall
```

## Local Mac alternative: nested Linux with Lima

The development template is [lima-arm64.yaml](../../deploy/dev/lima-arm64.yaml).
It requires Lima 2.2.1, Apple Silicon M3 or later, and macOS 15 or later with
Virtualization.framework reporting nested virtualization support. The local M5
host supports that API; only a successful Linux KVM probe establishes access
inside a particular VM. M1/M2 and unsupported hosts need a separate Linux/KVM
machine for runtime integration. Native Mac contract/CLI tests still run locally.

```sh
test "$(limactl --version)" = "limactl version 2.2.1"
limactl validate deploy/dev/lima-arm64.yaml
limactl start --name hypercall-dev --tty=false deploy/dev/lima-arm64.yaml
GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false \
  -o bin/hypercall-host-linux-arm64 ./cmd/hypercall-host
limactl copy bin/hypercall-host-linux-arm64 hypercall-dev:/tmp/hypercall-host
limactl shell hypercall-dev sudo /tmp/hypercall-host doctor --json
```

The final command should report KVM separately and remain blocked on missing
artifacts. Provisioning requires a `/dev/kvm` character device; doctor additionally
proves read/write access and the API ioctl. Do not use `chmod 666 /dev/kvm`.
Root use above is an isolated development probe, not a production privilege model.

The Ubuntu 24.04 ARM64 image is pinned to Canonical release `20260926`, with the
digest verified against [Canonical's SHA256SUMS](https://cloud-images.ubuntu.com/releases/noble/release-20260926/SHA256SUMS).
Lima enforces that digest; there is no mutable image fallback. The approximately
620 MB image uses a local 2-vCPU, 4-GiB RAM, 20-GiB disk cap, adjustable for local
tests. These are development defaults, not measured production capacity. No cloud
resources or paid providers are provisioned. Stop the VM when unused:

```sh
limactl stop hypercall-dev
```

The template mounts no host directories, forwards no SSH agent/public keys,
inherits no proxy variables, disables containerd, and suppresses application port
forwarding. Lima's management SSH remains available. A development firewall blocks
new guest connections to Lima's default Mac-loopback endpoint `192.168.5.2` and
cloud metadata `169.254.169.254`, while allowing established management replies.
This is an endpoint-specific development restriction; other host/private-network
paths and untrusted guest egress still need the sandbox network implementation
and real isolation tests. Keep credentials and untrusted workloads out of this
qualification VM. Review Lima's global configuration before creating it, since
global settings can change the effective configuration.

ARM64 development results do not qualify the planned Linux x86_64 production
pool, establish cross-architecture snapshot portability, or satisfy Phase 1B.

On 2026-11-03 the local Apple M5/macOS 27.0.1 host ran this development VM with
Lima 2.2.1 and Ubuntu kernel `6.8.0-142-generic`. The real doctor passed Linux
ARM64, KVM API 12, delegated cgroup creation/cleanup, and namespace creation.
It exited 1 with only `runtime.configuration` blocked because no runtime artifacts
were supplied. The [unaltered report](../validation/artifacts/1A/linux-arm64-host-prerequisites.json)
and [environment/build metadata](../validation/artifacts/1A/linux-arm64-host-environment.json)
record the executable and source hashes. This establishes a usable local KVM
development path while runtime qualification remains incomplete.

## Tests and remaining acceptance work

```sh
go test -race ./cmd/hypercall-host ./internal/sandbox/hostdoctor
go test -tags=kvm -v ./test/integration/kvm
HYPERCALL_KVM_TEST=1 HYPERCALL_HOST_CONFIG=/srv/hypercall/artifacts/runtime.json \
  HYPERCALL_CGROUP_DIR=/sys/fs/cgroup/hypercall \
  go test -count=1 -tags=kvm -v ./test/integration/kvm
```

Without explicit opt-in the KVM test reports a skip, which is not acceptance
evidence. With opt-in, missing artifacts or any unsuitable prerequisite fails.
CLI fixtures test real subprocess exits/JSON, hash rejection before execution,
version pinning, changed disk detection, prompt FIFO rejection, and pathname
replacement during executable interpretation. The [Linux ARM64 CLI evidence](../validation/artifacts/1A/linux-arm64-cli-regressions.json)
records the descriptor-execution and FIFO regressions inside the local VM;
native Mac tests exercise the private verified-copy path. They deliberately use a non-cgroup
temporary directory and fixture executables so they cannot qualify a host.
The next runtime work must implement and test actual jailer boot, vsock commands
and files, scoped authorization, limits, cancellation, cleanup, and failure
recovery on pinned artifacts. No upstream test suite was run by this original
host-diagnostic implementation; the provenance ledger records its source references.
