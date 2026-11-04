package controlplane_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	cp "github.com/gduchidze/hypercall/internal/controlplane"
)

type resumableBackend struct {
	snapshotBackend
	mu          sync.Mutex
	resumed     []cp.Assignment
	checkpoints []cp.CheckpointSpec
}

func (b *resumableBackend) Ensure(context.Context, cp.Assignment) (cp.Observation, error) {
	return cp.Observation{State: "running"}, nil
}
func (b *resumableBackend) Resume(_ context.Context, a cp.Assignment, checkpoint cp.CheckpointSpec) (cp.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resumed = append(b.resumed, a)
	b.checkpoints = append(b.checkpoints, checkpoint)
	return cp.Observation{State: "running"}, nil
}
func statusOf(t *testing.T, svc *cp.Service, scope cp.Scope, branch string) cp.BranchStatus {
	t.Helper()
	r, e := svc.GetResource(context.Background(), scope, "Branch", branch)
	if e != nil {
		t.Fatal(e)
	}
	var status cp.BranchStatus
	if e = json.Unmarshal(r.Status, &status); e != nil {
		t.Fatal(e)
	}
	return status
}
func tickController(t *testing.T, ctx context.Context, controller *cp.Controller) {
	t.Helper()
	for i := 0; i < 8; i++ {
		e := controller.Tick(ctx)
		if cp.Code(e) == "aborted" {
			continue
		}
		if e != nil {
			t.Fatal(e)
		}
		return
	}
	t.Fatal("controller did not reach current work after superseded jobs")
}
func suspendedBranch(t *testing.T) (*cp.Store, *cp.Service, cp.Scope, cp.Submission, *resumableBackend) {
	t.Helper()
	st, _, scope := fixture(t)
	backend := &resumableBackend{}
	svc := cp.NewService(st, backend)
	task := definitions(t, svc, scope)
	ctx := context.Background()
	sub, e := svc.Submit(ctx, scope, task.Metadata.ID, "submit")
	if e != nil {
		t.Fatal(e)
	}
	controller := cp.Controller{Store: st, Backend: backend, WorkerID: "worker"}
	tickController(t, ctx, &controller)
	if _, e = svc.Action(ctx, scope, sub.Branch.Metadata.ID, "suspend", "suspend"); e != nil {
		t.Fatal(e)
	}
	tickController(t, ctx, &controller)
	return st, svc, scope, sub, backend
}
func TestResumeUsesRetainedCheckpointAndFreshBoundedAssignment(t *testing.T) {
	st, svc, scope, sub, backend := suspendedBranch(t)
	ctx := context.Background()
	before := statusOf(t, svc, scope, sub.Branch.Metadata.ID)
	if before.CheckpointID == "" || before.BudgetRemainingMS <= 0 || before.BudgetRemainingMS > 60000 {
		t.Fatalf("missing suspended checkpoint/budget: %+v", before)
	}
	time.Sleep(30 * time.Millisecond)
	op, e := svc.Action(ctx, scope, sub.Branch.Metadata.ID, "resume", "resume")
	if e != nil {
		t.Fatal(e)
	}
	var spec cp.OperationSpec
	json.Unmarshal(op.Spec, &spec)
	admitted := statusOf(t, svc, scope, sub.Branch.Metadata.ID)
	if spec.CheckpointID != before.CheckpointID || admitted.ExecutionID == before.ExecutionID || admitted.Epoch != before.Epoch+1 || admitted.Observed != "resuming" || admitted.BudgetRemainingMS != before.BudgetRemainingMS {
		t.Fatalf("unsafe resume: operation=%+v before=%+v after=%+v", spec, before, admitted)
	}
	if admitted.LeaseUntil.Sub(time.Now()) > time.Duration(before.BudgetRemainingMS)*time.Millisecond {
		t.Fatal("resume reset or extended remaining budget")
	}
	controller := cp.Controller{Store: st, Backend: backend, WorkerID: "resume-worker"}
	tickController(t, ctx, &controller)
	after := statusOf(t, svc, scope, sub.Branch.Metadata.ID)
	if after.Observed != "running" || len(backend.resumed) != 1 || backend.checkpoints[0].BranchID != sub.Branch.Metadata.ID {
		t.Fatalf("resume did not use backend checkpoint: %+v", after)
	}
	if _, e = st.AppendEvent(ctx, scope, sub.Branch.Metadata.ID, before.ExecutionID, before.Epoch, "stale", json.RawMessage(`{}`)); cp.Code(e) != "aborted" {
		t.Fatalf("old execution retained authority: %v", e)
	}
}

