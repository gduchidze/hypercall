//go:build linux

package hostdoctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func platformChecks(ctx context.Context, options Options) []Check {
	return []Check{checkKVM(), checkCgroups(options.CgroupDir), checkNamespaces(ctx)}
}

func checkKVM() Check {
	const id = "host.kvm"
	file, err := os.OpenFile("/dev/kvm", os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return blocked(id, "open /dev/kvm read/write: "+err.Error())
	}
	defer file.Close()
	// KVM_GET_API_VERSION is _IO(KVMIO, 0x00) from linux/kvm.h. The stable
	// userspace ABI specifies version 12 on both supported Linux architectures.
	version, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), 0xAE00, 0)
	if errno != 0 {
		return blocked(id, "KVM_GET_API_VERSION: "+errno.Error())
	}
	check := passed(id, "opened /dev/kvm read/write and verified KVM API version 12")
	if version != 12 {
		check = blocked(id, fmt.Sprintf("KVM API version %d; require 12", version))
	}
	check.Evidence = map[string]string{"api_version": strconv.FormatUint(uint64(version), 10)}
	return check
}

func checkCgroups(dir string) Check {
	const id = "host.cgroups"
	if !filepath.IsAbs(dir) {
		return blocked(id, "--cgroup-dir must be an absolute delegated cgroups v2 directory")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return blocked(id, "stat cgroup filesystem: "+err.Error())
	}
	const cgroup2SuperMagic = 0x63677270
	if uint64(fs.Type) != cgroup2SuperMagic {
		return blocked(id, "configured directory is not on cgroups v2")
	}
	controllers, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		return blocked(id, "read available cgroup controllers: "+err.Error())
	}
	enabled, err := os.ReadFile(filepath.Join(dir, "cgroup.subtree_control"))
	if err != nil {
		return blocked(id, "read delegated cgroup controllers: "+err.Error())
	}
	for _, required := range []string{"cpu", "memory", "pids"} {
		if !containsWord(string(controllers), required) || !containsWord(string(enabled), required) {
			check := blocked(id, "delegate and enable cpu, memory and pids controllers in the configured directory; doctor does not modify the parent")
			check.Evidence = map[string]string{"directory": dir, "available": strings.TrimSpace(string(controllers)), "enabled": strings.TrimSpace(string(enabled))}
			return check
		}
	}
	// Empty transient child proves delegation for this process without moving
	// processes or altering the parent's controller configuration.
	child, err := os.MkdirTemp(dir, "hypercall-doctor-")
	if err != nil {
		return blocked(id, "create delegated cgroup child: "+err.Error())
	}
	var probeErr error
	for _, name := range []string{"cgroup.procs", "cpu.max", "memory.max", "pids.max"} {
		file, err := os.OpenFile(filepath.Join(child, name), os.O_WRONLY|syscall.O_CLOEXEC, 0)
		if err != nil {
			probeErr = fmt.Errorf("open child %s for writing: %w", name, err)
			break
		}
		if err := file.Close(); err != nil {
			probeErr = err
			break
		}
	}
	if err := os.Remove(child); err != nil {
		return blocked(id, fmt.Sprintf("remove empty cgroup probe %s: %v (probe error: %v)", child, err, probeErr))
	}
	if probeErr != nil {
		return blocked(id, probeErr.Error())
	}
	check := passed(id, "cgroups v2 cpu/memory/pids delegated; created and removed an empty writable child")
	check.Evidence = map[string]string{"directory": dir, "available": strings.TrimSpace(string(controllers)), "enabled": strings.TrimSpace(string(enabled))}
	return check
}

func containsWord(list, word string) bool {
	for _, candidate := range strings.Fields(list) {
		if candidate == word {
			return true
		}
	}
	return false
}

func checkNamespaces(ctx context.Context) Check {
	const id = "host.namespaces"
	// A child process owns all probe namespaces; the doctor process and host
	// retain their namespaces. This exercises clone permissions, not just paths.
	cmd := exec.CommandContext(ctx, "/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWNET | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS}
	if err := cmd.Run(); err != nil {
		return blocked(id, "create isolated mount/network/PID/IPC/UTS namespace child: "+err.Error())
	}
	return passed(id, "created and reaped child in new mount/network/PID/IPC/UTS namespaces")
}
