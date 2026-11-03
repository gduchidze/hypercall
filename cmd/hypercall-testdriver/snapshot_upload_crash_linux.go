//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gduchidze/hypercall/internal/sandbox"
	"github.com/gduchidze/hypercall/internal/snapshot"
)

func snapshotUploadHold(ctx context.Context, o Options) error {
	s := &snapshotSuite{suite: &suite{ctx: ctx, o: o}}
	if err := s.open(o.StateDir); err != nil {
		return err
	}
	if err := s.b.Close(); err != nil {
		return err
	}
	var a sandbox.Observation
	objects := &stalledMemoryUpload{Objects: s.objects, started: make(chan struct{}), notify: func() { _ = json.NewEncoder(os.Stdout).Encode(a) }}
	store, err := snapshot.New(objects, snapshot.Options{TempDir: filepath.Join(o.StateDir, "materialized"), StagingTTL: 3 * time.Second, OperationTimeout: 3 * time.Second})
	if err != nil {
		return err
	}
	cfg := s.config(o.StateDir)
	cfg.SnapshotStore = store
	s.b, err = sandbox.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.b.Close()
	a, err = s.b.Create(ctx, sandbox.CreateRequest{Allocation: snapshotAllocation("upload-crash-parent"), Limits: snapshotLimits()})
	if err != nil {
		return err
	}
	if _, err = s.b.StartControlled(ctx, a.Allocation, 41); err != nil {
		return err
	}
	_, err = s.b.Capture(ctx, sandbox.CaptureRequest{Allocation: a.Allocation, CheckpointID: "upload-crash-checkpoint"})
	return err
}
func (s *snapshotSuite) captureCrash() (evidence map[string]any, resultErr error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	state := filepath.Join(s.o.StateDir, "upload-crash")
	args := []string{"snapshot-upload-hold", "--config", s.o.ConfigPath, "--state-dir", state, "--cgroup-dir", s.o.CgroupDir, "--handoff", s.o.Handoff, "--jailer-uid", fmt.Sprint(s.o.UID), "--jailer-gid", fmt.Sprint(s.o.GID)}
	if s.o.GCSBucket != "" {
		args = append(args, "--gcs-bucket", s.o.GCSBucket, "--gcs-prefix", s.o.GCSPrefix, "--gcs-token-file", s.o.GCSTokenFile)
	} else {
		args = append(args, "--snapshot-store", s.o.SnapshotStore)
	}
	cmd := exec.Command(exe, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr safeBuffer
	cmd.Stderr = &stderr
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	store, err := snapshot.New(s.objects, snapshot.Options{TempDir: filepath.Join(s.o.StateDir, "materialized"), StagingTTL: 3 * time.Second, OperationTimeout: 3 * time.Second})
	if err != nil {
		return nil, err
	}
	cfg := s.config(state)
	cfg.SnapshotStore = store
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		b, e := sandbox.New(cleanupCtx, cfg)
		if e != nil {
			resultErr = errors.Join(resultErr, e)
			return
		}
		resultErr = errors.Join(resultErr, b.Close())
	}()
	type decoded struct {
		a   sandbox.Observation
		err error
	}
	ready := make(chan decoded, 1)
	go func() { var a sandbox.Observation; e := json.NewDecoder(out).Decode(&a); ready <- decoded{a, e} }()
	timer := time.NewTimer(55 * time.Second)
	defer timer.Stop()
	var a sandbox.Observation
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case <-timer.C:
		return nil, errors.New("upload crash helper did not reach staged memory Put")
	case r := <-ready:
		if r.err != nil {
			return nil, fmt.Errorf("upload crash helper: %w: %s", r.err, stderr.String())
		}
		a = r.a
	}
	scope := snapshot.Scope{TenantID: a.Allocation.TenantID, ProjectID: a.Allocation.ProjectID, CheckpointID: "upload-crash-checkpoint"}
	prefix := "tenants/" + scope.TenantID + "/projects/" + scope.ProjectID + "/checkpoints/" + scope.CheckpointID + "/"
	before, _, err := s.objects.List(s.ctx, prefix, "", 100)
	if err != nil {
		return nil, err
	}
	staged := false
	for _, o := range before {
		if strings.HasSuffix(o.Key, "/disk") {
			staged = true
		}
	}
	if !staged {
		return nil, errors.New("upload crash did not establish a staged disk artifact")
	}
	if err = cmd.Process.Signal(syscall.SIGKILL); err != nil {
		return nil, err
	}
	if err = cmd.Wait(); err == nil {
		return nil, errors.New("upload SIGKILL helper exited successfully")
	}
	if err = syscall.Kill(a.PID, 0); err != nil {
		return nil, fmt.Errorf("paused VM missing before owner recovery: %w", err)
	}
	b, err := sandbox.New(s.ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer b.Close()
	obs, err := b.Inspect(s.ctx, a.Allocation)
	if err != nil || obs.State != sandbox.StateStopped {
		return nil, fmt.Errorf("upload orphan state=%s: %v", obs.State, err)
	}
	if err = cleanObservation(a); err != nil {
		return nil, err
	}
	if err = cleanMaterialized(state); err != nil {
		return nil, err
	}
	if v, e := store.OpenVerified(s.ctx, scope, s.handoff.Manifest.Compatibility); e == nil {
		v.Close()
		return nil, errors.New("interrupted upload became a usable checkpoint")
	}
	deadline := time.Now().Add(8 * time.Second)
	passes := 0
	for {
		if _, err = b.Recover(s.ctx); err != nil {
			return nil, err
		}
		passes++
		objects, _, e := s.objects.List(s.ctx, prefix, "", 100)
		if e != nil {
			return nil, e
		}
		remaining := 0
		for _, o := range objects {
			if !strings.HasSuffix(o.Key, "/lifetime") {
				remaining++
			}
		}
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("capture intent did not collect %d staged objects after expiry", remaining)
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return nil, s.ctx.Err()
		case <-timer.C:
		}
	}
	again, err := b.Recover(s.ctx)
	if err != nil || len(again) != 0 {
		return nil, fmt.Errorf("repeat upload recovery=%d: %v", len(again), err)
	}
	return map[string]any{"owner_sigkill_during_memory_put": true, "disk_staged_before_crash": true, "paused_orphan_alive_before_recovery": true, "vm_cleanup_verified": true, "incomplete_checkpoint_unavailable": true, "staged_objects_collected": true, "recovery_passes": passes, "repeat_recover_count": len(again)}, nil
}
