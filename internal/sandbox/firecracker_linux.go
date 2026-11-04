//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

func (b *Backend) boot(ctx context.Context, h *instance) error {
	o := &h.record.Observation
	if len(o.VSockPath) > 100 {
		return fmt.Errorf("%w: state directory path too long for Unix sockets", ErrInvalid)
	}
	if err := os.MkdirAll(o.RootDir, 0700); err != nil {
		return err
	}
	for _, item := range []struct{ src, name string }{{b.assets.kernel, "kernel"}, {b.assets.rootfs, "rootfs.ext4"}} {
		if err := copyFile(ctx, item.src, filepath.Join(o.RootDir, item.name), b.cfg.JailerUID, b.cfg.JailerGID); err != nil {
			return err
		}
	}
	allocationDir := filepath.Join(b.cfg.StateDir, "allocations", h.record.Token)
	bootstrap := filepath.Join(allocationDir, "bootstrap")
	if err := os.Mkdir(bootstrap, 0700); err != nil {
		return err
	}
	session := struct {
		HarnessSession *harnessv1.HarnessSession   `json:"harness_session,omitempty"`
		Identity       *guestv1.AllocationIdentity `json:"identity"`
		Credential     string                      `json:"credential"`
		ExpiresAt      time.Time                   `json:"expires_at"`
		Entropy        string                      `json:"entropy"`
	}{HarnessSession: h.record.HarnessSession, Identity: o.Allocation.identity(), Credential: h.credential, ExpiresAt: o.ExpiresAt}
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return err
	}
	session.Entropy = hex.EncodeToString(entropy)
	if err := writeJSON(filepath.Join(bootstrap, "session.json"), session, 0600); err != nil {
		return err
	}
	limits := struct {
		MemoryBytes    uint64 `json:"memory_bytes"`
		PIDsMax        uint32 `json:"pids_max"`
		WorkspaceBytes int64  `json:"workspace_bytes"`
		CPUQuotaUS     uint32 `json:"cpu_quota_us"`
		CPUPeriodUS    uint32 `json:"cpu_period_us"`
	}{uint64(h.record.Limits.MemoryMiB-64) << 20, h.record.Limits.PIDs, h.record.Limits.WorkspaceBytes, h.record.Limits.VCPUs * 100000, 100000}
	if err := writeJSON(filepath.Join(bootstrap, "limits.json"), limits, 0600); err != nil {
		return err
	}
	sessionDisk := filepath.Join(o.RootDir, "session.ext4")
	mkfs := "/usr/sbin/mkfs.ext4"
	if _, err := os.Stat(mkfs); err != nil {
		mkfs = "/sbin/mkfs.ext4"
	}
	cmd := exec.CommandContext(ctx, mkfs, "-q", "-F", "-t", "ext4", "-O", "^metadata_csum_seed", "-d", bootstrap, sessionDisk, "4096")
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return errors.New("create bounded bootstrap ext4 disk failed")
	}
	if err := os.Chmod(sessionDisk, 0400); err != nil {
		return err
	}
	if err := os.Chown(sessionDisk, b.cfg.JailerUID, b.cfg.JailerGID); err != nil {
		return err
	}

	if h.restore != nil {
		for _, item := range []struct{ src, name string }{{h.restore.disk, "workspace.ext4"}, {h.restore.memory, "memory"}, {h.restore.state, "vmstate"}} {
			if err := copyFile(ctx, item.src, filepath.Join(o.RootDir, item.name), b.cfg.JailerUID, b.cfg.JailerGID); err != nil {
				return err
			}
		}
		if err := os.Chmod(filepath.Join(o.RootDir, "workspace.ext4"), 0600); err != nil {
			return err
		}
	} else {
		workspace := filepath.Join(allocationDir, "workspace")
		if err := os.Mkdir(workspace, 0700); err != nil {
			return err
		}
		if err := os.Chown(workspace, 1000, 1000); err != nil {
			return err
		}
		disk := filepath.Join(o.RootDir, "workspace.ext4")
		// ext4 metadata needs room beyond the user-visible capacity; project quotas
		// are out of scope, so the filesystem itself is the bounded workspace.
		blocks := h.record.Limits.WorkspaceBytes / 1024
		if blocks < 1024 {
			blocks = 1024
		}
		cmd := exec.CommandContext(ctx, mkfs, "-q", "-F", "-t", "ext4", "-O", "^metadata_csum_seed", "-d", workspace, disk, strconv.FormatInt(blocks, 10))
		cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			return errors.New("create private workspace disk failed")
		}
		if err := os.Chmod(disk, 0600); err != nil {
			return err
		}
		if err := os.Chown(disk, b.cfg.JailerUID, b.cfg.JailerGID); err != nil {
			return err
		}
	}
	cg, err := b.createCgroup(h)
	if err != nil {
		return err
	}
	defer cg.Close()
	parent, err := filepath.Rel("/sys/fs/cgroup", o.CgroupPath)
	if err != nil || strings.HasPrefix(parent, "..") {
		return errors.New("invalid delegated cgroup path")
	}
	args := []string{"--id", h.record.Token, "--exec-file", b.assets.firecracker, "--uid", strconv.Itoa(b.cfg.JailerUID), "--gid", strconv.Itoa(b.cfg.JailerGID), "--chroot-base-dir", filepath.Join(b.cfg.StateDir, "jails"), "--cgroup-version", "2", "--parent-cgroup", parent, "--new-pid-ns", "--", "--api-sock", "/api.sock"}
	// Start jailer already inside the bounded cgroup. This closes the crash
	// window before jailer's own cgroup movement and makes recovery PID-safe.
	cmd = exec.Command(b.assets.jailer, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	diagnostics := &bootDiagnostics{credential: h.credential}
	defer diagnostics.finish()
	cmd.Stdout = diagnostics
	cmd.Stderr = diagnostics
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cg.Fd()), Cloneflags: syscall.CLONE_NEWNET | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS}
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("start jailed runtime: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	api := filepath.Join(o.RootDir, "api.sock")
	if err = waitForAPI(ctx, api, func() (bool, error) { return b.populated(h.record.Token) }); err != nil {
		return fmt.Errorf("wait for Firecracker API: %w; diagnostics: %s", err, diagnostics.finish())
	}
	// Diagnostics are retained only until authenticated readiness. Tenant
	// commands cannot begin before Create returns, and their output is never logged.
	// A successful jailer may exit after creating the new PID namespace. Its
	// child is identified for observations only; all ownership uses the cgroup.
	select {
	case e := <-wait:
		if e != nil {
			return errors.New("jailer failed")
		}
	default:
	}
	pidData, err := os.ReadFile(filepath.Join(o.RootDir, "firecracker.pid"))
	if err != nil {
		return fmt.Errorf("read jailed process identity: %w", err)
	}
	o.PID, err = strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil || o.PID <= 1 {
		return errors.New("invalid jailed process identity")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", api)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	if h.restore == nil {
		for _, call := range []struct {
			path string
			body any
		}{
			{"/machine-config", map[string]any{"vcpu_count": h.record.Limits.VCPUs, "mem_size_mib": h.record.Limits.MemoryMiB, "smt": false}},
			{"/boot-source", map[string]any{"kernel_image_path": "/kernel", "boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro rootfstype=ext4 init=/sbin/hypercall-init"}},
			{"/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": "/rootfs.ext4", "is_root_device": true, "is_read_only": true}},
			{"/drives/session", map[string]any{"drive_id": "session", "path_on_host": "/session.ext4", "is_root_device": false, "is_read_only": true}},
			{"/drives/workspace", map[string]any{"drive_id": "workspace", "path_on_host": "/workspace.ext4", "is_root_device": false, "is_read_only": false}},
			{"/vsock", map[string]any{"guest_cid": 3, "uds_path": "/vsock.sock"}},
			{"/actions", map[string]any{"action_type": "InstanceStart"}},
		} {
			if err = firecrackerPut(ctx, client, call.path, call.body); err != nil {
				return err
			}
		}
	} else {
		if err = firecrackerPut(ctx, client, "/snapshot/load", map[string]any{"snapshot_path": "/vmstate", "mem_backend": map[string]any{"backend_path": "/memory", "backend_type": "File"}, "resume_vm": false}); err != nil {
			return err
		}
		if err = firecrackerRequest(ctx, client, http.MethodPatch, "/vm", map[string]any{"state": "Resumed"}); err != nil {
			return err
		}
	}
	dialer := func(ctx context.Context, _ string) (net.Conn, error) { return DialVSock(ctx, o.VSockPath) }
	conn, err := grpc.NewClient("passthrough:///guest", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(dialer), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize((1<<20)+(64<<10)), grpc.MaxCallSendMsgSize((1<<20)+(64<<10))))
	if err != nil {
		return err
	}
	h.conn = conn
	h.client = guestv1.NewGuestClient(conn)
	if err = waitFor(ctx, func() error {
		infoCtx, cancel := context.WithTimeout(authContext(ctx, h.credential), 5*time.Second)
		defer cancel()
		live, liveErr := b.populated(h.record.Token)
		if liveErr != nil {
			return terminalBootError{liveErr}
		}
		if !live {
			return terminalBootError{errors.New("guest runtime exited during boot")}
		}

		var info *guestv1.GetInfoResponse
		var e error
		if h.restore != nil {
			info, e = h.client.RebindClone(infoCtx, &guestv1.RebindCloneRequest{HostUnixNano: time.Now().UnixNano()})
			if e != nil { // A lost rebind response may already have opened the new session.
				probe, probeErr := h.client.GetInfo(infoCtx, &guestv1.GetInfoRequest{})
				if probeErr == nil {
					info, e = probe, nil
				}
			}
		} else {
			info, e = h.client.GetInfo(infoCtx, &guestv1.GetInfoRequest{})
		}
		if e != nil {
			return e
		}
		if h.restore != nil && (info.ServerTimeUnixNano <= 0 || time.Since(time.Unix(0, info.ServerTimeUnixNano)) > 2*time.Second || time.Until(time.Unix(0, info.ServerTimeUnixNano)) > 2*time.Second) {
			return errors.New("guest UTC renewal acknowledgement outside supported bound")
		}
		if h.record.HarnessSession != nil && info.HarnessSessionHash != h.record.HarnessSession.SessionHash {
			return errors.New("guest harness session hash acknowledgement mismatch")
		}
		if info.ProtocolVersion != "hypercall.guest.v1" || !proto.Equal(info.Identity, o.Allocation.identity()) || info.MaxFileBytes != 1<<20 || info.MaxExecOutputBytes != 1<<20 {
			return errors.New("guest protocol or allocation identity mismatch")
		}
		return nil
	}); err != nil {
		return fmt.Errorf("guest authenticated readiness failed: %w; diagnostics: %s", err, diagnostics.finish())
	}
	return nil
}
func firecrackerPut(ctx context.Context, c *http.Client, path string, body any) error {
	return firecrackerRequest(ctx, c, http.MethodPut, path, body)
}
func firecrackerRequest(ctx context.Context, c *http.Client, method, path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("Firecracker %s: %w", path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("Firecracker %s rejected configuration (status %d)", path, resp.StatusCode)
	}
	return nil
}
func waitFor(ctx context.Context, f func() error) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, last)
		}
		if last = f(); last == nil {
			return nil
		}
		var terminal terminalBootError
		if errors.As(last, &terminal) {
			return terminal.err
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), last)
		case <-ticker.C:
		}
	}
}
func copyFile(ctx context.Context, src, dst string, uid, gid int) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return err
	}
	defer out.Close()
	buf := make([]byte, 1<<20)
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		n, e := in.Read(buf)
		if n > 0 {
			if _, err = out.Write(buf[:n]); err != nil {
				return err
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
	}
	if err = out.Sync(); err != nil {
		return err
	}
	return out.Chown(uid, gid)
}
func writeJSON(path string, v any, mode os.FileMode) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}
func authContext(ctx context.Context, credential string) context.Context {
	// Never forward control-plane credentials or arbitrary caller metadata into a guest.
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+credential))
}

type bootDiagnostics struct {
	mu         sync.Mutex
	data       []byte
	credential string
	finished   bool
}

func (d *bootDiagnostics) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(p)
	if !d.finished {
		const limit = 16384
		if len(p) >= limit {
			d.data = append(d.data[:0], p[len(p)-limit:]...)
		} else {
			if len(d.data)+len(p) > limit {
				d.data = append(d.data[:0], d.data[len(d.data)+len(p)-limit:]...)
			}
			d.data = append(d.data, p...)
		}
	}
	return n, nil
}

var bootHex = regexp.MustCompile(`[A-Fa-f0-9]{16,}`)

func (d *bootDiagnostics) finish() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.finished = true
	out := strings.ReplaceAll(string(d.data), d.credential, "[redacted]")
	return bootHex.ReplaceAllString(out, "[redacted]")
}

type terminalBootError struct{ err error }

func (e terminalBootError) Error() string { return e.err.Error() }
func waitForAPI(ctx context.Context, path string, populated func() (bool, error)) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		live, err := populated()
		if err != nil {
			return err
		}
		if !live {
			return errors.New("jailed runtime exited before API became ready")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
