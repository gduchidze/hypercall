//go:build linux

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
	"github.com/gduchidze/hypercall/internal/harness"
	"github.com/gduchidze/hypercall/internal/snapshot"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

type restoreSpec struct {
	checkpoint, reference, memory, state, disk string
	admitted                                   bool
	token                                      string
}

func (b *Backend) lockSnapshot(ctx context.Context) error {
	for !b.snapshotMu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		b.snapshotMu.Unlock()
		return err
	}
	return nil
}

func (b *Backend) reserveEpoch(a Allocation, retry bool) error {
	if retry && b.fences[fenceFor(a).name()] == a.Epoch {
		return nil
	}
	return b.advanceFence(a)
}
func (b *Backend) releaseSnapshotReference(ctx context.Context, r record) error {
	if r.CheckpointID == "" {
		return nil
	}
	if b.cfg.SnapshotStore == nil {
		return errors.New("snapshot store required to release recovered reference")
	}
	err := b.cfg.SnapshotStore.ReleaseReference(ctx, snapshot.Scope{TenantID: r.Observation.Allocation.TenantID, ProjectID: r.Observation.Allocation.ProjectID, CheckpointID: r.CheckpointID}, r.SnapshotReference)
	if errors.Is(err, snapshot.ErrNotFound) {
		return nil
	}
	return err
}
func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func (b *Backend) compatibility(l Limits, session *harnessv1.HarnessSession) (snapshot.Compatibility, error) {
	c := b.compatibilityBase
	if session != nil {
		c.BarrierProtocolVersion = harness.Protocol
		c.RNGPolicy = "host256-rndaddentropy-rndreseedcrng-python-session-v1"
	}
	c.VCPUs = l.VCPUs
	c.MemoryMiB = l.MemoryMiB
	kernel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return c, err
	}
	c.HostKernelRelease = strings.TrimSpace(string(kernel))
	cpu, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return c, err
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(cpu), "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.Join(strings.Fields(parts[1]), " ")
		switch key {
		case "vendor_id", "cpu family", "model", "model name", "stepping", "flags", "Features", "CPU implementer", "CPU architecture", "CPU variant", "CPU part", "CPU revision":
			if old, ok := fields[key]; ok && old != value {
				return c, errors.New("heterogeneous host CPUs are not supported")
			}
			fields[key] = value
		}
	}
	if len(fields) == 0 {
		return c, errors.New("host CPU identity unavailable")
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var identity strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&identity, "%s=%s;", key, fields[key])
	}
	c.CPUIdentity = digest([]byte(identity.String()))
	config, _ := json.Marshal(struct {
		CPU   string
		VCPUs uint32
		SMT   bool
	}{c.CPUIdentity, l.VCPUs, false})
	c.CPUConfigSHA256 = digest(config)
	// Capture the complete admitted workload limits; restore cannot silently
	// inherit weaker PID/disk/CPU settings from the source guest kernel.
	barrier, _ := json.Marshal(struct {
		WorkspaceBytes int64
		PIDs           uint32
	}{l.WorkspaceBytes, l.PIDs})
	c.BarrierConfigSHA256 = digest(barrier)
	if runtime.GOARCH == "arm64" {
		c.Architecture = "aarch64"
	} else {
		c.Architecture = "x86_64"
	}
	return c, nil
}
func (b *Backend) StartControlled(ctx context.Context, a Allocation, counter int64) (*guestv1.ControlledState, error) {
	h, e := b.guest(a)
	if e != nil {
		return nil, e
	}
	defer h.rpcMu.RUnlock()
	return h.client.StartControlled(authContext(ctx, h.credential), &guestv1.StartControlledRequest{Counter: counter})
}
func (b *Backend) InspectControlled(ctx context.Context, a Allocation) (*guestv1.ControlledState, error) {
	h, e := b.guest(a)
	if e != nil {
		return nil, e
	}
	defer h.rpcMu.RUnlock()
	return h.client.InspectControlled(authContext(ctx, h.credential), &guestv1.InspectControlledRequest{})
}
func (b *Backend) ContinueControlled(ctx context.Context, a Allocation, delta int64) (*guestv1.ControlledState, error) {
	h, e := b.guest(a)
	if e != nil {
		return nil, e
	}
	defer h.rpcMu.RUnlock()
	return h.client.ContinueControlled(authContext(ctx, h.credential), &guestv1.ContinueControlledRequest{Delta: delta})
}

