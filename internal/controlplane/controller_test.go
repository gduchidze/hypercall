package controlplane_test

import (
	"context"
	"encoding/json"
	cp "github.com/gduchidze/hypercall/internal/controlplane"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCancelledIntentFencesLateWorkerAndRejectsStaleEvents(t *testing.T) {
	st, svc, scope := fixture(t)
	task := definitions(t, svc, scope)
	ctx := context.Background()
	sub, e := svc.Submit(ctx, scope, task.Metadata.ID, "submit")
	if e != nil {
		t.Fatal(e)
	}
	old, e := st.Claim(ctx, "worker-before-restart", time.Second)
	if e != nil {
		t.Fatal(e)
	}
	if old.Assignment.Epoch != 1 {
		t.Fatalf("first epoch %d", old.Assignment.Epoch)
	}
	if _, e = svc.Action(ctx, scope, sub.Branch.Metadata.ID, "cancel", "cancel"); e != nil {
		t.Fatal(e)
	}
	if e = st.Finish(ctx, old, cp.Observation{State: "running"}); cp.Code(e) != "aborted" {
		t.Fatalf("late running must be fenced: %v", e)
	}
	if _, e = st.AppendEvent(ctx, scope, sub.Branch.Metadata.ID, old.Assignment.ExecutionID, old.Assignment.Epoch, "late", json.RawMessage(`{}`)); cp.Code(e) != "aborted" {
		t.Fatalf("cancelled event accepted: %v", e)
	}
	branch, e := svc.GetResource(ctx, scope, "Branch", sub.Branch.Metadata.ID)
	if e != nil {
		t.Fatal(e)
	}
	var status cp.BranchStatus
	json.Unmarshal(branch.Status, &status)
	if status.Desired != "cancelled" || status.Observed == "running" {
		t.Fatalf("cancel overwritten: %+v", status)
	}
}

func TestExpiredJobClaimCannotRenewOrCompleteAndRecoversSameExecution(t *testing.T) {
	st, svc, scope := fixture(t)
	task := definitions(t, svc, scope)
	ctx := context.Background()
	_, e := svc.Submit(ctx, scope, task.Metadata.ID, "submit")
	if e != nil {
		t.Fatal(e)
	}
	old, e := st.Claim(ctx, "old", 20*time.Millisecond)
	if e != nil {
		t.Fatal(e)
	}
	time.Sleep(40 * time.Millisecond)
	if e = st.RenewClaim(ctx, old, time.Second); cp.Code(e) != "aborted" {
		t.Fatalf("expired lease renewed: %v", e)
	}
	fresh, e := st.Claim(ctx, "after-restart", time.Second)
	if e != nil {
		t.Fatal(e)
	}
	if fresh.ClaimEpoch <= old.ClaimEpoch || fresh.Assignment.ExecutionID != old.Assignment.ExecutionID || fresh.Assignment.Epoch != old.Assignment.Epoch {
		t.Fatalf("reclaim duplicated execution: old=%+v fresh=%+v", old, fresh)
	}
	if e = st.Finish(ctx, old, cp.Observation{State: "running"}); cp.Code(e) != "aborted" {
		t.Fatalf("stale completion accepted: %v", e)
	}
	if e = st.Finish(ctx, fresh, cp.Observation{State: "running"}); e != nil {
		t.Fatal(e)
	}
}

func TestEquivalentDefinitionsAndConflictingIdempotency(t *testing.T) {
	_, svc, scope := fixture(t)
	ctx := context.Background()
	one, e := svc.PutResource(ctx, scope, cp.Resource{Kind: "Gateway", Metadata: cp.Metadata{Name: "gateway"}, Spec: json.RawMessage(`{"routes":{"a":"https://a.invalid","b":"https://b.invalid"},"secret_refs":[]}`)}, "one")
	if e != nil {
		t.Fatal(e)
	}
	two, e := svc.PutResource(ctx, scope, cp.Resource{Kind: "Gateway", Metadata: cp.Metadata{Name: "gateway"}, Spec: json.RawMessage(`{"secret_refs":[],"routes":{"b":"https://b.invalid","a":"https://a.invalid"}}`)}, "two")
	if e != nil || one.Metadata.ID != two.Metadata.ID {
		t.Fatalf("equivalent immutable input rejected %v", e)
	}
	if _, e = svc.PutResource(ctx, scope, cp.Resource{Kind: "Gateway", Metadata: cp.Metadata{Name: "other"}, Spec: json.RawMessage(`{"routes":{"a":"https://a.invalid"}}`)}, "one"); cp.Code(e) != "already_exists" {
		t.Fatalf("idempotency conflict not rejected %v", e)
	}
}

func TestOutboxRedeliveryIsFencedAndEventsDeduplicate(t *testing.T) {
	st, svc, scope := fixture(t)
	task := definitions(t, svc, scope)
	ctx := context.Background()
	sub, e := svc.Submit(ctx, scope, task.Metadata.ID, "submit")
	if e != nil {
		t.Fatal(e)
	}
	job, e := st.Claim(ctx, "worker", time.Second)
	if e != nil {
		t.Fatal(e)
	}
	one, e := st.AppendEventID(ctx, scope, sub.Branch.Metadata.ID, job.Assignment.ExecutionID, job.Assignment.Epoch, "call-1", "tool_complete", json.RawMessage(`{"answer":42}`))
	if e != nil {
		t.Fatal(e)
	}
	two, e := st.AppendEventID(ctx, scope, sub.Branch.Metadata.ID, job.Assignment.ExecutionID, job.Assignment.Epoch, "call-1", "tool_complete", json.RawMessage(`{"answer":42}`))
	if e != nil || one.Sequence != two.Sequence {
		t.Fatalf("event duplicate: %v", e)
	}
	if _, e = st.AppendEventID(ctx, scope, sub.Branch.Metadata.ID, job.Assignment.ExecutionID, job.Assignment.Epoch, "call-1", "tool_complete", json.RawMessage(`{"answer":0}`)); cp.Code(e) != "already_exists" {
		t.Fatalf("conflicting event accepted: %v", e)
	}
	old, e := st.ClaimOutbox(ctx, 20*time.Millisecond)
	if e != nil {
		t.Fatal(e)
	}
	time.Sleep(40 * time.Millisecond)
	fresh, e := st.ClaimOutbox(ctx, time.Second)
	if e != nil {
		t.Fatal(e)
	}
	if old.Event.ID != fresh.Event.ID || old.Token == fresh.Token {
		t.Fatalf("redelivery did not fence: old=%+v new=%+v", old, fresh)
	}
	if e = st.AckOutbox(ctx, old); cp.Code(e) != "aborted" {
		t.Fatalf("expired delivery acknowledged: %v", e)
	}
	if e = st.AckOutbox(ctx, fresh); e != nil {
		t.Fatal(e)
	}
}

type uncertainBackend struct {
	cp.UnavailableBackend
	execution    string
	allocations  int
	lostResponse bool
}

func (b *uncertainBackend) Ensure(_ context.Context, a cp.Assignment) (cp.Observation, error) {
	if b.execution == "" {
		b.execution = a.ExecutionID
		b.allocations++
	}
	if b.execution != a.ExecutionID {
		return cp.Observation{}, &cp.Error{Kind: "failed_precondition", Message: "duplicate execution"}
	}
	if !b.lostResponse {
		b.lostResponse = true
		return cp.Observation{}, &cp.Error{Kind: "unavailable", Message: "response lost after allocation"}
	}
	return cp.Observation{State: "running"}, nil
}
func TestControllerRestartRecoversUnknownAllocationOutcome(t *testing.T) {
	st, svc, scope := fixture(t)
	task := definitions(t, svc, scope)
	ctx := context.Background()
	sub, e := svc.Submit(ctx, scope, task.Metadata.ID, "submit")
	if e != nil {
		t.Fatal(e)
	}
	runtime := &uncertainBackend{}
	before := cp.Controller{Store: st, Backend: runtime, WorkerID: "before"}
	if e = before.Tick(ctx); cp.Code(e) != "unavailable" {
		t.Fatalf("unknown create must stay pending: %v", e)
	}
	branch, e := svc.GetResource(ctx, scope, "Branch", sub.Branch.Metadata.ID)
	if e != nil {
		t.Fatal(e)
	}
	var status cp.BranchStatus
	json.Unmarshal(branch.Status, &status)
	if status.Observed == "running" {
		t.Fatal("lost response manufactured running state")
	}
	time.Sleep(1100 * time.Millisecond)
	after := cp.Controller{Store: st, Backend: runtime, WorkerID: "after"}
	if e = after.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	branch, e = svc.GetResource(ctx, scope, "Branch", sub.Branch.Metadata.ID)
	if e != nil {
		t.Fatal(e)
	}
	json.Unmarshal(branch.Status, &status)
	if status.Observed != "running" || runtime.allocations != 1 {
		t.Fatalf("recovery: state=%s allocations=%d", status.Observed, runtime.allocations)
	}
}

func shortTask(t *testing.T, svc *cp.Service, scope cp.Scope) cp.Resource {
	t.Helper()
	task := definitions(t, svc, scope)
	var spec cp.TaskSpec
	json.Unmarshal(task.Spec, &spec)
	spec.Limits.TimeoutSeconds = 1
	raw, _ := json.Marshal(spec)
	r, e := svc.PutResource(context.Background(), scope, cp.Resource{Kind: "Task", Metadata: cp.Metadata{Name: "short-task"}, Spec: raw}, "put-short")
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func TestConcurrentDuplicateSubmitCreatesOneRun(t *testing.T) {
	_, svc, scope := fixture(t)
	task := definitions(t, svc, scope)
	ctx := context.Background()
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, e := svc.Submit(ctx, scope, task.Metadata.ID, "same")
			if e != nil {
				errs <- e
				return
			}
			ids <- sub.Run.Metadata.ID
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
			t.Fatal("duplicate request created distinct runs")
		}
	}
	runs, e := svc.ListResources(ctx, scope, "Run", "", 100)
	if e != nil || len(runs) != 1 {
		t.Fatalf("runs=%d error=%v", len(runs), e)
	}
}

