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
	"syscall"
	"time"

	"github.com/gduchidze/hypercall/internal/sandbox"
)

func snapshotHold(ctx context.Context, o Options) error {
	s := &snapshotSuite{suite: &suite{ctx: ctx, o: o}}
	if err := s.readHandoff(); err != nil {
		return err
	}
	if err := s.open(o.StateDir); err != nil {
		return err
	}
	defer s.b.Close()
	a, err := s.restore("crash-restored-child", "crash-restored-operation")
	if err != nil {
		return err
	}
	if _, err = stableRestored(ctx, s.b, []sandbox.Observation{a}); err != nil {
		return s.helperDiagnostic(err)
	}
	if err = json.NewEncoder(os.Stdout).Encode(a); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}
func (s *snapshotSuite) restoreCrash() (evidence map[string]any, resultErr error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	state := filepath.Join(s.o.StateDir, "restore-crash")
	cmd := exec.Command(exe, "snapshot-hold", "--config", s.o.ConfigPath, "--state-dir", state, "--cgroup-dir", s.o.CgroupDir, "--handoff", s.o.Handoff, "--jailer-uid", fmt.Sprint(s.o.UID), "--jailer-gid", fmt.Sprint(s.o.GID))
	if s.o.GCSBucket != "" {
		cmd.Args = append(cmd.Args, "--gcs-bucket", s.o.GCSBucket, "--gcs-prefix", s.o.GCSPrefix, "--gcs-token-file", s.o.GCSTokenFile)
	} else {
		cmd.Args = append(cmd.Args, "--snapshot-store", s.o.SnapshotStore)
	}
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
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		// A readiness timeout can occur after VM creation. Recover the dedicated
		// helper state even when it never returned its observation.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cfg := s.config(state)
		cfg.SnapshotStore = s.store
		recovered, err := sandbox.New(cleanupCtx, cfg)
		if err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("crash helper cleanup: %w", err))
			return
		}
		resultErr = errors.Join(resultErr, recovered.Close())
	}()
	type result struct {
		obs sandbox.Observation
		err error
	}
	ready := make(chan result, 1)
	go func() { var a sandbox.Observation; e := json.NewDecoder(out).Decode(&a); ready <- result{a, e} }()
	timer := time.NewTimer(55 * time.Second)
	defer timer.Stop()
	var a sandbox.Observation
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case <-timer.C:
		return nil, errors.New("restored crash helper timed out")
	case r := <-ready:
		if r.err != nil {
			return nil, fmt.Errorf("restored crash helper: %w: %s", r.err, stderr.String())
		}
		a = r.obs
	}
	if a.State != sandbox.StateRunning || a.PID <= 1 {
		return nil, errors.New("crash helper did not restore a live VM")
	}
	if err = cmd.Process.Signal(syscall.SIGKILL); err != nil {
		return nil, err
	}
	if err = cmd.Wait(); err == nil {
		return nil, errors.New("SIGKILL helper exited successfully")
	}
	if err = syscall.Kill(a.PID, 0); err != nil {
		return nil, fmt.Errorf("restored VM not alive before recovery: %w", err)
	}
	cfg := s.config(state)
	cfg.SnapshotStore = s.store
	start := time.Now()
	b, err := sandbox.New(s.ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer b.Close()
	obs, err := b.Inspect(s.ctx, a.Allocation)
	if err != nil || obs.State != sandbox.StateStopped {
		return nil, fmt.Errorf("restored orphan recovery state=%s: %v", obs.State, err)
	}
	if err = cleanObservation(a); err != nil {
		return nil, err
	}
	if err = cleanMaterialized(state); err != nil {
		return nil, err
	}
	again, err := b.Recover(s.ctx)
	if err != nil || len(again) != 0 {
		return nil, fmt.Errorf("repeat recovery count=%d: %v", len(again), err)
	}
	return map[string]any{"restored_owner_sigkill": true, "orphan_alive_before_recovery": true, "cleanup_verified": true, "recovery_ms": time.Since(start).Milliseconds(), "repeat_recover_count": len(again)}, nil
}
