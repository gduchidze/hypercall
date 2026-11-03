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
hypercall-guest --workspace /workspace --session-config /run/hypercall/session.json --uid 1000 --gid 1000
```

`--test-unix-socket PATH` explicitly enables **trusted local tests only**, using
same-user commands with no VM isolation. It refuses an existing socket path.
Unix mode never starts by default. TCP is not supported. Commands use a child
helper with `fchdir` on the inherited, confined directory descriptor, after any
UID/GID drop and before replacing the helper with the requested executable.

Commands use explicit argv, workspace-relative cwd, a required positive timeout
up to ten minutes, and no inherited environment. PATH is fixed to
`/usr/local/bin:/usr/bin:/bin`; LANG defaults to `C.UTF-8`. Request environment keys
must appear in `--allow-env` (default `LANG,LC_ALL,TZ,PYTHONUNBUFFERED`); the map,
names, values and total request bytes are bounded. At most 16 commands run at
once. Output is raw bytes, split into stdout/stderr events, with a shared 1 MiB
budget. Exceeding the budget kills the group and marks the terminal result.
Nonzero exits and command timeouts are terminal results; malformed requests and
RPC cancellation are gRPC errors. A successful stream has exactly one terminal
result, following all output. A receiver stalled for one second is rejected.

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
exercise workload UID/GID and transfer ownership; a tmpfs-capability test checks
ENOSPC atomicity. These do not qualify KVM boot, vsock connectivity, VM security,
cgroups, guest crash recovery or hostile descendants. Process-group kill cannot
contain a descendant that escapes its group. Workload cgroups and whole-VM stop
must own that recovery. `os.Root` does not prohibit bind mounts, hard links or
special filesystem mounts; the template must provide a private workspace and
deny workload mount privileges. Qualification belongs to the Linux VM suite.
