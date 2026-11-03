//go:build linux

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/gduchidze/hypercall/internal/sandbox"
)

// A restored transport must survive multiple production monitor periods. An
// immediate successful RPC does not prove that the allocation remains healthy.
func stableRestored(ctx context.Context, b *sandbox.Backend, children []sandbox.Observation) (map[string]any, error) {
	start := time.Now()
	samples := 0
	for {
		for _, a := range children {
			observed, err := b.Inspect(ctx, a.Allocation)
			if err != nil || observed.State != sandbox.StateRunning {
				return nil, fmt.Errorf("restored health after %dms sandbox=%s state=%s: %v", time.Since(start).Milliseconds(), a.Allocation.SandboxID, observed.State, err)
			}
			state, err := b.InspectControlled(ctx, a.Allocation)
			if err != nil {
				return nil, fmt.Errorf("restored controlled health after %dms sandbox=%s: %w", time.Since(start).Milliseconds(), a.Allocation.SandboxID, err)
			}
			if state == nil || !state.AtBarrier || state.Identity == nil || state.Identity.SandboxId != a.Allocation.SandboxID {
				return nil, fmt.Errorf("restored controlled state invalid after %dms sandbox=%s", time.Since(start).Milliseconds(), a.Allocation.SandboxID)
			}
		}
		samples++
		if time.Since(start) >= 10*time.Second {
			return map[string]any{"children": len(children), "samples_per_child": samples, "stable_ms": time.Since(start).Milliseconds()}, nil
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
