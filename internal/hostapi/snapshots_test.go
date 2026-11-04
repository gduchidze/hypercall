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
	"github.com/gduchidze/hypercall/internal/snapshot"
)

// snapshotDriver models the external runtime contract with real immutable
// snapshot storage and synthetic artifact bytes. It does not emulate VM restore.
type snapshotDriver struct {
	runtimeDriver
	t                  *testing.T
	store              *snapshot.Store
	opMu               sync.Mutex
	controlled         *guestv1.ControlledState
	expiry             time.Time
	stopUnavailable    bool
	loseRestoreReply   bool
	loseCaptureReply   bool
	captureUnavailable bool
	requests           map[string]string
}

func (d *snapshotDriver) Create(ctx context.Context, r sandbox.CreateRequest) (sandbox.Observation, error) {
	o, e := d.runtimeDriver.Create(ctx, r)
	if e != nil {
		return o, e
	}
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.expiry, e = r.Deadline(time.Now())
	o.ExpiresAt = d.expiry
	d.controlled = nil
	return o, e
}
func (d *snapshotDriver) Inspect(ctx context.Context, a sandbox.Allocation) (sandbox.Observation, error) {
	o, e := d.runtimeDriver.Inspect(ctx, a)
	d.opMu.Lock()
	defer d.opMu.Unlock()
	o.ExpiresAt = d.expiry
	return o, e
}
func (d *snapshotDriver) Stop(ctx context.Context, a sandbox.Allocation) error {
	d.opMu.Lock()
	lost := d.stopUnavailable
	d.opMu.Unlock()
	if lost {
		return sandbox.ErrUnavailable
	}
	return d.runtimeDriver.Stop(ctx, a)
}
func (d *snapshotDriver) StartControlled(_ context.Context, a sandbox.Allocation, counter int64) (*guestv1.ControlledState, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if d.controlled != nil {
		return nil, sandbox.ErrConflict
	}
	d.controlled = &guestv1.ControlledState{Counter: counter, AtBarrier: true, Identity: &guestv1.AllocationIdentity{TenantId: a.TenantID, ProjectId: a.ProjectID, SandboxId: a.SandboxID, ExecutionId: a.ExecutionID, Epoch: a.Epoch}}
	return d.controlled, nil
}
func (d *snapshotDriver) InspectControlled(context.Context, sandbox.Allocation) (*guestv1.ControlledState, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if d.controlled == nil {
		return nil, sandbox.ErrNotFound
	}
	return d.controlled, nil
}
func (d *snapshotDriver) ContinueControlled(ctx context.Context, a sandbox.Allocation, _ int64) (*guestv1.ControlledState, error) {
	return d.InspectControlled(ctx, a)
}
func (d *snapshotDriver) Capture(ctx context.Context, r sandbox.CaptureRequest) (snapshot.Manifest, error) {
	d.opMu.Lock()
	unavailable, lost := d.captureUnavailable, d.loseCaptureReply
	d.loseCaptureReply = false
	d.opMu.Unlock()
	if unavailable {
		return snapshot.Manifest{}, sandbox.ErrUnavailable
	}
	dir := d.t.TempDir()
	sources := []snapshot.Source{}
	for _, kind := range []string{snapshot.Disk, snapshot.Memory, snapshot.VMState} {
		p := filepath.Join(dir, kind)
		if e := os.WriteFile(p, []byte("synthetic-"+kind), 0600); e != nil {
			return snapshot.Manifest{}, e
		}
		sources = append(sources, snapshot.Source{Kind: kind, Path: p})
	}
	h := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	g := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	m, e := d.store.Publish(ctx, snapshot.PublishRequest{Scope: snapshot.Scope{TenantID: r.Allocation.TenantID, ProjectID: r.Allocation.ProjectID, CheckpointID: r.CheckpointID}, ParentCheckpointID: r.ParentCheckpointID, Compatibility: snapshot.Compatibility{Architecture: "x86_64", CPUIdentity: "synthetic", CPUConfigSHA256: h, HostKernelRelease: "synthetic", FirecrackerVersion: "1.14.0", SnapshotFormatVersion: "8.0.0", TemplateSHA256: h, RootFSSHA256: h, KernelSHA256: h, GuestSHA256: g, DeviceLayoutVersion: "synthetic", BarrierProtocolVersion: "controlled-python-v2-host-clock", BarrierConfigSHA256: h, RNGPolicy: "synthetic", VCPUs: 1, MemoryMiB: 256}, Sources: sources})
	if e == nil && lost {
		return snapshot.Manifest{}, sandbox.ErrUnavailable
	}
	return m, e
}
func (d *snapshotDriver) Restore(ctx context.Context, r sandbox.RestoreRequest) (sandbox.Observation, error) {
	data, _ := json.Marshal(r)
	d.opMu.Lock()
	if old, ok := d.requests[r.OperationID]; ok {
		if old != string(data) {
			d.opMu.Unlock()
			return sandbox.Observation{}, sandbox.ErrConflict
		}
		d.opMu.Unlock()
		return d.Inspect(ctx, r.Allocation)
	}
	d.requests[r.OperationID] = string(data)
	lost := d.loseRestoreReply
	d.loseRestoreReply = false
	counter := int64(7)
	d.opMu.Unlock()
	o, e := d.Create(ctx, sandbox.CreateRequest{Allocation: r.Allocation, Limits: r.Limits, AbsoluteExpiresAt: r.AbsoluteExpiresAt})
	if e != nil {
		return o, e
	}
	if _, e = d.StartControlled(ctx, r.Allocation, counter); e != nil {
		return o, e
	}
	if lost {
		return o, sandbox.ErrUnavailable
	}
	return o, nil
}
func (d *snapshotDriver) Fork(context.Context, sandbox.ForkRequest) ([]sandbox.ChildResult, error) {
	return nil, sandbox.ErrUnavailable
}
func controlledSetup(t *testing.T) (*hostapi.Bridge, *snapshotDriver, controlplane.Assignment, hostapi.Config) {
	t.Helper()
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	objects, e := snapshot.NewFileObjects(filepath.Join(dir, "objects"))
	if e != nil {
		t.Fatal(e)
	}
	store, e := snapshot.New(objects, snapshot.Options{})
	if e != nil {
		t.Fatal(e)
	}
	a := assignment()
	a.SandboxID = "parent-vm"
	var task controlplane.TaskSpec
	if e = json.Unmarshal(a.Run.Task.Spec, &task); e != nil {
		t.Fatal(e)
	}
	task.ExecutionMode = "controlled_python_v1"
	task.ControlledCounter = 7
	task.Command = nil
	task.AgentDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	a.Run.Task.Spec, _ = json.Marshal(task)
	d := &snapshotDriver{runtimeDriver: runtimeDriver{files: map[string][]byte{}, executed: make(chan *guestv1.ExecRequest, 2)}, t: t, store: store, requests: map[string]string{}}
	cfg := hostapi.Config{Scope: a.Scope, StateDir: filepath.Join(dir, "ledger"), TemplateDigest: task.TemplateDigest, ControlledSnapshots: true, ControlledAgentDigest: task.AgentDigest, SnapshotStore: store}
	b, e := hostapi.NewBridge(cfg, d)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { b.Close() })
	return b, d, a, cfg
}
func TestControlledSuspendPinsReadyManifestBeforeConfirmingParentTermination(t *testing.T) {
	b, d, a, _ := controlledSetup(t)
	if o, e := b.Ensure(t.Context(), a); e != nil || o.State != "running" {
		t.Fatalf("controlled start: %v %v", o, e)
	}
	d.opMu.Lock()
	d.stopUnavailable = true
	d.opMu.Unlock()
	if o, e := b.Suspend(t.Context(), a); controlplane.Code(e) != "unavailable" || o.State == "suspended" {
		t.Fatalf("unconfirmed parent stop: %v %v", o, e)
	}
	d.opMu.Lock()
	d.stopUnavailable = false
	d.opMu.Unlock()
	o, e := b.Suspend(t.Context(), a)
	if e != nil || o.State != "suspended" || o.Checkpoint == nil || !o.Checkpoint.Verified {
		t.Fatalf("suspend retry: %v %v", o, e)
	}
	cp := o.Checkpoint
	scope := snapshot.Scope{TenantID: a.Scope.TenantID, ProjectID: a.Scope.ProjectID, CheckpointID: cp.NativeCheckpointID}
	if e = d.store.DeleteUnreferenced(t.Context(), scope); !errors.Is(e, snapshot.ErrReferenced) {
		t.Fatalf("checkpoint not permanently retained: %v", e)
	}
	if v, e := b.Observe(t.Context(), a); e != nil || v.State != "suspended" {
		t.Fatalf("observation: %v %v", v, e)
	}
}
func TestControlledResumeRetriesFixedIdentityRequestAndAbsoluteLeaseAfterRestart(t *testing.T) {
	b, d, a, cfg := controlledSetup(t)
	if _, e := b.Ensure(t.Context(), a); e != nil {
		t.Fatal(e)
	}
	suspended, e := b.Suspend(t.Context(), a)
	if e != nil {
		t.Fatal(e)
	}
	cp := *suspended.Checkpoint
	if e = b.Close(); e != nil {
		t.Fatal(e)
	}
	b, e = hostapi.NewBridge(cfg, d)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	child := a
	child.SandboxID = "child-vm"
	child.ExecutionID = "child-execution"
	child.Epoch = 2
	child.LeaseUntil = time.Now().Add(30 * time.Second)
	d.opMu.Lock()
	d.loseRestoreReply = true
	d.opMu.Unlock()
	if _, e = b.Resume(t.Context(), child, cp); controlplane.Code(e) != "unavailable" {
		t.Fatalf("lost native restore reply: %v", e)
	}
	changed := child
	changed.LeaseUntil = child.LeaseUntil.Add(time.Second)
	if _, e = b.Resume(t.Context(), changed, cp); controlplane.Code(e) != "aborted" {
		t.Fatalf("lease mutated on retry: %v", e)
	}
	if e = b.Close(); e != nil {
		t.Fatal(e)
	}
	b, e = hostapi.NewBridge(cfg, d)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	if o, e := b.Resume(t.Context(), child, cp); e != nil || o.State != "running" {
		t.Fatalf("resume after restart: %v %v", o, e)
	}
	observed, e := d.Inspect(t.Context(), sandbox.Allocation{})
	if e != nil || observed.ExpiresAt.After(child.LeaseUntil) {
		t.Fatalf("native expiry exceeded controller lease: %v %v", observed, e)
	}
	if _, e = b.Stop(t.Context(), child); e != nil {
		t.Fatal(e)
	}
	if _, e = b.Resume(t.Context(), child, cp); controlplane.Code(e) != "aborted" {
		t.Fatalf("canceled resume resurrected: %v", e)
	}
}
func TestSnapshotProfileRejectsForegroundTasksAndTamperedRetainedManifest(t *testing.T) {
	b, _, a, _ := controlledSetup(t)
	generic := assignment()
	if b.SupportsRun("suspend", generic.Run) {
		t.Fatal("snapshot config enabled arbitrary foreground tasks")
	}
	if _, e := b.Ensure(t.Context(), a); e != nil {
		t.Fatal(e)
	}
	o, e := b.Suspend(t.Context(), a)
	if e != nil {
		t.Fatal(e)
	}
	child := a
	child.SandboxID = "child-vm"
	child.ExecutionID = "child-execution"
	child.Epoch++
	cp := *o.Checkpoint
	cp.SnapshotManifest = append([]byte(nil), cp.SnapshotManifest...)
	cp.SnapshotManifest[0] = '['
	if _, e = b.Resume(t.Context(), child, cp); controlplane.Code(e) != "failed_precondition" {
		t.Fatalf("tampered manifest accepted: %v", e)
	}
}

