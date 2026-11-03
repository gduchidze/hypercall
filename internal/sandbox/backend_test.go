package sandbox_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gduchidze/hypercall/internal/sandbox"
)

func TestCreateRejectsUnboundedAndUnscopedAllocations(t *testing.T) {
	valid := sandbox.CreateRequest{Allocation: sandbox.Allocation{TenantID: "tenant", ProjectID: "project", SandboxID: "sandbox", ExecutionID: "execution", Epoch: 1}, Limits: sandbox.Limits{VCPUs: 1, MemoryMiB: 256, WorkspaceBytes: 16 << 20, PIDs: 64, TTL: time.Minute}}
	for _, mutate := range []func(*sandbox.CreateRequest){
		func(r *sandbox.CreateRequest) { r.Allocation.TenantID = "../tenant" },
		func(r *sandbox.CreateRequest) { r.Allocation.Epoch = 0 },
		func(r *sandbox.CreateRequest) { r.Limits.TTL = 0 },
		func(r *sandbox.CreateRequest) { r.Limits.MemoryMiB = 0 },
		func(r *sandbox.CreateRequest) { r.Limits.WorkspaceBytes = 0 },
		func(r *sandbox.CreateRequest) { r.Limits.PIDs = 0 },
		func(r *sandbox.CreateRequest) { r.Limits.VCPUs = 0 },
	} {
		r := valid
		mutate(&r)
		if err := r.Validate(); !errors.Is(err, sandbox.ErrInvalid) {
			t.Fatalf("expected invalid allocation, got %v", err)
		}
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationFailsBeforeRuntimeUse(t *testing.T) {
	_, err := sandbox.New(context.Background(), sandbox.Config{})
	if !errors.Is(err, sandbox.ErrInvalid) {
		t.Fatalf("expected invalid configuration, got %v", err)
	}
}
