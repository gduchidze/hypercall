//go:build linux

package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"github.com/gduchidze/hypercall/internal/snapshot"
	"google.golang.org/grpc"
)

type Backend struct {
	mu                sync.Mutex
	snapshotMu        sync.Mutex
	compatibilityBase snapshot.Compatibility
	ownerID           string
	cfg               Config
	root              *os.Root
	cgroot            *os.Root
	lock              *os.File
	assets            assets
	assetDir          string
	active            map[Allocation]*instance
	stopped           map[Allocation]Observation
	fences            map[string]uint64
	closed            bool
}
type instance struct {
	rpcMu      sync.RWMutex
	restore    *restoreSpec
	mu         sync.Mutex
	cleaned    bool
	lifetime   context.Context
	record     record
	credential string
	conn       *grpc.ClientConn
	client     guestv1.GuestClient
	cancel     context.CancelFunc
	done       chan struct{}
}
type record struct {
	SchemaVersion           int         `json:"schema_version"`
	CheckpointID            string      `json:"checkpoint_id,omitempty"`
	SnapshotReference       string      `json:"snapshot_reference,omitempty"`
	PendingReferenceRelease bool        `json:"pending_reference_release,omitempty"`
	Token                   string      `json:"token"`
	Observation             Observation `json:"observation"`
	Limits                  Limits      `json:"limits"`
}

func New(ctx context.Context, c Config) (backend *Backend, result error) {
	if err := validateConfig(c); err != nil {
		return nil, err
	}
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("%w: privileged Linux host required", ErrUnavailable)
	}
	for _, p := range []string{c.ArtifactConfigPath, c.StateDir, c.CgroupDir} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return nil, fmt.Errorf("%w: absolute clean paths required", ErrInvalid)
		}
	}
	if !strings.HasPrefix(c.CgroupDir, "/sys/fs/cgroup/") {
		return nil, fmt.Errorf("%w: cgroup parent must be below /sys/fs/cgroup", ErrInvalid)
	}
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		return nil, err
	}
	for _, p := range []string{c.StateDir, c.CgroupDir} {
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil || resolved != p {
			return nil, fmt.Errorf("%w: state/cgroup paths cannot contain symlinks", ErrInvalid)
		}
	}
	st, err := os.Stat(c.StateDir)
	if err != nil {
		return nil, err
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || st.Mode().Perm() != 0700 {
		return nil, fmt.Errorf("%w: state directory must be root-owned 0700", ErrInvalid)
	}
	root, err := os.OpenRoot(c.StateDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			root.Close()
		}
	}()
	lock, err := root.OpenFile(".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			lock.Close()
		}
	}()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("%w: state directory already owned", ErrConflict)
	}
	var fs syscall.Statfs_t
	if err = syscall.Statfs(c.CgroupDir, &fs); err != nil {
		return nil, err
	}
	if uint64(fs.Type) != 0x63677270 {
		return nil, fmt.Errorf("%w: cgroups v2 required", ErrUnavailable)
	}
	cgroot, err := os.OpenRoot(c.CgroupDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			cgroot.Close()
		}
	}()
	enabled, err := cgroot.ReadFile("cgroup.subtree_control")
	if err != nil {
		return nil, err
	}
	for _, word := range []string{"cpu", "memory", "pids"} {
		if !hasWord(string(enabled), word) {
			return nil, fmt.Errorf("%w: cpu memory pids must be delegated", ErrUnavailable)
		}
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	b := &Backend{cfg: c, root: root, cgroot: cgroot, lock: lock, active: make(map[Allocation]*instance), stopped: make(map[Allocation]Observation), fences: make(map[string]uint64)}
	if err = root.MkdirAll("allocations", 0700); err != nil {
		return nil, err
	}
	if err = root.MkdirAll("jails", 0700); err != nil {
		return nil, err
	}
	if err = root.MkdirAll("materialized", 0700); err != nil {
		return nil, err
	}
	if err = root.MkdirAll("captures", 0700); err != nil {
		return nil, err
	}
	if err = root.MkdirAll("operations", 0700); err != nil {
		return nil, err
	}
	if err = root.MkdirAll("fences", 0700); err != nil {
		return nil, err
	}
	if err = b.loadSnapshotOwner(); err != nil {
		return nil, err
	}
	if err = b.loadFences(); err != nil {
		return nil, err
	}
	if dir, e := root.Open("."); e != nil {
		return nil, e
	} else {
		e = dir.Sync()
		_ = dir.Close()
		if e != nil {
			return nil, e
		}
	}
	if _, err = b.Recover(ctx); err != nil {
		return nil, err
	}
	// Asset directories contain verified immutable cache copies and no processes.
	entries, err := readDir(root, ".")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "assets-") {
			if err = root.RemoveAll(entry.Name()); err != nil {
				return nil, err
			}
		}
	}
	if err = b.stage(ctx); err != nil {
		if b.assetDir != "" {
			_ = root.RemoveAll(filepath.Base(b.assetDir))
		}
		return nil, err
	}
	return b, nil
}
func hasWord(s, w string) bool {
	for _, v := range strings.Fields(s) {
		if v == w {
			return true
		}
	}
	return false
}
func (b *Backend) authorize(a Allocation) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if a.TenantID != b.cfg.Scope.TenantID || a.ProjectID != b.cfg.Scope.ProjectID {
		return ErrNotFound
	}
	return nil
}
func (b *Backend) get(a Allocation) (*instance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}
	if err := b.authorize(a); err != nil {
		return nil, err
	}
	h, ok := b.active[a]
	if !ok {
		return nil, ErrNotFound
	}
	return h, nil
}