func firecrackerClient(root string) (*http.Client, func()) {
	t := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(root, "api.sock"))
	}}
	return &http.Client{Transport: t, Timeout: 30 * time.Second}, t.CloseIdleConnections
}
func connectGuest(h *instance) error {
	conn, err := grpc.NewClient("passthrough:///guest", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return DialVSock(ctx, h.record.Observation.VSockPath)
	}), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize((1<<20)+(64<<10)), grpc.MaxCallSendMsgSize((1<<20)+(64<<10))))
	if err != nil {
		return err
	}
	h.conn = conn
	h.client = guestv1.NewGuestClient(conn)
	return nil
}
func (b *Backend) Capture(ctx context.Context, r CaptureRequest) (manifest snapshot.Manifest, result error) {
	if !identifier.MatchString(r.CheckpointID) || (r.ParentCheckpointID != "" && !identifier.MatchString(r.ParentCheckpointID)) {
		return manifest, ErrInvalid
	}
	if b.cfg.SnapshotStore == nil {
		return manifest, fmt.Errorf("%w: snapshot store required", ErrUnavailable)
	}
	if err := b.lockSnapshot(ctx); err != nil {
		return manifest, err
	}
	defer b.snapshotMu.Unlock()
	h, err := b.get(r.Allocation)
	if err != nil {
		return manifest, err
	}
	captureCtx, captureCancel := context.WithCancel(ctx)
	stopCapture := context.AfterFunc(h.lifetime, captureCancel)
	defer captureCancel()
	defer stopCapture()
	ctx = captureCtx
	for !h.rpcMu.TryLock() {
		select {
		case <-ctx.Done():
			return manifest, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer h.rpcMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cleaned || h.record.Observation.State != StateRunning {
		return manifest, ErrUnavailable
	}
	compatibility, err := b.compatibility(h.record.Limits, h.record.HarnessSession)
	if err != nil {
		return manifest, err
	}
	capability, capabilityErr := h.client.GetInfo(authContext(ctx, h.credential), &guestv1.GetInfoRequest{})
	if capabilityErr != nil {
		return manifest, capabilityErr
	}
	if capability.ServerTimeUnixNano <= 0 {
		return manifest, fmt.Errorf("%w: guest lacks authenticated UTC renewal protocol", ErrUnavailable)
	}
	captured, captureErr := h.client.PrepareCapture(authContext(ctx, h.credential), &guestv1.PrepareCaptureRequest{})
	err = captureErr
	if err != nil {
		return manifest, err
	}
	client, closeClient := firecrackerClient(h.record.Observation.RootDir)
	defer closeClient()
	paused := false
	defer func() {
		// A parent also loses captured vsock connections. Resume/reconnect and verify
		// its immutable session before reopening workload access; otherwise kill it.
		if h.lifetime.Err() != nil || !time.Now().Before(h.record.Observation.ExpiresAt) {
			result = errors.Join(result, h.lifetime.Err(), b.stopLocked(h))
			return
		}
		cleanupCtx, cancel := context.WithTimeout(h.lifetime, 15*time.Second)
		defer cancel()
		if paused {
			if e := firecrackerRequest(cleanupCtx, client, http.MethodPatch, "/vm", map[string]any{"state": "Resumed"}); e != nil {
				result = errors.Join(result, e, b.stopLocked(h))
				return
			}
		}
		if h.conn != nil {
			_ = h.conn.Close()
		}
		if e := connectGuest(h); e != nil {
			result = errors.Join(result, e, b.stopLocked(h))
			return
		}
		e := waitFor(cleanupCtx, func() error {
			_, releaseErr := h.client.ReleaseCapture(authContext(cleanupCtx, h.credential), &guestv1.ReleaseCaptureRequest{HostUnixNano: time.Now().UnixNano()})
			// A successful release reply may be lost. GetInfo is unavailable at
			// a closed gate, so this verifies reopening without mutating UTC on
			// an already running parent during an idempotent retry.
			info, infoErr := h.client.GetInfo(authContext(cleanupCtx, h.credential), &guestv1.GetInfoRequest{})
			if infoErr != nil {
				return errors.Join(releaseErr, infoErr)
			}
			if info.ProtocolVersion != "hypercall.guest.v1" || !proto.Equal(info.Identity, h.record.Observation.Allocation.identity()) || info.MaxFileBytes != 1<<20 || info.MaxExecOutputBytes != 1<<20 || info.ServerTimeUnixNano <= 0 || time.Since(time.Unix(0, info.ServerTimeUnixNano)) > 2*time.Second || time.Until(time.Unix(0, info.ServerTimeUnixNano)) > 2*time.Second {
				return errors.New("parent session or UTC renewal acknowledgement mismatch")
			}
			return nil
		})
		if e != nil {
			result = errors.Join(result, e, b.stopLocked(h))
		}
	}()
	if err = firecrackerRequest(ctx, client, http.MethodPatch, "/vm", map[string]any{"state": "Paused"}); err != nil {
		return manifest, err
	}
	paused = true
	root := h.record.Observation.RootDir
	for _, name := range []string{"capture-memory", "capture-vmstate", "capture-disk"} {
		if err = os.Remove(filepath.Join(root, name)); err != nil && !os.IsNotExist(err) {
			return manifest, err
		}
	}
	defer func() {
		for _, name := range []string{"capture-memory", "capture-vmstate", "capture-disk"} {
			_ = os.Remove(filepath.Join(root, name))
		}
	}()
	if err = firecrackerPut(ctx, client, "/snapshot/create", map[string]any{"snapshot_type": "Full", "snapshot_path": "/capture-vmstate", "mem_file_path": "/capture-memory"}); err != nil {
		return manifest, err
	}
	if err = copyFile(ctx, filepath.Join(root, "workspace.ext4"), filepath.Join(root, "capture-disk"), 0, 0); err != nil {
		return manifest, err
	}
	intentPath, intentErr := b.writeCaptureIntent(snapshot.Scope{TenantID: r.Allocation.TenantID, ProjectID: r.Allocation.ProjectID, CheckpointID: r.CheckpointID})
	if intentErr != nil {
		return manifest, intentErr
	}
	manifest, err = b.cfg.SnapshotStore.Publish(ctx, snapshot.PublishRequest{SemanticBarrier: semanticFromWire(captured.SemanticBarrier), Scope: snapshot.Scope{TenantID: r.Allocation.TenantID, ProjectID: r.Allocation.ProjectID, CheckpointID: r.CheckpointID}, ParentCheckpointID: r.ParentCheckpointID, Compatibility: compatibility, Sources: []snapshot.Source{{Kind: snapshot.Memory, Path: filepath.Join(root, "capture-memory")}, {Kind: snapshot.VMState, Path: filepath.Join(root, "capture-vmstate")}, {Kind: snapshot.Disk, Path: filepath.Join(root, "capture-disk")}}})
	if err == nil {
		err = b.clearCaptureIntent(intentPath)
	}
	b.cfg.Logger.InfoContext(ctx, "sandbox snapshot capture", "sandbox_id", r.Allocation.SandboxID, "checkpoint_id", r.CheckpointID, "success", err == nil)
	return manifest, err
}