func TestConcurrentResumeRequestsRetainOneSelectedCheckpointAndAssignment(t *testing.T) {
	st, svc, scope, sub, backend := suspendedBranch(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, e := svc.Action(ctx, scope, sub.Branch.Metadata.ID, "resume", "same-resume")
			if e != nil {
				errs <- e
				return
			}
			ids <- op.Metadata.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("duplicate resume created distinct operations")
		}
	}
	admitted := statusOf(t, svc, scope, sub.Branch.Metadata.ID)
	if _, e := svc.Action(ctx, scope, sub.Branch.Metadata.ID, "resume", "competing-resume"); cp.Code(e) != "failed_precondition" {
		t.Fatalf("competing resume accepted: %v", e)
	}
	controller := cp.Controller{Store: st, Backend: backend, WorkerID: "resume"}
	if e := controller.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	if len(backend.resumed) != 1 || backend.resumed[0].ExecutionID != admitted.ExecutionID {
		t.Fatal("duplicate resume allocated multiple assignments")
	}
	replay, e := svc.Action(ctx, scope, sub.Branch.Metadata.ID, "resume", "same-resume")
	if e != nil || replay.Metadata.ID != first {
		t.Fatalf("completed resume replay changed operation: %v", e)
	}
}

type lostResumeBackend struct {
	*resumableBackend
	lost          bool
	continuations map[string]bool
}

func (b *lostResumeBackend) Resume(ctx context.Context, a cp.Assignment, checkpoint cp.CheckpointSpec) (cp.Observation, error) {
	b.resumableBackend.Resume(ctx, a, checkpoint)
	if b.continuations == nil {
		b.continuations = map[string]bool{}
	}
	b.continuations[a.ExecutionID] = true
	if !b.lost {
		b.lost = true
		return cp.Observation{}, &cp.Error{Kind: "unavailable", Message: "resume succeeded but response was lost"}
	}
	return cp.Observation{State: "running"}, nil
}
func TestRestartAfterUnknownResumeOutcomeReusesCheckpointEpochAndDeadline(t *testing.T) {
	st, svc, scope, sub, original := suspendedBranch(t)
	backend := &lostResumeBackend{resumableBackend: original}
	svc = cp.NewService(st, backend)
	ctx := context.Background()
	op, e := svc.Action(ctx, scope, sub.Branch.Metadata.ID, "resume", "resume")
	if e != nil {
		t.Fatal(e)
	}
	before := cp.Controller{Store: st, Backend: backend, WorkerID: "before-restart"}
	if e = before.Tick(ctx); cp.Code(e) != "unavailable" {
		t.Fatalf("unknown resume was finalized: %v", e)
	}
	if statusOf(t, svc, scope, sub.Branch.Metadata.ID).Observed != "resuming" {
		t.Fatal("lost response manufactured running observation")
	}
	time.Sleep(1100 * time.Millisecond)
	after := cp.Controller{Store: st, Backend: backend, WorkerID: "after-restart"}
	for i := 0; i < 4; i++ {
		e = after.Tick(ctx)
		if cp.Code(e) != "aborted" {
			break
		}
	}
	if e != nil {
		t.Fatal(e)
	}
	if len(backend.resumed) != 2 || len(backend.continuations) != 1 {
		t.Fatalf("unknown retry created new continuation: calls=%d continuations=%d", len(backend.resumed), len(backend.continuations))
	}
	a, b := backend.resumed[0], backend.resumed[1]
	if a.ExecutionID != b.ExecutionID || a.Epoch != b.Epoch || !a.LeaseUntil.Equal(b.LeaseUntil) || backend.checkpoints[0].ManifestDigest != backend.checkpoints[1].ManifestDigest {
		t.Fatal("resume retry changed retained assignment/checkpoint/deadline")
	}
	finished, e := svc.GetResource(ctx, scope, "Operation", op.Metadata.ID)
	if e != nil {
		t.Fatal(e)
	}
	var status cp.OperationStatus
	json.Unmarshal(finished.Status, &status)
	if status.State != "succeeded" {
		t.Fatalf("resume operation not completed: %+v", status)
	}
}