func (b *Backend) Create(ctx context.Context, r CreateRequest) (Observation, error) {
	return b.create(ctx, r, nil, false)
}
func (b *Backend) create(ctx context.Context, r CreateRequest, restored *restoreSpec, retry bool) (o Observation, result error) {
	if err := r.Validate(); err != nil {
		return o, err
	}
	if err := b.authorize(r.Allocation); err != nil {
		return o, err
	}
	if err := ctx.Err(); err != nil {
		return o, err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return o, err
	}
	token := "hc-" + hex.EncodeToString(raw)
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return o, err
	}
	now := time.Now().UTC()
	expiresAt, expiryErr := r.Deadline(now)
	if expiryErr != nil {
		return o, expiryErr
	}
	o = Observation{Allocation: r.Allocation, State: StateStarting, CreatedAt: now, ExpiresAt: expiresAt, CgroupPath: filepath.Join(b.cfg.CgroupDir, token), RootDir: filepath.Join(b.cfg.StateDir, "jails", "firecracker", token, "root")}
	o.VSockPath = filepath.Join(o.RootDir, "vsock.sock")
	lifetime, cancel := context.WithDeadline(context.Background(), o.ExpiresAt)
	h := &instance{restore: restored, record: record{SchemaVersion: 1, Token: token, Observation: o, Limits: r.Limits}, credential: hex.EncodeToString(secret), lifetime: lifetime, cancel: cancel, done: make(chan struct{})}
	if restored != nil {
		h.record.CheckpointID = restored.checkpoint
		h.record.SnapshotReference = restored.reference
	}
	// Reservation alone holds the global lock. No VM boot, RPC or process wait
	// may block another allocation's TTL enforcement or cancellation.
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		cancel()
		return o, ErrClosed
	}
	if len(b.active) >= 8 {
		b.mu.Unlock()
		cancel()
		return o, fmt.Errorf("%w: host concurrency exhausted", ErrUnavailable)
	}
	for a := range b.active {
		if a.TenantID == r.Allocation.TenantID && a.ProjectID == r.Allocation.ProjectID && a.SandboxID == r.Allocation.SandboxID {
			b.mu.Unlock()
			cancel()
			return o, ErrConflict
		}
	}
	if _, ok := b.stopped[r.Allocation]; ok && !retry {
		b.mu.Unlock()
		cancel()
		return o, ErrConflict
	}
	if err := b.reserveEpoch(r.Allocation, retry); err != nil {
		b.mu.Unlock()
		cancel()
		return o, err
	}
	if retry {
		delete(b.stopped, r.Allocation)
	}
	h.mu.Lock()
	b.active[r.Allocation] = h
	if restored != nil {
		restored.admitted = true
		restored.token = h.record.Token
	}
	b.mu.Unlock()
	defer h.mu.Unlock()
	defer func() {
		if result != nil {
			h.record.Observation.State = StateFailed
			b.cfg.Logger.WarnContext(ctx, "sandbox create failed", "sandbox_id", r.Allocation.SandboxID, "epoch", r.Allocation.Epoch, "error", result.Error())
			result = errors.Join(result, b.stopLocked(h))
			o = h.record.Observation
		}
	}()
	allocationDir := filepath.Join("allocations", token)
	if err := b.root.Mkdir(allocationDir, 0700); err != nil {
		return o, err
	}
	if err := b.writeRecord(h.record); err != nil {
		return o, err
	}
	bootCtx, bootCancel := context.WithTimeout(ctx, 45*time.Second)
	defer bootCancel()
	stopBoot := context.AfterFunc(lifetime, bootCancel)
	defer stopBoot()
	go b.monitor(h)
	if err := b.boot(bootCtx, h); err != nil {
		return o, err
	}
	if err := lifetime.Err(); err != nil {
		return o, err
	}
	h.record.Observation.State = StateRunning
	if err := b.writeRecord(h.record); err != nil {
		return o, err
	}
	b.cfg.Logger.InfoContext(ctx, "sandbox running", "tenant_id", r.Allocation.TenantID, "project_id", r.Allocation.ProjectID, "sandbox_id", r.Allocation.SandboxID, "epoch", r.Allocation.Epoch, "boot_ms", time.Since(now).Milliseconds())
	return h.record.Observation, nil
}

