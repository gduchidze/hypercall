package sandbox

import (
	"context"
	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
	"github.com/gduchidze/hypercall/internal/snapshot"
)

// AgentBackend transports a bounded guest mailbox; durable gateway authority
// stays in the controller and no provider credentials enter the guest.
type AgentBackend interface {
	StartAgent(context.Context, Allocation) (*guestv1.GetInfoResponse, error)
	InspectAgent(context.Context, Allocation) (*guestv1.GetInfoResponse, error)
	PollAgent(context.Context, Allocation) (*harnessv1.AgentPoll, error)
	DeliverAgent(context.Context, Allocation, *harnessv1.AgentDelivery) error
}

func semanticFromWire(b *harnessv1.SemanticBarrier) *snapshot.SemanticBarrier {
	if b == nil {
		return nil
	}
	return &snapshot.SemanticBarrier{ProtocolVersion: b.ProtocolVersion, Step: b.Step, EventSequence: b.EventSequence, StateDigest: b.StateDigest, SourceSessionHash: b.SourceSessionHash, SourceBranchID: b.SourceBranchId, SourceExecutionID: b.SourceExecutionId, SourceEpoch: b.SourceEpoch}
}
