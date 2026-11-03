//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"github.com/gduchidze/hypercall/internal/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Timestamp the completed capture, making this a conservative minimum snapshot
// age. Wait before restoring, so an inherited stale clock can fail independently
// of the restored child's subsequent health or continuation behavior.
func (s *snapshotSuite) waitCheckpointAge() error {
	captured := s.handoff.CapturedAt
	if captured.IsZero() {
		return errors.New("fresh capture handoff with captured_at is required for the delayed clock gate")
	}
	if captured.After(time.Now().Add(2 * time.Second)) {
		return errors.New("capture host timestamp exceeds allowed host UTC skew")
	}
	wait := time.Until(captured.Add(5 * time.Second))
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (s *snapshotSuite) clockSample(a sandbox.Observation) (map[string]any, error) {
	before := time.Now()
	r, err := s.exec(s.ctx, a, []string{"/usr/bin/python3", "-I", "-c", "import time; print(time.time_ns())"}, 3000)
	after := time.Now()
	if err != nil {
		return nil, err
	}
	if r.result == nil || r.result.ExitCode != 0 || r.terminals != 1 {
		return nil, fmt.Errorf("guest clock command failed: %s", r.summary())
	}
	guest, err := strconv.ParseInt(strings.TrimSpace(string(r.stdout)), 10, 64)
	if err != nil {
		return nil, errors.New("guest clock command did not return one UTC nanosecond value")
	}
	within := guest >= before.Add(-2*time.Second).UnixNano() && guest <= after.Add(2*time.Second).UnixNano()
	evidence := map[string]any{"allocation": a.Allocation, "host_before_ns": before.UnixNano(), "guest_time_ns": guest, "host_after_ns": after.UnixNano(), "tolerance_ns": int64(2 * time.Second), "within_host_bounds": within}
	if !within {
		return evidence, errors.New("restored guest UTC falls outside recorded host bounds plus/minus two seconds")
	}
	return evidence, nil
}
func (s *snapshotSuite) restoredClock(children []sandbox.Observation) (map[string]any, error) {
	evidence := map[string]any{"checkpoint_minimum_age_ms": time.Since(s.handoff.CapturedAt).Milliseconds(), "minimum_age_required_ms": 5000}
	if time.Since(s.handoff.CapturedAt) < 5*time.Second {
		return evidence, errors.New("restored-clock fixture was not delayed at least five seconds before restore")
	}
	var samples []map[string]any
	for _, a := range children {
		before, err := s.clockSample(a)
		samples = append(samples, map[string]any{"before_negative_requests": before})
		evidence["children"] = samples
		if err != nil {
			return evidence, err
		}
		sess, err := s.session(a)
		if err != nil {
			return evidence, err
		}
		client, err := rawClient(a.VSockPath)
		if err != nil {
			return evidence, err
		}
		call := func(credential string, rebind bool) error {
			ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
			defer cancel()
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+credential)
			guest := guestv1.NewGuestClient(client)
			if rebind {
				_, e := guest.RebindClone(ctx, &guestv1.RebindCloneRequest{HostUnixNano: 0})
				return e
			}
			_, e := guest.ReleaseCapture(ctx, &guestv1.ReleaseCaptureRequest{HostUnixNano: 0})
			return e
		}
		oldParent := call(s.handoff.ParentCredential, false)
		malformedRelease := call(sess.Credential, false)
		malformedRebind := call(sess.Credential, true)
		_ = client.Close()
		if status.Code(oldParent) != codes.Unauthenticated {
			return evidence, fmt.Errorf("old parent clock renewal returned %s instead of Unauthenticated", status.Code(oldParent))
		}
		if status.Code(malformedRelease) != codes.FailedPrecondition || status.Code(malformedRebind) != codes.FailedPrecondition {
			return evidence, fmt.Errorf("active child accepted malformed clock renewal: release=%s rebind=%s", status.Code(malformedRelease), status.Code(malformedRebind))
		}
		after, err := s.clockSample(a)
		samples[len(samples)-1]["after_negative_requests"] = after
		samples[len(samples)-1]["old_parent_release_status"] = status.Code(oldParent).String()
		samples[len(samples)-1]["active_malformed_release_status"] = status.Code(malformedRelease).String()
		samples[len(samples)-1]["active_malformed_rebind_status"] = status.Code(malformedRebind).String()
		if err != nil {
			return evidence, err
		}
	}
	evidence["negative_scope"] = "active child rejects old parent bearer and malformed renewal; clock remains within real host bounds"
	evidence["gated_malformed_clock_injection_tested"] = false
	return evidence, nil
}