func (b *Backend) monitor(h *instance) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-h.lifetime.Done():
			if err := b.stop(h, false); err != nil {
				b.cfg.Logger.Error("sandbox cleanup failed", "sandbox_id", h.record.Observation.Allocation.SandboxID)
			}
			return
		case <-ticker.C:
			// Do not wait for a starting VM's mutex: its independent lifetime timer
			// already cancels boot and no other VM's monitor is blocked by it.
			if !h.rpcMu.TryRLock() {
				continue
			}
			if !h.mu.TryLock() {
				h.rpcMu.RUnlock()
				continue
			}
			if h.cleaned {
				h.mu.Unlock()
				h.rpcMu.RUnlock()
				return
			}
			running := h.record.Observation.State == StateRunning
			if !running {
				h.mu.Unlock()
				h.rpcMu.RUnlock()
				continue
			}
			populated, err := b.populated(h.record.Token)
			client := h.client
			h.mu.Unlock()
			if err == nil && populated {
				ctx, cancel := context.WithTimeout(authContext(h.lifetime, h.credential), time.Second)
				_, err = client.GetInfo(ctx, &guestv1.GetInfoRequest{})
				cancel()
			}
			h.rpcMu.RUnlock()
			if err != nil || !populated {
				memoryEvents, _ := b.cgroot.ReadFile(filepath.Join(h.record.Token, "memory.events"))
				b.cfg.Logger.Warn("sandbox health failed", "sandbox_id", h.record.Observation.Allocation.SandboxID, "error", err, "populated", populated, "memory_events", strings.TrimSpace(string(memoryEvents)))
				if e := b.stop(h, true); e != nil {
					b.cfg.Logger.Error("sandbox failure cleanup failed", "sandbox_id", h.record.Observation.Allocation.SandboxID)
				}
				return
			}
		}
	}
}
func (b *Backend) writeRecord(r record) error {
	dir := filepath.Join("allocations", r.Token)
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := b.root.OpenFile(filepath.Join(dir, "record.tmp"), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = b.root.Rename(filepath.Join(dir, "record.tmp"), filepath.Join(dir, "record.json")); err != nil {
		return err
	}
	d, err := b.root.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err = d.Sync(); err != nil {
		return err
	}
	parent, err := b.root.Open("allocations")
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
func (b *Backend) Inspect(ctx context.Context, a Allocation) (Observation, error) {
	if err := ctx.Err(); err != nil {
		return Observation{}, err
	}
	if err := b.authorize(a); err != nil {
		return Observation{}, err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return Observation{}, ErrClosed
	}
	if o, ok := b.stopped[a]; ok {
		b.mu.Unlock()
		return o, nil
	}
	h := b.active[a]
	b.mu.Unlock()
	if h == nil {
		return Observation{}, ErrNotFound
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cleaned {
		return h.record.Observation, nil
	}
	populated, err := b.populated(h.record.Token)
	if err != nil || !populated || !time.Now().Before(h.record.Observation.ExpiresAt) {
		h.record.Observation.State = StateFailed
		if e := b.stopLocked(h); e != nil {
			return h.record.Observation, e
		}
	}
	return h.record.Observation, nil
}
func (b *Backend) Stop(ctx context.Context, a Allocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.authorize(a); err != nil {
		return err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrClosed
	}
	if _, ok := b.stopped[a]; ok {
		b.mu.Unlock()
		return nil
	}
	h := b.active[a]
	b.mu.Unlock()
	if h == nil {
		return ErrNotFound
	}
	return b.stop(h, false)
}
func (b *Backend) stop(h *instance, failed bool) error {
	h.cancel()
	h.mu.Lock()
	defer h.mu.Unlock()
	if failed {
		h.record.Observation.State = StateFailed
	}
	return b.stopLocked(h)
}

// stopLocked holds only the allocation mutex; unrelated VM lifetimes proceed.
func (b *Backend) stopLocked(h *instance) error {
	if h.cleaned {
		return nil
	}
	h.cancel()
	if h.conn != nil {
		_ = h.conn.Close()
	}
	if err := b.killCgroup(h.record.Token); err != nil {
		return err
	}
	if err := b.root.RemoveAll(filepath.Join("jails", "firecracker", h.record.Token)); err != nil {
		return err
	}
	if err := b.purgeAllocationFiles(h.record.Token); err != nil {
		return err
	}
	if err := b.releaseSnapshotReference(context.Background(), h.record); err != nil {
		return err
	}
	if err := b.root.RemoveAll(filepath.Join("allocations", h.record.Token)); err != nil {
		return err
	}
	if h.record.Observation.State != StateFailed {
		h.record.Observation.State = StateStopped
	}
	h.cleaned = true
	b.mu.Lock()
	delete(b.active, h.record.Observation.Allocation)
	b.rememberStopped(h.record.Observation)
	b.mu.Unlock()
	close(h.done)
	b.cfg.Logger.Info("sandbox stopped", "sandbox_id", h.record.Observation.Allocation.SandboxID, "epoch", h.record.Observation.Allocation.Epoch)
	return nil
}
func (b *Backend) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	active := make([]*instance, 0, len(b.active))
	for _, h := range b.active {
		active = append(active, h)
		h.cancel()
	}
	b.mu.Unlock()
	var errs []error
	for _, h := range active {
		errs = append(errs, b.stop(h, false))
	}
	if err := errors.Join(errs...); err != nil {
		b.mu.Lock()
		b.closed = false
		b.mu.Unlock()
		return err
	}
	if b.assetDir != "" {
		if err := b.root.RemoveAll(filepath.Base(b.assetDir)); err != nil {
			// Keep ownership and descriptors until the caller removes the fault
			// and retries Close, matching the allocation-cleanup error path.
			b.mu.Lock()
			b.closed = false
			b.mu.Unlock()
			return err
		}
	}
	return errors.Join(b.cgroot.Close(), b.root.Close(), b.lock.Close())
}

// Recover destroys only prior host-process allocations recorded below the
// locked private state directory. It never adopts a guest session after a
// restart, guesses ownership from a PID, or kills a numeric (reusable) PID.
func (b *Backend) Recover(ctx context.Context) ([]Observation, error) {
	if err := b.lockSnapshot(ctx); err != nil {
		return nil, err
	}
	defer b.snapshotMu.Unlock()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrClosed
	}
	entries, err := readDir(b.root, "allocations")
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	type orphan struct {
		record             record
		fenced, localClean bool
	}
	var prior []*orphan
	var failures []error
	allPriorFenced := true
	// Enumerate and validate before touching any prior-owner process. An invalid
	// entry remains untouched, but cannot prevent fencing other validated owners.
	for _, entry := range entries {
		token := entry.Name()
		if !validToken(token) || !entry.IsDir() {
			failures = append(failures, errors.New("unrecognized allocation state; operator intervention required"))
			allPriorFenced = false
			continue
		}
		current := false
		for _, h := range b.active {
			if h.record.Token == token {
				current = true
				break
			}
		}
		if current {
			continue
		}
		var r record
		if err = decodeFile(filepath.Join(b.cfg.StateDir, "allocations", token, "record.json"), &r); err != nil {
			pending, e := readDir(b.root, filepath.Join("allocations", token))
			if e == nil && (len(pending) == 0 || (len(pending) == 1 && pending[0].Name() == "record.tmp")) {
				if e = b.root.RemoveAll(filepath.Join("allocations", token)); e != nil {
					failures = append(failures, e)
				}
				continue
			}
			failures = append(failures, fmt.Errorf("invalid recovery record: %w", err))
			allPriorFenced = false
			continue
		}
		o := r.Observation
		referenceValid := (r.CheckpointID == "" && r.SnapshotReference == "" && !r.PendingReferenceRelease) || (identifier.MatchString(r.CheckpointID) && identifier.MatchString(r.SnapshotReference))
		stateValid := o.State == StateStarting || o.State == StateRunning || o.State == StateStopped || o.State == StateFailed
		if r.SchemaVersion != 1 || r.Token != token || o.Allocation.Validate() != nil || !referenceValid || !stateValid || o.CgroupPath != filepath.Join(b.cfg.CgroupDir, token) || o.RootDir != filepath.Join(b.cfg.StateDir, "jails", "firecracker", token, "root") || o.VSockPath != filepath.Join(o.RootDir, "vsock.sock") {
			failures = append(failures, errors.New("recovery record ownership mismatch"))
			allPriorFenced = false
			continue
		}
		prior = append(prior, &orphan{record: r})
		if b.fences[fenceFor(o.Allocation).name()] < o.Allocation.Epoch {
			if e := b.advanceFence(o.Allocation); e != nil {
				failures = append(failures, e)
			}
		}
	}
	b.mu.Unlock()
	// Local safety never waits for a cloud operation and is not cancelled halfway
	// through an already-enumerated set. Every valid old cgroup is fenced first.
	for _, old := range prior {
		if e := b.killCgroup(old.record.Token); e != nil {
			failures = append(failures, e)
			allPriorFenced = false
			continue
		}
		old.fenced = true
	}
	var out []Observation
	for _, old := range prior {
		if !old.fenced {
			continue
		}
		r := old.record
		jailErr := b.root.RemoveAll(filepath.Join("jails", "firecracker", r.Token))
		// Remove bootstrap bearers before attempting remote reference release. Keep
		// only the small ownership/reference record so a cloud outage is recoverable.
		privateErr := b.purgeAllocationFiles(r.Token)
		r.Observation.State = StateStopped
		r.Observation.PID = 0
		r.PendingReferenceRelease = r.CheckpointID != ""
		recordErr := b.writeRecord(r)
		old.record = r
		old.localClean = jailErr == nil && privateErr == nil && recordErr == nil
		failures = append(failures, jailErr, privateErr, recordErr)
		b.mu.Lock()
		b.rememberStopped(r.Observation)
		b.mu.Unlock()
		out = append(out, r.Observation)
		b.cfg.Logger.InfoContext(ctx, "sandbox recovered", "sandbox_id", r.Observation.Allocation.SandboxID, "epoch", r.Observation.Allocation.Epoch)
	}
	if allPriorFenced {
		failures = append(failures, b.recoverMaterialized())
	}
	networkFailed := false
	for _, old := range prior {
		if !old.fenced {
			continue
		}
		if e := b.releaseSnapshotReference(ctx, old.record); e != nil {
			failures = append(failures, e)
			networkFailed = true
			continue
		}
		if old.localClean {
			failures = append(failures, b.root.RemoveAll(filepath.Join("allocations", old.record.Token)))
		}
	}
	// Operation journals may contain pins from a crash before allocation intent.
	// Only reconcile them once every old process is known stopped. A failed pin
	// release remains durable and is retried by a later Recover, not immediately
	// retried through a second journal path in this same call.
	if allPriorFenced && !networkFailed {
		failures = append(failures, b.recoverSnapshotOperations(ctx))
		failures = append(failures, b.recoverCaptureIntents(ctx))
	}
	return out, errors.Join(failures...)
}

// purgeAllocationFiles preserves only the durable ownership/reference record.
// The caller must first prove that this token's cgroup is empty.
func (b *Backend) purgeAllocationFiles(token string) error {
	dir := filepath.Join("allocations", token)
	entries, err := readDir(b.root, dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if entry.Name() == "record.json" {
			continue
		}
		failures = append(failures, b.root.RemoveAll(filepath.Join(dir, entry.Name())))
	}
	return errors.Join(failures...)
}
func validToken(s string) bool {
	if len(s) != 35 || !strings.HasPrefix(s, "hc-") {
		return false
	}
	_, err := hex.DecodeString(s[3:])
	return err == nil
}

func readDir(root *os.Root, path string) ([]os.DirEntry, error) {
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

// The durable controller owns history. Keep only the latest stopped allocation
// for a sandbox so repeated executions cannot grow the host cache without bound.
func (b *Backend) rememberStopped(o Observation) {
	for a := range b.stopped {
		if a.TenantID == o.Allocation.TenantID && a.ProjectID == o.Allocation.ProjectID && a.SandboxID == o.Allocation.SandboxID {
			delete(b.stopped, a)
		}
	}
	b.stopped[o.Allocation] = o
}
