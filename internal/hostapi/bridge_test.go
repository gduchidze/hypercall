package hostapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"github.com/gduchidze/hypercall/internal/controlplane"
	"github.com/gduchidze/hypercall/internal/hostapi"
	"github.com/gduchidze/hypercall/internal/sandbox"
)

type runtimeDriver struct {
	mu         sync.Mutex
	state      sandbox.State
	allocation sandbox.Allocation
	files      map[string][]byte
	executed   chan *guestv1.ExecRequest
}

func (d *runtimeDriver) Create(_ context.Context, r sandbox.CreateRequest) (sandbox.Observation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state = sandbox.StateRunning
	d.allocation = r.Allocation
	return sandbox.Observation{State: d.state, ExpiresAt: time.Now().Add(r.Limits.TTL)}, nil
}
func (d *runtimeDriver) Inspect(context.Context, sandbox.Allocation) (sandbox.Observation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state == "" {
		return sandbox.Observation{}, sandbox.ErrNotFound
	}
	return sandbox.Observation{State: d.state, ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (d *runtimeDriver) Stop(context.Context, sandbox.Allocation) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state == "" {
		return sandbox.ErrNotFound
	}
	d.state = sandbox.StateStopped
	return nil
}
func (d *runtimeDriver) Exec(ctx context.Context, _ sandbox.Allocation, r *guestv1.ExecRequest, emit func(*guestv1.ExecEvent) error) error {
	select {
	case d.executed <- r:
	case <-ctx.Done():
		return ctx.Err()
	}
	return emit(&guestv1.ExecEvent{Event: &guestv1.ExecEvent_Result{Result: &guestv1.ExecResult{ExitCode: 0}}})
}
func (d *runtimeDriver) WriteFile(_ context.Context, _ sandbox.Allocation, r *guestv1.WriteFileRequest) (*guestv1.WriteFileResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.files[r.Path] = append([]byte(nil), r.Data...)
	return &guestv1.WriteFileResponse{BytesWritten: uint64(len(r.Data))}, nil
}
func (d *runtimeDriver) ReadFile(_ context.Context, _ sandbox.Allocation, r *guestv1.ReadFileRequest) (*guestv1.ReadFileResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return &guestv1.ReadFileResponse{Data: d.files[r.Path]}, nil
}
func (d *runtimeDriver) Recover(context.Context) ([]sandbox.Observation, error) { return nil, nil }
func (d *runtimeDriver) Close() error                                           { return nil }
func assignment() controlplane.Assignment {
	task, _ := json.Marshal(controlplane.TaskSpec{TemplateDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Command: []string{"python3", "main.py"}, Limits: controlplane.Limits{CPU: 1, MemoryMB: 256, DiskMB: 8, TimeoutSeconds: 60}})
	workspace, _ := json.Marshal(controlplane.WorkspaceSpec{PrivateWrites: true, Files: []controlplane.InputFile{{Path: "main.py", Content: []byte("print('hello')"), Digest: "sha256:96f43d529af3430cb6b0e2c02f6b38ef1a121e8a31d2d09a3ebb716f2f35c9de"}}})
	return controlplane.Assignment{Scope: controlplane.Scope{TenantID: "tenant", ProjectID: "project"}, BranchID: "branch", ExecutionID: "execution", Epoch: 1, LeaseUntil: time.Now().Add(time.Minute), Run: controlplane.RunSpec{Task: controlplane.Resource{Spec: task}, Workspace: controlplane.Resource{Spec: workspace}}}
}
func TestStoppedAbsentAssignmentCannotBeCreatedAfterBridgeRestart(t *testing.T) {
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	d := &runtimeDriver{files: map[string][]byte{}, executed: make(chan *guestv1.ExecRequest, 2)}
	a := assignment()
	cfg := hostapi.Config{Scope: a.Scope, StateDir: dir, TemplateDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	b, e := hostapi.NewBridge(cfg, d)
	if e != nil {
		t.Fatal(e)
	}
	if o, e := b.Stop(t.Context(), a); e != nil || o.State != "stopped" {
		t.Fatalf("stop absent: %v %v", o, e)
	}
	if e = b.Close(); e != nil {
		t.Fatal(e)
	}
	b, e = hostapi.NewBridge(cfg, d)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	if _, e = b.Ensure(t.Context(), a); e == nil {
		t.Fatal("closed assignment resurrected")
	}
	if _, e = d.Inspect(t.Context(), sandbox.Allocation{}); !errors.Is(e, sandbox.ErrNotFound) {
		t.Fatal("runtime allocated despite closed assignment")
	}
}

func TestResolvedWorkspaceTaskExecutesOnceAndChangedLeaseIsFenced(t *testing.T) {
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	d := &runtimeDriver{files: map[string][]byte{}, executed: make(chan *guestv1.ExecRequest, 4)}
	a := assignment()
	b, e := hostapi.NewBridge(hostapi.Config{Scope: a.Scope, StateDir: dir, TemplateDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, d)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	if o, e := b.Ensure(t.Context(), a); e != nil || o.State != "running" {
		t.Fatalf("ensure: %v %v", o, e)
	}
	select {
	case command := <-d.executed:
		if len(command.Argv) != 2 || command.Argv[0] != "python3" || command.Argv[1] != "main.py" {
			t.Fatalf("task argv: %v", command)
		}
	case <-time.After(time.Second):
		t.Fatal("task command was not executed")
	}
	if _, e = b.Ensure(t.Context(), a); e != nil {
		t.Fatal(e)
	}
	changed := a
	changed.LeaseUntil = a.LeaseUntil.Add(time.Second)
	if _, e = b.Ensure(t.Context(), changed); controlplane.Code(e) != "aborted" {
		t.Fatalf("extended assignment accepted: %v", e)
	}
	select {
	case <-d.executed:
		t.Fatal("task replayed on retry")
	case <-time.After(20 * time.Millisecond):
	}
	d.mu.Lock()
	content := string(d.files["main.py"])
	d.mu.Unlock()
	if content != "print('hello')" {
		t.Fatalf("workspace file: %q", content)
	}
}

func TestUnsupportedNestedWorkspaceIsRejectedBeforeAllocation(t *testing.T) {
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	d := &runtimeDriver{files: map[string][]byte{}, executed: make(chan *guestv1.ExecRequest, 2)}
	a := assignment()
	var w controlplane.WorkspaceSpec
	if e = json.Unmarshal(a.Run.Workspace.Spec, &w); e != nil {
		t.Fatal(e)
	}
	w.Files[0].Path = "src/main.py"
	a.Run.Workspace.Spec, _ = json.Marshal(w)
	b, e := hostapi.NewBridge(hostapi.Config{Scope: a.Scope, StateDir: dir, TemplateDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, d)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	if _, e = b.Ensure(t.Context(), a); controlplane.Code(e) != "failed_precondition" {
		t.Fatalf("unsupported workspace accepted: %v", e)
	}
	if _, e = d.Inspect(t.Context(), sandbox.Allocation{}); !errors.Is(e, sandbox.ErrNotFound) {
		t.Fatalf("runtime allocated unsupported input: %v", e)
	}
}

type partitionedStopRuntime struct {
	runtimeDriver
	stopMu      sync.Mutex
	unreachable bool
}

func (d *partitionedStopRuntime) Stop(ctx context.Context, a sandbox.Allocation) error {
	d.stopMu.Lock()
	lost := d.unreachable
	d.stopMu.Unlock()
	if lost {
		return sandbox.ErrUnavailable
	}
	return d.runtimeDriver.Stop(ctx, a)
}
func TestTaskCompletionRetainsCleanupUntilTerminationIsConfirmed(t *testing.T) {
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	a := assignment()
	d := &partitionedStopRuntime{runtimeDriver: runtimeDriver{files: map[string][]byte{}, executed: make(chan *guestv1.ExecRequest, 2)}, unreachable: true}
	b, e := hostapi.NewBridge(hostapi.Config{Scope: a.Scope, StateDir: dir, TemplateDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, d)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	if _, e = b.Ensure(t.Context(), a); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(time.Second)
	for {
		o, e := b.Observe(t.Context(), a)
		if controlplane.Code(e) == "unavailable" {
			break
		}
		if o.State == "stopped" || o.State == "failed" {
			t.Fatalf("unconfirmed cleanup became terminal: %v %v", o, e)
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup uncertainty not exposed")
		}
		time.Sleep(time.Millisecond)
	}
	d.stopMu.Lock()
	d.unreachable = false
	d.stopMu.Unlock()
	if o, e := b.Observe(t.Context(), a); e != nil || o.State != "stopped" {
		t.Fatalf("cleanup did not converge: %v %v", o, e)
	}
}

func TestUnsupportedLedgerSchemaFailsClosedAfterRestart(t *testing.T) {
	for _, change := range []string{"future_version", "unknown_field"} {
		t.Run(change, func(t *testing.T) {
			dir, e := filepath.EvalSymlinks(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			if e = os.Chmod(dir, 0700); e != nil {
				t.Fatal(e)
			}
			a := assignment()
			d := &runtimeDriver{files: map[string][]byte{}, executed: make(chan *guestv1.ExecRequest, 2)}
			cfg := hostapi.Config{Scope: a.Scope, StateDir: dir, TemplateDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
			b, e := hostapi.NewBridge(cfg, d)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = b.Stop(t.Context(), a); e != nil {
				t.Fatal(e)
			}
			if e = b.Close(); e != nil {
				t.Fatal(e)
			}
			records, e := filepath.Glob(filepath.Join(dir, "*.json"))
			if e != nil || len(records) != 1 {
				t.Fatalf("ledger fixture: %v %v", records, e)
			}
			data, e := os.ReadFile(records[0])
			if e != nil {
				t.Fatal(e)
			}
			var doc map[string]json.RawMessage
			if e = json.Unmarshal(data, &doc); e != nil {
				t.Fatal(e)
			}
			if string(doc["version"]) != "1" {
				t.Fatalf("ledger schema version was not recorded: %s", doc["version"])
			}
			if change == "future_version" {
				doc["version"] = json.RawMessage("2")
			} else {
				doc["future_capability"] = json.RawMessage("true")
			}
			data, e = json.Marshal(doc)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(records[0], data, 0600); e != nil {
				t.Fatal(e)
			}
			b, e = hostapi.NewBridge(cfg, d)
			if e != nil {
				t.Fatal(e)
			}
			defer b.Close()
			if _, e = b.Ensure(t.Context(), a); controlplane.Code(e) != "failed_precondition" {
				t.Fatalf("unsupported ledger admitted assignment: %v", e)
			}
			if _, e = b.Stop(t.Context(), a); controlplane.Code(e) != "failed_precondition" {
				t.Fatalf("unsupported ledger was overwritten: %v", e)
			}
			if _, e = d.Inspect(t.Context(), sandbox.Allocation{}); !errors.Is(e, sandbox.ErrNotFound) {
				t.Fatal("unsupported ledger caused allocation")
			}
		})
	}
}

func TestLegacyPhysicalSandboxAliasKeepsItsClosedFenceAfterUpgrade(t *testing.T) {
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	a := assignment()
	a.SandboxID = ""
	d := &runtimeDriver{files: map[string][]byte{}, executed: make(chan *guestv1.ExecRequest, 2)}
	cfg := hostapi.Config{Scope: a.Scope, StateDir: dir, TemplateDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	b, e := hostapi.NewBridge(cfg, d)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = b.Stop(t.Context(), a); e != nil {
		t.Fatal(e)
	}
	if e = b.Close(); e != nil {
		t.Fatal(e)
	}
	b, e = hostapi.NewBridge(cfg, d)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	a.SandboxID = a.BranchID
	if o, e := b.Stop(t.Context(), a); e != nil || o.State != "stopped" {
		t.Fatalf("legacy explicit alias lost its fence: %v %v", o, e)
	}
	if _, e = b.Ensure(t.Context(), a); controlplane.Code(e) != "aborted" {
		t.Fatalf("legacy assignment resurrected: %v", e)
	}
}
