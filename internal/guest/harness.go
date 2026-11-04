package guest

import (
	"context"
	"regexp"
	"sync"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
	"github.com/gduchidze/hypercall/internal/harness"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var requestIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._/-]{0,127}$`)

type pendingAgentRequest struct {
	request *harnessv1.AgentRequest
	hash    string
	result  *harnessv1.AgentResult
}
type harnessState struct {
	mu                   sync.Mutex
	session              *harnessv1.HarnessSession
	localAck, durableAck bool
	changed              chan struct{}
	pending, last        *pendingAgentRequest
	lastCommit, lastStep int64
	lastStateDigest      string
	barrier              *harnessv1.AgentBarrier
	sealed               bool
	finalized            bool
	captureMarker        *harnessv1.SemanticBarrier
	sealedBarrier        *harnessv1.AgentBarrier
}

func newHarnessState(s *harnessv1.HarnessSession) *harnessState {
	if s == nil {
		return nil
	}
	return &harnessState{session: proto.Clone(s).(*harnessv1.HarnessSession), changed: make(chan struct{})}
}
func (h *harnessState) notify() { close(h.changed); h.changed = make(chan struct{}) }
func (s *Server) harnessState() (*harnessState, error) {
	if s.harness == nil {
		return nil, status.Error(codes.FailedPrecondition, "cooperative harness session is not configured")
	}
	return s.harness, nil
}
func (s *Server) GetSession(ctx context.Context, _ *harnessv1.GetSessionRequest) (*harnessv1.HarnessSession, error) {
	h, e := s.harnessState()
	if e != nil {
		return nil, e
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return proto.Clone(h.session).(*harnessv1.HarnessSession), nil
}
func (s *Server) AcknowledgeResume(ctx context.Context, r *harnessv1.SessionAckRequest) (*harnessv1.SessionAck, error) {
	h, e := s.harnessState()
	if e != nil {
		return nil, e
	}
	h.mu.Lock()
	if r == nil || r.SessionHash != h.session.SessionHash {
		h.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "harness acknowledgement configuration mismatch")
	}
	if !h.localAck {
		h.localAck = true
		h.notify()
	}
	for {
		if r.SessionHash != h.session.SessionHash {
			h.mu.Unlock()
			return nil, status.Error(codes.Aborted, "harness session replaced")
		}
		if h.durableAck {
			h.mu.Unlock()
			return &harnessv1.SessionAck{SessionHash: r.SessionHash}, nil
		}
		changed := h.changed
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		case <-changed:
		}
		h.mu.Lock()
	}
}
func (s *Server) Submit(ctx context.Context, r *harnessv1.AgentRequest) (*harnessv1.AgentResult, error) {
	h, e := s.harnessState()
	if e != nil {
		return nil, e
	}
	if r == nil || !requestIdentifier.MatchString(r.Id) || r.Step < 1 || r.Step > 100000 || r.Position < 1 || r.Position > 1024 || len(r.Name) > 128 || len(r.Payload) > 64<<10 || harness.Digest(r.Payload) != r.PayloadDigest || (r.Kind != "inference" && r.Kind != "tool" && r.Kind != "step_commit" && r.Kind != "finalize") {
		return nil, status.Error(codes.InvalidArgument, "invalid bounded sequential harness request")
	}
	hash, e := harness.MessageHash(r)
	if e != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid harness request")
	}
	h.mu.Lock()
	if !h.durableAck || h.barrier != nil || h.finalized || r.SessionHash != h.session.SessionHash {
		h.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "harness session lacks durable acknowledgement")
	}
	if h.last != nil && h.last.request.Id == r.Id {
		last := h.last
		if last.hash != hash {
			h.mu.Unlock()
			return nil, status.Error(codes.Aborted, "request ID reused with different input")
		}
		result := proto.Clone(last.result).(*harnessv1.AgentResult)
		h.mu.Unlock()
		return result, nil
	}
	if h.pending != nil && (h.pending.request.Id != r.Id || h.pending.hash != hash) {
		h.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "background or overlapping harness actions are unsupported")
	}
	if h.pending == nil {
		if h.last != nil && (r.Step < h.last.request.Step || r.Step == h.last.request.Step && r.Position <= h.last.request.Position) {
			h.mu.Unlock()
			return nil, status.Error(codes.FailedPrecondition, "registered actions cannot move backward")
		}
		h.pending = &pendingAgentRequest{request: proto.Clone(r).(*harnessv1.AgentRequest), hash: hash}
		h.notify()
	}
	pending := h.pending
	for {
		if pending.result != nil {
			result := proto.Clone(pending.result).(*harnessv1.AgentResult)
			if h.pending == pending {
				h.last = pending
				h.pending = nil
				h.notify()
			}
			h.mu.Unlock()
			return result, nil
		}
		changed := h.changed
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		case <-changed:
		}
		h.mu.Lock()
	}
}
func (s *Server) PollAgent(ctx context.Context, _ *guestv1.PollAgentRequest) (*harnessv1.AgentPoll, error) {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	if e := s.authenticate(ctx); e != nil {
		return nil, e
	}
	if s.captureGate {
		return nil, status.Error(codes.FailedPrecondition, "capture session is not rebound")
	}
	h, e := s.harnessState()
	if e != nil {
		return nil, e
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.localAck && !h.durableAck {
		return &harnessv1.AgentPoll{Pending: &harnessv1.AgentPoll_SessionAck{SessionAck: h.session.SessionHash}}, nil
	}
	if h.pending != nil && h.pending.result == nil {
		return &harnessv1.AgentPoll{Pending: &harnessv1.AgentPoll_Request{Request: proto.Clone(h.pending.request).(*harnessv1.AgentRequest)}}, nil
	}
	if h.barrier != nil && !h.sealed {
		return &harnessv1.AgentPoll{Pending: &harnessv1.AgentPoll_Barrier{Barrier: proto.Clone(h.barrier).(*harnessv1.AgentBarrier)}}, nil
	}
	return &harnessv1.AgentPoll{}, nil
}
func (s *Server) DeliverAgent(ctx context.Context, r *harnessv1.AgentDelivery) (*guestv1.DeliverAgentResponse, error) {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	if e := s.authenticate(ctx); e != nil {
		return nil, e
	}
	if s.captureGate {
		return nil, status.Error(codes.FailedPrecondition, "capture session is not rebound")
	}
	h, e := s.harnessState()
	if e != nil {
		return nil, e
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if r == nil || r.SessionHash != h.session.SessionHash {
		return nil, status.Error(codes.Aborted, "delivery targets stale harness session")
	}
	if hash := r.GetSessionAck(); hash != "" {
		if hash != h.session.SessionHash || !h.localAck || r.RequestHash != "" {
			return nil, status.Error(codes.FailedPrecondition, "local session acknowledgement required")
		}
		h.durableAck = true
		if h.barrier != nil && h.barrier.SessionHash != h.session.SessionHash {
			h.barrier = nil
			h.sealed = false
			h.sealedBarrier = nil
		}
		h.notify()
		return &guestv1.DeliverAgentResponse{}, nil
	}
	if result := r.GetResult(); result != nil {
		p := h.pending
		if p == nil && h.last != nil && h.last.request.Id == result.Id {
			p = h.last
		}
		if p == nil || p.request.Id != result.Id || p.hash != r.RequestHash || result.EventSequence < 1 || len(result.Payload) > 64<<10 {
			return nil, status.Error(codes.Aborted, "delivery request identity/hash mismatch")
		}
		if p.result != nil {
			old, next := proto.Clone(p.result).(*harnessv1.AgentResult), proto.Clone(result).(*harnessv1.AgentResult)
			old.Replayed = false
			next.Replayed = false
			if !proto.Equal(old, next) {
				return nil, status.Error(codes.Aborted, "conflicting committed result")
			}
			return &guestv1.DeliverAgentResponse{}, nil
		}
		p.result = proto.Clone(result).(*harnessv1.AgentResult)
		if p.request.Kind == "step_commit" && result.State == "committed" {
			h.lastCommit = result.EventSequence
			h.lastStep = p.request.Step
			h.lastStateDigest = p.request.PayloadDigest
		}
		if p.request.Kind == "finalize" && result.State == "finalized" {
			h.finalized = true
		}
		h.notify()
		return &guestv1.DeliverAgentResponse{}, nil
	}
	if b := r.GetSealedBarrier(); b != nil {
		hash, _ := harness.MessageHash(h.barrier)
		if h.barrier == nil || b.Step != h.barrier.Step || b.StateDigest != h.barrier.StateDigest || b.ProtocolVersion != h.barrier.ProtocolVersion || b.SessionHash != h.barrier.SessionHash || b.EventSequence < h.barrier.EventSequence || hash != r.RequestHash || h.pending != nil {
			return nil, status.Error(codes.Aborted, "sealed barrier does not match pending committed marker")
		}
		if h.sealedBarrier != nil && !proto.Equal(h.sealedBarrier, b) {
			return nil, status.Error(codes.Aborted, "sealed barrier cursor changed")
		}
		h.sealedBarrier = proto.Clone(b).(*harnessv1.AgentBarrier)
		h.sealed = true
		h.notify()
		return &guestv1.DeliverAgentResponse{}, nil
	}
	return nil, status.Error(codes.InvalidArgument, "one supported delivery required")
}

func (s *Server) AwaitResume(ctx context.Context, b *harnessv1.AgentBarrier) (*harnessv1.HarnessSession, error) {
	h, e := s.harnessState()
	if e != nil {
		return nil, e
	}
	h.mu.Lock()
	if b != nil && h.barrier != nil && proto.Equal(h.barrier, b) && h.session.SessionHash != b.SessionHash {
		session := proto.Clone(h.session).(*harnessv1.HarnessSession)
		h.mu.Unlock()
		return session, nil
	}
	if b == nil || b.ProtocolVersion != harness.Protocol || !harness.ValidDigest(b.StateDigest) || b.SessionHash != h.session.SessionHash || !h.durableAck || h.pending != nil || h.finalized || h.last == nil || h.last.request.Kind != "step_commit" || h.last.result.State != "committed" || b.Step != h.lastStep || b.EventSequence != h.lastCommit || b.StateDigest != h.lastStateDigest {
		h.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "barrier requires exact acknowledged committed state with no pending actions")
	}
	if h.barrier != nil && !proto.Equal(h.barrier, b) {
		h.mu.Unlock()
		return nil, status.Error(codes.Aborted, "barrier marker changed")
	}
	if h.barrier == nil {
		h.barrier = proto.Clone(b).(*harnessv1.AgentBarrier)
		h.notify()
	}
	for {
		if h.session.SessionHash != b.SessionHash {
			session := proto.Clone(h.session).(*harnessv1.HarnessSession)
			h.mu.Unlock()
			return session, nil
		}
		changed := h.changed
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		case <-changed:
		}
		h.mu.Lock()
	}
}
func (h *harnessState) semanticBarrier() (*harnessv1.SemanticBarrier, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.barrier
	if b == nil || !h.sealed || h.sealedBarrier == nil || h.pending != nil || !h.durableAck || h.last == nil || h.last.request.Kind != "step_commit" || h.last.result.State != "committed" || b.Step != h.lastStep || b.EventSequence != h.lastCommit || b.StateDigest != h.lastStateDigest || b.SessionHash != h.session.SessionHash {
		return nil, status.Error(codes.FailedPrecondition, "a durably sealed cooperative barrier is required")
	}
	marker := &harnessv1.SemanticBarrier{ProtocolVersion: b.ProtocolVersion, Step: b.Step, EventSequence: h.sealedBarrier.EventSequence, StateDigest: b.StateDigest, SourceSessionHash: b.SessionHash, SourceBranchId: h.session.BranchId, SourceExecutionId: h.session.ExecutionId, SourceEpoch: h.session.Epoch}
	h.captureMarker = proto.Clone(marker).(*harnessv1.SemanticBarrier)
	return marker, nil
}

func (h *harnessState) proposeClone(next *harnessv1.HarnessSession) error {
	if e := harness.ValidateSession(next); e != nil {
		return status.Error(codes.FailedPrecondition, "invalid fresh harness session")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.captureMarker == nil || !proto.Equal(next.InheritedBarrier, h.captureMarker) {
		return status.Error(codes.FailedPrecondition, "fresh session inherited barrier mismatch")
	}
	if h.session.SessionHash == next.SessionHash {
		return nil
	}
	if h.session.SessionHash != h.captureMarker.SourceSessionHash {
		return status.Error(codes.Aborted, "another fresh session is already proposed")
	}
	h.session = proto.Clone(next).(*harnessv1.HarnessSession)
	h.localAck = false
	h.durableAck = false
	h.pending = nil
	h.last = nil
	h.notify()
	return nil
}
func (h *harnessState) waitLocalAck(ctx context.Context, hash string) error {
	h.mu.Lock()
	for {
		if h.session.SessionHash != hash {
			h.mu.Unlock()
			return status.Error(codes.Aborted, "fresh session replaced")
		}
		if h.localAck {
			h.mu.Unlock()
			return nil
		}
		changed := h.changed
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-changed:
		}
		h.mu.Lock()
	}
}