type delayedBackend struct {
	cp.UnavailableBackend
	entered         chan struct{}
	release         chan struct{}
	mu              sync.Mutex
	closed, running bool
}

func (b *delayedBackend) Ensure(ctx context.Context, a cp.Assignment) (cp.Observation, error) {
	close(b.entered)
	select {
	case <-ctx.Done():
		return cp.Observation{}, ctx.Err()
	case <-b.release:
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return cp.Observation{State: "stopped"}, nil
	}
	b.running = true
	return cp.Observation{State: "running"}, nil
}
func (b *delayedBackend) Stop(context.Context, cp.Assignment) (cp.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.running = false
	return cp.Observation{State: "stopped"}, nil
}
func TestCancellationDuringUnknownEnsureLeavesNoRunningAllocation(t *testing.T) {
	st, svc, scope := fixture(t)
	task := definitions(t, svc, scope)
	ctx := context.Background()
	sub, e := svc.Submit(ctx, scope, task.Metadata.ID, "submit")
	if e != nil {
		t.Fatal(e)
	}
	runtime := &delayedBackend{entered: make(chan struct{}), release: make(chan struct{})}
	start := &cp.Controller{Store: st, Backend: runtime, WorkerID: "start"}
	done := make(chan error, 1)
	go func() { done <- start.Tick(ctx) }()
	<-runtime.entered
	if _, e = svc.Action(ctx, scope, sub.Branch.Metadata.ID, "cancel", "cancel"); e != nil {
		t.Fatal(e)
	}
	cancel := &cp.Controller{Store: st, Backend: runtime, WorkerID: "cancel"}
	if e = cancel.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	close(runtime.release)
	if e = <-done; cp.Code(e) != "aborted" {
		t.Fatalf("old Ensure completion not fenced: %v", e)
	}
	branch, e := svc.GetResource(ctx, scope, "Branch", sub.Branch.Metadata.ID)
	if e != nil {
		t.Fatal(e)
	}
	var status cp.BranchStatus
	json.Unmarshal(branch.Status, &status)
	runtime.mu.Lock()
	running := runtime.running
	runtime.mu.Unlock()
	if running || status.Observed != "cancelled" {
		t.Fatalf("cancel left running=%v status=%+v", running, status)
	}
}
func TestExecutionExpiryQueuesCleanupAndPreservesTerminalOutcomes(t *testing.T) {
	for _, terminal := range []string{"running", "stopped", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			st, svc, scope := fixture(t)
			task := shortTask(t, svc, scope)
			ctx := context.Background()
			sub, e := svc.Submit(ctx, scope, task.Metadata.ID, "submit")
			if e != nil {
				t.Fatal(e)
			}
			job, e := st.Claim(ctx, "first", time.Second)
			if e != nil {
				t.Fatal(e)
			}
			if e = st.Finish(ctx, job, cp.Observation{State: terminal}); e != nil {
				t.Fatal(e)
			}
			before, e := svc.Events(ctx, scope, sub.Branch.Metadata.ID, 0, 100)
			if e != nil {
				t.Fatal(e)
			}
			time.Sleep(1100 * time.Millisecond)
			if e = st.ExpireExecutions(ctx); e != nil {
				t.Fatal(e)
			}
			if _, e = st.AppendEvent(ctx, scope, sub.Branch.Metadata.ID, job.Assignment.ExecutionID, job.Assignment.Epoch, "late", json.RawMessage(`{}`)); cp.Code(e) != "aborted" {
				t.Fatalf("expired event accepted: %v", e)
			}
			branch, e := svc.GetResource(ctx, scope, "Branch", sub.Branch.Metadata.ID)
			if e != nil {
				t.Fatal(e)
			}
			var status cp.BranchStatus
			json.Unmarshal(branch.Status, &status)
			after, e := svc.Events(ctx, scope, sub.Branch.Metadata.ID, 0, 100)
			if e != nil {
				t.Fatal(e)
			}
			if terminal == "running" {
				if status.Observed != "interrupted" || len(after) != len(before)+1 || after[len(after)-1].Type != "execution_expired" {
					t.Fatalf("expiry missing cleanup: %+v events=%+v", status, after)
				}
				runtime := &delayedBackend{}
				controller := &cp.Controller{Store: st, Backend: runtime, WorkerID: "cleanup"}
				for i := 0; i < 2; i++ {
					_ = controller.Tick(ctx)
				}
				branch, e = svc.GetResource(ctx, scope, "Branch", sub.Branch.Metadata.ID)
				if e != nil {
					t.Fatal(e)
				}
				json.Unmarshal(branch.Status, &status)
				if !runtime.closed || status.Observed != "interrupted" {
					t.Fatalf("cleanup not confirmed: %+v", status)
				}
			} else if status.Observed != terminal || len(after) != len(before) {
				t.Fatalf("completed %s overwritten after lease expiry: %+v", terminal, status)
			}
		})
	}
}

