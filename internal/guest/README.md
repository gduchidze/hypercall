# Guest RPC execution

`Guest` is the private `hypercall.guest.v1` gRPC adapter. Its production listener
is Linux AF_VSOCK (default port 1024), with a root daemon and an explicitly
configured non-root workload UID/GID. The workspace must already exist, belong
to that UID/GID, and grant its owner read/write/search permission. Command
execution itself is **not a host sandbox**; the daemon belongs in a VM.

Provision a regular mode-0600 `--session-config` JSON file out of band:

```json
{"identity":{"tenant_id":"tenant-1","project_id":"project-1","sandbox_id":"sandbox-1","execution_id":"execution-1","epoch":1},"credential":"replace-with-32-or-more-random-credential-bytes","expires_at":"2026-11-03T20:00:00Z"}
```

Expiry must be in the future and no more than 24 hours away. Send exactly one
`authorization: Bearer <credential>` metadata value on every RPC. Identity is
server-configured; a fresh allocation/epoch must receive a fresh credential.
Credential expiry terminates an admitted command. No credential is logged.

```sh
hypercall-guest --workspace /workspace --session-config /run/hypercall/session.json --uid 1000 --gid 1000 --workload-cgroup /sys/fs/cgroup/workload
```

`--test-unix-socket PATH` explicitly enables **trusted local tests only**, using
same-user commands with no VM isolation. It refuses an existing socket path.
Unix mode never starts by default. TCP is not supported. Commands use a child
helper with `fchdir` on the inherited, confined directory descriptor, after any
UID/GID drop and before replacing the helper with the requested executable.
Linux helpers set `no_new_privs`. Production requires a root-owned cgroup v2
subtree with finite CPU, memory and PID limits and those controllers delegated.
Each command enters its own child cgroup atomically before executing. Cancellation,
timeout and foreground completion kill all members, including `setsid` descendants.
The workload UID must have no write access to the cgroup hierarchy.

Commands use explicit argv, workspace-relative cwd, a required positive timeout
up to ten minutes, and no inherited environment. PATH is fixed to
`/usr/local/bin:/usr/bin:/bin`; LANG defaults to `C.UTF-8`. Request environment keys
must appear in `--allow-env` (default `LANG,LC_ALL,TZ,PYTHONUNBUFFERED`); the map,
names, values and total request bytes are bounded. At most 16 commands run at
once. Output is raw bytes, split into stdout/stderr events, with a shared 1 MiB
budget. Exceeding the budget kills the command descendants and marks the terminal result.
Nonzero exits and command timeouts are terminal results; malformed requests and
RPC cancellation are gRPC errors. A successful stream has exactly one terminal
result, following all output. A receiver stalled for one second is rejected.
With the pinned Go/Python gRPC versions, a client deadline can surface as
`DEADLINE_EXCEEDED` or as `CANCELLED` with HTTP/2 reset code 8 when the server's
deadline timer wins. Both abort the RPC and its command; command-level timeouts
remain explicit terminal results.

Read/write transfers are unary binary bodies limited to 1 MiB. Paths must be
relative, without `..`; `os.Root` confines resolution even across symlink races.
Only regular files are transferred. Parent directories must exist. Writes stage
an exclusive mode-0600 file in an opened parent, set workload ownership, sync,
then rename atomically. Failed staging leaves an existing destination intact.
RPC cancellation racing the final rename can leave a complete published file
without acknowledgement; callers must read back before deciding to retry.

Structured logs contain authentication rejection, operation byte counts and
command duration/result, without command arguments, file paths or output.

The RPC tests run real processes over gRPC bufconn. Linux root tests additionally
exercise workload UID/GID, transfer ownership and cgroup cancellation of detached
descendants; a tmpfs-capability test checks ENOSPC atomicity. These do not qualify
KVM boot, vsock connectivity or guest crash recovery. Whole-VM stop owns recovery
after guest daemon failure. `os.Root` does not prohibit bind mounts, hard links or
special filesystem mounts; the template must provide a private workspace and
deny workload mount privileges. Qualification belongs to the Linux VM suite.

Phase 1B uses a separate guest-owned controlled Python process rather than
foreground `Exec` reconnection. It preserves an in-memory counter, writes
`controlled-state.json`, and creates an unchanged `controlled-origin.txt` disk
sentinel. Its stdin/stdout pipes remain inside the guest across transport reset.
The template mounts a private writable ext4 workspace at `/dev/vdc`; snapshots
capture that disk while paused after the workload barrier and guest `sync`.

The capture gate rejects ordinary operations until the source session has been
verified or a clone has rebound. A clone reads a new root-only session image from
unmounted `/dev/vdb`, first issuing `BLKFLSBUF`, and rejects inherited credentials.
It verifies fresh scoped sandbox/execution identity and the new bearer before
accepting a host UTC sample. With the gate closed it validates session expiry
against that sample and updates `CLOCK_REALTIME`; `CLOCK_MONOTONIC` is retained.
It rotates the bearer and expiry, uses privileged `RNDADDENTROPY` and `RNDRESEEDCRNG` with 256 fresh host bits,
then explicitly reseeds the controlled Python RNG. This supports the pinned ARM
kernel 6.1.155 without claiming ARM VMGenID support or generic userspace RNG
renewal. No network device or provider routing exists in this qualification
workload. Any renewal failure leaves the gate closed and the host terminates the
child. See [snapshot qualification](../../docs/development/snapshots.md).
