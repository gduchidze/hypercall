//go:build linux

package guest_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"github.com/gduchidze/hypercall/internal/guest"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCancellationStopsDescendantsThatCreateNewSessions(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires Linux root and cgroup v2 delegation")
	}
	client, _ := startGuest(t, func(cfg *guest.Config) {
		cfg.TrustedLocal = false
		cfg.UID, cfg.GID = 65534, 65534
		cfg.ExecHelper = workloadExecutable(t)
		cfg.WorkloadCgroup = workloadCgroup(t)
		if err := os.Chown(cfg.Workspace, cfg.UID, cfg.GID); err != nil {
			t.Fatal(err)
		}
	})
	ctx, cancel := context.WithCancel(authorized(t))
	defer cancel()
	stream, err := client.Exec(ctx, &guestv1.ExecRequest{Argv: []string{"/bin/sh", "-c", `setsid /bin/sh -c 'echo ready; sleep 0.5; echo escaped > escaped' & wait`}, TimeoutMs: 3000})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || !strings.Contains(string(first.GetStdout()), "ready") {
		t.Fatalf("descendant not ready: %v %v", first, err)
	}
	cancel()
	for err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
	time.Sleep(700 * time.Millisecond)
	_, _, result, err := collect(t, client, authorized(t), &guestv1.ExecRequest{Argv: []string{"/bin/sh", "-c", "test ! -e escaped"}, TimeoutMs: 1000})
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("detached descendant survived cancellation: %v %v", result, err)
	}
}

// This exercises real Linux uid/gid dropping and transfer ownership, separately
// from trusted local RPC tests. It does not qualify VM/cgroup isolation.
func TestProductionWorkloadCanReadTransfersAndWriteOutputAsNonRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires Linux root daemon to exercise workload uid/gid")
	}
	client, _ := startGuest(t, func(cfg *guest.Config) {
		cfg.TrustedLocal = false
		cfg.UID, cfg.GID = 65534, 65534
		// Go's root-owned build directory can be 0700. Place this test helper
		// in a dedicated searchable directory, like the installed guest binary.
		cfg.ExecHelper = workloadExecutable(t)
		cfg.WorkloadCgroup = workloadCgroup(t)
		if err := os.Chown(cfg.Workspace, cfg.UID, cfg.GID); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := client.WriteFile(authorized(t), &guestv1.WriteFileRequest{Path: "input", Data: []byte("transferred")}); err != nil {
		t.Fatal(err)
	}
	out, _, result, err := collect(t, client, authorized(t), &guestv1.ExecRequest{Argv: []string{"/bin/sh", "-c", "id -u; cat input; printf guest > output"}, TimeoutMs: 1000})
	if err != nil || result == nil || result.ExitCode != 0 || !strings.Contains(out, "65534\ntransferred") {
		t.Fatalf("workload identity/ownership: out=%q result=%v err=%v", out, result, err)
	}
	read, err := client.ReadFile(authorized(t), &guestv1.ReadFileRequest{Path: "output"})
	if err != nil || string(read.GetData()) != "guest" {
		t.Fatalf("workload output: %v %v", read, err)
	}
	_, _, result, err = collect(t, client, authorized(t), &guestv1.ExecRequest{Argv: []string{"/bin/sh", "-c", `group=/sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup); cat "$group/cgroup.procs" && test ! -w "$group/pids.max" && grep -q '^NoNewPrivs:[[:space:]]*1$' /proc/self/status`}, TimeoutMs: 1000})
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("workload cannot inspect its containment without modifying it: %v %v", result, err)
	}
}

func workloadCgroup(t *testing.T) string {
	t.Helper()
	parent := os.Getenv("HYPERCALL_GUEST_TEST_CGROUP")
	if parent == "" {
		parent = "/sys/fs/cgroup"
	}
	path, err := os.MkdirTemp(parent, "hypercall-guest-test-")
	if err != nil {
		t.Fatalf("create delegated workload cgroup: %v", err)
	}
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0)
		if err := os.Remove(path); err != nil {
			t.Errorf("workload cgroup cleanup: %v", err)
		}
	})
	for name, value := range map[string]string{"memory.max": "268435456", "pids.max": "64", "cpu.max": "100000 100000", "cgroup.subtree_control": "+cpu +memory +pids"} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(value), 0); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func workloadExecutable(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hypercall-test-helper-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	helper := filepath.Join(dir, "guest.test")
	destination, err := os.OpenFile(helper, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(destination, source); err != nil {
		_ = destination.Close()
		t.Fatal(err)
	}
	if err := destination.Close(); err != nil {
		t.Fatal(err)
	}
	return helper
}

func TestStorageFailureDoesNotPublishPartialDestination(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires Linux root and tmpfs mount capability")
	}
	mount := t.TempDir()
	if err := unix.Mount("tmpfs", mount, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "size=65536"); err != nil {
		t.Skipf("tmpfs mount capability unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount(mount, 0); err != nil {
			t.Errorf("unmount tmpfs: %v", err)
		}
	})
	client, _ := startGuest(t, func(cfg *guest.Config) { cfg.Workspace = mount })
	original := []byte("previous complete destination")
	if _, err := client.WriteFile(authorized(t), &guestv1.WriteFileRequest{Path: "destination", Data: original}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.WriteFile(authorized(t), &guestv1.WriteFileRequest{Path: "destination", Data: bytes.Repeat([]byte("x"), 128<<10)}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("storage failure: %v", err)
	}
	read, err := client.ReadFile(authorized(t), &guestv1.ReadFileRequest{Path: "destination"})
	if err != nil || !bytes.Equal(read.GetData(), original) {
		t.Fatalf("failed write changed destination: %v %v", read, err)
	}
}
