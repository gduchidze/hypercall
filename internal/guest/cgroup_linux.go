package guest

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func validateWorkloadCgroup(path string, trusted bool) error {
	if path == "" && trusted {
		return nil
	}
	if !filepath.IsAbs(path) {
		return errors.New("production workload cgroup must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return errors.New("workload cgroup must be a protected directory")
	}
	if owner, ok := info.Sys().(*syscall.Stat_t); !ok || owner.Uid != 0 {
		return errors.New("workload cgroup must be root owned")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil || fs.Type != unix.CGROUP2_SUPER_MAGIC {
		return errors.New("workload cgroup must use cgroup v2")
	}
	for _, name := range []string{"memory.max", "pids.max"} {
		data, err := os.ReadFile(filepath.Join(path, name))
		n, parseErr := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		if err != nil || parseErr != nil || n == 0 {
			return fmt.Errorf("workload %s must be finite and positive", name)
		}
	}
	data, err := os.ReadFile(filepath.Join(path, "cpu.max"))
	fields := strings.Fields(string(data))
	if err != nil || len(fields) != 2 || fields[0] == "max" {
		return errors.New("workload cpu.max must be finite")
	}
	for _, value := range fields {
		if n, err := strconv.ParseUint(value, 10, 64); err != nil || n == 0 {
			return errors.New("workload cpu.max must be positive")
		}
	}
	data, err = os.ReadFile(filepath.Join(path, "cgroup.subtree_control"))
	if err != nil {
		return err
	}
	controllers := " " + strings.TrimSpace(string(data)) + " "
	for _, name := range []string{"cpu", "memory", "pids"} {
		if !strings.Contains(controllers, " "+name+" ") {
			return errors.New("workload cgroup must delegate cpu, memory and pids")
		}
	}
	return nil
}

func preventPrivilegeGain() error { return unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) }

type execCgroup struct {
	path string
	fd   *os.File
}

func commandCgroup(parent string, cmd *exec.Cmd) (*execCgroup, error) {
	g := &execCgroup{}
	if parent == "" {
		return g, nil
	}
	path, err := os.MkdirTemp(parent, "exec-")
	if err != nil {
		return nil, err
	}
	g.path = path
	// Workloads may inspect their resource counters, but all control files
	// remain root owned and only root can add/remove cgroup directories.
	if err := os.Chmod(path, 0755); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	fd, err := os.Open(path)
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	g.fd = fd
	// CLONE_INTO_CGROUP makes admission atomic: no workload instruction runs
	// before containment, and descendants inherit membership across setsid.
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(fd.Fd())
	return g, nil
}

func (g *execCgroup) kill() error {
	if g.path == "" {
		return nil
	}
	return os.WriteFile(filepath.Join(g.path, "cgroup.kill"), []byte("1"), 0)
}

func (g *execCgroup) close() error {
	if g.path == "" {
		return nil
	}
	defer g.fd.Close()
	if err := g.kill(); err != nil {
		return err
	}
	deadline := time.Now().Add(time.Second)
	for {
		err := os.Remove(g.path)
		if err == nil || !errors.Is(err, unix.EBUSY) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
}
