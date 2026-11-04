//go:build linux

package sandbox

import (
	"context"
	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
)

func (b *Backend) StartAgent(ctx context.Context, a Allocation) (*guestv1.GetInfoResponse, error) {
	h, e := b.guest(a)
	if e != nil {
		return nil, e
	}
	defer h.rpcMu.RUnlock()
	return h.client.StartAgent(authContext(ctx, h.credential), &guestv1.StartAgentRequest{})
}
func (b *Backend) PollAgent(ctx context.Context, a Allocation) (*harnessv1.AgentPoll, error) {
	h, e := b.guest(a)
	if e != nil {
		return nil, e
	}
	defer h.rpcMu.RUnlock()
	return h.client.PollAgent(authContext(ctx, h.credential), &guestv1.PollAgentRequest{})
}
func (b *Backend) DeliverAgent(ctx context.Context, a Allocation, d *harnessv1.AgentDelivery) error {
	h, e := b.guest(a)
	if e != nil {
		return e
	}
	defer h.rpcMu.RUnlock()
	_, e = h.client.DeliverAgent(authContext(ctx, h.credential), d)
	return e
}

func (b *Backend) InspectAgent(ctx context.Context, a Allocation) (*guestv1.GetInfoResponse, error) {
	h, e := b.guest(a)
	if e != nil {
		return nil, e
	}
	defer h.rpcMu.RUnlock()
	return h.client.GetInfo(authContext(ctx, h.credential), &guestv1.GetInfoRequest{})
}
