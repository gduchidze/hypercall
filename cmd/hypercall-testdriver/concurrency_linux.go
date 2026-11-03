//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gduchidze/hypercall/internal/sandbox"
)

// The operator freezes a second allocation's owned cgroup during boot. This
// injects a real stalled jailer/VM without adding a fault hook to the backend.
// An unrelated running allocation must still expire and clean up on time.
func (s *suite) concurrentTTL() (evidence map[string]any, result error) {
	a, err := s.create(18 * time.Second)
	if err != nil {
		return nil, err
	}
	s.serial++
	bID := sandbox.Allocation{TenantID: "qualification", ProjectID: "phase1a", SandboxID: fmt.Sprintf("sandbox-%d", s.serial), ExecutionID: "blocked-boot", Epoch: 1}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	type outcome struct {
		o   sandbox.Observation
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		o, err := s.b.Create(ctx, sandbox.CreateRequest{Allocation: bID, Limits: sandbox.Limits{VCPUs: 1, MemoryMiB: 256, WorkspaceBytes: 8 << 20, PIDs: 32, TTL: time.Minute}})
		done <- outcome{o, err}
	}()
	var frozen string
	var completed *outcome
	defer func() {
		if frozen != "" {
			if err := os.WriteFile(filepath.Join(frozen, "cgroup.freeze"), []byte("0"), 0600); err != nil && !errors.Is(err, os.ErrNotExist) {
				result = errors.Join(result, err)
			}
		}
		cancel()
		if completed == nil {
			select {
			case o := <-done:
				completed = &o
			case <-time.After(6 * time.Second):
				result = errors.Join(result, errors.New("blocked Create did not finish after unfreeze/cancel"))
			}
		}
		if completed != nil {
			if completed.err == nil {
				s.allocs = append(s.allocs, completed.o)
				result = errors.Join(result, errors.New("stalled boot unexpectedly became ready"))
			} else if err := cleanObservation(completed.o); err != nil {
				result = errors.Join(result, err)
			}
		}
	}()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for frozen == "" {
		select {
		case o := <-done:
			completed = &o
			return nil, fmt.Errorf("second Create finished before freeze: %v", o.err)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			paths, _ := filepath.Glob(filepath.Join(s.o.StateDir, "primary", "allocations", "*", "record.json"))
			for _, path := range paths {
				data, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				var record struct {
					Observation sandbox.Observation `json:"observation"`
				}
				if json.Unmarshal(data, &record) != nil || record.Observation.Allocation != bID {
					continue
				}
				cg := record.Observation.CgroupPath
				// Freezing a child before exec can suspend Go's shared-VM
				// fork/vfork handshake and stall the host process itself.
				// The API socket proves Firecracker completed exec; guest
				// readiness still remains blocked by this operator fault.
				if !operatorVMStarted(ctx, record.Observation) {
					continue
				}
				if err = os.WriteFile(filepath.Join(cg, "cgroup.freeze"), []byte("1"), 0600); err == nil {
					frozen = cg
					break
				} else if !errors.Is(err, os.ErrNotExist) {
					return nil, err
				}
			}
		}
	}
	frozenConfirmed := false
	deadline := a.ExpiresAt.Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case o := <-done:
			completed = &o
			return nil, fmt.Errorf("frozen Create finished while awaiting independent expiry: %v", o.err)
		case <-s.ctx.Done():
			return nil, s.ctx.Err()
		case <-ticker.C:
			events, _ := os.ReadFile(filepath.Join(frozen, "cgroup.events"))
			if strings.Contains(string(events), "frozen 1") {
				frozenConfirmed = true
			}
			_, cgErr := os.Stat(a.CgroupPath)
			_, rootErr := os.Stat(a.RootDir)
			if errors.Is(cgErr, os.ErrNotExist) && errors.Is(rootErr, os.ErrNotExist) {
				if !frozenConfirmed {
					return nil, errors.New("stalled cgroup was not confirmed frozen")
				}
				if time.Now().Before(a.ExpiresAt.Add(-2 * time.Second)) {
					return nil, errors.New("independent VM terminated unexpectedly early")
				}
				return map[string]any{"boot_cgroup_frozen": true, "freeze_after_instance_start": true, "firecracker_executable_verified": true, "independent_ttl_cleanup": true, "cleanup_delay_ms": time.Since(a.ExpiresAt).Milliseconds()}, nil
			}
		}
	}
	return nil, errors.New("unrelated expiry/cleanup was blocked by a stalled Create")
}

// operatorVMStarted checks the operator's Firecracker API and its owned cgroup.
// Waiting for Running avoids freezing both the pre-exec Go child and an API
// request whose shorter timeout would finish before the independent TTL.
func operatorVMStarted(ctx context.Context, o sandbox.Observation) bool {
	api := filepath.Join(o.RootDir, "api.sock")
	if _, err := os.Stat(api); err != nil {
		return false
	}
	procs, err := os.ReadFile(filepath.Join(o.CgroupPath, "cgroup.procs"))
	if err != nil {
		return false
	}
	verified := false
	for _, pid := range strings.Fields(string(procs)) {
		executable, err := os.Readlink("/proc/" + pid + "/exe")
		if err == nil && filepath.Base(executable) == "firecracker" {
			verified = true
			break
		}
	}
	if !verified {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", api)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://firecracker/", nil)
	if err != nil {
		return false
	}
	response, err := client.Do(req)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	var info struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&info) != nil {
		return false
	}
	return info.ID == filepath.Base(o.CgroupPath) && info.State == "Running"
}
