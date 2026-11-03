//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"github.com/gduchidze/hypercall/internal/sandbox"
	"github.com/gduchidze/hypercall/internal/sandbox/hostdoctor"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.b.Len() < 8<<20 {
		return b.b.Write(p)
	}
	return len(p), nil
}
func (b *safeBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

type suite struct {
	ctx     context.Context
	o       Options
	r       Report
	b       *sandbox.Backend
	logs    safeBuffer
	secrets []string
	allocs  []sandbox.Observation
	serial  int
}

func (s *suite) check(id string, fn func() (map[string]any, error)) bool {
	start := time.Now()
	e, err := fn()
	c := Check{ID: id, Passed: err == nil, DurationMS: time.Since(start).Milliseconds(), Evidence: e}
	if err != nil {
		c.Error = err.Error()
	}
	s.r.Checks = append(s.r.Checks, c)
	fmt.Fprintf(os.Stderr, "qualification %s passed=%t duration_ms=%d\n", id, c.Passed, c.DurationMS)
	return err == nil
}
func (s *suite) config(state string) sandbox.Config {
	return sandbox.Config{ArtifactConfigPath: s.o.ConfigPath, StateDir: state, CgroupDir: s.o.CgroupDir, JailerUID: s.o.UID, JailerGID: s.o.GID, Scope: sandbox.Scope{TenantID: "qualification", ProjectID: "phase1a"}, Logger: slog.New(slog.NewJSONHandler(&s.logs, nil))}
}
func (s *suite) create(ttl time.Duration) (sandbox.Observation, error) {
	s.serial++
	a := sandbox.Allocation{TenantID: "qualification", ProjectID: "phase1a", SandboxID: fmt.Sprintf("sandbox-%d", s.serial), ExecutionID: "attempt-1", Epoch: 1}
	ctx, cancel := context.WithTimeout(s.ctx, 45*time.Second)
	defer cancel()
	obs, err := s.b.Create(ctx, sandbox.CreateRequest{Allocation: a, Limits: sandbox.Limits{VCPUs: 1, MemoryMiB: 256, WorkspaceBytes: 8 << 20, PIDs: 32, TTL: ttl}})
	if err == nil {
		s.allocs = append(s.allocs, obs)
	}
	return obs, err
}
func qualify(ctx context.Context, o Options) (Report, []string) {
	s := &suite{ctx: ctx, o: o, r: newReport()}
	if !s.check("host-prerequisites", func() (map[string]any, error) {
		r := hostdoctor.Run(ctx, hostdoctor.Options{ConfigPath: o.ConfigPath, CgroupDir: o.CgroupDir})
		if !r.Ready {
			return map[string]any{"doctor": r}, fmt.Errorf("host prerequisite blockers: %v", r.Blockers)
		}
		kernel, _ := os.ReadFile("/proc/sys/kernel/osrelease")
		cpu, _ := os.ReadFile("/proc/cpuinfo")
		mem, _ := os.ReadFile("/proc/meminfo")
		return map[string]any{"doctor": r, "host_kernel": strings.TrimSpace(string(kernel)), "host_cpuinfo": string(cpu), "host_meminfo": string(mem)}, nil
	}) {
		s.r.FinishedAt = time.Now().UTC()
		return s.r, nil
	}
	if err := os.MkdirAll(o.StateDir, 0700); err != nil {
		s.check("boot", func() (map[string]any, error) { return nil, err })
		s.r.FinishedAt = time.Now().UTC()
		return s.r, nil
	}
	var primary sandbox.Observation
	ready := s.check("boot", func() (map[string]any, error) {
		var err error
		s.b, err = sandbox.New(ctx, s.config(filepath.Join(o.StateDir, "primary")))
		if err != nil {
			return nil, err
		}
		primary, err = s.create(10 * time.Minute)
		return map[string]any{"observation": primary}, err
	})
	if ready {
		s.check("commands", func() (map[string]any, error) {
			r, err := s.exec(ctx, primary, []string{"/bin/sh", "-c", "printf 'out'; printf 'err' >&2; exit 7"}, 3000)
			if err != nil {
				return nil, err
			}
			if string(r.stdout) != "out" || string(r.stderr) != "err" || r.result == nil || r.result.ExitCode != 7 || r.terminals != 1 {
				return nil, fmt.Errorf("incorrect stdout/stderr/nonzero/terminal result: %s", r.summary())
			}
			return map[string]any{"stdout_bytes": len(r.stdout), "stderr_bytes": len(r.stderr), "exit_code": r.result.ExitCode, "terminal_results": r.terminals}, nil
		})
		s.check("files", func() (map[string]any, error) {
			payload := []byte{0, 255, 128, 'b', 'i', 'n', 'a', 'r', 'y', 10, 0}
			w, err := s.b.WriteFile(ctx, primary.Allocation, &guestv1.WriteFileRequest{Path: "binary.dat", Data: payload})
			if err != nil {
				return nil, err
			}
			r, err := s.b.ReadFile(ctx, primary.Allocation, &guestv1.ReadFileRequest{Path: "binary.dat"})
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(r.Data, payload) || w.BytesWritten != uint64(len(payload)) {
				return nil, errors.New("binary file bytes changed")
			}
			_, err = s.b.WriteFile(ctx, primary.Allocation, &guestv1.WriteFileRequest{Path: "../escape", Data: payload})
			if err == nil {
				return nil, errors.New("workspace escape accepted")
			}
			rExec, execErr := s.pythonExec(primary, "import os; os.symlink('/etc/passwd','escape-link')", 3000)
			if execErr != nil || rExec.result == nil || rExec.result.ExitCode != 0 {
				return nil, fmt.Errorf("symlink setup failed: %v", execErr)
			}
			if _, err := s.b.ReadFile(ctx, primary.Allocation, &guestv1.ReadFileRequest{Path: "escape-link"}); err == nil {
				return nil, errors.New("symlink escaped workspace")
			}
			cwdErr := s.b.Exec(ctx, primary.Allocation, &guestv1.ExecRequest{Argv: []string{"/bin/true"}, Cwd: "../", TimeoutMs: 1000}, func(*guestv1.ExecEvent) error { return nil })
			if status.Code(cwdErr) != codes.InvalidArgument {
				return nil, fmt.Errorf("cwd traversal returned %v", cwdErr)
			}
			return map[string]any{"bytes": len(payload), "traversal_rejected": true, "symlink_escape_rejected": true, "cwd_escape_rejected": true}, nil
		})
		s.check("python-vsock", func() (map[string]any, error) { return s.python(primary) })
		s.check("authentication", func() (map[string]any, error) { return s.authentication(primary) })
		s.check("scope-epoch", func() (map[string]any, error) { return s.scopeEpoch(primary) })
		s.check("concurrent-ttl", func() (map[string]any, error) { return s.concurrentTTL() })
		s.check("cancellation", func() (map[string]any, error) { return s.cancelTree(primary, "cancel") })
		s.check("deadline", func() (map[string]any, error) { return s.cancelTree(primary, "deadline") })
		s.check("disconnect", func() (map[string]any, error) { return s.disconnect(primary) })
		s.check("resource-limits", func() (map[string]any, error) { return s.limits(primary) })
		s.check("deny-network", func() (map[string]any, error) { return s.network(primary) })
		s.check("vm-isolation", func() (map[string]any, error) { return s.isolation(primary) })
		s.check("guest-failure", func() (map[string]any, error) { return s.guestFailure() })
	}
	s.check("host-crash-recovery", func() (map[string]any, error) { return s.hostCrash() })
	s.check("artifact-failures", func() (map[string]any, error) { return s.artifactFailures() })
	s.check("cleanup", func() (map[string]any, error) {
		var errs []error
		for _, a := range s.allocs {
			if err := s.b.Stop(context.Background(), a.Allocation); err != nil {
				errs = append(errs, err)
			}
			if err := s.b.Stop(context.Background(), a.Allocation); err != nil {
				errs = append(errs, err)
			}
			if err := cleanObservation(a); err != nil {
				errs = append(errs, err)
			}
		}
		if s.b != nil {
			if len(s.allocs) > 0 {
				stale := s.allocs[0].Allocation
				stale.ExecutionID = "stale-other-attempt"
				_, err := s.b.Create(context.Background(), sandbox.CreateRequest{Allocation: stale, Limits: sandbox.Limits{VCPUs: 1, MemoryMiB: 256, WorkspaceBytes: 8 << 20, PIDs: 32, TTL: time.Minute}})
				if !errors.Is(err, sandbox.ErrConflict) {
					errs = append(errs, fmt.Errorf("stale epoch accepted after Stop: %v", err))
				}
			}
			if err := s.b.Close(); err != nil {
				errs = append(errs, err)
			}
			if len(s.allocs) > 0 {
				restarted, err := sandbox.New(context.Background(), s.config(filepath.Join(s.o.StateDir, "primary")))
				if err != nil {
					errs = append(errs, err)
				} else {
					stale := s.allocs[0].Allocation
					stale.ExecutionID = "stale-after-restart"
					_, err = restarted.Create(context.Background(), sandbox.CreateRequest{Allocation: stale, Limits: sandbox.Limits{VCPUs: 1, MemoryMiB: 256, WorkspaceBytes: 8 << 20, PIDs: 32, TTL: time.Minute}})
					if !errors.Is(err, sandbox.ErrConflict) {
						errs = append(errs, fmt.Errorf("stale epoch accepted after restart: %v", err))
					}
					if err := restarted.Close(); err != nil {
						errs = append(errs, err)
					}
				}
			}
		}
		return map[string]any{"allocations": len(s.allocs), "repeated_stop": true, "stale_epoch_after_stop_and_restart_rejected": true}, errors.Join(errs...)
	})
	s.check("log-redaction", func() (map[string]any, error) {
		for _, secret := range s.secrets {
			if bytes.Contains([]byte(s.logs.String()), []byte(secret)) {
				return nil, errors.New("runtime lifecycle logs contained credential")
			}
		}
		return map[string]any{"log_bytes": len(s.logs.String()), "known_credentials_checked": len(s.secrets), "lifecycle_jsonl": s.logs.String()}, nil
	})
	s.r.FinishedAt = time.Now().UTC()
	return s.r, s.secrets
}

type execResult struct {
	stdout, stderr []byte
	result         *guestv1.ExecResult
	terminals      int
}

func (r execResult) summary() string {
	return fmt.Sprintf("stdout=%d stderr=%d terminals=%d result=%v", len(r.stdout), len(r.stderr), r.terminals, r.result)
}
func (s *suite) exec(ctx context.Context, a sandbox.Observation, argv []string, timeout uint32) (execResult, error) {
	var r execResult
	err := s.b.Exec(ctx, a.Allocation, &guestv1.ExecRequest{Argv: argv, TimeoutMs: timeout}, func(e *guestv1.ExecEvent) error {
		r.stdout = append(r.stdout, e.GetStdout()...)
		r.stderr = append(r.stderr, e.GetStderr()...)
		if e.GetResult() != nil {
			r.result = e.GetResult()
			r.terminals++
		}
		return nil
	})
	return r, err
}
func (s *suite) pythonExec(a sandbox.Observation, script string, timeout uint32) (execResult, error) {
	return s.exec(s.ctx, a, []string{"/usr/bin/python3", "-c", script}, timeout)
}
func cleanObservation(a sandbox.Observation) error {
	var errs []error
	if a.PID > 0 {
		if err := syscall.Kill(a.PID, 0); !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Errorf("VM PID %d remains after stop", a.PID))
		}
	}
	for _, p := range []string{a.RootDir, a.CgroupPath} {
		if p != "" {
			if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("runtime resource remains after stop: %s", p))
			}
		}
	}
	return errors.Join(errs...)
}
func hold(ctx context.Context, o Options) error {
	s := &suite{ctx: ctx, o: o}
	b, err := sandbox.New(ctx, s.config(o.StateDir))
	if err != nil {
		return err
	}
	s.b = b
	defer b.Close()
	a, err := s.create(5 * time.Minute)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(a); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}
