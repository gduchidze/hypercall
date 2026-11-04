package hostapi

import (
	"context"
	"errors"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
	"github.com/gduchidze/hypercall/internal/controlplane"
	"github.com/gduchidze/hypercall/internal/sandbox"
)

func (b *Bridge) startAgentLocked(ctx context.Context, a controlplane.Assignment, r record) (controlplane.Observation, error) {
	native, ok := b.runtime.(sandbox.AgentBackend)
	if !ok || a.HarnessSession == nil {
		return controlplane.Observation{}, fail("failed_precondition", "cooperative native backend and sealed session required")
	}
	var info *guestv1.GetInfoResponse
	var e error
	if r.Stage == "agent_starting" {
		info, e = native.StartAgent(ctx, allocation(a))
	} else {
		info, e = native.InspectAgent(ctx, allocation(a))
	}
	if e != nil {
		return controlplane.Observation{}, runtimeError(e)
	}
	if info.HarnessSessionHash != a.HarnessSession.SessionHash || !info.AgentStarted {
		return controlplane.Observation{}, fail("failed_precondition", "guest cooperative session readiness mismatch")
	}
	r.Stage = "agent_running"
	if e = b.save(a, r); e != nil {
		return controlplane.Observation{}, e
	}
	return controlplane.Observation{State: "running"}, nil
}
func (b *Bridge) observeAgentLocked(ctx context.Context, a controlplane.Assignment, r record) (controlplane.Observation, error) {
	if r.Stage == "capturing" || r.Stage == "captured" {
		return b.finishSuspendLocked(ctx, a, r)
	}
	if r.Stage == "suspended" {
		return controlplane.Observation{State: "suspended", Checkpoint: r.Checkpoint}, nil
	}
	if r.Stage == "restoring" {
		return controlplane.Observation{}, fail("unavailable", "cooperative restore needs its durable Resume request")
	}
	if r.Stage == "failed" {
		return controlplane.Observation{State: "failed"}, nil
	}
	o, e := b.runtime.Inspect(ctx, allocation(a))
	if e != nil || !time.Now().Before(r.ExpiresAt) || o.State != sandbox.StateRunning {
		if e != nil && !errors.Is(e, sandbox.ErrNotFound) {
			return controlplane.Observation{}, runtimeError(e)
		}
		if e = b.runtime.Stop(ctx, allocation(a)); e != nil && !errors.Is(e, sandbox.ErrNotFound) {
			return controlplane.Observation{}, runtimeError(e)
		}
		r.Stage = "failed"
		if e = b.save(a, r); e != nil {
			return controlplane.Observation{}, e
		}
		return controlplane.Observation{State: "failed"}, nil
	}
	return b.startAgentLocked(ctx, a, r)
}
func (b *Bridge) AgentPoll(ctx context.Context, a controlplane.Assignment) (controlplane.AgentPoll, error) {
	c, cancel, e := b.proxy(ctx, a)
	if e != nil {
		return controlplane.AgentPoll{}, e
	}
	defer cancel()
	native, ok := b.runtime.(sandbox.AgentBackend)
	if !ok || a.HarnessSession == nil {
		return controlplane.AgentPoll{}, fail("failed_precondition", "cooperative native backend required")
	}
	p, e := native.PollAgent(c, allocation(a))
	if e != nil {
		return controlplane.AgentPoll{}, runtimeError(e)
	}
	return pollFromWire(p), nil
}
func (b *Bridge) AgentDeliver(ctx context.Context, a controlplane.Assignment, d controlplane.AgentDelivery) error {
	c, cancel, e := b.proxy(ctx, a)
	if e != nil {
		return e
	}
	defer cancel()
	native, ok := b.runtime.(sandbox.AgentBackend)
	if !ok || a.HarnessSession == nil {
		return fail("failed_precondition", "cooperative native backend required")
	}
	return runtimeError(native.DeliverAgent(c, allocation(a), deliveryWire(d)))
}
func pollFromWire(p *harnessv1.AgentPoll) controlplane.AgentPoll {
	if p == nil {
		return controlplane.AgentPoll{}
	}
	out := controlplane.AgentPoll{SessionAck: p.GetSessionAck()}
	if r := p.GetRequest(); r != nil {
		out.Request = &controlplane.AgentRequest{ID: r.Id, Kind: r.Kind, Step: r.Step, Position: r.Position, Name: r.Name, Payload: r.Payload, PayloadDigest: r.PayloadDigest, SessionHash: r.SessionHash}
	}
	if b := p.GetBarrier(); b != nil {
		out.Barrier = barrierFromWire(b)
	}
	return out
}
func pollWire(p controlplane.AgentPoll) *harnessv1.AgentPoll {
	if p.SessionAck != "" {
		return &harnessv1.AgentPoll{Pending: &harnessv1.AgentPoll_SessionAck{SessionAck: p.SessionAck}}
	}
	if r := p.Request; r != nil {
		return &harnessv1.AgentPoll{Pending: &harnessv1.AgentPoll_Request{Request: &harnessv1.AgentRequest{Id: r.ID, Kind: r.Kind, Step: r.Step, Position: r.Position, Name: r.Name, Payload: r.Payload, PayloadDigest: r.PayloadDigest, SessionHash: r.SessionHash}}}
	}
	if p.Barrier != nil {
		return &harnessv1.AgentPoll{Pending: &harnessv1.AgentPoll_Barrier{Barrier: barrierWire(p.Barrier)}}
	}
	return &harnessv1.AgentPoll{}
}
func barrierWire(b *controlplane.AgentBarrier) *harnessv1.AgentBarrier {
	if b == nil {
		return nil
	}
	return &harnessv1.AgentBarrier{Step: b.Step, EventSequence: b.EventSequence, StateDigest: b.StateDigest, ProtocolVersion: b.ProtocolVersion, SessionHash: b.SessionHash}
}
func barrierFromWire(b *harnessv1.AgentBarrier) *controlplane.AgentBarrier {
	if b == nil {
		return nil
	}
	return &controlplane.AgentBarrier{Step: b.Step, EventSequence: b.EventSequence, StateDigest: b.StateDigest, ProtocolVersion: b.ProtocolVersion, SessionHash: b.SessionHash}
}
func deliveryWire(d controlplane.AgentDelivery) *harnessv1.AgentDelivery {
	out := &harnessv1.AgentDelivery{SessionHash: d.SessionHash, RequestHash: d.RequestHash}
	if d.SessionAck != "" {
		out.Delivery = &harnessv1.AgentDelivery_SessionAck{SessionAck: d.SessionAck}
	} else if r := d.Result; r != nil {
		out.Delivery = &harnessv1.AgentDelivery_Result{Result: &harnessv1.AgentResult{Id: r.ID, State: r.State, Payload: r.Payload, EventSequence: r.EventSequence, ErrorCode: r.ErrorCode, Replayed: r.Replayed}}
	} else if d.SealedBarrier != nil {
		out.Delivery = &harnessv1.AgentDelivery_SealedBarrier{SealedBarrier: barrierWire(d.SealedBarrier)}
	}
	return out
}
func deliveryFromWire(d *harnessv1.AgentDelivery) controlplane.AgentDelivery {
	if d == nil {
		return controlplane.AgentDelivery{}
	}
	out := controlplane.AgentDelivery{SessionHash: d.SessionHash, RequestHash: d.RequestHash, SessionAck: d.GetSessionAck(), SealedBarrier: barrierFromWire(d.GetSealedBarrier())}
	if r := d.GetResult(); r != nil {
		out.Result = &controlplane.AgentResult{ID: r.Id, State: r.State, Payload: r.Payload, EventSequence: r.EventSequence, ErrorCode: r.ErrorCode, Replayed: r.Replayed}
	}
	return out
}