func TestControlledResumeCanceledBeforeAllocationCannotBeRevived(t *testing.T) {
	b, _, a, _ := controlledSetup(t)
	if _, e := b.Ensure(t.Context(), a); e != nil {
		t.Fatal(e)
	}
	o, e := b.Suspend(t.Context(), a)
	if e != nil {
		t.Fatal(e)
	}
	child := a
	child.SandboxID = "never-created-child"
	child.ExecutionID = "new-execution"
	child.Epoch++
	if _, e = b.Stop(t.Context(), child); e != nil {
		t.Fatal(e)
	}
	if _, e = b.Resume(t.Context(), child, *o.Checkpoint); controlplane.Code(e) != "aborted" {
		t.Fatalf("delayed restore resurrected canceled child: %v", e)
	}
}

func TestLostCaptureAcknowledgementRecoversReadyManifestWithoutRecapture(t *testing.T) {
	b, d, a, cfg := controlledSetup(t)
	if _, e := b.Ensure(t.Context(), a); e != nil {
		t.Fatal(e)
	}
	d.opMu.Lock()
	d.loseCaptureReply = true
	d.opMu.Unlock()
	if _, e := b.Suspend(t.Context(), a); controlplane.Code(e) != "unavailable" {
		t.Fatalf("lost capture reply: %v", e)
	}
	if e := b.Close(); e != nil {
		t.Fatal(e)
	}
	b, e := hostapi.NewBridge(cfg, d)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	d.opMu.Lock()
	d.captureUnavailable = true
	d.stopUnavailable = true
	d.opMu.Unlock()
	if o, e := b.Suspend(t.Context(), a); controlplane.Code(e) != "unavailable" || o.State == "suspended" {
		t.Fatalf("recovery skipped termination confirmation: %v %v", o, e)
	}
	d.opMu.Lock()
	d.stopUnavailable = false
	d.opMu.Unlock()
	if o, e := b.Suspend(t.Context(), a); e != nil || o.State != "suspended" || o.Checkpoint == nil {
		t.Fatalf("ready capture acknowledgement did not recover independently of runtime Capture: %v %v", o, e)
	}
}
func TestAmbiguousMissingCaptureIsNotReplayed(t *testing.T) {
	b, d, a, _ := controlledSetup(t)
	if _, e := b.Ensure(t.Context(), a); e != nil {
		t.Fatal(e)
	}
	d.opMu.Lock()
	d.captureUnavailable = true
	d.opMu.Unlock()
	if _, e := b.Suspend(t.Context(), a); controlplane.Code(e) != "unavailable" {
		t.Fatalf("capture failure: %v", e)
	}
	d.opMu.Lock()
	d.captureUnavailable = false
	d.opMu.Unlock()
	if o, e := b.Suspend(t.Context(), a); controlplane.Code(e) != "failed_precondition" || o.State == "suspended" {
		t.Fatalf("ambiguous capture was replayed: %v %v", o, e)
	}
	if _, e := b.Stop(t.Context(), a); e != nil {
		t.Fatal(e)
	}
}
