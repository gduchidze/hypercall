//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"github.com/gduchidze/hypercall/internal/sandbox"
	"github.com/gduchidze/hypercall/internal/sandbox/hostdoctor"
	"github.com/gduchidze/hypercall/internal/snapshot"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// The handoff is a private operator fixture, never a public API or report. Its
// parent bearer exists only to test that the restored guest rejects it.
type snapshotHandoff struct {
	Version          int                      `json:"version"`
	CapturedAt       time.Time                `json:"captured_at"`
	SourceHost       string                   `json:"source_host_sha256"`
	Manifest         snapshot.Manifest        `json:"manifest"`
	Captured         *guestv1.ControlledState `json:"captured"`
	ParentCredential string                   `json:"parent_credential"`
}
type snapshotSuite struct {
	*suite
	objects            snapshot.Objects
	store              *snapshot.Store
	faults             *objectReadFault
	handoff            snapshotHandoff
	expiredPublication *snapshot.Store
	expiredScope       snapshot.Scope
	expiryCleanupAfter time.Time
}

func snapshotLimits() sandbox.Limits {
	return sandbox.Limits{VCPUs: 1, MemoryMiB: 256, WorkspaceBytes: 8 << 20, PIDs: 32, TTL: 10 * time.Minute}
}
func snapshotAllocation(name string) sandbox.Allocation {
	return sandbox.Allocation{TenantID: "qualification", ProjectID: "phase1a", SandboxID: name, ExecutionID: name + "-execution", Epoch: 1}
}
func hostFingerprint() (string, error) {
	b, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", errors.New("host machine-id is empty")
	}
	h := sha256.Sum256(bytes.TrimSpace(b))
	return hex.EncodeToString(h[:]), nil
}
func (s *snapshotSuite) open(state string) error {
	if err := os.MkdirAll(s.o.StateDir, 0700); err != nil {
		return err
	}
	cache := filepath.Join(s.o.StateDir, "materialized")
	if err := os.MkdirAll(cache, 0700); err != nil {
		return err
	}
	var err error
	if s.o.GCSBucket != "" {
		s.objects, err = snapshot.NewGCSObjects(snapshot.GCSConfig{Bucket: s.o.GCSBucket, Prefix: s.o.GCSPrefix, TempDir: cache, TokenSource: func(ctx context.Context) (string, error) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			f, e := os.Open(s.o.GCSTokenFile)
			if e != nil {
				return "", errors.New("GCS token file unavailable")
			}
			defer f.Close()
			st, e := f.Stat()
			if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
				return "", errors.New("GCS token file must be a private regular file")
			}
			b, e := io.ReadAll(io.LimitReader(f, 16385))
			if e != nil || len(b) > 16384 {
				return "", errors.New("GCS token file unreadable or oversized")
			}
			token := strings.TrimSpace(string(b))
			if token == "" {
				return "", errors.New("GCS token file empty")
			}
			return token, nil
		}})
	} else {
		s.objects, err = snapshot.NewFileObjects(s.o.SnapshotStore)
	}
	if err != nil {
		return err
	}
	s.faults = &objectReadFault{Objects: s.objects}
	s.store, err = snapshot.New(s.faults, snapshot.Options{TempDir: cache})
	if err != nil {
		return err
	}
	c := s.config(state)
	c.SnapshotStore = s.store
	s.b, err = sandbox.New(s.ctx, c)
	return err
}
func (s *snapshotSuite) readHandoff() error {
	f, err := os.Open(s.o.Handoff)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0077 != 0 {
		return errors.New("handoff must be private (0600)")
	}
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&s.handoff); err != nil {
		return err
	}
	if s.handoff.Version != 1 || s.handoff.CapturedAt.IsZero() || s.handoff.Captured == nil || s.handoff.ParentCredential == "" {
		return errors.New("incomplete snapshot handoff")
	}
	s.secrets = append(s.secrets, s.handoff.ParentCredential)
	return nil
}
func (s *snapshotSuite) exportHandoff() error {
	if s.o.Handoff == "" {
		s.o.Handoff = filepath.Join(s.o.StateDir, "handoff.json")
	}
	if err := writeJSONFile(s.o.Handoff, s.handoff); err != nil {
		return err
	}
	return os.Chmod(s.o.Handoff, 0600)
}
func (s *snapshotSuite) restore(name, op string) (sandbox.Observation, error) {
	if err := s.waitCheckpointAge(); err != nil {
		return sandbox.Observation{}, err
	}
	a, err := s.b.Restore(s.ctx, sandbox.RestoreRequest{OperationID: op, CheckpointID: s.handoff.Manifest.Scope.CheckpointID, Allocation: snapshotAllocation(name), Limits: snapshotLimits()})
	if err == nil {
		s.allocs = append(s.allocs, a)
	}
	return a, err
}
func inherited(want, got *guestv1.ControlledState) error {
	if got == nil || want == nil || got.Counter != want.Counter || got.Pid != want.Pid || !got.AtBarrier {
		return fmt.Errorf("controlled Python continuity mismatch: got=%v want=%v", got, want)
	}
	var disk struct {
		Counter int64 `json:"counter"`
	}
	if err := json.Unmarshal(got.FileData, &disk); err != nil {
		return fmt.Errorf("controlled file is invalid JSON: %w", err)
	}
	if disk.Counter != want.Counter {
		return errors.New("captured file counter differs from memory")
	}
	return nil
}
func (s *snapshotSuite) continuation(a sandbox.Observation, delta int64) (*guestv1.ControlledState, error) {
	got, err := s.b.ContinueControlled(s.ctx, a.Allocation, delta)
	if err != nil {
		return nil, err
	}
	want := s.handoff.Captured
	if got.Counter != want.Counter+delta || got.Pid != want.Pid || !got.AtBarrier || bytes.Equal(got.FileData, want.FileData) {
		return nil, fmt.Errorf("Python did not independently continue: %v", got)
	}
	return got, nil
}
func (s *snapshotSuite) authenticationFor(a sandbox.Observation) error {
	sess, err := s.session(a)
	if err != nil {
		return err
	}
	if sess.Credential == s.handoff.ParentCredential {
		return errors.New("restored credential equals parent credential")
	}
	if _, err = info(s.ctx, a.VSockPath, s.handoff.ParentCredential); status.Code(err) != codes.Unauthenticated {
		return fmt.Errorf("parent bearer returned %v", status.Code(err))
	}
	c, err := rawClient(a.VSockPath)
	if err != nil {
		return err
	}
	rebindCtx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()
	rebindCtx = metadata.AppendToOutgoingContext(rebindCtx, "authorization", "Bearer "+s.handoff.ParentCredential)
	_, rebindErr := guestv1.NewGuestClient(c).RebindClone(rebindCtx, &guestv1.RebindCloneRequest{})
	_ = c.Close()
	if rebindErr == nil {
		return errors.New("parent bearer was allowed to rebind restored child")
	}
	got, err := info(s.ctx, a.VSockPath, sess.Credential)
	if err != nil {
		return err
	}
	if got.Identity == nil || got.Identity.SandboxId != a.Allocation.SandboxID || got.Identity.ExecutionId != a.Allocation.ExecutionID {
		return errors.New("restored guest has stale identity")
	}
	return nil
}
func qualifySnapshot(ctx context.Context, o Options, command string) (Report, []string) {
	profile := map[string]string{"snapshot-qualify": "phase1b-local", "snapshot-capture": "phase1b-capture", "snapshot-restore": "phase1b-restore"}[command]
	s := &snapshotSuite{suite: &suite{ctx: ctx, o: o, r: newReport()}}
	s.r.Profile = profile
	defer func() {
		if s.b != nil {
			_ = s.b.Close()
		}
	}()
	ready := s.check("host-prerequisites", func() (map[string]any, error) {
		r := hostdoctor.Run(ctx, hostdoctor.Options{ConfigPath: o.ConfigPath, CgroupDir: o.CgroupDir})
		if !r.Ready {
			return map[string]any{"doctor": r}, fmt.Errorf("host prerequisite blockers: %v", r.Blockers)
		}
		h, e := hostFingerprint()
		return map[string]any{"doctor": r, "host_sha256": h}, e
	})
	if ready {
		ready = s.check("snapshot-backend", func() (map[string]any, error) {
			err := s.open(filepath.Join(o.StateDir, "primary"))
			return map[string]any{"storage": s.storageKind(), "capture_mode": "fully-materialized", "lazy_restore": false}, err
		})
	}
	var parent sandbox.Observation
	if ready && command != "snapshot-restore" {
		ready = s.check("snapshot-boot", func() (map[string]any, error) {
			var err error
			parent, err = s.create(10 * time.Minute)
			if err != nil {
				return nil, err
			}
			st, err := s.b.StartControlled(ctx, parent.Allocation, 41)
			if err != nil {
				return nil, err
			}
			if st.Counter != 41 || !st.AtBarrier || st.Pid <= 1 || len(st.FileData) == 0 {
				return nil, fmt.Errorf("Python initial state not established: %v", st)
			}
			s.handoff.Captured = st
			return map[string]any{"observation": parent, "controlled": st}, nil
		})
		if ready {
			ready = s.check("checkpoint-capture", func() (map[string]any, error) {
				sess, err := s.session(parent)
				if err != nil {
					return nil, err
				}
				s.handoff.ParentCredential = sess.Credential
				s.handoff.SourceHost, err = hostFingerprint()
				if err != nil {
					return nil, err
				}
				s.handoff.Version = 1
				start := time.Now()
				s.handoff.Manifest, err = s.b.Capture(ctx, sandbox.CaptureRequest{Allocation: parent.Allocation, CheckpointID: "checkpoint-41"})
				if err != nil {
					return nil, err
				}
				s.handoff.CapturedAt = time.Now().UTC()
				if err = s.exportHandoff(); err != nil {
					return nil, err
				}
				var total int64
				for _, a := range s.handoff.Manifest.Artifacts {
					total += a.Size
				}
				return map[string]any{"manifest": s.handoff.Manifest, "capture_ms": time.Since(start).Milliseconds(), "materialized_bytes": total}, nil
			})
		}
	}
	if ready {
		switch command {
		case "snapshot-capture":
			s.check("cross-host-export", func() (map[string]any, error) {
				return map[string]any{"checkpoint_id": s.handoff.Manifest.Scope.CheckpointID, "source_host_sha256": s.handoff.SourceHost, "handoff_written": true, "object_store": s.storageKind()}, nil
			})
		case "snapshot-restore":
			s.crossHost()
		default:
			s.local(parent)
		}
	}
	s.check("cleanup", func() (map[string]any, error) {
		var errs []error
		if s.b != nil {
			for _, a := range s.allocs {
				errs = append(errs, s.b.Stop(context.Background(), a.Allocation), s.b.Stop(context.Background(), a.Allocation), cleanObservation(a))
			}
			errs = append(errs, s.b.Close(), cleanMaterialized(filepath.Join(s.o.StateDir, "primary")))
		}
		if s.expiredPublication != nil {
			timer := time.NewTimer(time.Until(s.expiryCleanupAfter))
			select {
			case <-timer.C:
			case <-s.ctx.Done():
				errs = append(errs, s.ctx.Err())
			}
			timer.Stop()
			errs = append(errs, s.expiredPublication.RecoverExpired(context.Background(), s.expiredScope))
		}
		deleted := false
		if command != "snapshot-capture" && s.store != nil && s.handoff.Manifest.Scope.CheckpointID != "" {
			err := s.store.DeleteUnreferenced(context.Background(), s.handoff.Manifest.Scope)
			errs = append(errs, err)
			if err == nil {
				deleted = true
				for _, a := range s.handoff.Manifest.Artifacts {
					r, _, e := s.objects.Get(context.Background(), a.Key)
					if r != nil {
						r.Close()
					}
					if !errors.Is(e, snapshot.ErrNotFound) {
						errs = append(errs, fmt.Errorf("checkpoint artifact remains after cleanup: %s: %v", a.Kind, e))
					}
				}
			}
		}
		return map[string]any{"allocations": len(s.allocs), "repeat_stop": true, "checkpoint_artifacts_deleted": deleted, "capture_export_retained": command == "snapshot-capture"}, errors.Join(errs...)
	})
	s.check("log-redaction", func() (map[string]any, error) {
		for _, secret := range s.secrets {
			if secret != "" && strings.Contains(s.logs.String(), secret) {
				return nil, errors.New("runtime logs exposed credential")
			}
		}
		return map[string]any{"credentials_checked": len(s.secrets), "lifecycle_jsonl": s.logs.String()}, nil
	})
	s.r.FinishedAt = time.Now().UTC()
	return s.r, s.secrets
}
func (s *snapshotSuite) crossHost() {
	ready := s.check("cross-host-source", func() (map[string]any, error) {
		if err := s.readHandoff(); err != nil {
			return nil, err
		}
		here, err := hostFingerprint()
		if err != nil {
			return nil, err
		}
		if here == s.handoff.SourceHost {
			return nil, errors.New("cross-host evidence requires distinct machine IDs")
		}
		return map[string]any{"source_host_sha256": s.handoff.SourceHost, "destination_host_sha256": here}, nil
	})
	var child sandbox.Observation
	if ready {
		ready = s.check("cross-host-restore", func() (map[string]any, error) {
			var err error
			start := time.Now()
			child, err = s.restore("cross-host-child", "cross-host-operation")
			if err != nil {
				return nil, err
			}
			st, err := s.b.InspectControlled(s.ctx, child.Allocation)
			if err == nil {
				err = s.verifyInherited(child, st)
			}
			return map[string]any{"observation": child, "controlled": st, "restore_ms": time.Since(start).Milliseconds(), "materialization": "full"}, err
		})
	}
	if ready {
		s.check("restored-clock", func() (map[string]any, error) { return s.restoredClock([]sandbox.Observation{child}) })
		ready = s.check("restored-health-stability", func() (map[string]any, error) { return stableRestored(s.ctx, s.b, []sandbox.Observation{child}) })
	}
	if ready {
		s.check("restored-continuation", func() (map[string]any, error) {
			st, e := s.continuation(child, 9)
			return map[string]any{"controlled": st}, e
		})
		s.check("restored-authentication", func() (map[string]any, error) {
			return map[string]any{"parent_bearer_rejected": true}, s.authenticationFor(child)
		})
	}
}
func (s *snapshotSuite) local(parent sandbox.Observation) {
	s.check("capture-expiry", func() (map[string]any, error) { return s.captureExpiry() })
	s.check("capture-crash-recovery", func() (map[string]any, error) { return s.captureCrash() })
	children := []sandbox.CreateRequest{{Allocation: snapshotAllocation("child-a"), Limits: snapshotLimits()}, {Allocation: snapshotAllocation("child-b"), Limits: snapshotLimits()}}
	var observations []sandbox.Observation
	ready := s.check("fork-continuity", func() (map[string]any, error) {
		if err := s.waitCheckpointAge(); err != nil {
			return nil, err
		}
		start := time.Now()
		results, err := s.b.Fork(s.ctx, sandbox.ForkRequest{OperationID: "fork-two", CheckpointID: s.handoff.Manifest.Scope.CheckpointID, Children: children})
		if err != nil {
			return nil, err
		}
		if len(results) != 2 {
			return nil, fmt.Errorf("got %d child results", len(results))
		}
		var states []*guestv1.ControlledState
		for _, r := range results {
			if r.Error != "" {
				return nil, errors.New(r.Error)
			}
			observations = append(observations, r.Observation)
			s.allocs = append(s.allocs, r.Observation)
			st, e := s.b.InspectControlled(s.ctx, r.Allocation)
			if e != nil {
				return nil, e
			}
			if e = s.verifyInherited(r.Observation, st); e != nil {
				return nil, e
			}
			states = append(states, st)
		}
		return map[string]any{"results": results, "controlled": states, "fork_ms": time.Since(start).Milliseconds(), "materialization": "full", "simultaneous_vms": 3, "fanout": 2}, nil
	})
	if ready {
		s.check("restored-clock", func() (map[string]any, error) { return s.restoredClock(observations) })
		ready = s.check("restored-health-stability", func() (map[string]any, error) { return stableRestored(s.ctx, s.b, observations) })
	}
	if ready {
		s.check("fork-isolation", func() (map[string]any, error) {
			all := append([]sandbox.Observation{parent}, observations...)
			var states []*guestv1.ControlledState
			for i, a := range all {
				st, e := s.continuation(a, int64(i+1))
				if e != nil {
					return nil, e
				}
				states = append(states, st)
			}
			for i, a := range all {
				st, e := s.b.InspectControlled(s.ctx, a.Allocation)
				if e != nil {
					return nil, e
				}
				if st.Counter != int64(42+i) || !bytes.Equal(st.FileData, states[i].FileData) {
					return nil, errors.New("parent or sibling state changed through another branch")
				}
			}
			for i, a := range all {
				data := []byte(fmt.Sprintf("branch-%d\n", i))
				if _, e := s.b.WriteFile(s.ctx, a.Allocation, &guestv1.WriteFileRequest{Path: "controlled-origin.txt", Data: data}); e != nil {
					return nil, e
				}
			}
			for i, a := range all {
				f, e := s.b.ReadFile(s.ctx, a.Allocation, &guestv1.ReadFileRequest{Path: "controlled-origin.txt"})
				if e != nil {
					return nil, e
				}
				if string(f.Data) != fmt.Sprintf("branch-%d\n", i) {
					return nil, errors.New("private workspace write leaked to sibling or parent")
				}
			}
			return map[string]any{"parent_and_children": states, "workspace_writes_independent": true}, nil
		})
		s.check("fork-authentication", func() (map[string]any, error) {
			parentState, e := s.b.InspectControlled(s.ctx, parent.Allocation)
			if e != nil {
				return nil, e
			}
			seen := map[string]bool{hex.EncodeToString(parentState.RandomProbe): true}
			for _, a := range observations {
				if err := s.authenticationFor(a); err != nil {
					return nil, err
				}
				st, err := s.b.InspectControlled(s.ctx, a.Allocation)
				if err != nil {
					return nil, err
				}
				probe := hex.EncodeToString(st.RandomProbe)
				if probe == "" || seen[probe] {
					return nil, errors.New("restored RNG probe empty or duplicated")
				}
				seen[probe] = true
			}
			return map[string]any{"parent_bearer_rejected_by_children": true, "fresh_identity": true, "independent_rng_probes": len(seen)}, nil
		})
	}
	for _, a := range observations {
		if err := s.b.Stop(s.ctx, a.Allocation); err != nil {
			s.check("fork-branch-stop", func() (map[string]any, error) { return nil, err })
		}
	}
	s.check("fork-retry", func() (map[string]any, error) { return s.partialRetry(parent) })
	s.check("artifact-rejection", func() (map[string]any, error) { return s.rejectArtifacts() })
	s.check("restore-crash-recovery", func() (map[string]any, error) { return s.restoreCrash() })
}
func (s *snapshotSuite) partialRetry(_ sandbox.Observation) (map[string]any, error) {
	req := sandbox.ForkRequest{OperationID: "partial-fanout", CheckpointID: s.handoff.Manifest.Scope.CheckpointID, Children: []sandbox.CreateRequest{{Allocation: snapshotAllocation("partial-a"), Limits: snapshotLimits()}, {Allocation: snapshotAllocation("partial-b"), Limits: snapshotLimits()}}}
	for _, a := range s.handoff.Manifest.Artifacts {
		if a.Kind == snapshot.Memory {
			s.faults.arm(a.Key, 2)
		}
	}
	first, err := s.b.Fork(s.ctx, req)
	if err != nil {
		return nil, err
	}
	if len(first) != 2 || first[0].Error != "" || first[1].Error == "" {
		return nil, fmt.Errorf("partial fan-out not established: %+v", first)
	}
	s.allocs = append(s.allocs, first[0].Observation)
	again, err := s.b.Fork(s.ctx, req)
	if err != nil {
		return nil, err
	}
	if len(again) != 2 || again[0].Error != "" || again[0].Observation.PID != first[0].Observation.PID || again[1].Error != "" || again[1].Observation.State != sandbox.StateRunning {
		return nil, fmt.Errorf("retry did not preserve successful child and recover failed child: %+v", again)
	}
	s.allocs = append(s.allocs, again[1].Observation)
	for _, r := range again {
		st, e := s.b.InspectControlled(s.ctx, r.Allocation)
		if e != nil {
			return nil, e
		}
		if e = s.verifyInherited(r.Observation, st); e != nil {
			return nil, e
		}
	}
	for _, r := range again {
		if err = s.b.Stop(s.ctx, r.Allocation); err != nil {
			return nil, err
		}
	}
	return map[string]any{"first": first, "retry": again, "successful_child_reused": true, "failed_child_recovered": true, "fault": "one missing memory object read on second child"}, nil
}
func (s *snapshotSuite) rejectArtifacts() (map[string]any, error) {
	a := s.handoff.Manifest.Artifacts[0]
	r, g, err := s.objects.Get(s.ctx, a.Key)
	if err != nil {
		return nil, err
	}
	original, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		return nil, err
	}
	if len(original) == 0 {
		return nil, errors.New("empty disk artifact")
	}
	corrupt := append([]byte(nil), original...)
	corrupt[len(corrupt)/2] ^= 0xff
	current, err := s.objects.Put(s.ctx, a.Key, bytes.NewReader(corrupt), int64(len(corrupt)), g)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = s.objects.Put(context.Background(), a.Key, bytes.NewReader(original), int64(len(original)), current)
	}()
	_, badErr := s.restore("corrupt-child", "corrupt-restore")
	if badErr == nil || !strings.Contains(badErr.Error(), snapshot.ErrIntegrity.Error()) {
		return nil, fmt.Errorf("corrupt artifact returned %v", badErr)
	}
	if _, err = s.b.Inspect(s.ctx, snapshotAllocation("corrupt-child")); !errors.Is(err, sandbox.ErrNotFound) {
		return nil, fmt.Errorf("corrupt artifact admitted a child before rejection: %v", err)
	}
	if err = s.objects.Delete(s.ctx, a.Key, current); err != nil {
		return nil, err
	}
	current = ""
	_, missingErr := s.restore("missing-child", "missing-restore")
	if missingErr == nil || !strings.Contains(missingErr.Error(), snapshot.ErrNotFound.Error()) {
		return nil, fmt.Errorf("missing artifact returned %v", missingErr)
	}
	if _, err = s.b.Inspect(s.ctx, snapshotAllocation("missing-child")); !errors.Is(err, sandbox.ErrNotFound) {
		return nil, fmt.Errorf("missing artifact admitted a child before rejection: %v", err)
	}
	current, err = s.objects.Put(s.ctx, a.Key, bytes.NewReader(original), int64(len(original)), "")
	if err != nil {
		return nil, err
	}
	return map[string]any{"corrupt_rejected": true, "missing_rejected": true, "rejected_before_allocation": true, "artifact_kind": a.Kind}, nil
}

