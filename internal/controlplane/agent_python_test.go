package controlplane_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
	publicv1 "github.com/gduchidze/hypercall/gen/go/hypercall/v1"
	"github.com/gduchidze/hypercall/internal/api"
	cp "github.com/gduchidze/hypercall/internal/controlplane"
	"github.com/gduchidze/hypercall/internal/guest"
	"github.com/gduchidze/hypercall/internal/harness"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestPythonCooperativeHookCommitsThroughGuestMailboxAndPostgres(t *testing.T) {
	store, service, scope, assignment := agentAssignment(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const credential = "python-tracer-private-guest-session-0123456789"
	server, err := guest.New(guest.Config{Workspace: t.TempDir(), Identity: &guestv1.AllocationIdentity{TenantId: scope.TenantID, ProjectId: scope.ProjectID, SandboxId: assignment.SandboxID, ExecutionId: assignment.ExecutionID, Epoch: uint64(assignment.Epoch)}, Credential: credential, ExpiresAt: assignment.LeaseUntil, TrustedLocal: true, ExecHelper: executable, HarnessSession: assignment.HarnessSession})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	local := grpc.NewServer()
	harnessv1.RegisterHarnessServer(local, server)
	directory, err := os.MkdirTemp("", "hc-agent-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	socket := filepath.Join(directory, "harness.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	go local.Serve(listener)
	defer local.Stop()
	private := grpc.NewServer()
	guestv1.RegisterGuestServer(private, server)
	buffer := bufconn.Listen(2 << 20)
	go private.Serve(buffer)
	defer private.Stop()
	conn, err := grpc.NewClient("passthrough:///private", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return buffer.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := guestv1.NewGuestClient(conn)
	authorized := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+credential)
	loopDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				loopDone <- nil
				return
			case <-ticker.C:
			}
			pending, e := client.PollAgent(authorized, &guestv1.PollAgentRequest{})
			if e != nil {
				loopDone <- e
				return
			}
			delivery := &harnessv1.AgentDelivery{SessionHash: assignment.HarnessSession.SessionHash}
			switch p := pending.Pending.(type) {
			case *harnessv1.AgentPoll_SessionAck:
				if e = store.AcknowledgeAgentSession(ctx, assignment, p.SessionAck); e != nil {
					loopDone <- e
					return
				}
				delivery.Delivery = &harnessv1.AgentDelivery_SessionAck{SessionAck: p.SessionAck}
			case *harnessv1.AgentPoll_Request:
				request := p.Request
				result, e := service.ProcessAgentRequest(ctx, assignment, cp.AgentRequest{ID: request.Id, Kind: request.Kind, Step: request.Step, Position: request.Position, Name: request.Name, Payload: request.Payload, PayloadDigest: request.PayloadDigest, SessionHash: request.SessionHash})
				if e != nil {
					loopDone <- e
					return
				}
				hash, e := harness.MessageHash(request)
				if e != nil {
					loopDone <- e
					return
				}
				delivery.RequestHash = hash
				delivery.Delivery = &harnessv1.AgentDelivery_Result{Result: &harnessv1.AgentResult{Id: result.ID, State: result.State, Payload: result.Payload, EventSequence: result.EventSequence, ErrorCode: result.ErrorCode, Replayed: result.Replayed}}
			default:
				continue
			}
			if _, e = client.DeliverAgent(authorized, delivery); e != nil {
				loopDone <- e
				return
			}
		}
	}()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	process := exec.CommandContext(ctx, filepath.Join(root, ".venv/bin/python"), "test/contract/agent_tracer_scenario.py", socket)
	process.Dir = root
	output, err := process.CombinedOutput()
	cancel()
	if pollErr := <-loopDone; pollErr != nil && err == nil && status.Code(pollErr) != codes.Canceled {
		t.Fatal(pollErr)
	}
	if err != nil {
		t.Fatalf("Python hook tracer: %v\n%s", err, output)
	}
	var observed struct {
		Step          int64  `json:"step"`
		EventSequence int64  `json:"event_sequence"`
		StateDigest   string `json:"state_digest"`
		SessionHash   string `json:"session_hash"`
	}
	if err = json.Unmarshal(output, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Step != 1 || observed.EventSequence < 3 || observed.StateDigest != pinBytes([]byte(`{"prefix":"shared-prefix"}`)) || observed.SessionHash != assignment.HarnessSession.SessionHash {
		t.Fatalf("uncommitted Python observation: %s", output)
	}
	events, err := service.Events(t.Context(), scope, assignment.BranchID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(events)) != observed.EventSequence {
		t.Fatalf("Python acknowledged cursor not durable: %d events, cursor%d", len(events), observed.EventSequence)
	}
}

