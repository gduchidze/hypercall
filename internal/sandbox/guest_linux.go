//go:build linux

package sandbox

import (
	"context"
	"errors"
	"io"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
)

func (b *Backend) guest(a Allocation) (*instance, error) {
	h, err := b.get(a)
	if err != nil {
		return nil, err
	}
	h.rpcMu.RLock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cleaned || h.record.Observation.State != StateRunning || h.lifetime.Err() != nil {
		h.rpcMu.RUnlock()
		return nil, ErrUnavailable
	}
	return h, nil
}
func (b *Backend) Exec(ctx context.Context, a Allocation, req *guestv1.ExecRequest, emit func(*guestv1.ExecEvent) error) error {
	if req == nil || emit == nil {
		return ErrInvalid
	}
	h, err := b.guest(a)
	if err != nil {
		return err
	}
	defer h.rpcMu.RUnlock()
	ctx, cancel := context.WithCancel(authContext(ctx, h.credential))
	defer cancel()
	stream, err := h.client.Exec(ctx, req)
	if err != nil {
		return err
	}
	result := false
	for {
		event, e := stream.Recv()
		if e == io.EOF {
			if !result {
				return errors.New("guest execution ended without result")
			}
			return nil
		}
		if e != nil {
			return e
		}
		if result {
			return errors.New("guest sent output after execution result")
		}
		if event.GetResult() != nil {
			result = true
		}
		if err = emit(event); err != nil {
			return err
		}
	}
}
func (b *Backend) ReadFile(ctx context.Context, a Allocation, req *guestv1.ReadFileRequest) (*guestv1.ReadFileResponse, error) {
	if req == nil {
		return nil, ErrInvalid
	}
	h, err := b.guest(a)
	if err != nil {
		return nil, err
	}
	defer h.rpcMu.RUnlock()
	return h.client.ReadFile(authContext(ctx, h.credential), req)
}
func (b *Backend) WriteFile(ctx context.Context, a Allocation, req *guestv1.WriteFileRequest) (*guestv1.WriteFileResponse, error) {
	if req == nil {
		return nil, ErrInvalid
	}
	h, err := b.guest(a)
	if err != nil {
		return nil, err
	}
	defer h.rpcMu.RUnlock()
	return h.client.WriteFile(authContext(ctx, h.credential), req)
}