func (s *snapshotSuite) storageKind() string {
	if s.o.GCSBucket != "" {
		return "gcs"
	}
	return "file"
}

func (s *snapshotSuite) verifyInherited(a sandbox.Observation, got *guestv1.ControlledState) error {
	if err := inherited(s.handoff.Captured, got); err != nil {
		return err
	}
	if got.Identity == nil || got.Identity.SandboxId != a.Allocation.SandboxID || got.Identity.ExecutionId != a.Allocation.ExecutionID || got.Identity.Epoch != a.Allocation.Epoch {
		return errors.New("controlled process retained parent identity")
	}
	f, err := s.b.ReadFile(s.ctx, a.Allocation, &guestv1.ReadFileRequest{Path: "controlled-origin.txt"})
	if err != nil {
		return err
	}
	if string(f.Data) != "counter=41\n" {
		return errors.New("immutable Python-created disk sentinel was not inherited")
	}
	current, err := s.b.ReadFile(s.ctx, a.Allocation, &guestv1.ReadFileRequest{Path: "controlled-state.json"})
	if err != nil {
		return err
	}
	if !bytes.Equal(current.Data, got.FileData) {
		return errors.New("Python disk file differs from inspected state")
	}
	return nil
}

// objectReadFault injects one unavailable artifact read at the actual public
// object seam. Every successful child still uses the real store and Firecracker.
type objectReadFault struct {
	snapshot.Objects
	mu        sync.Mutex
	key       string
	remaining int
}

