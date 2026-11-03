# Host development and qualification

`hypercall-host doctor` checks prerequisites only. A ready report does not prove
microVM boot, guest communication, isolation, cleanup under failure, or runtime
qualification. The separate sandbox backend and acceptance driver exercise the
runtime lifecycle. Run doctor with the same identity, capabilities, and delegated cgroup
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
environment, and bounded output. The configured version is the exact first
stdout line; Firecracker adds a timestamped exit log after its version banner.
On Linux it executes the verified open descriptor through `/proc/self/fd`,
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
The cold-boot adapter explicitly selects official Firecracker/jailer 1.14.0.
The unavailable E2B custom fork and its snapshot extensions are not qualified by
this selection. Snapshot, restore and fork are separate work.

```sh
bin/hypercall-host doctor --json --config /srv/hypercall/artifacts/runtime.json \
  --cgroup-dir /sys/fs/cgroup/hypercall
```

## Local Mac alternative: nested Linux with Lima

The development template is [lima-arm64.yaml](../../deploy/dev/lima-arm64.yaml).
It requires Lima 2.2.1, Apple Silicon M3 or later, and macOS 15 or later with
Virtualization.framework reporting nested virtualization support. Only a
successful Linux KVM probe establishes access inside a particular VM.
M1/M2 and unsupported hosts need a separate Linux/KVM
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

ARM64 development results do not qualify an x86_64 deployment or establish
cross-architecture snapshot portability.

## Pinned templates and sandbox execution

[artifacts.lock.json](../../deploy/runtime/artifacts.lock.json) pins the official
Firecracker release, guest kernel and Canonical root filesystem inputs. Fetching
verifies every digest before use. Build templates inside a dedicated Linux VM
without host mounts, using the matching architecture and e2fsprogs 1.47.0:

```sh
scripts/fetch-runtime --arch arm64 --cache /srv/hypercall/inputs
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags=-buildid= \
  -o bin/hypercall-guest ./cmd/hypercall-guest
scripts/guest-notices --guest-agent bin/hypercall-guest --output bin/guest-notices.tar
sudo scripts/build-template --arch arm64 --cache /srv/hypercall/inputs \
  --output /srv/hypercall/template --guest-agent "$PWD/bin/hypercall-guest" \
  --guest-sha256 "$(sha256sum bin/hypercall-guest | cut -d ' ' -f 1)" \
  --guest-notices "$PWD/bin/guest-notices.tar" \
  --guest-notices-sha256 "$(sha256sum bin/guest-notices.tar | cut -d ' ' -f 1)"
```

On a Mac, fetch and cross-build locally, then use `limactl copy` to transfer the
checkout's build scripts, locked inputs, guest binary and notices into the Linux builder.
The builder uses private mount, PID and network namespaces and offline inputs.
It emits an immutable rootfs, kernel, guest binary, manifests, provenance and
upstream license material. Keep that generated license material with distributed
artifacts. Rootfs metadata is normalized for reproducible bytes.

The private Go backend takes a trusted tenant/project scope, explicit artifact
configuration, root-owned mode-0700 state directory, delegated cgroup parent and
unprivileged jailer UID/GID. Use a short state path on a disk-backed filesystem;
the backend stages images and Unix socket paths have a length limit. Never use
a small `/run` tmpfs for image staging.

Guests have a read-only root filesystem and per-allocation bootstrap drive,
ephemeral bounded workspace, authenticated vsock RPC, finite CPU/memory/PID/time
limits and no network device. This profile denies all network egress. Provider
access and persistent workspaces are not part of this profile. Workload processes
enter guest cgroups before executing; jailer processes enter host cgroups before
starting. Runtime recovery kills recorded orphan cgroups before removing their
private disks and secrets. Configure the host supervisor to restart after failure
so recovery can run.

## Real runtime acceptance

```sh
go test -race ./cmd/hypercall-host ./internal/sandbox/hostdoctor
make build-testdriver
sudo bin/hypercall-testdriver qualify \
  --config /srv/hypercall/template/runtime.json \
  --state-dir /var/lib/hcq --cgroup-dir /sys/fs/cgroup/hypercall \
  --python "$PWD/.venv/bin/python" --python-probe "$PWD/scripts/qualify-python.py"
```

The cgroup parent must already have `cpu memory pids` delegated. The acceptance
driver exercises destructive failure scenarios only within its dedicated runtime
state and allocations. Use a dedicated development host and preserve its JSON
report with the source and artifact hashes. Python needs the locked dependencies
and generated package (`uv sync --frozen`).
Use a fresh state directory for each qualification run: host-local epoch fences
deliberately survive shutdown. Admission consumes an epoch even if boot fails;
retries require a higher epoch. Each state directory admits at most 4096 distinct
sandbox fence identities before operator maintenance is required.

The opt-in Go KVM suite runs this driver through `make test-kvm`, with a
nine-minute timeout. Run it as root on the isolated qualification host with the
pinned Go and uv tools on PATH, and set these environment variables to absolute
paths:

| Variable | Value |
| --- | --- |
| `HYPERCALL_HOST_CONFIG` | Pinned template's `runtime.json` |
| `HYPERCALL_CGROUP_DIR` | Dedicated delegated cgroup v2 parent |
| `HYPERCALL_QUALIFICATION_STATE` | Private, fresh runtime state directory |
| `HYPERCALL_PYTHON` | Python executable with the locked dependencies |
| `HYPERCALL_PYTHON_PROBE` | Checkout's `scripts/qualify-python.py` |
| `HYPERCALL_TESTDRIVER` | Built `bin/hypercall-testdriver` |

Optionally set `HYPERCALL_QUALIFICATION_REPORT` to save the JSON report. The Make
target sets `HYPERCALL_KVM_TEST=1`; without that opt-in, direct Go test runs skip
the suite. Skips are not acceptance evidence. Missing configuration or any
failed required gate fails an opted-in run.
CLI fixtures test real subprocess exits/JSON, hash rejection before execution,
version pinning, changed disk detection, prompt FIFO rejection, and pathname
replacement during executable interpretation. Native Mac tests exercise the
private verified-copy path. They deliberately use a non-cgroup
temporary directory and fixture executables so they cannot qualify a host.
The runtime driver checks actual jailer boot, vsock commands/files, Python RPC,
authorization, limits, network denial, independent VMs, cancellation, stop,
crash recovery and cleanup. Native checks alone do not qualify these behaviors.
