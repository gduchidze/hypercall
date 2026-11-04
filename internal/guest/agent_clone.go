package guest

import (
	"context"
	"encoding/hex"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"github.com/gduchidze/hypercall/internal/harness"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (s *Server) rebindAgentClone(ctx context.Context, r *guestv1.RebindCloneRequest) (*guestv1.GetInfoResponse, error) {
	s.opMu.Lock()
	if !s.captureGate {
		s.opMu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "clone is not capture-gated")
	}
	session, e := readCloneSession()
	if e != nil {
		s.opMu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "fresh cooperative session disk unavailable")
	}
	if e = session.ValidateClone(s.identity, ctx); e != nil {
		s.opMu.Unlock()
		return nil, e
	}
	next := session.HarnessSession
	if session.CredentialHash() == s.credentialHash || next == nil || harness.ValidateSession(next) != nil || next.TenantId != session.Identity.TenantId || next.ProjectId != session.Identity.ProjectId || next.SandboxId != session.Identity.SandboxId || next.ExecutionId != session.Identity.ExecutionId || next.Epoch != int64(session.Identity.Epoch) {
		s.opMu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "fresh cooperative identity/configuration required")
	}
	if e = refreshGuestClock(r.HostUnixNano, session.ExpiresAt); e != nil {
		s.opMu.Unlock()
		return nil, e
	}
	entropy, e := hex.DecodeString(session.Entropy)
	if e != nil || len(entropy) != 32 {
		s.opMu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "fresh cooperative RNG entropy required")
	}
	if e = reseedKernel(entropy); e != nil {
		s.opMu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "cooperative RNG renewal failed")
	}
	if e = s.harness.proposeClone(next); e != nil {
		s.opMu.Unlock()
		return nil, e
	}
	s.opMu.Unlock()
	// The captured Python AwaitResume RPC receives the fresh configuration. It
	// closes/recreates clients and records LOCAL acknowledgement. Do not hold
	// opMu while it does so, and never wait here for the controller's durable ack.
	if e = s.harness.waitLocalAck(ctx, next.SessionHash); e != nil {
		return nil, e
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if !s.captureGate {
		if proto.Equal(s.identity, session.Identity) && s.credentialHash == session.CredentialHash() {
			return s.infoLocked(), nil
		}
		return nil, status.Error(codes.Aborted, "clone session changed")
	}
	s.harness.mu.Lock()
	valid := s.harness.localAck && s.harness.session.SessionHash == next.SessionHash
	s.harness.mu.Unlock()
	if !valid {
		return nil, status.Error(codes.Aborted, "local cooperative acknowledgement changed")
	}
	s.identity = proto.Clone(session.Identity).(*guestv1.AllocationIdentity)
	s.credentialHash = session.CredentialHash()
	s.expiresAt = session.ExpiresAt
	s.captureGate = false
	// Only authenticated host control reopens here. Harness.Submit and Python's
	// Ack RPC stay blocked until the controller commits and delivers this hash.
	return s.infoLocked(), nil
}
