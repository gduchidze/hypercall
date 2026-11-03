package sandbox_test

import (
	"errors"
	"testing"
	"time"

	"github.com/gduchidze/hypercall/internal/sandbox"
)

func TestRestoreRequiresFixedIdentityAndOperation(t *testing.T) {
	for _, r := range []sandbox.RestoreRequest{{}, {OperationID: "operation", CheckpointID: "checkpoint"}} {
		if err := r.Validate(); !errors.Is(err, sandbox.ErrInvalid) {
			t.Fatalf("unsafe restore accepted: %v", err)
		}
	}
}

func TestForkRejectsDuplicateChildrenAndCrossScope(t *testing.T) {
	child := sandbox.CreateRequest{Allocation: sandbox.Allocation{TenantID: "tenant", ProjectID: "project", SandboxID: "child", ExecutionID: "exec", Epoch: 1}, Limits: sandbox.Limits{VCPUs: 1, MemoryMiB: 256, WorkspaceBytes: 16 << 20, PIDs: 64, TTL: time.Minute}}
	r := sandbox.ForkRequest{OperationID: "fork", CheckpointID: "checkpoint", Children: []sandbox.CreateRequest{child, child}}
	if err := r.Validate(); !errors.Is(err, sandbox.ErrInvalid) {
		t.Fatalf("duplicate identity accepted: %v", err)
	}
	r.Children[1].Allocation.SandboxID = "second"
	r.Children[1].Allocation.TenantID = "other"
	if err := r.Validate(); !errors.Is(err, sandbox.ErrInvalid) {
		t.Fatalf("cross scope accepted: %v", err)
	}
}