func TestAcceptedLifecycleReplaySurvivesUnavailableBackendAfterRestart(t *testing.T) {
	st, svc, scope, sub, _ := suspendedBranch(t)
	ctx := context.Background()
	op, e := svc.Action(ctx, scope, sub.Branch.Metadata.ID, "resume", "resume")
	if e != nil {
		t.Fatal(e)
	}
	restarted := cp.NewService(st, cp.UnavailableBackend{})
	replay, e := restarted.Action(ctx, scope, sub.Branch.Metadata.ID, "resume", "resume")
	if e != nil || replay.Metadata.ID != op.Metadata.ID {
		t.Fatalf("backend capability change lost durable operation: %v", e)
	}
	if _, e = restarted.Action(ctx, scope, sub.Branch.Metadata.ID, "resume", "new-resume"); cp.Code(e) != "failed_precondition" {
		t.Fatalf("incapable backend admitted new intent: %v", e)
	}
}

type blockedResumeBackend struct {
	*resumableBackend
	entered chan struct{}
	release chan struct{}
	closed  map[string]bool
	running map[string]bool
}

func (b *blockedResumeBackend) Resume(ctx context.Context, a cp.Assignment, checkpoint cp.CheckpointSpec) (cp.Observation, error) {
	close(b.entered)
	select {
	case <-ctx.Done():
		return cp.Observation{}, ctx.Err()
	case <-b.release:
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed[a.ExecutionID] {
		return cp.Observation{State: "stopped"}, nil
	}
	b.running[a.ExecutionID] = true
	return cp.Observation{State: "running"}, nil
}
func (b *blockedResumeBackend) Stop(_ context.Context, a cp.Assignment) (cp.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed[a.ExecutionID] = true
	b.running[a.ExecutionID] = false
	return cp.Observation{State: "stopped"}, nil
}
func TestCancellationTombstonesFreshAssignmentBeforeDelayedResume(t *testing.T) {
	st, svc, scope, sub, original := suspendedBranch(t)
	backend := &blockedResumeBackend{resumableBackend: original, entered: make(chan struct{}), release: make(chan struct{}), closed: map[string]bool{}, running: map[string]bool{}}
	svc = cp.NewService(st, backend)
	ctx, cancelContext := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelContext()
	if _, e := svc.Action(ctx, scope, sub.Branch.Metadata.ID, "resume", "resume"); e != nil {
		t.Fatal(e)
	}
	assignment := statusOf(t, svc, scope, sub.Branch.Metadata.ID)
	start := cp.Controller{Store: st, Backend: backend, WorkerID: "resume"}
	done := make(chan error, 1)
	go func() {
		for {
			e := start.Tick(ctx)
			select {
			case <-backend.entered:
				done <- e
				return
			default:
			}
			if cp.Code(e) == "aborted" {
				continue
			}
			done <- e
			return
		}
	}()
	select {
	case <-backend.entered:
	case e := <-done:
		t.Fatalf("resume did not reach backend: %v", e)
	case <-ctx.Done():
		t.Fatal("resume did not enter backend before deadline")
	}
	if _, e := st.AppendEvent(ctx, scope, sub.Branch.Metadata.ID, assignment.ExecutionID, assignment.Epoch, "new-session", json.RawMessage(`{}`)); e != nil {
		t.Fatalf("fresh authenticated epoch denied before resume response: %v", e)
	}
	if _, e := svc.Action(ctx, scope, sub.Branch.Metadata.ID, "cancel", "cancel"); e != nil {
		t.Fatal(e)
	}
	cancel := cp.Controller{Store: st, Backend: backend, WorkerID: "cancel"}
	if e := cancel.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	close(backend.release)
	select {
	case e := <-done:
		if cp.Code(e) != "aborted" && cp.Code(e) != "not_found" {
			t.Fatalf("late resume overwrote cancellation: %v", e)
		}
	case <-ctx.Done():
		t.Fatal("late resume did not complete before deadline")
	}
	status := statusOf(t, svc, scope, sub.Branch.Metadata.ID)
	backend.mu.Lock()
	running := backend.running[assignment.ExecutionID]
	tombstoned := backend.closed[assignment.ExecutionID]
	backend.mu.Unlock()
	if status.Observed != "cancelled" || running || !tombstoned {
		t.Fatalf("restore survived cancellation: %+v running=%v tombstone=%v", status, running, tombstoned)
	}
	if _, e := st.AppendEvent(ctx, scope, sub.Branch.Metadata.ID, assignment.ExecutionID, assignment.Epoch, "late", json.RawMessage(`{}`)); cp.Code(e) != "aborted" {
		t.Fatalf("cancelled fresh epoch kept authority: %v", e)
	}
}

func TestResumeRejectsScopeMissingAndCorruptCheckpoint(t *testing.T) {
	for _, fault := range []string{"other-tenant", "other-project", "missing", "corrupt"} {
		t.Run(fault, func(t *testing.T) {
			_, svc, scope, sub, _ := suspendedBranch(t)
			ctx := context.Background()
			switch fault {
			case "other-tenant":
				scope.TenantID = "other"
			case "other-project":
				scope.ProjectID = "other"
			case "missing", "corrupt":
				db, e := sql.Open("pgx", testDSN(t))
				if e != nil {
					t.Fatal(e)
				}
				defer db.Close()
				query := `UPDATE branches SET checkpoint_id=NULL WHERE tenant_id=$1 AND project_id=$2 AND id=$3`
				if fault == "corrupt" {
					query = `UPDATE resources SET body=jsonb_set(body,'{spec,manifest_digest}','"sha256:corrupt"') WHERE tenant_id=$1 AND project_id=$2 AND id=(SELECT checkpoint_id FROM branches WHERE tenant_id=$1 AND project_id=$2 AND id=$3)`
				}
				if _, e = db.ExecContext(ctx, query, scope.TenantID, scope.ProjectID, sub.Branch.Metadata.ID); e != nil {
					t.Fatal(e)
				}
			}
			_, e := svc.Action(ctx, scope, sub.Branch.Metadata.ID, "resume", "resume")
			expected := "failed_precondition"
			if fault == "other-tenant" || fault == "other-project" {
				expected = "not_found"
			}
			if cp.Code(e) != expected {
				t.Fatalf("%s checkpoint accepted: %v", fault, e)
			}
		})
	}
}

func TestSuspendedTimeDoesNotResetActiveBudgetAndHungSuspendStillExpires(t *testing.T) {
	t.Run("pause-and-resume", func(t *testing.T) {
		st, _, scope := fixture(t)
		backend := &resumableBackend{}
		svc := cp.NewService(st, backend)
		ctx := context.Background()
		task := shortTask(t, svc, scope)
		sub, e := svc.Submit(ctx, scope, task.Metadata.ID, "submit")
		if e != nil {
			t.Fatal(e)
		}
		controller := cp.Controller{Store: st, Backend: backend, WorkerID: "worker"}
		if e = controller.Tick(ctx); e != nil {
			t.Fatal(e)
		}
		time.Sleep(100 * time.Millisecond)
		if _, e = svc.Action(ctx, scope, sub.Branch.Metadata.ID, "suspend", "suspend"); e != nil {
			t.Fatal(e)
		}
		if e = controller.Tick(ctx); e != nil {
			t.Fatal(e)
		}
		paused := statusOf(t, svc, scope, sub.Branch.Metadata.ID)
		if paused.BudgetRemainingMS <= 0 || paused.BudgetRemainingMS >= 950 {
			t.Fatalf("active time was not deducted: %+v", paused)
		}
		time.Sleep(1100 * time.Millisecond)
		if e = st.ExpireExecutions(ctx); e != nil {
			t.Fatal(e)
		}
		if statusOf(t, svc, scope, sub.Branch.Metadata.ID).Observed != "suspended" {
			t.Fatal("suspended checkpoint expired with predecessor lease")
		}
		if _, e = svc.Action(ctx, scope, sub.Branch.Metadata.ID, "resume", "resume"); e != nil {
			t.Fatal(e)
		}
		admitted := statusOf(t, svc, scope, sub.Branch.Metadata.ID)
		if admitted.BudgetRemainingMS != paused.BudgetRemainingMS || admitted.LeaseUntil.Sub(time.Now()) > time.Duration(paused.BudgetRemainingMS)*time.Millisecond {
			t.Fatal("resume increased frozen budget")
		}
		for i := 0; i < 4; i++ {
			e = controller.Tick(ctx)
			if cp.Code(e) != "aborted" {
				break
			}
		}
		if e != nil {
			t.Fatal(e)
		}
		time.Sleep(time.Duration(paused.BudgetRemainingMS+50) * time.Millisecond)
		if e = st.ExpireExecutions(ctx); e != nil {
			t.Fatal(e)
		}
		expired := statusOf(t, svc, scope, sub.Branch.Metadata.ID)
		if expired.Observed != "interrupted" || expired.Desired != "stopped" {
			t.Fatalf("resumed execution exceeded active budget: %+v", expired)
		}
	})
	t.Run("hung-suspend", func(t *testing.T) {
		st, _, scope := fixture(t)
		backend := &resumableBackend{}
		svc := cp.NewService(st, backend)
		ctx := context.Background()
		task := shortTask(t, svc, scope)
		sub, e := svc.Submit(ctx, scope, task.Metadata.ID, "submit")
		if e != nil {
			t.Fatal(e)
		}
		controller := cp.Controller{Store: st, Backend: backend, WorkerID: "start"}
		if e = controller.Tick(ctx); e != nil {
			t.Fatal(e)
		}
		if _, e = svc.Action(ctx, scope, sub.Branch.Metadata.ID, "suspend", "suspend"); e != nil {
			t.Fatal(e)
		}
		hung := statusOf(t, svc, scope, sub.Branch.Metadata.ID)
		if hung.Observed != "suspending" {
			t.Fatalf("suspend admission not visible: %+v", hung)
		}
		time.Sleep(1100 * time.Millisecond)
		if e = st.ExpireExecutions(ctx); e != nil {
			t.Fatal(e)
		}
		expired := statusOf(t, svc, scope, sub.Branch.Metadata.ID)
		if expired.Observed != "interrupted" || expired.Desired != "stopped" {
			t.Fatalf("hung suspension escaped lifetime fence: %+v", expired)
		}
	})
}

func TestSuspensionRejectsContradictoryBackendCheckpointIdentityAndCursor(t *testing.T) {
	for _, fault := range []string{"branch", "cursor"} {
		t.Run(fault, func(t *testing.T) {
			st, _, scope := fixture(t)
			backend := &resumableBackend{}
			svc := cp.NewService(st, backend)
			ctx := context.Background()
			task := definitions(t, svc, scope)
			sub, e := svc.Submit(ctx, scope, task.Metadata.ID, "submit")
			if e != nil {
				t.Fatal(e)
			}
			controller := cp.Controller{Store: st, Backend: backend, WorkerID: "start"}
			tickController(t, ctx, &controller)
			if _, e = svc.Action(ctx, scope, sub.Branch.Metadata.ID, "suspend", "suspend"); e != nil {
				t.Fatal(e)
			}
			job, e := st.Claim(ctx, "snapshot", time.Second)
			if e != nil {
				t.Fatal(e)
			}
			obs, e := backend.Suspend(ctx, job.Assignment)
			if e != nil {
				t.Fatal(e)
			}
			if fault == "branch" {
				obs.Checkpoint.BranchID = "another-branch"
			} else {
				obs.Checkpoint.EventPosition = 999
			}
			if e = st.Finish(ctx, job, obs); cp.Code(e) != "failed_precondition" {
				t.Fatalf("contradictory %s metadata was relabeled as valid: %v", fault, e)
			}
			all, e := svc.ListResources(ctx, scope, "Checkpoint", "", 100)
			if e != nil || len(all) != 0 {
				t.Fatalf("invalid checkpoint published: count=%d error=%v", len(all), e)
			}
		})
	}
}

func TestPhysicalSandboxIdentityChangesWithoutChangingLogicalBranch(t *testing.T) {
	st, svc, scope, sub, backend := suspendedBranch(t)
	before := statusOf(t, svc, scope, sub.Branch.Metadata.ID)
	if before.SandboxID == "" || before.SandboxID == sub.Branch.Metadata.ID || before.SandboxID == before.ExecutionID {
		t.Fatalf("cold execution lacks independent physical identity: %+v", before)
	}
	if _, e := svc.Action(context.Background(), scope, sub.Branch.Metadata.ID, "resume", "resume"); e != nil {
		t.Fatal(e)
	}
	after := statusOf(t, svc, scope, sub.Branch.Metadata.ID)
	if after.SandboxID == "" || after.SandboxID == before.SandboxID || after.SandboxID == after.ExecutionID || after.SandboxID == sub.Branch.Metadata.ID {
		t.Fatalf("resume reused physical sandbox identity: %+v", after)
	}
	controller := cp.Controller{Store: st, Backend: backend, WorkerID: "restore"}
	tickController(t, context.Background(), &controller)
	if backend.resumed[0].SandboxID != after.SandboxID {
		t.Fatal("backend did not receive durable physical identity")
	}
}

type profileBackend struct {
	*resumableBackend
	enabled bool
}

func (b *profileBackend) SupportsRun(action string, run cp.RunSpec) bool {
	var task cp.TaskSpec
	_ = json.Unmarshal(run.Task.Spec, &task)
	return b.enabled && task.ExecutionMode == "controlled_python_v1"
}
func TestSnapshotCapabilityIsScopedToResolvedExecutionProfileBeforeIntent(t *testing.T) {
	st, _, scope := fixture(t)
	backend := &profileBackend{resumableBackend: &resumableBackend{}, enabled: true}
	svc := cp.NewService(st, backend)
	ctx := context.Background()
	foreground := definitions(t, svc, scope)
	submit := func(task cp.Resource, key string) cp.Submission {
		sub, e := svc.Submit(ctx, scope, task.Metadata.ID, key)
		if e != nil {
			t.Fatal(e)
		}
		controller := cp.Controller{Store: st, Backend: backend, WorkerID: key}
		tickController(t, ctx, &controller)
		return sub
	}
	generic := submit(foreground, "generic")
	before := statusOf(t, svc, scope, generic.Branch.Metadata.ID)
	if _, e := svc.Action(ctx, scope, generic.Branch.Metadata.ID, "suspend", "generic-suspend"); cp.Code(e) != "failed_precondition" {
		t.Fatalf("global snapshot capability admitted generic task: %v", e)
	}
	after := statusOf(t, svc, scope, generic.Branch.Metadata.ID)
	if after.Desired != before.Desired || after.Observed != before.Observed || after.LastEvent != before.LastEvent {
		t.Fatal("unsupported profile changed durable intent")
	}
	var spec cp.TaskSpec
	json.Unmarshal(foreground.Spec, &spec)
	spec.ExecutionMode = "controlled_python_v1"
	spec.Command = nil
	spec.Env = nil
	raw, _ := json.Marshal(spec)
	controlled, e := svc.PutResource(ctx, scope, cp.Resource{Kind: "Task", Metadata: cp.Metadata{Name: "controlled"}, Spec: raw}, "controlled")
	if e != nil {
		t.Fatal(e)
	}
	sub := submit(controlled, "controlled-submit")
	op, e := svc.Action(ctx, scope, sub.Branch.Metadata.ID, "suspend", "controlled-suspend")
	if e != nil {
		t.Fatal(e)
	}
	backend.enabled = false
	replay, e := svc.Action(ctx, scope, sub.Branch.Metadata.ID, "suspend", "controlled-suspend")
	if e != nil || replay.Metadata.ID != op.Metadata.ID {
		t.Fatalf("capability change lost accepted intent: %v", e)
	}
	if _, e = svc.Action(ctx, cp.Scope{TenantID: "other", ProjectID: scope.ProjectID}, sub.Branch.Metadata.ID, "suspend", "other-scope"); cp.Code(e) != "not_found" {
		t.Fatalf("capability evaluation bypassed resource scope: %v", e)
	}
}

type nativeSnapshotBackend struct {
	snapshotBackend
	invalid bool
}

func (b nativeSnapshotBackend) Suspend(ctx context.Context, a cp.Assignment) (cp.Observation, error) {
	obs, e := b.snapshotBackend.Suspend(ctx, a)
	if e != nil {
		return obs, e
	}
	manifest := []byte(`{"schema_version":1,"snapshot":"native-conformance"}`)
	sum := sha256.Sum256(manifest)
	obs.Checkpoint.NativeCheckpointID = "native-checkpoint"
	obs.Checkpoint.SnapshotManifest = manifest
	obs.Checkpoint.ManifestDigest = "sha256:" + hex.EncodeToString(sum[:])
	if b.invalid {
		obs.Checkpoint.SnapshotManifest = []byte(`{"schema_version":1,"snapshot":"changed"}`)
	}
	return obs, nil
}
func TestNativeCheckpointManifestBytesAreBoundToRetainedDigest(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			st, _, scope := fixture(t)
			backend := nativeSnapshotBackend{invalid: invalid}
			svc := cp.NewService(st, backend)
			ctx := context.Background()
			task := definitions(t, svc, scope)
			sub, e := svc.Submit(ctx, scope, task.Metadata.ID, "submit")
			if e != nil {
				t.Fatal(e)
			}
			job, e := st.Claim(ctx, "start", time.Second)
			if e != nil {
				t.Fatal(e)
			}
			if e = st.Finish(ctx, job, cp.Observation{State: "running"}); e != nil {
				t.Fatal(e)
			}
			if _, e = svc.Action(ctx, scope, sub.Branch.Metadata.ID, "suspend", "suspend"); e != nil {
				t.Fatal(e)
			}
			job, e = st.Claim(ctx, "snapshot", time.Second)
			if e != nil {
				t.Fatal(e)
			}
			obs, e := backend.Suspend(ctx, job.Assignment)
			if e != nil {
				t.Fatal(e)
			}
			e = st.Finish(ctx, job, obs)
			if invalid {
				if cp.Code(e) != "failed_precondition" {
					t.Fatalf("changed native manifest accepted: %v", e)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			all, e := svc.ListResources(ctx, scope, "Checkpoint", "", 100)
			if e != nil || len(all) != 1 {
				t.Fatalf("native checkpoint absent: %v", e)
			}
			var retained cp.CheckpointSpec
			json.Unmarshal(all[0].Spec, &retained)
			if retained.NativeCheckpointID != "native-checkpoint" || !bytes.Equal(retained.SnapshotManifest, obs.Checkpoint.SnapshotManifest) {
				t.Fatal("native identity or manifest bytes changed in storage")
			}
		})
	}
}
