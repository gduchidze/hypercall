package guest_test

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
	"github.com/gduchidze/hypercall/internal/guest"
	"github.com/gduchidze/hypercall/internal/harness"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

func harnessSession(t *testing.T) *harnessv1.HarnessSession {
	t.Helper()
	h := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s, e := harness.SealSession(&harnessv1.HarnessSession{TenantId: "tenant", ProjectId: "project", BranchId: "branch", SandboxId: "sandbox", ExecutionId: "execution", Epoch: 1, CallNamespace: "calls", RouteDigest: h, ModelDigest: h, ProtocolVersion: harness.Protocol})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func startHarnessGuest(t *testing.T) (guestv1.GuestClient, harnessv1.HarnessClient, *harnessv1.HarnessSession) {
	t.Helper()
	session := harnessSession(t)
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	svc, e := guest.New(guest.Config{Workspace: t.TempDir(), Identity: &guestv1.AllocationIdentity{TenantId: session.TenantId, ProjectId: session.ProjectId, SandboxId: session.SandboxId, ExecutionId: session.ExecutionId, Epoch: uint64(session.Epoch)}, Credential: credential, ExpiresAt: time.Now().Add(time.Minute), TrustedLocal: true, ExecHelper: exe, HarnessSession: session})
	if e != nil {
		t.Fatal(e)
	}
	private := grpc.NewServer()
	guestv1.RegisterGuestServer(private, svc)
	local := grpc.NewServer()
	harnessv1.RegisterHarnessServer(local, svc)
	dial := func(s *grpc.Server) *grpc.ClientConn {
		lis := bufconn.Listen(2 << 20)
		go s.Serve(lis)
		conn, e := grpc.NewClient("passthrough:///harness-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { conn.Close(); s.Stop(); lis.Close() })
		return conn
	}
	p, l := dial(private), dial(local)
	t.Cleanup(func() { svc.Close() })
	return guestv1.NewGuestClient(p), harnessv1.NewHarnessClient(l), session
}
func TestHarnessCallRequiresDurableSessionAckAndCommittedDelivery(t *testing.T) {
	private, local, session := startHarnessGuest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ackDone := make(chan error, 1)
	go func() {
		_, e := local.AcknowledgeResume(ctx, &harnessv1.SessionAckRequest{SessionHash: session.SessionHash})
		ackDone <- e
	}()
	for {
		p, e := private.PollAgent(authorized(t), &guestv1.PollAgentRequest{})
		if e != nil {
			t.Fatal(e)
		}
		if p.GetSessionAck() == session.SessionHash {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case e := <-ackDone:
		t.Fatalf("local ack opened gate before durableCPack: %v", e)
	default:
	}
	if _, e := private.DeliverAgent(authorized(t), &harnessv1.AgentDelivery{SessionHash: session.SessionHash, Delivery: &harnessv1.AgentDelivery_SessionAck{SessionAck: session.SessionHash}}); e != nil {
		t.Fatal(e)
	}
	if e := <-ackDone; e != nil {
		t.Fatal(e)
	}
	request := &harnessv1.AgentRequest{Id: "calls-1", Kind: "inference", Name: "model", Step: 1, Position: 1, Payload: []byte("hello"), PayloadDigest: harness.Digest([]byte("hello")), SessionHash: session.SessionHash}
	resultDone := make(chan *harnessv1.AgentResult, 1)
	errDone := make(chan error, 1)
	go func() { r, e := local.Submit(ctx, request); resultDone <- r; errDone <- e }()
	for {
		p, e := private.PollAgent(authorized(t), &guestv1.PollAgentRequest{})
		if e != nil {
			t.Fatal(e)
		}
		if p.GetRequest() != nil {
			if p.GetRequest().Id != request.Id {
				t.Fatal("mailbox returned wrong action")
			}
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	hash, e := harness.MessageHash(request)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = private.DeliverAgent(authorized(t), &harnessv1.AgentDelivery{SessionHash: session.SessionHash, RequestHash: hash, Delivery: &harnessv1.AgentDelivery_Result{Result: &harnessv1.AgentResult{Id: request.Id, State: "completed", Payload: []byte("fixture-result"), EventSequence: 5}}}); e != nil {
		t.Fatal(e)
	}
	r := <-resultDone
	if e = <-errDone; e != nil || string(r.GetPayload()) != "fixture-result" || r.GetEventSequence() != 5 {
		t.Fatalf("committed result %v: %v", r, e)
	}
}

func TestNativeCaptureRequiresSealedCommittedCooperativeBarrier(t *testing.T) {
	private, local, session := startHarnessGuest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	go local.AcknowledgeResume(ctx, &harnessv1.SessionAckRequest{SessionHash: session.SessionHash})
	for {
		p, e := private.PollAgent(authorized(t), &guestv1.PollAgentRequest{})
		if e != nil {
			t.Fatal(e)
		}
		if p.GetSessionAck() != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, e := private.DeliverAgent(authorized(t), &harnessv1.AgentDelivery{SessionHash: session.SessionHash, Delivery: &harnessv1.AgentDelivery_SessionAck{SessionAck: session.SessionHash}}); e != nil {
		t.Fatal(e)
	}
	request := &harnessv1.AgentRequest{Id: "commit-1", Kind: "step_commit", Step: 1, Position: 1, Payload: []byte("agent-state"), PayloadDigest: harness.Digest([]byte("agent-state")), SessionHash: session.SessionHash}
	result := make(chan error, 1)
	go func() { _, e := local.Submit(ctx, request); result <- e }()
	for {
		p, e := private.PollAgent(authorized(t), &guestv1.PollAgentRequest{})
		if e != nil {
			t.Fatal(e)
		}
		if p.GetRequest() != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	hash, _ := harness.MessageHash(request)
	if _, e := private.DeliverAgent(authorized(t), &harnessv1.AgentDelivery{SessionHash: session.SessionHash, RequestHash: hash, Delivery: &harnessv1.AgentDelivery_Result{Result: &harnessv1.AgentResult{Id: request.Id, State: "committed", EventSequence: 7}}}); e != nil {
		t.Fatal(e)
	}
	if e := <-result; e != nil {
		t.Fatal(e)
	}
	marker := &harnessv1.AgentBarrier{Step: 1, EventSequence: 7, StateDigest: harness.Digest([]byte("agent-state")), ProtocolVersion: harness.Protocol, SessionHash: session.SessionHash}
	go local.AwaitResume(ctx, marker)
	for {
		p, e := private.PollAgent(authorized(t), &guestv1.PollAgentRequest{})
		if e != nil {
			t.Fatal(e)
		}
		if p.GetBarrier() != nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	if _, e := private.PrepareCapture(authorized(t), &guestv1.PrepareCaptureRequest{}); e == nil {
		t.Fatal("unsealed agent barrier was captured")
	}
	hash, _ = harness.MessageHash(marker)
	sealed := proto.Clone(marker).(*harnessv1.AgentBarrier)
	sealed.EventSequence = 8
	if _, e := private.DeliverAgent(authorized(t), &harnessv1.AgentDelivery{SessionHash: session.SessionHash, RequestHash: hash, Delivery: &harnessv1.AgentDelivery_SealedBarrier{SealedBarrier: sealed}}); e != nil {
		t.Fatal(e)
	}
	captured, e := private.PrepareCapture(authorized(t), &guestv1.PrepareCaptureRequest{})
	if e != nil || captured.GetSemanticBarrier().GetEventSequence() != 8 || captured.GetSemanticBarrier().GetSourceBranchId() != session.BranchId {
		t.Fatalf("capture lacks committed semantic marker: %v %v", captured, e)
	}
}

func acknowledgeHarness(t *testing.T, p guestv1.GuestClient, l harnessv1.HarnessClient, s *harnessv1.HarnessSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, e := l.AcknowledgeResume(ctx, &harnessv1.SessionAckRequest{SessionHash: s.SessionHash})
		done <- e
	}()
	for {
		v, e := p.PollAgent(authorized(t), &guestv1.PollAgentRequest{})
		if e != nil {
			t.Fatal(e)
		}
		if v.GetSessionAck() != "" {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	if _, e := p.DeliverAgent(authorized(t), &harnessv1.AgentDelivery{SessionHash: s.SessionHash, Delivery: &harnessv1.AgentDelivery_SessionAck{SessionAck: s.SessionHash}}); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}
func completeHarnessAction(t *testing.T, p guestv1.GuestClient, l harnessv1.HarnessClient, r *harnessv1.AgentRequest, state string, sequence int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := l.Submit(ctx, r); done <- e }()
	for {
		v, e := p.PollAgent(authorized(t), &guestv1.PollAgentRequest{})
		if e != nil {
			t.Fatal(e)
		}
		if v.GetRequest() != nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	hash, _ := harness.MessageHash(r)
	if _, e := p.DeliverAgent(authorized(t), &harnessv1.AgentDelivery{SessionHash: r.SessionHash, RequestHash: hash, Delivery: &harnessv1.AgentDelivery_Result{Result: &harnessv1.AgentResult{Id: r.Id, State: state, EventSequence: sequence}}}); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}
func TestHarnessRejectsEarlierBarrierAfterObservingLaterStepAction(t *testing.T) {
	p, l, s := startHarnessGuest(t)
	acknowledgeHarness(t, p, l, s)
	payload := []byte("state")
	completeHarnessAction(t, p, l, &harnessv1.AgentRequest{Id: "commit-1", Kind: "step_commit", Step: 1, Position: 1, Payload: payload, PayloadDigest: harness.Digest(payload), SessionHash: s.SessionHash}, "committed", 7)
	completeHarnessAction(t, p, l, &harnessv1.AgentRequest{Id: "model-2", Kind: "inference", Step: 2, Position: 1, Name: "model", PayloadDigest: harness.Digest(nil), SessionHash: s.SessionHash}, "completed", 9)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, e := l.AwaitResume(ctx, &harnessv1.AgentBarrier{Step: 1, EventSequence: 7, StateDigest: harness.Digest(payload), SessionHash: s.SessionHash, ProtocolVersion: harness.Protocol}); status.Code(e) != codes.FailedPrecondition {
		t.Fatalf("past step accepted after later observation: %v", e)
	}
}