type snapshotBackend struct{ advertisedSnapshots }

func (snapshotBackend) Suspend(context.Context, cp.Assignment) (cp.Observation, error) {
	return cp.Observation{State: "suspended", Checkpoint: &cp.CheckpointSpec{ManifestDigest: "sha256:" + strings.Repeat("b", 64), ManifestURI: "gs://fixture/checkpoint", ProtocolVersion: "hypercall.snapshot.v1", Verified: true, Compatibility: map[string]string{"template_digest": "sha256:" + strings.Repeat("a", 64), "snapshot_format": "fixture", "architecture": "fixture"}}}, nil
}
func TestVerifiedCheckpointRetainsBranchCompatibilityAndCommittedPosition(t *testing.T) {
	st, _, scope := fixture(t)
	svc := cp.NewService(st, snapshotBackend{})
	task := definitions(t, svc, scope)
	ctx := context.Background()
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
	controller := cp.Controller{Store: st, Backend: snapshotBackend{}, WorkerID: "snapshot"}
	if e = controller.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	all, e := svc.ListResources(ctx, scope, "Checkpoint", "", 100)
	if e != nil || len(all) != 1 {
		t.Fatalf("checkpoints=%d error=%v", len(all), e)
	}
	var checkpoint cp.CheckpointSpec
	if e = json.Unmarshal(all[0].Spec, &checkpoint); e != nil {
		t.Fatal(e)
	}
	if checkpoint.BranchID != sub.Branch.Metadata.ID || checkpoint.EventPosition != 3 || checkpoint.Compatibility["snapshot_format"] != "fixture" {
		t.Fatalf("checkpoint lost committed metadata: %+v", checkpoint)
	}

}