func TestPublicTracePaginationAndForkPreserveSemanticLineage(t *testing.T) {
	// Cold CLI compilation is test preparation, separate from the live assignment
	// and public-client deadline. Keep both phases bounded independently.
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "hypercall")
	buildCtx, cancelBuild := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, filepath.Join(runtime.GOROOT(), "bin/go"), "build", "-o", binary, "./cmd/hypercall")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	cancelBuild()
	store, service, scope, assignment := agentAssignment(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for i, payload := range []string{"prefix", "second"} {
		if _, err := service.ProcessAgentRequest(ctx, assignment, cp.AgentRequest{ID: payload, Kind: "tool", Step: 1, Position: int32(i + 1), Name: "echo", Payload: []byte(payload), PayloadDigest: pinBytes([]byte(payload)), SessionHash: assignment.HarnessSession.SessionHash}); err != nil {
			t.Fatal(err)
		}
	}
	state := pinBytes([]byte(`{"prefix":true}`))
	result, err := service.ProcessAgentRequest(ctx, assignment, cp.AgentRequest{ID: "prefix-commit", Kind: "step_commit", Step: 1, Position: 3, Payload: []byte(`{"prefix":true}`), PayloadDigest: state, SessionHash: assignment.HarnessSession.SessionHash})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SealAgentBarrier(ctx, assignment, cp.AgentBarrier{Step: 1, EventSequence: result.EventSequence, StateDigest: state, SessionHash: assignment.HarnessSession.SessionHash, ProtocolVersion: "hypercall.harness.v1"}); err != nil {
		t.Fatal(err)
	}
	backend := &semanticBackend{}
	service = cp.NewService(store, backend)
	const key = "public-agent-test-key-012345678901234567890"
	auth, err := api.NewAuthenticator(map[string]api.Principal{key: {TenantID: scope.TenantID, Projects: []string{scope.ProjectID}}})
	if err != nil {
		t.Fatal(err)
	}
	server := api.NewServer(service, auth)
	listener := bufconn.Listen(4 << 20)
	go server.GRPC().Serve(listener)
	defer server.GRPC().Stop()
	conn, err := grpc.NewClient("passthrough:///public-agent", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := publicv1.NewControlPlaneClient(conn)
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+key)
	first, err := client.Calls(ctx, &publicv1.CallsRequest{ProjectId: scope.ProjectID, BranchId: assignment.BranchID, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Calls) != 1 || first.NextSequence < 1 || string(first.Calls[0].Result.Payload) != "prefix" {
		t.Fatalf("first trace page: %v", first)
	}
	second, err := client.Calls(ctx, &publicv1.CallsRequest{ProjectId: scope.ProjectID, BranchId: assignment.BranchID, AfterSequence: first.NextSequence, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Calls) != 1 || string(second.Calls[0].Result.Payload) != "second" || second.NextSequence <= first.NextSequence {
		t.Fatalf("cursor skipped or repeated registered call: %v", second)
	}
	final, err := client.Calls(ctx, &publicv1.CallsRequest{ProjectId: scope.ProjectID, BranchId: assignment.BranchID, AfterSequence: second.NextSequence, Limit: 1})
	if err != nil || len(final.GetCalls()) != 0 {
		t.Fatalf("final trace page: %v %v", final, err)
	}
	if _, err = client.Calls(ctx, &publicv1.CallsRequest{ProjectId: scope.ProjectID, BranchId: assignment.BranchID, Limit: 1001}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unbounded trace page accepted: %v", err)
	}
	steps, err := client.Steps(ctx, &publicv1.StepsRequest{ProjectId: scope.ProjectID, BranchId: assignment.BranchID, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps.Steps) != 1 || !steps.Steps[0].Complete || steps.Steps[0].StateDigest != state || steps.NextStep != 1 {
		t.Fatalf("committed step absent: %v", steps)
	}
	if _, err = client.Action(ctx, &publicv1.ActionRequest{ProjectId: scope.ProjectID, BranchId: assignment.BranchID, Action: "suspend", IdempotencyKey: "public-suspend"}); err != nil {
		t.Fatal(err)
	}
	controller := cp.Controller{Store: store, Backend: backend, WorkerID: "public-fork"}
	tickController(t, ctx, &controller)
	checkpoints, err := client.ListResources(ctx, &publicv1.ListResourcesRequest{ProjectId: scope.ProjectID, Kind: "Checkpoint"})
	if err != nil || len(checkpoints.GetResources()) != 1 {
		t.Fatalf("public checkpoint: %v %v", checkpoints, err)
	}
	checkpoint := checkpoints.Resources[0]
	model := &publicv1.Ref{Id: assignment.SelectedModel.Metadata.ID, Digest: assignment.SelectedModel.Metadata.Digest}
	request := &publicv1.ForkRequest{ProjectId: scope.ProjectID, CheckpointId: checkpoint.Metadata.Id, Models: []*publicv1.Ref{model, model}, IdempotencyKey: "public-fork"}
	fork, err := client.Fork(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := client.Fork(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(fork.Children) != 2 || fork.Children[0].Metadata.Id == fork.Children[1].Metadata.Id || replayed.Children[0].Metadata.Id != fork.Children[0].Metadata.Id {
		t.Fatalf("duplicate or unstable fork children: %v", fork)
	}
	for _, child := range fork.Children {
		spec := child.GetBranchSpec()
		if spec.ParentBranchId != assignment.BranchID || spec.ParentCheckpointId != checkpoint.Metadata.Id || spec.ForkOperationId != fork.Operation.Metadata.Id {
			t.Fatalf("public lineage lost: %v", spec)
		}
	}
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.GRPC().Serve(tcp)
	process := exec.CommandContext(ctx, filepath.Join(root, ".venv/bin/python"), "test/contract/public_agent_scenario.py", tcp.Addr().String(), scope.ProjectID, assignment.BranchID, checkpoint.Metadata.Id, model.Id, model.Digest, fork.Operation.Metadata.Id)
	process.Dir = root
	process.Env = append(os.Environ(), "HYPERCALL_API_KEY="+key, "HYPERCALL_TEST_CLI="+binary)
	if output, err := process.CombinedOutput(); err != nil {
		t.Fatalf("public trajectory clients: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
}
