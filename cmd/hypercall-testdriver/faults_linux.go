//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gduchidze/hypercall/internal/sandbox"
	"github.com/gduchidze/hypercall/internal/sandbox/hostdoctor"
)

func (s *suite) hostCrash() (map[string]any, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	state := filepath.Join(s.o.StateDir, "crash")
	cmd := exec.Command(exe, "hold", "--config", s.o.ConfigPath, "--state-dir", state, "--cgroup-dir", s.o.CgroupDir, "--jailer-uid", fmt.Sprint(s.o.UID), "--jailer-gid", fmt.Sprint(s.o.GID))
	stdout, err := cmd.StdoutPipe()
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
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	type decoded struct {
		a   sandbox.Observation
		err error
	}
	ready := make(chan decoded, 1)
	go func() { var a sandbox.Observation; err := json.NewDecoder(stdout).Decode(&a); ready <- decoded{a, err} }()
	var a sandbox.Observation
	timer := time.NewTimer(55 * time.Second)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case <-timer.C:
		return nil, errors.New("host crash helper did not boot before timeout")
	case result := <-ready:
		if result.err != nil {
			return nil, fmt.Errorf("host crash helper failed: %w %s", result.err, stderr.String())
		}
		a = result.a
	}
	if a.State != sandbox.StateRunning || a.PID <= 1 {
		return nil, errors.New("host crash helper did not report running VM")
	}
	if err = cmd.Process.Signal(syscall.SIGKILL); err != nil {
		return nil, err
	}
	if err = cmd.Wait(); err == nil {
		return nil, errors.New("SIGKILL helper unexpectedly exited successfully")
	}
	if err = syscall.Kill(a.PID, 0); err != nil {
		return nil, fmt.Errorf("VM was not alive after owner SIGKILL; recovery drill not established: %w", err)
	}
	started := time.Now()
	b, err := sandbox.New(s.ctx, s.config(state))
	if err != nil {
		return nil, fmt.Errorf("reopen after host SIGKILL: %w", err)
	}
	defer b.Close()
	obs, err := b.Inspect(s.ctx, a.Allocation)
	if err != nil || obs.State != sandbox.StateStopped {
		return nil, fmt.Errorf("orphan not marked stopped after recovery: %v state=%s", err, obs.State)
	}
	if err = cleanObservation(a); err != nil {
		return nil, err
	}
	repeated, err := b.Recover(s.ctx)
	if err != nil || len(repeated) != 0 {
		return nil, fmt.Errorf("repeated recovery not idempotent: %v count=%d", err, len(repeated))
	}
	return map[string]any{"owner_sigkill": true, "orphan_alive_before_recovery": true, "recovered_state": obs.State, "recovery_ms": time.Since(started).Milliseconds(), "cleanup_verified": true, "repeat_recover_count": len(repeated)}, nil
}
func digestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func writeJSONFile(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
func copyFixture(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}
func (s *suite) artifactFailures() (map[string]any, error) {
	data, err := os.ReadFile(s.o.ConfigPath)
	if err != nil {
		return nil, err
	}
	var config hostdoctor.ArtifactConfig
	if err = json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	data, err = os.ReadFile(config.Template.Path)
	if err != nil {
		return nil, err
	}
	var manifest hostdoctor.TemplateManifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.o.StateDir, "fixtures")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	bad := config
	bad.Firecracker.SHA256 = strings.Repeat("0", 64)
	badPath := filepath.Join(dir, "bad-hash.json")
	if err = writeJSONFile(badPath, bad); err != nil {
		return nil, err
	}
	cfg := s.config(filepath.Join(s.o.StateDir, "bad-hash"))
	cfg.ArtifactConfigPath = badPath
	b, err := sandbox.New(s.ctx, cfg)
	if err == nil {
		b.Close()
		return nil, errors.New("bad artifact SHA-256 accepted")
	}
	failed := map[string]any{"bad_hash_rejected": true}
	for _, mode := range []string{"unbootable", "no-readiness"} {
		m := manifest
		c := config
		if mode == "unbootable" {
			path := filepath.Join(dir, "not-a-kernel")
			if err = os.WriteFile(path, []byte("not a bootable kernel\n"), 0400); err != nil {
				return nil, err
			}
			m.Kernel.Path = path
			m.Kernel.SHA256, err = digestFile(path)
			if err != nil {
				return nil, err
			}
		} else {
			rootfs := filepath.Join(dir, "no-ready.ext4")
			if err = copyFixture(manifest.RootFS.Path, rootfs); err != nil {
				return nil, err
			}
			init := filepath.Join(dir, "no-ready-init")
			if err = os.WriteFile(init, []byte("#!/bin/sh\nexec /bin/sleep 600\n"), 0700); err != nil {
				return nil, err
			}
			for _, command := range []string{"rm /usr/sbin/hypercall-init", "write " + init + " /usr/sbin/hypercall-init", "set_inode_field /usr/sbin/hypercall-init mode 0100755"} {
				cmd := exec.CommandContext(s.ctx, "/usr/sbin/debugfs", "-w", "-R", command, rootfs)
				out, e := cmd.CombinedOutput()
				if e != nil {
					return nil, fmt.Errorf("build no-readiness fault fixture: %w %s", e, out)
				}
			}
			if err = os.Chmod(rootfs, 0400); err != nil {
				return nil, err
			}
			m.RootFS.Path = rootfs
			m.RootFS.SHA256, err = digestFile(rootfs)
			if err != nil {
				return nil, err
			}
		}
		manifestPath := filepath.Join(dir, mode+"-template.json")
		if err = writeJSONFile(manifestPath, m); err != nil {
			return nil, err
		}
		c.Template.Path = manifestPath
		c.Template.SHA256, err = digestFile(manifestPath)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, mode+"-runtime.json")
		if err = writeJSONFile(path, c); err != nil {
			return nil, err
		}
		stateName := "u"
		if mode == "no-readiness" {
			stateName = "n"
		}
		cfg := s.config(filepath.Join(s.o.StateDir, stateName))
		cfg.ArtifactConfigPath = path
		b, err := sandbox.New(s.ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("%s fixture did not pass artifact integrity: %w", mode, err)
		}
		a := sandbox.Allocation{TenantID: "qualification", ProjectID: "phase1a", SandboxID: mode, ExecutionID: "fault", Epoch: 1}
		ctx, cancel := context.WithTimeout(s.ctx, 7*time.Second)
		start := time.Now()
		obs, createErr := b.Create(ctx, sandbox.CreateRequest{Allocation: a, Limits: sandbox.Limits{VCPUs: 1, MemoryMiB: 256, WorkspaceBytes: 8 << 20, PIDs: 32, TTL: time.Minute}})
		cancel()
		closeErr := b.Close()
		if createErr == nil {
			return nil, fmt.Errorf("%s artifact unexpectedly booted authenticated guest", mode)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if err = cleanObservation(obs); err != nil {
			return nil, fmt.Errorf("%s partial boot cleanup: %w", mode, err)
		}
		if mode == "no-readiness" && !errors.Is(createErr, context.DeadlineExceeded) {
			return nil, fmt.Errorf("no-readiness fixture failed before deadline: %w", createErr)
		}
		failed[mode] = map[string]any{"create_failed": true, "duration_ms": time.Since(start).Milliseconds(), "cleanup_verified": true, "reason": createErr.Error()}
	}
	return failed, nil
}
