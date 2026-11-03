//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gduchidze/hypercall/internal/sandbox"
	"github.com/gduchidze/hypercall/internal/snapshot"
)

// A real object publication is allowed to stage its disk before its memory Put
// stalls. Cancellation must come from the allocation lifetime, while caller and
// store deadlines still have time remaining.
type stalledMemoryUpload struct {
	snapshot.Objects
	started chan struct{}
	once    sync.Once
	notify  func()
}

func (o *stalledMemoryUpload) Put(ctx context.Context, key string, r io.Reader, n int64, g string) (string, error) {
	if strings.Contains(key, "/staging/") && strings.HasSuffix(key, "/memory") {
		o.once.Do(func() {
			close(o.started)
			if o.notify != nil {
				o.notify()
			}
		})
		<-ctx.Done()
		return "", ctx.Err()
	}
	return o.Objects.Put(ctx, key, r, n, g)
}
func (s *snapshotSuite) captureExpiry() (evidence map[string]any, resultErr error) {
	objects := &stalledMemoryUpload{Objects: s.objects, started: make(chan struct{})}
	store, err := snapshot.New(objects, snapshot.Options{TempDir: filepath.Join(s.o.StateDir, "materialized"), StagingTTL: 30 * time.Second, OperationTimeout: 30 * time.Second})
	if err != nil {
		return nil, err
	}
	cfg := s.config(filepath.Join(s.o.StateDir, "expiry"))
	cfg.SnapshotStore = store
	b, err := sandbox.New(s.ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, b.Close()) }()
	limits := snapshotLimits()
	limits.TTL = 20 * time.Second
	a, err := b.Create(s.ctx, sandbox.CreateRequest{Allocation: snapshotAllocation("expiry-parent"), Limits: limits})
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, b.Stop(context.Background(), a.Allocation), cleanObservation(a))
	}()
	if _, err = b.StartControlled(s.ctx, a.Allocation, 41); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(s.ctx, 45*time.Second)
	defer cancel()
	scope := snapshot.Scope{TenantID: a.Allocation.TenantID, ProjectID: a.Allocation.ProjectID, CheckpointID: "expiry-checkpoint"}
	start := time.Now()
	_, captureErr := b.Capture(ctx, sandbox.CaptureRequest{Allocation: a.Allocation, CheckpointID: scope.CheckpointID})
	select {
	case <-objects.started:
	default:
		return nil, fmt.Errorf("expiry test did not reach blocked memory upload: %v", captureErr)
	}
	s.expiredPublication = store
	s.expiredScope = scope
	// Publication's lease begins before the blocked Put. This conservative bound
	// permits recovery even when the rest of qualification finishes very quickly.
	s.expiryCleanupAfter = start.Add(store.RecoveryGrace() + time.Second)
	if captureErr == nil {
		return nil, errors.New("capture succeeded with stalled upload after allocation expiry")
	}
	if ctx.Err() != nil {
		return nil, errors.New("capture reached caller deadline instead of allocation expiry")
	}
	if time.Now().Before(a.ExpiresAt) || time.Now().After(a.ExpiresAt.Add(3*time.Second)) {
		return nil, fmt.Errorf("capture did not follow allocation expiry: expires=%s finished=%s", a.ExpiresAt, time.Now())
	}
	var observed sandbox.Observation
	deadline := time.Now().Add(3 * time.Second)
	for {
		observed, err = b.Inspect(s.ctx, a.Allocation)
		if err != nil {
			return nil, err
		}
		if observed.State == sandbox.StateStopped || observed.State == sandbox.StateFailed {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("expired parent remained %s after stalled capture", observed.State)
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return nil, s.ctx.Err()
		case <-timer.C:
		}
	}
	if err = cleanObservation(a); err != nil {
		return nil, err
	}
	return map[string]any{"blocked_memory_upload": true, "capture_failed": true, "caller_deadline_reached": false, "expires_at": a.ExpiresAt, "capture_duration_ms": time.Since(start).Milliseconds(), "observed_state": observed.State, "vm_cleanup_verified": true}, nil
}
