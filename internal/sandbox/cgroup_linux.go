//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (b *Backend) createCgroup(h *instance) (*os.File, error) {
	token := h.record.Token
	if err := b.cgroot.Mkdir(token, 0700); err != nil {
		return nil, err
	}
	for _, item := range []struct{ name, value string }{
		{"memory.max", strconv.FormatUint(uint64(h.record.Limits.MemoryMiB+128)<<20, 10)},
		{"memory.swap.max", "0"},
		{"memory.oom.group", "1"},
		{"pids.max", "64"},
		{"cpu.max", fmt.Sprintf("%d 100000", h.record.Limits.VCPUs*100000)},
	} {
		if err := b.cgroot.WriteFile(filepath.Join(token, item.name), []byte(item.value), 0600); err != nil {
			return nil, fmt.Errorf("set cgroup %s: %w", item.name, err)
		}
	}
	return b.cgroot.Open(token)
}
func (b *Backend) populated(token string) (bool, error) {
	data, err := b.cgroot.ReadFile(filepath.Join(token, "cgroup.events"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "populated 1" {
			return true, nil
		}
	}
	return false, nil
}
func (b *Backend) killCgroup(token string) error {
	if !validToken(token) {
		return errors.New("invalid cgroup owner token")
	}
	_, err := b.cgroot.Stat(token)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// cgroup.kill is atomic over the cgroup's processes and descendants. No
	// PID from a state file is ever signalled, including during crash recovery.
	if err = b.cgroot.WriteFile(filepath.Join(token, "cgroup.kill"), []byte("1"), 0600); err != nil {
		return fmt.Errorf("kill owned cgroup: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, e := b.populated(token)
		if e != nil {
			return e
		}
		if !p {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("owned cgroup remained populated after kill")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return b.cgroot.Remove(token)
}
