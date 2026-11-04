package controlplane_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	cp "github.com/gduchidze/hypercall/internal/controlplane"
	"github.com/gduchidze/hypercall/internal/snapshot"
	"testing"
	"time"
)

func pinBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func agentAssignment(t *testing.T) (*cp.Store, *cp.Service, cp.Scope, cp.Assignment) {
	t.Helper()
	st, svc, scope := fixture(t)
	scope.TenantID = "agent-" + pinBytes([]byte(t.Name() + t.TempDir()))[7:31]
	base := definitions(t, svc, scope)
	ctx := context.Background()
	put := func(kind, name string, spec any) cp.Resource {
		raw, _ := json.Marshal(spec)
		r, e := svc.PutResource(ctx, scope, cp.Resource{Kind: kind, Metadata: cp.Metadata{Name: name}, Spec: raw}, "put-"+name)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	model := put("ModelConfig", "agent-model", cp.ModelConfigSpec{Provider: "fixture", Model: "alpha", Revision: "text-v1", ArtifactDigest: pinBytes([]byte("alpha-output")), Parameters: map[string]string{"fixture_response": "alpha-output"}})
	otherModel := put("ModelConfig", "agent-model-beta", cp.ModelConfigSpec{Provider: "fixture", Model: "beta", Revision: "text-v1", ArtifactDigest: pinBytes([]byte("beta-output")), Parameters: map[string]string{"fixture_response": "beta-output"}})
	gateway := put("Gateway", "agent-gateway", cp.GatewaySpec{Routes: map[string]string{"inference": "fixture://text-v1", "tool.echo": "fixture://echo-v1", "tool.unknown": "fixture://unknown-effect-v1"}})
	var spec cp.TaskSpec
	json.Unmarshal(base.Spec, &spec)
	spec.ExecutionMode = "cooperative_python_v1"
	spec.Command = nil
	spec.Env = nil
	spec.Models = []cp.Ref{{ID: model.Metadata.ID, Digest: model.Metadata.Digest}, {ID: otherModel.Metadata.ID, Digest: otherModel.Metadata.Digest}}
	spec.Gateway = cp.Ref{ID: gateway.Metadata.ID, Digest: gateway.Metadata.Digest}
	task := put("Task", "agent-task", spec)
	if _, e := svc.Submit(ctx, scope, task.Metadata.ID, "agent-submit"); e != nil {
		t.Fatal(e)
	}
	job, e := st.Claim(ctx, "agent-worker", time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	a := job.Assignment
	if e = st.Finish(ctx, job, cp.Observation{State: "running"}); e != nil {
		t.Fatal(e)
	}
	if a.HarnessSession == nil {
		t.Fatal("instrumented assignment has no immutable session")
	}
	if e = st.AcknowledgeAgentSession(ctx, a, a.HarnessSession.SessionHash); e != nil {
		t.Fatal(e)
	}
	return st, svc, scope, a
}
func TestInstrumentedStepRetainsFixtureResultAndCommittedState(t *testing.T) {
	_, svc, scope, a := agentAssignment(t)
	ctx := context.Background()
	request := cp.AgentRequest{ID: "prefix-echo", Kind: "tool", Step: 1, Position: 1, Name: "echo", Payload: []byte("shared-prefix"), PayloadDigest: pinBytes([]byte("shared-prefix")), SessionHash: a.HarnessSession.SessionHash}
	result, e := svc.ProcessAgentRequest(ctx, a, request)
	if e != nil {
		t.Fatal(e)
	}
	if result.State != "completed" || string(result.Payload) != "shared-prefix" {
		t.Fatalf("fixture result: %+v", result)
	}
	duplicate, e := svc.ProcessAgentRequest(ctx, a, request)
	if e != nil || !duplicate.Replayed || duplicate.EventSequence != result.EventSequence {
		t.Fatalf("lost result retry did not retain response: %+v %v", duplicate, e)
	}
	request.Payload = []byte("changed")
	request.PayloadDigest = pinBytes(request.Payload)
	if _, e = svc.ProcessAgentRequest(ctx, a, request); cp.Code(e) != "already_exists" {
		t.Fatalf("request ID collision accepted: %v", e)
	}
	state := []byte(`{"prefix":"committed"}`)
	commit := cp.AgentRequest{ID: "step-1", Kind: "step_commit", Step: 1, Position: 2, Payload: state, PayloadDigest: pinBytes(state), SessionHash: a.HarnessSession.SessionHash}
	committed, e := svc.ProcessAgentRequest(ctx, a, commit)
	if e != nil {
		t.Fatal(e)
	}
	if committed.State != "committed" {
		t.Fatalf("step not committed: %+v", committed)
	}
	steps, e := svc.Steps(ctx, scope, a.BranchID, 0, 100)
	if e != nil || len(steps) != 1 || steps[0].StateDigest != pinBytes(state) {
		t.Fatalf("steps=%+v error=%v", steps, e)
	}
}

func commitPrefix(t *testing.T, svc *cp.Service, a cp.Assignment) (cp.AgentResult, string) {
	t.Helper()
	ctx := context.Background()
	payload := []byte("prefix")
	if _, e := svc.ProcessAgentRequest(ctx, a, cp.AgentRequest{ID: "prefix", Kind: "tool", Step: 1, Position: 1, Name: "echo", Payload: payload, PayloadDigest: pinBytes(payload), SessionHash: a.HarnessSession.SessionHash}); e != nil {
		t.Fatal(e)
	}
	state := []byte(`{"prefix":true}`)
	result, e := svc.ProcessAgentRequest(ctx, a, cp.AgentRequest{ID: "commit-prefix", Kind: "step_commit", Step: 1, Position: 2, Payload: state, PayloadDigest: pinBytes(state), SessionHash: a.HarnessSession.SessionHash})
	if e != nil {
		t.Fatal(e)
	}
	return result, pinBytes(state)
}
func TestSealedAgentBarrierRejectsNewCallsAndPreservesExactCommittedCursor(t *testing.T) {
	st, svc, scope, a := agentAssignment(t)
	commit, state := commitPrefix(t, svc, a)
	barrier := cp.AgentBarrier{Step: 1, EventSequence: commit.EventSequence, StateDigest: state, SessionHash: a.HarnessSession.SessionHash, ProtocolVersion: "hypercall.harness.v1"}
	sealed, e := st.SealAgentBarrier(context.Background(), a, barrier)
	if e != nil {
		t.Fatal(e)
	}
	if sealed.EventSequence <= commit.EventSequence {
		t.Fatal("barrier marker was not durably ordered after committed step")
	}
	again, e := st.SealAgentBarrier(context.Background(), a, barrier)
	if e != nil || again.EventSequence != sealed.EventSequence {
		t.Fatalf("barrier retry moved checkpoint cursor: %v", e)
	}
	payload := []byte("next")
	if _, e = svc.ProcessAgentRequest(context.Background(), a, cp.AgentRequest{ID: "next", Kind: "inference", Step: 2, Position: 1, Name: "inference", Payload: payload, PayloadDigest: pinBytes(payload), SessionHash: a.HarnessSession.SessionHash}); cp.Code(e) != "failed_precondition" {
		t.Fatalf("sealed gateway accepted new action: %v", e)
	}
	events, e := svc.Events(context.Background(), scope, a.BranchID, 0, 100)
	if e != nil || events[len(events)-1].Sequence != sealed.EventSequence {
		t.Fatal("rejected call advanced sealed marker")
	}
}

type semanticBackend struct {
	resumableBackend
	failBeta bool
}

func (*semanticBackend) Supports(string) bool { return true }
func (b *semanticBackend) Resume(ctx context.Context, a cp.Assignment, checkpoint cp.CheckpointSpec) (cp.Observation, error) {
	var model cp.ModelConfigSpec
	json.Unmarshal(a.SelectedModel.Spec, &model)
	if b.failBeta && model.Model == "beta" {
		return cp.Observation{State: "failed"}, nil
	}
	return b.resumableBackend.Resume(ctx, a, checkpoint)
}
func (*semanticBackend) Observe(context.Context, cp.Assignment) (cp.Observation, error) {
	return cp.Observation{State: "running"}, nil
}
func (*semanticBackend) Stop(context.Context, cp.Assignment) (cp.Observation, error) {
	return cp.Observation{State: "stopped"}, nil
}

func (*semanticBackend) Suspend(_ context.Context, a cp.Assignment) (cp.Observation, error) {
	if a.Barrier == nil {
		return cp.Observation{}, &cp.Error{Kind: "failed_precondition", Message: "sealed barrier required"}
	}
	var task cp.TaskSpec
	json.Unmarshal(a.Run.Task.Spec, &task)
	id := "native-" + a.ExecutionID
	b := a.Barrier
	m := snapshot.Manifest{SchemaVersion: 1, Scope: snapshot.Scope{TenantID: a.Scope.TenantID, ProjectID: a.Scope.ProjectID, CheckpointID: id}, SemanticBarrier: &snapshot.SemanticBarrier{ProtocolVersion: b.ProtocolVersion, Step: b.Step, EventSequence: b.EventSequence, StateDigest: b.StateDigest, SourceSessionHash: b.SessionHash, SourceBranchID: a.BranchID, SourceExecutionID: a.ExecutionID, SourceEpoch: a.Epoch}}
	raw, _ := json.Marshal(m)
	return cp.Observation{State: "suspended", Checkpoint: &cp.CheckpointSpec{NativeCheckpointID: id, SnapshotManifest: raw, BranchID: a.BranchID, Barrier: a.Barrier, RouteDigest: a.HarnessSession.RouteDigest, ManifestDigest: pinBytes(raw), ManifestURI: "fixture://native", ProtocolVersion: "hypercall.snapshot.v1", EventPosition: b.EventSequence, Verified: true, Compatibility: map[string]string{"snapshot_format": "fixture", "architecture": "fixture", "template_digest": task.TemplateDigest}}}, nil
}
func TestSemanticCheckpointPreservesSealedCursorDespiteLifecycleEvents(t *testing.T) {
	st, svc, scope, a := agentAssignment(t)
	ctx := context.Background()
	result, state := commitPrefix(t, svc, a)
	sealed, e := st.SealAgentBarrier(ctx, a, cp.AgentBarrier{Step: 1, EventSequence: result.EventSequence, StateDigest: state, SessionHash: a.HarnessSession.SessionHash, ProtocolVersion: "hypercall.harness.v1"})
	if e != nil {
		t.Fatal(e)
	}
	backend := &semanticBackend{}
	svc = cp.NewService(st, backend)
	if _, e = svc.Action(ctx, scope, a.BranchID, "suspend", "suspend"); e != nil {
		t.Fatal(e)
	}
	controller := cp.Controller{Store: st, Backend: backend, WorkerID: "snapshot"}
	tickController(t, ctx, &controller)
	all, e := svc.ListResources(ctx, scope, "Checkpoint", "", 100)
	if e != nil || len(all) != 1 {
		t.Fatalf("semantic checkpoint absent: %v", e)
	}
	var checkpoint cp.CheckpointSpec
	json.Unmarshal(all[0].Spec, &checkpoint)
	if checkpoint.EventPosition != sealed.EventSequence || checkpoint.Barrier == nil || checkpoint.Barrier.StateDigest != state || checkpoint.BudgetRemainingMS <= 0 {
		t.Fatalf("semantic marker replaced with lifecycle cursor: %+v", checkpoint)
	}
}

func TestForkCreatesDurableDistinctChildrenFromAuthorizedSemanticCheckpoint(t *testing.T) {
	st, svc, scope, a := agentAssignment(t)
	ctx := context.Background()
	result, state := commitPrefix(t, svc, a)
	if _, e := st.SealAgentBarrier(ctx, a, cp.AgentBarrier{Step: 1, EventSequence: result.EventSequence, StateDigest: state, SessionHash: a.HarnessSession.SessionHash, ProtocolVersion: "hypercall.harness.v1"}); e != nil {
		t.Fatal(e)
	}
	backend := &semanticBackend{}
	svc = cp.NewService(st, backend)
	if _, e := svc.Action(ctx, scope, a.BranchID, "suspend", "suspend"); e != nil {
		t.Fatal(e)
	}
	controller := cp.Controller{Store: st, Backend: backend, WorkerID: "snapshot"}
	tickController(t, ctx, &controller)
	all, e := svc.ListResources(ctx, scope, "Checkpoint", "", 100)
	if e != nil || len(all) != 1 {
		t.Fatal("checkpoint absent")
	}
	model := cp.Ref{ID: a.SelectedModel.Metadata.ID, Digest: a.SelectedModel.Metadata.Digest}
	fork, e := svc.Fork(ctx, scope, all[0].Metadata.ID, []cp.Ref{model, model}, "fork")
	if e != nil {
		t.Fatal(e)
	}
	if len(fork.Children) != 2 || fork.Children[0].Metadata.ID == fork.Children[1].Metadata.ID {
		t.Fatalf("fork identities: %+v", fork)
	}
	replay, e := svc.Fork(ctx, scope, all[0].Metadata.ID, []cp.Ref{model, model}, "fork")
	if e != nil || replay.Children[0].Metadata.ID != fork.Children[0].Metadata.ID {
		t.Fatalf("fork retry changed children: %v", e)
	}
	ids := map[string]bool{}
	for _, child := range fork.Children {
		var spec cp.BranchSpec
		json.Unmarshal(child.Spec, &spec)
		if spec.ParentBranchID != a.BranchID || spec.ParentCheckpointID != all[0].Metadata.ID || spec.Model.Metadata.Digest != model.Digest {
			t.Fatalf("child lineage/config mismatch: %+v", spec)
		}
		status := statusOf(t, svc, scope, child.Metadata.ID)
		if status.AgentStep != 1 || status.Epoch != 1 || status.SandboxID == a.SandboxID || ids[status.SandboxID] || status.ExecutionID == a.ExecutionID {
			t.Fatalf("child assignment reused identity/prefix: %+v", status)
		}
		ids[status.SandboxID] = true
	}
	for i := 0; i < 2; i++ {
		tickController(t, ctx, &controller)
	}
	if len(backend.resumed) != 2 || backend.resumed[0].HarnessSession == nil || backend.resumed[0].HarnessSession.ParentCheckpointId == "" || backend.resumed[0].HarnessSession.CallNamespace == a.HarnessSession.CallNamespace {
		t.Fatal("child restore did not carry fresh inherited routing session")
	}
	for _, restored := range backend.resumed {
		if e = st.AcknowledgeAgentSession(ctx, restored, restored.HarnessSession.SessionHash); e != nil {
			t.Fatal(e)
		}
	}
	aggregate, e := svc.GetResource(ctx, scope, "Operation", fork.Operation.Metadata.ID)
	if e != nil {
		t.Fatal(e)
	}
	var status cp.OperationStatus
	json.Unmarshal(aggregate.Status, &status)
	if status.State != "succeeded" {
		t.Fatalf("completed child restores left aggregate pending: %+v", status)
	}

}

func TestUnknownFixtureOutcomeBlocksCommitAndCheckpointWithoutReexecution(t *testing.T) {
	st, svc, _, a := agentAssignment(t)
	ctx := context.Background()
	payload := []byte("synthetic-mutation")
	request := cp.AgentRequest{ID: "unknown-effect", Kind: "tool", Step: 1, Position: 1, Name: "unknown", Payload: payload, PayloadDigest: pinBytes(payload), SessionHash: a.HarnessSession.SessionHash}
	result, e := svc.ProcessAgentRequest(ctx, a, request)
	if e != nil || result.State != "unknown" || result.ErrorCode != "unknown_effect" {
		t.Fatalf("unknown outcome was hidden: %+v %v", result, e)
	}
	replay, e := svc.ProcessAgentRequest(ctx, a, request)
	if e != nil || !replay.Replayed || replay.EventSequence != result.EventSequence || replay.State != "unknown" {
		t.Fatalf("unknown effect was replayed: %+v %v", replay, e)
	}
	state := []byte(`{}`)
	if _, e = svc.ProcessAgentRequest(ctx, a, cp.AgentRequest{ID: "commit", Kind: "step_commit", Step: 1, Position: 2, Payload: state, PayloadDigest: pinBytes(state), SessionHash: a.HarnessSession.SessionHash}); cp.Code(e) != "failed_precondition" {
		t.Fatalf("unknown effect silently committed: %v", e)
	}
	if _, e = st.SealAgentBarrier(ctx, a, cp.AgentBarrier{Step: 1, EventSequence: result.EventSequence, StateDigest: pinBytes(state), SessionHash: a.HarnessSession.SessionHash, ProtocolVersion: "hypercall.harness.v1"}); cp.Code(e) != "failed_precondition" {
		t.Fatalf("unknown effect became checkpointable: %v", e)
	}
}

func forkedAgents(t *testing.T, fail ...bool) (*cp.Store, *cp.Service, cp.Scope, []cp.Assignment) {
	t.Helper()
	st, svc, scope, a := agentAssignment(t)
	ctx := context.Background()
	// Branch-local event cursors deliberately differ across inherited generations.
	for i := 0; i < 10; i++ {
		if _, e := st.AppendEvent(ctx, scope, a.BranchID, a.ExecutionID, a.Epoch, "lifecycle_diagnostic", json.RawMessage(`{"synthetic":true}`)); e != nil {
			t.Fatal(e)
		}
	}

	committed, state := commitPrefix(t, svc, a)
	if _, e := st.SealAgentBarrier(ctx, a, cp.AgentBarrier{Step: 1, EventSequence: committed.EventSequence, StateDigest: state, SessionHash: a.HarnessSession.SessionHash, ProtocolVersion: "hypercall.harness.v1"}); e != nil {
		t.Fatal(e)
	}
	backend := &semanticBackend{}
	if len(fail) > 0 {
		backend.failBeta = fail[0]
	}
	svc = cp.NewService(st, backend)
	if _, e := svc.Action(ctx, scope, a.BranchID, "suspend", "suspend"); e != nil {
		t.Fatal(e)
	}
	controller := cp.Controller{Store: st, Backend: backend, WorkerID: "fork"}
	tickController(t, ctx, &controller)
	checkpoints, e := svc.ListResources(ctx, scope, "Checkpoint", "", 10)
	if e != nil || len(checkpoints) != 1 {
		t.Fatal("missing checkpoint")
	}
	refs := []cp.Ref{}
	for _, model := range a.Run.Models {
		refs = append(refs, cp.Ref{ID: model.Metadata.ID, Digest: model.Metadata.Digest})
	}
	if _, e = svc.Fork(ctx, scope, checkpoints[0].Metadata.ID, refs, "fork"); e != nil {
		t.Fatal(e)
	}
	for range refs {
		tickController(t, ctx, &controller)
	}
	for _, child := range backend.resumed {
		if e = st.AcknowledgeAgentSession(ctx, child, child.HarnessSession.SessionHash); e != nil {
			t.Fatal(e)
		}
	}
	return st, svc, scope, backend.resumed
}
func finishAgentSuffix(t *testing.T, svc *cp.Service, a cp.Assignment) {
	t.Helper()
	ctx := context.Background()
	request := func(id, kind, name string, step int64, pos int32, payload []byte) cp.AgentResult {
		result, e := svc.ProcessAgentRequest(ctx, a, cp.AgentRequest{ID: id, Kind: kind, Step: step, Position: pos, Name: name, Payload: payload, PayloadDigest: pinBytes(payload), SessionHash: a.HarnessSession.SessionHash})
		if e != nil {
			t.Fatal(e)
		}
		return result
	}
	answer := request("infer", "inference", "inference", 2, 1, []byte("same-question"))
	request("step2", "step_commit", "", 2, 2, answer.Payload)
	observation := request("echo", "tool", "echo", 3, 1, answer.Payload)
	request("step3", "step_commit", "", 3, 2, observation.Payload)
	request("finalize", "finalize", "", 3, 3, observation.Payload)
}
func TestComparatorsReportFirstObservedContinuationDifference(t *testing.T) {
	_, svc, scope, children := forkedAgents(t)
	for _, child := range children {
		finishAgentSuffix(t, svc, child)
	}
	for _, want := range []struct {
		name string
		step int64
	}{{"output_bytes_v1", 2}, {"output_normalized_v1", 2}, {"tool_calls_v1", 3}, {"observations_v1", 3}} {
		difference, e := svc.Compare(context.Background(), scope, children[0].BranchID, children[1].BranchID, want.name)
		if e != nil || difference.State != "diverged" || difference.Step != want.step || difference.Position != 1 {
			t.Fatalf("%s comparison: %+v %v", want.name, difference, e)
		}
	}
}

func TestFinalizeCannotSkipACompletedUncommittedStep(t *testing.T) {
	_, svc, scope, a := agentAssignment(t)
	ctx := context.Background()
	commitPrefix(t, svc, a)
	payload := []byte("new-step")
	request := func(id, kind, name string, step int64, position int32) cp.AgentRequest {
		return cp.AgentRequest{ID: id, Kind: kind, Name: name, Step: step, Position: position, Payload: payload, PayloadDigest: pinBytes(payload), SessionHash: a.HarnessSession.SessionHash}
	}
	if _, e := svc.ProcessAgentRequest(ctx, a, request("infer2", "inference", "inference", 2, 1)); e != nil {
		t.Fatal(e)
	}
	before, e := svc.Events(ctx, scope, a.BranchID, 0, 100)
	if e != nil {
		t.Fatal(e)
	}
	for range 2 {
		if result, e := svc.ProcessAgentRequest(ctx, a, request("finalize", "finalize", "", 1, 3)); cp.Code(e) != "failed_precondition" || result.Replayed {
			t.Fatalf("finalize skipped an uncommitted tail or replayed rejected input: %+v %v", result, e)
		}
	}
	after, e := svc.Events(ctx, scope, a.BranchID, 0, 100)
	if e != nil || len(after) != len(before) {
		t.Fatalf("rejected finalization registered an action: events %d -> %d, %v", len(before), len(after), e)
	}
	branch, e := svc.GetResource(ctx, scope, "Branch", a.BranchID)
	if e != nil {
		t.Fatal(e)
	}
	var status cp.BranchStatus
	if e = json.Unmarshal(branch.Status, &status); e != nil || status.TraceComplete {
		t.Fatalf("uncommitted trace marked complete: %+v %v", status, e)
	}
	if _, e = svc.ProcessAgentRequest(ctx, a, request("commit2", "step_commit", "", 2, 2)); e != nil {
		t.Fatal(e)
	}
	result, e := svc.ProcessAgentRequest(ctx, a, request("finalize", "finalize", "", 2, 3))
	if e != nil || result.State != "finalized" || result.Replayed {
		t.Fatalf("committed trace could not finalize with previously rejected ID: %+v %v", result, e)
	}
}

func TestComparatorMisalignedPositionIsIndependentOfBranchOrder(t *testing.T) {
	_, svc, scope, children := forkedAgents(t)
	ctx := context.Background()
	for index, a := range children {
		position := int32(1)
		request := func(id, kind, name string) {
			payload := []byte("same-question")
			_, e := svc.ProcessAgentRequest(ctx, a, cp.AgentRequest{ID: id, Kind: kind, Name: name, Step: 2, Position: position, Payload: payload, PayloadDigest: pinBytes(payload), SessionHash: a.HarnessSession.SessionHash})
			if e != nil {
				t.Fatal(e)
			}
			position++
		}
		if index == 0 {
			request("extra-tool", "tool", "echo")
		}
		request("infer2", "inference", "inference")
		request("commit2", "step_commit", "")
		request("finalize2", "finalize", "")
	}
	for _, pair := range [][2]int{{0, 1}, {1, 0}} {
		difference, e := svc.Compare(ctx, scope, children[pair[0]].BranchID, children[pair[1]].BranchID, "output_bytes_v1")
		if e != nil || difference.State != "diverged" || difference.Step != 2 || difference.Position != 1 {
			t.Fatalf("branch order %v changed first recorded ordinal: %+v %v", pair, difference, e)
		}
	}
}

func TestBarrierCannotRewindPastACompletedUncommittedAction(t *testing.T) {
	st, svc, _, a := agentAssignment(t)
	committed, state := commitPrefix(t, svc, a)
	payload := []byte("new-step")
	if _, e := svc.ProcessAgentRequest(context.Background(), a, cp.AgentRequest{ID: "new-step", Kind: "inference", Step: 2, Position: 1, Name: "inference", Payload: payload, PayloadDigest: pinBytes(payload), SessionHash: a.HarnessSession.SessionHash}); e != nil {
		t.Fatal(e)
	}
	if _, e := st.SealAgentBarrier(context.Background(), a, cp.AgentBarrier{Step: 1, EventSequence: committed.EventSequence, StateDigest: state, SessionHash: a.HarnessSession.SessionHash, ProtocolVersion: "hypercall.harness.v1"}); cp.Code(e) != "failed_precondition" {
		t.Fatalf("checkpoint rewound past already observed step2: %v", e)
	}
}

type partitionRelay struct {
	semanticBackend
	unavailable string
	delivered   map[string]int
}

func (b *partitionRelay) AgentPoll(_ context.Context, a cp.Assignment) (cp.AgentPoll, error) {
	if a.BranchID == b.unavailable {
		return cp.AgentPoll{}, &cp.Error{Kind: "unavailable", Message: "synthetic partition"}
	}
	return cp.AgentPoll{SessionAck: a.HarnessSession.SessionHash}, nil
}
func (b *partitionRelay) AgentDeliver(_ context.Context, a cp.Assignment, d cp.AgentDelivery) error {
	b.delivered[a.BranchID]++
	return nil
}
func TestPartitionedBranchDoesNotStarveSiblingRelay(t *testing.T) {
	st, _, _, children := forkedAgents(t)
	first, other := children[0].BranchID, children[1].BranchID
	if other < first {
		first, other = other, first
	}
	backend := &partitionRelay{unavailable: first, delivered: map[string]int{}}
	controller := cp.Controller{Store: st, Backend: backend, WorkerID: "relay"}
	if e := controller.AgentTick(context.Background()); cp.Code(e) != "unavailable" {
		t.Fatalf("partition outcome lost: %v", e)
	}
	if backend.delivered[other] != 1 {
		t.Fatal("partitioned first branch starved independent sibling")
	}
}
func TestComparatorsSeparateUnknownEffectsFromIncompleteInstrumentation(t *testing.T) {
	_, svc, scope, children := forkedAgents(t)
	ctx := context.Background()
	difference, e := svc.Compare(ctx, scope, children[0].BranchID, children[1].BranchID, "output_bytes_v1")
	if e != nil || difference.State != "incomplete" {
		t.Fatalf("unfinished trace reported complete: %+v %v", difference, e)
	}
	a := children[0]
	payload := []byte("unknown")
	if _, e = svc.ProcessAgentRequest(ctx, a, cp.AgentRequest{ID: "unknown", Kind: "tool", Step: 2, Position: 1, Name: "unknown", Payload: payload, PayloadDigest: pinBytes(payload), SessionHash: a.HarnessSession.SessionHash}); e != nil {
		t.Fatal(e)
	}
	difference, e = svc.Compare(ctx, scope, a.BranchID, children[1].BranchID, "output_bytes_v1")
	if e != nil || difference.State != "unknown" {
		t.Fatalf("unknown tool effect flattened into incomplete/equal trace: %+v %v", difference, e)
	}
}
func TestStaleAndForgedRoutingAssignmentsCannotAppendAgentActions(t *testing.T) {
	_, svc, _, a := agentAssignment(t)
	payload := []byte("x")
	request := cp.AgentRequest{ID: "bound", Kind: "tool", Step: 1, Position: 1, Name: "echo", Payload: payload, PayloadDigest: pinBytes(payload), SessionHash: a.HarnessSession.SessionHash}
	stale := a
	stale.Epoch++
	if _, e := svc.ProcessAgentRequest(context.Background(), stale, request); cp.Code(e) != "aborted" {
		t.Fatalf("stale epoch accepted: %v", e)
	}
	forged := a
	forged.SelectedModel.Spec = json.RawMessage(`{"provider":"fixture","model":"different","revision":"text-v1","artifact_digest":"sha256:irrelevant","parameters":{"fixture_response":"other"}}`)
	if _, e := svc.ProcessAgentRequest(context.Background(), forged, request); cp.Code(e) != "aborted" {
		t.Fatalf("session reused with forged model data: %v", e)
	}
}

func TestExpiredFixtureDispatchRecoversSafeCallButNeverReexecutesUnknownMutation(t *testing.T) {
	for _, tool := range []string{"echo", "unknown"} {
		t.Run(tool, func(t *testing.T) {
			_, svc, scope, a := agentAssignment(t)
			ctx := context.Background()
			payload := []byte("retained-input")
			request := cp.AgentRequest{ID: "recover", Kind: "tool", Step: 1, Position: 1, Name: tool, Payload: payload, PayloadDigest: pinBytes(payload), SessionHash: a.HarnessSession.SessionHash}
			first, e := svc.ProcessAgentRequest(ctx, a, request)
			if e != nil {
				t.Fatal(e)
			}
			// Model a dispatcher crash after registered execution but before result commit.
			db, e := sql.Open("pgx", testDSN(t))
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			if _, e = db.ExecContext(ctx, `UPDATE agent_calls SET state='executing',result=NULL,claim_token='lost-worker',lease_until=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND project_id=$2 AND branch_id=$3 AND id=$4`, scope.TenantID, scope.ProjectID, a.BranchID, request.ID); e != nil {
				t.Fatal(e)
			}
			recovered, e := svc.ProcessAgentRequest(ctx, a, request)
			if e != nil {
				t.Fatal(e)
			}
			if tool == "echo" && (recovered.State != "completed" || string(recovered.Payload) != "retained-input") {
				t.Fatalf("safe fixture did not recover: %+v", recovered)
			}
			if tool == "unknown" && (recovered.State != "unknown" || recovered.ErrorCode != "unknown_effect") {
				t.Fatalf("ambiguous mutation automatically replayed: %+v", recovered)
			}
			if recovered.EventSequence <= first.EventSequence {
				t.Fatal("recovered outcome was not durably appended")
			}
			calls, e := svc.Calls(ctx, scope, a.BranchID, 0, 100)
			if e != nil || len(calls) != 1 {
				t.Fatalf("recovery duplicated call identity: %+v %v", calls, e)
			}
		})
	}
}
func TestCancellationAndScopeChangesFenceAgentRequests(t *testing.T) {
	_, svc, scope, a := agentAssignment(t)
	ctx := context.Background()
	if _, e := svc.Calls(ctx, cp.Scope{TenantID: "other", ProjectID: scope.ProjectID}, a.BranchID, 0, 100); cp.Code(e) != "not_found" {
		t.Fatalf("tenant isolation failed: %v", e)
	}
	if _, e := svc.Action(ctx, scope, a.BranchID, "cancel", "cancel"); e != nil {
		t.Fatal(e)
	}
	payload := []byte("late")
	if _, e := svc.ProcessAgentRequest(ctx, a, cp.AgentRequest{ID: "late", Kind: "tool", Step: 1, Position: 1, Name: "echo", Payload: payload, PayloadDigest: pinBytes(payload), SessionHash: a.HarnessSession.SessionHash}); cp.Code(e) != "aborted" {
		t.Fatalf("cancelled execution appended a new agent action: %v", e)
	}
}

func TestForkAggregateRetainsPartialFailureAndSuccessfulChild(t *testing.T) {
	_, svc, scope, children := forkedAgents(t, true)
	if len(children) != 1 {
		t.Fatalf("successful restore lost or failed child retried: %d", len(children))
	}
	branch, e := svc.GetResource(context.Background(), scope, "Branch", children[0].BranchID)
	if e != nil {
		t.Fatal(e)
	}
	var spec cp.BranchSpec
	json.Unmarshal(branch.Spec, &spec)
	op, e := svc.GetResource(context.Background(), scope, "Operation", spec.ForkOperationID)
	if e != nil {
		t.Fatal(e)
	}
	var status cp.OperationStatus
	json.Unmarshal(op.Status, &status)
	if status.State != "partial_failure" {
		t.Fatalf("fork aggregate hid per-child failure: %+v", status)
	}
	all, e := svc.ListResources(context.Background(), scope, "Branch", "", 100)
	if e != nil {
		t.Fatal(e)
	}
	failed := 0
	for _, b := range all {
		var bs cp.BranchSpec
		json.Unmarshal(b.Spec, &bs)
		if bs.ForkOperationID == spec.ForkOperationID && statusOf(t, svc, scope, b.Metadata.ID).Observed == "failed" {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("failed child result was not retained: %d", failed)
	}
}

func TestNestedLineageDoesNotClipAncestorTraceByAnotherBranchCursor(t *testing.T) {
	st, _, scope, children := forkedAgents(t)
	ctx := context.Background()
	source := children[0]
	backend := &semanticBackend{}
	svc := cp.NewService(st, backend)
	payload := []byte("nested-prefix")
	if _, e := svc.ProcessAgentRequest(ctx, source, cp.AgentRequest{ID: "nested-tool", Kind: "tool", Step: 2, Position: 1, Name: "echo", Payload: payload, PayloadDigest: pinBytes(payload), SessionHash: source.HarnessSession.SessionHash}); e != nil {
		t.Fatal(e)
	}
	committed, e := svc.ProcessAgentRequest(ctx, source, cp.AgentRequest{ID: "nested-step", Kind: "step_commit", Step: 2, Position: 2, Payload: payload, PayloadDigest: pinBytes(payload), SessionHash: source.HarnessSession.SessionHash})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = st.SealAgentBarrier(ctx, source, cp.AgentBarrier{Step: 2, EventSequence: committed.EventSequence, StateDigest: pinBytes(payload), SessionHash: source.HarnessSession.SessionHash, ProtocolVersion: "hypercall.harness.v1"}); e != nil {
		t.Fatal(e)
	}
	if _, e = svc.Action(ctx, scope, source.BranchID, "suspend", "nested-suspend"); e != nil {
		t.Fatal(e)
	}
	controller := cp.Controller{Store: st, Backend: backend, WorkerID: "nested"}
	for i := 0; i < 16 && statusOf(t, svc, scope, source.BranchID).Observed != "suspended"; i++ {
		tickController(t, ctx, &controller)
	}
	all, e := svc.ListResources(ctx, scope, "Checkpoint", "", 100)
	if e != nil {
		t.Fatal(e)
	}
	checkpointID := ""
	for _, resource := range all {
		var cpSpec cp.CheckpointSpec
		json.Unmarshal(resource.Spec, &cpSpec)
		if cpSpec.BranchID == source.BranchID {
			checkpointID = resource.Metadata.ID
		}
	}
	if checkpointID == "" {
		t.Fatal("nested checkpoint missing")
	}
	ref := cp.Ref{ID: source.SelectedModel.Metadata.ID, Digest: source.SelectedModel.Metadata.Digest}
	if _, e = svc.Fork(ctx, scope, checkpointID, []cp.Ref{ref, ref}, "nested-fork"); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 16 && len(backend.resumed) < 2; i++ {
		tickController(t, ctx, &controller)
	}
	if len(backend.resumed) != 2 {
		t.Fatal("nested children missing")
	}
	for _, a := range backend.resumed {
		if e = st.AcknowledgeAgentSession(ctx, a, a.HarnessSession.SessionHash); e != nil {
			t.Fatal(e)
		}
		for _, r := range []cp.AgentRequest{{ID: "tool3", Kind: "tool", Step: 3, Position: 1, Name: "echo"}, {ID: "commit3", Kind: "step_commit", Step: 3, Position: 2}, {ID: "final3", Kind: "finalize", Step: 3, Position: 3}} {
			r.Payload = payload
			r.PayloadDigest = pinBytes(payload)
			r.SessionHash = a.HarnessSession.SessionHash
			if _, e = svc.ProcessAgentRequest(ctx, a, r); e != nil {
				t.Fatal(e)
			}
		}
	}
	// Simulate retained ancestor trace damage after the native checkpoint: an
	// unresolved old outcome must not disappear behind a child's smaller cursor.
	db, e := sql.Open("pgx", testDSN(t))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.ExecContext(ctx, `UPDATE agent_calls SET state='unknown',result=jsonb_set(result,'{state}','"unknown"') WHERE tenant_id=$1 AND project_id=$2 AND branch_id=$3 AND id='prefix'`, scope.TenantID, scope.ProjectID, source.ParentBranchID); e != nil {
		t.Fatal(e)
	}
	difference, e := svc.Compare(ctx, scope, backend.resumed[0].BranchID, backend.resumed[1].BranchID, "observations_v1")
	if e != nil || difference.State != "unknown" {
		t.Fatalf("ancestor unknown outcome was clipped by descendant event namespace: %+v %v", difference, e)
	}
}