func (f *objectReadFault) arm(key string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.key = key
	f.remaining = n
}
func (f *objectReadFault) Get(ctx context.Context, key string) (io.ReadCloser, string, error) {
	f.mu.Lock()
	fail := false
	if key == f.key && f.remaining > 0 {
		f.remaining--
		fail = f.remaining == 0
	}
	f.mu.Unlock()
	if fail {
		return nil, "", snapshot.ErrNotFound
	}
	return f.Objects.Get(ctx, key)
}

func cleanMaterialized(state string) error {
	entries, err := os.ReadDir(filepath.Join(state, "materialized"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("%d materialized snapshot resources remain after VM cleanup", len(entries))
	}
	return nil
}

// The helper has no JSON report on failure. Return bounded lifecycle diagnostics
// with the operator's parent and all locally staged allocation bearers removed.
func (s *snapshotSuite) helperDiagnostic(cause error) error {
	details := cause.Error() + "; lifecycle_jsonl=" + s.logs.String()
	secrets := append([]string(nil), s.secrets...)
	for _, root := range []string{s.o.StateDir, filepath.Join(s.o.StateDir, "primary")} {
		paths, _ := filepath.Glob(filepath.Join(root, "allocations", "*", "bootstrap", "session.json"))
		for _, p := range paths {
			b, e := os.ReadFile(p)
			if e != nil {
				continue
			}
			var sess session
			if json.Unmarshal(b, &sess) == nil && sess.Credential != "" {
				secrets = append(secrets, sess.Credential)
			}
		}
	}
	if s.o.GCSTokenFile != "" {
		if b, e := os.ReadFile(s.o.GCSTokenFile); e == nil {
			secrets = append(secrets, strings.TrimSpace(string(b)))
		}
	}
	for _, secret := range secrets {
		if secret != "" {
			details = strings.ReplaceAll(details, secret, "[REDACTED]")
		}
	}
	return errors.New(details)
}
