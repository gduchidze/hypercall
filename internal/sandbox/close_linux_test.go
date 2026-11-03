//go:build linux

package sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/gduchidze/hypercall/internal/sandbox"
)

// This operator fault test uses real pinned artifacts and a delegated cgroup
// parent. It never boots a VM. The bind mount makes asset removal fail with
// EBUSY even for root, exercising Close's actual filesystem error path.
func TestCloseRetriesAssetCleanupAndReleasesStateOwnership(t *testing.T) {
	config, cgroup, state := os.Getenv("HYPERCALL_RUNTIME_CONFIG"), os.Getenv("HYPERCALL_RUNTIME_CGROUP"), os.Getenv("HYPERCALL_RUNTIME_STATE")
	if config == "" || cgroup == "" || state == "" {
		t.Skip("set HYPERCALL_RUNTIME_CONFIG, HYPERCALL_RUNTIME_CGROUP and HYPERCALL_RUNTIME_STATE for the Linux operator fault test")
	}
	if os.Geteuid() != 0 {
		t.Fatal("Linux operator fault test requires root mount/cgroup privileges")
	}
	dir, err := os.MkdirTemp(state, "close-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfg := sandbox.Config{Scope: sandbox.Scope{TenantID: "qualification", ProjectID: "close-retry"}, ArtifactConfigPath: config, StateDir: dir, CgroupDir: cgroup, JailerUID: 1000, JailerGID: 1000}
	backend, err := sandbox.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	assets, err := filepath.Glob(filepath.Join(dir, "assets-*"))
	if err != nil || len(assets) != 1 {
		t.Fatalf("expected one verified asset directory: %v %v", assets, err)
	}
	if err = syscall.Mount(assets[0], assets[0], "", syscall.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	mounted := true
	defer func() {
		if mounted {
			_ = syscall.Unmount(assets[0], 0)
		}
	}()
	if err = backend.Close(); err == nil {
		t.Fatal("Close must report the mounted asset cleanup failure")
	}
	if err = syscall.Unmount(assets[0], 0); err != nil {
		t.Fatal(err)
	}
	mounted = false
	if err = backend.Close(); err != nil {
		t.Fatalf("Close must retry cleanup after fault removal: %v", err)
	}
	reopened, err := sandbox.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("successful Close retry must release state ownership: %v", err)
	}
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
