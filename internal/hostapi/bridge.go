// Package hostapi authenticates controller assignments and adapts the host-local
// sandbox runtime. It owns no scheduling decisions or product resource state.
package hostapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"google.golang.org/grpc/status"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"github.com/gduchidze/hypercall/internal/controlplane"
	"github.com/gduchidze/hypercall/internal/harness"
	"github.com/gduchidze/hypercall/internal/sandbox"
	"github.com/gduchidze/hypercall/internal/snapshot"
)

type Config struct {
	Scope    controlplane.Scope
	StateDir string
	// Digest of the template manifest verified by sandbox.New, including guest.
	TemplateDigest        string
	Logger                *slog.Logger
	SnapshotStore         *snapshot.Store
	ControlledSnapshots   bool
	CooperativeAgents     bool
	ControlledAgentDigest string
}
type record struct {
	Version           int                          `json:"version"`
	Assignment        controlplane.Assignment      `json:"assignment"`
	RunHash           string                       `json:"run_hash"`
	Stage             string                       `json:"stage"`
	ExpiresAt         time.Time                    `json:"expires_at"`
	Mode              string                       `json:"mode,omitempty"`
	CaptureID         string                       `json:"capture_id,omitempty"`
	Checkpoint        *controlplane.CheckpointSpec `json:"checkpoint,omitempty"`
	RestoreCheckpoint *controlplane.CheckpointSpec `json:"restore_checkpoint,omitempty"`
	Create            *sandbox.CreateRequest       `json:"create,omitempty"`
	Restore           *sandbox.RestoreRequest      `json:"restore,omitempty"`
}
type Bridge struct {
	mu      sync.Mutex
	cfg     Config
	runtime sandbox.SandboxBackend
	root    *os.Root
	lock    *os.File
	workers map[string]context.CancelFunc
	closed  bool
	wg      sync.WaitGroup
}

func fail(code, msg string) error { return &controlplane.Error{Kind: code, Message: msg} }
func NewBridge(c Config, r sandbox.SandboxBackend) (*Bridge, error) {
	if r == nil || c.Scope.TenantID == "" || c.Scope.ProjectID == "" || !validDigest(c.TemplateDigest) || !filepath.IsAbs(c.StateDir) {
		return nil, fail("invalid_argument", "trusted scope, runtime, template digest and absolute ledger path required")
	}
	if e := os.MkdirAll(c.StateDir, 0700); e != nil {
		return nil, e
	}
	p, e := filepath.EvalSymlinks(c.StateDir)
	if e != nil || p != c.StateDir {
		return nil, fail("invalid_argument", "ledger path must be clean without symlinks")
	}
	st, e := os.Stat(p)
	if e != nil {
		return nil, e
	}
	owner, owned := st.Sys().(*syscall.Stat_t)
	if !owned || owner.Uid != uint32(os.Geteuid()) || st.Mode().Perm() != 0700 {
		return nil, fail("invalid_argument", "ledger must be owned by this user and private mode 0700")
	}
	root, e := os.OpenRoot(p)
	if e != nil {
		return nil, e
	}
	lock, e := root.OpenFile(".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		root.Close()
		return nil, e
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		lock.Close()
		root.Close()
		return nil, fail("failed_precondition", "ledger already owned")
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &Bridge{cfg: c, runtime: r, root: root, lock: lock, workers: map[string]context.CancelFunc{}}, nil
}
func (b *Bridge) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	for _, cancel := range b.workers {
		cancel()
	}
	b.mu.Unlock()
	b.wg.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.lock.Close()
	return errors.Join(e, b.root.Close())
}
func (b *Bridge) Supports(action string) bool {
	if action == "suspend" || action == "resume" {
		_, ok := b.runtime.(sandbox.SnapshotBackend)
		return (b.cfg.ControlledSnapshots || b.cfg.CooperativeAgents) && b.cfg.SnapshotStore != nil && validDigest(b.cfg.ControlledAgentDigest) && ok
	}
	return true
}
func (b *Bridge) SupportsRun(action string, run controlplane.RunSpec) bool {
	if action != "suspend" && action != "resume" && action != "fork" {
		return true
	}
	var t controlplane.TaskSpec
	if decode(run.Task.Spec, &t) != nil || t.AgentDigest != b.cfg.ControlledAgentDigest || !b.Supports("suspend") {
		return false
	}
	if t.ExecutionMode == "controlled_python_v1" {
		return b.cfg.ControlledSnapshots
	}
	if t.ExecutionMode == "cooperative_python_v1" {
		_, ok := b.runtime.(sandbox.AgentBackend)
		return b.cfg.CooperativeAgents && ok
	}
	return false
}
func sessionHash(a controlplane.Assignment) string {
	if a.HarnessSession != nil {
		return a.HarnessSession.SessionHash
	}
	return ""
}
func sandboxID(a controlplane.Assignment) string {
	if a.SandboxID != "" {
		return a.SandboxID
	}
	return a.BranchID
}
func validDigest(s string) bool {
	if !strings.HasPrefix(s, "sha256:") {
		return false
	}
	v, e := hex.DecodeString(strings.TrimPrefix(s, "sha256:"))
	return e == nil && len(v) == 32
}
func allocation(a controlplane.Assignment) sandbox.Allocation {
	return sandbox.Allocation{TenantID: a.Scope.TenantID, ProjectID: a.Scope.ProjectID, SandboxID: sandboxID(a), ExecutionID: a.ExecutionID, Epoch: uint64(a.Epoch)}
}
func workerKey(a controlplane.Assignment) string {
	return key(a) + "/" + a.ExecutionID + "/" + strconv.FormatInt(a.Epoch, 10)
}
func key(a controlplane.Assignment) string {
	h := sha256.Sum256([]byte(a.Scope.TenantID + "\x00" + a.Scope.ProjectID + "\x00" + a.BranchID))
	return hex.EncodeToString(h[:]) + ".json"
}
func (b *Bridge) validate(a controlplane.Assignment, live bool) error {
	if b.closed {
		return fail("unavailable", "host bridge closed")
	}
	if a.Scope != b.cfg.Scope {
		return fail("permission_denied", "assignment outside authenticated host scope")
	}
	if a.HarnessSession != nil {
		h := a.HarnessSession
		if harness.ValidateSession(h) != nil || h.TenantId != a.Scope.TenantID || h.ProjectId != a.Scope.ProjectID || h.BranchId != a.BranchID || h.SandboxId != sandboxID(a) || h.ExecutionId != a.ExecutionID || h.Epoch != a.Epoch || h.ModelDigest != a.SelectedModel.Metadata.Digest {
			return fail("invalid_argument", "immutable harness session assignment mismatch")
		}
	}
	if a.Epoch < 1 || allocation(a).Validate() != nil || a.LeaseUntil.IsZero() {
		return fail("invalid_argument", "invalid assignment identity, epoch or lease")
	}
	if live && !time.Now().Before(a.LeaseUntil) {
		return fail("failed_precondition", "execution lease expired")
	}
	return nil
}
func (b *Bridge) read(a controlplane.Assignment) (record, bool, error) {
	var r record
	p := key(a)
	st, e := b.root.Lstat(p)
	if errors.Is(e, os.ErrNotExist) {
		return r, false, nil
	}
	if e != nil {
		return r, false, e
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Size() > 8<<20 {
		return r, false, fail("failed_precondition", "invalid ledger record")
	}
	data, e := b.root.ReadFile(p)
	if e != nil {
		return r, false, e
	}
	if e = decode(data, &r); e != nil {
		return r, false, fail("failed_precondition", "invalid or unsupported host ledger schema")
	}
	if r.Version != 1 {
		return r, false, fail("failed_precondition", "unsupported host ledger version")
	}
	if r.Assignment.Scope != a.Scope || r.Assignment.BranchID != a.BranchID {
		return r, false, fail("failed_precondition", "ledger identity mismatch")
	}
	return r, true, nil
}
func (b *Bridge) save(a controlplane.Assignment, r record) error {
	r.Version = 1
	r.Assignment.Run = controlplane.RunSpec{}
	data, e := json.Marshal(r)
	if e != nil {
		return e
	}
	p := key(a)
	tmp := p + ".tmp"
	f, e := b.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(e, os.ErrExist) {
		if e = b.root.Remove(tmp); e != nil {
			return e
		}
		f, e = b.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	}
	if e != nil {
		return e
	}
	_, e = f.Write(data)
	if e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	if e = b.root.Rename(tmp, p); e != nil {
		return e
	}
	dir, e := b.root.Open(".")
	if e != nil {
		return e
	}
	return errors.Join(dir.Sync(), dir.Close())
}
func same(a, c controlplane.Assignment) bool {
	return sessionHash(a) == sessionHash(c) && a.Scope == c.Scope && a.BranchID == c.BranchID && sandboxID(a) == sandboxID(c) && a.ExecutionID == c.ExecutionID && a.Epoch == c.Epoch && a.LeaseUntil.Equal(c.LeaseUntil)
}
func (b *Bridge) existing(a controlplane.Assignment) (record, bool, error) {
	r, ok, e := b.read(a)
	if e != nil || !ok {
		return r, ok, e
	}
	if a.Epoch < r.Assignment.Epoch || a.Epoch == r.Assignment.Epoch && !same(a, r.Assignment) {
		return r, ok, fail("aborted", "stale epoch or changed execution lease")
	}
	if a.Epoch > r.Assignment.Epoch && r.Stage != "closed" && r.Stage != "suspended" {
		return r, ok, fail("failed_precondition", "previous assignment is not confirmed closed")
	}
	return r, ok, nil
}
func (b *Bridge) Stop(ctx context.Context, a controlplane.Assignment) (controlplane.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.validate(a, false); e != nil {
		return controlplane.Observation{}, e
	}
	r, ok, e := b.existing(a)
	if e != nil {
		return controlplane.Observation{}, e
	}
	if !ok || a.Epoch > r.Assignment.Epoch {
		r = record{Assignment: a}
	}
	r.Stage = "closing"
	b.cfg.Logger.InfoContext(ctx, "host assignment closing", "branch_id", a.BranchID, "execution_id", a.ExecutionID, "epoch", a.Epoch)
	if e = b.save(a, r); e != nil {
		return controlplane.Observation{}, e
	}
	if cancel := b.workers[workerKey(a)]; cancel != nil {
		cancel()
	}
	e = b.runtime.Stop(ctx, allocation(a))
	if errors.Is(e, sandbox.ErrNotFound) {
		e = nil
	}
	if e != nil {
		return controlplane.Observation{}, runtimeError(e)
	}
	r.Stage = "closed"
	if e = b.save(a, r); e != nil {
		return controlplane.Observation{}, e
	}
	return controlplane.Observation{State: "stopped"}, nil
}
func runtimeError(e error) error {
	if e == nil {
		return nil
	}
	if _, ok := e.(*controlplane.Error); ok {
		return e
	}
	if _, ok := status.FromError(e); ok {
		return clientError(e)
	}
	switch {
	case errors.Is(e, sandbox.ErrInvalid):
		return fail("invalid_argument", "runtime rejected input")
	case errors.Is(e, sandbox.ErrConflict):
		return fail("aborted", "runtime epoch already consumed; never retry creation blindly")
	case errors.Is(e, sandbox.ErrNotFound):
		return fail("not_found", "allocation absent")
	case errors.Is(e, context.Canceled):
		return fail("cancelled", "runtime call canceled")
	case errors.Is(e, context.DeadlineExceeded):
		return fail("deadline_exceeded", "runtime call deadline")
	}
	return fail("unavailable", "host runtime unavailable")
}
func decode(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	if e := d.Decode(new(any)); e != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func inputs(a controlplane.Assignment, digest string) (controlplane.TaskSpec, controlplane.WorkspaceSpec, error) {
	var t controlplane.TaskSpec
	var w controlplane.WorkspaceSpec
	if decode(a.Run.Task.Spec, &t) != nil || decode(a.Run.Workspace.Spec, &w) != nil {
		return t, w, fail("invalid_argument", "resolved task/workspace required")
	}
	if t.TemplateDigest != digest {
		return t, w, fail("failed_precondition", "template does not match verified host template")
	}
	if len(w.Repositories) > 0 || !w.PrivateWrites {
		return t, w, fail("failed_precondition", "only private file workspaces supported; repository materialization unavailable")
	}
	controlled := t.ExecutionMode == "controlled_python_v1" || t.ExecutionMode == "cooperative_python_v1"
	if t.ExecutionMode != "" && t.ExecutionMode != "foreground_v1" && !controlled {
		return t, w, fail("failed_precondition", "unsupported task execution mode")
	}
	if controlled && (len(t.Command) > 0 || len(t.Env) > 0) {
		return t, w, fail("invalid_argument", "controlled profile requires empty command and environment")
	}
	if (!controlled && len(t.Command) == 0) || len(t.Command) > 128 || t.Limits.TimeoutSeconds < 1 || t.Limits.TimeoutSeconds > 600 {
		return t, w, fail("invalid_argument", "task argv and 1..600s timeout required")
	}
	size := 0
	for _, arg := range t.Command {
		size += len(arg)
		if strings.ContainsRune(arg, 0) || len(arg) > 32768 {
			return t, w, fail("invalid_argument", "invalid argv")
		}
	}
	if (!controlled && t.Command[0] == "") || size > 32768 {
		return t, w, fail("invalid_argument", "invalid argv budget")
	}
	for k, v := range t.Env {
		if k != "LANG" && k != "LC_ALL" && k != "TZ" && k != "PYTHONUNBUFFERED" {
			return t, w, fail("failed_precondition", "environment key unsupported by guest policy")
		}
		if strings.ContainsRune(v, 0) || len(v) > 4096 {
			return t, w, fail("invalid_argument", "invalid environment value")
		}
		size += len(k) + len(v)
	}
	if len(t.Env) > 64 || size > 65536 {
		return t, w, fail("invalid_argument", "task environment exceeds budget")
	}
	paths := map[string]bool{}
	total := 0
	for _, f := range w.Files {
		if controlled && (f.Path == "controlled-origin.txt" || f.Path == "controlled-state.json" || f.Path == "agent-state.json" || f.Path == ".agent-state.pending") {
			return t, w, fail("invalid_argument", "controlled profile workspace names are reserved")
		}
		if strings.Contains(f.Path, "/") {
			return t, w, fail("failed_precondition", "nested workspace inputs require directory materialization support")
		}
		h := sha256.Sum256(f.Content)
		if f.Path == "" || f.Path == "." || len(f.Path) > 255 || path.Clean(f.Path) != f.Path || path.IsAbs(f.Path) || f.Path == ".." || strings.HasPrefix(f.Path, "../") || strings.ContainsRune(f.Path, 0) || paths[f.Path] || len(f.Content) > 1<<20 || f.Digest != "sha256:"+hex.EncodeToString(h[:]) {
			return t, w, fail("invalid_argument", "invalid workspace file path, size or digest")
		}
		paths[f.Path] = true
		total += len(f.Content)
	}
	if total > 1<<20 {
		return t, w, fail("invalid_argument", "workspace input budget exceeds 1 MiB")
	}
	return t, w, nil
}
func (b *Bridge) Ensure(ctx context.Context, a controlplane.Assignment) (controlplane.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.validate(a, true); e != nil {
		return controlplane.Observation{}, e
	}
	t, w, e := inputs(a, b.cfg.TemplateDigest)
	if e != nil {
		return controlplane.Observation{}, e
	}
	if e = b.profile(t); e != nil {
		return controlplane.Observation{}, e
	}
	if (t.ExecutionMode == "cooperative_python_v1") != (a.HarnessSession != nil) {
		return controlplane.Observation{}, fail("failed_precondition", "execution mode requires its immutable cooperative session")
	}

	data, _ := json.Marshal(a.Run)
	h := sha256.Sum256(data)
	hash := hex.EncodeToString(h[:])
	r, ok, e := b.existing(a)
	if e != nil {
		return controlplane.Observation{}, e
	}
	if ok && same(a, r.Assignment) {
		if r.Stage == "closed" || r.Stage == "closing" {
			return controlplane.Observation{}, fail("aborted", "assignment permanently closed")
		}
		if r.RunHash != hash {
			return controlplane.Observation{}, fail("aborted", "resolved run changed")
		}
		return b.observeLocked(ctx, a, r)
	}
	if ok && r.Stage == "suspended" {
		return controlplane.Observation{}, fail("failed_precondition", "suspended assignment requires Resume")
	}
	req := sandbox.CreateRequest{HarnessSession: a.HarnessSession, Allocation: allocation(a), Limits: taskLimits(t), AbsoluteExpiresAt: a.LeaseUntil}
	if e = req.Validate(); e != nil {
		return controlplane.Observation{}, runtimeError(e)
	}
	// Persist admission BEFORE side effects. A crash here makes the outcome
	// ambiguous; same-epoch recovery observes/stops and never repeats a command.
	r = record{Assignment: a, RunHash: hash, Stage: "admitted", ExpiresAt: a.LeaseUntil, Mode: t.ExecutionMode, Create: &req}
	b.cfg.Logger.InfoContext(ctx, "host assignment admitted", "branch_id", a.BranchID, "execution_id", a.ExecutionID, "epoch", a.Epoch, "lease_until", a.LeaseUntil)
	if e = b.save(a, r); e != nil {
		return controlplane.Observation{}, e
	}
	o, e := b.runtime.Inspect(ctx, allocation(a))
	if errors.Is(e, sandbox.ErrNotFound) {
		o, e = b.runtime.Create(ctx, req)
	}
	if e != nil {
		return controlplane.Observation{}, runtimeError(e)
	}
	if o.State != sandbox.StateRunning {
		return controlplane.Observation{State: "failed"}, nil
	}
	if o.ExpiresAt.Before(r.ExpiresAt) {
		r.ExpiresAt = o.ExpiresAt
	}
	for _, f := range w.Files {
		if _, e = b.runtime.WriteFile(ctx, allocation(a), &guestv1.WriteFileRequest{Path: f.Path, Data: f.Content}); e != nil {
			return controlplane.Observation{}, runtimeError(e)
		}
	}
	if t.ExecutionMode == "cooperative_python_v1" {
		r.Stage = "agent_starting"
		if e = b.save(a, r); e != nil {
			return controlplane.Observation{}, e
		}
		return b.startAgentLocked(ctx, a, r)
	}
	if t.ExecutionMode == "controlled_python_v1" {
		r.Stage = "controlled_starting"
		if e = b.save(a, r); e != nil {
			return controlplane.Observation{}, e
		}
		native := b.runtime.(sandbox.SnapshotBackend)
		state, startErr := native.StartControlled(ctx, allocation(a), t.ControlledCounter)
		if startErr != nil {
			return controlplane.Observation{}, runtimeError(startErr)
		}
		if e = controlledIdentity(state, a); e != nil {
			return controlplane.Observation{}, e
		}
		r.Stage = "controlled_running"
		if e = b.save(a, r); e != nil {
			return controlplane.Observation{}, e
		}
		return controlplane.Observation{State: "running"}, nil
	}
	r.Stage = "executing"
	if e = b.save(a, r); e != nil {
		return controlplane.Observation{}, e
	}
	runCtx, cancel := context.WithDeadline(context.Background(), r.ExpiresAt)
	b.workers[workerKey(a)] = cancel
	b.wg.Add(1)
	go b.execute(runCtx, cancel, a, t)
	return controlplane.Observation{State: "running"}, nil
}
func (b *Bridge) execute(ctx context.Context, cancel context.CancelFunc, a controlplane.Assignment, t controlplane.TaskSpec) {
	defer b.wg.Done()
	defer cancel()
	success := false
	e := b.runtime.Exec(ctx, allocation(a), &guestv1.ExecRequest{Argv: t.Command, Env: t.Env, TimeoutMs: uint32(t.Limits.TimeoutSeconds) * 1000}, func(ev *guestv1.ExecEvent) error {
		if result := ev.GetResult(); result != nil {
			success = result.ExitCode == 0 && !result.TimedOut
		}
		return nil
	})
	cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
	stopErr := b.runtime.Stop(cleanup, allocation(a))
	done()
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.workers, workerKey(a))
	if b.closed {
		return
	}
	r, ok, readErr := b.read(a)
	if readErr != nil || !ok || !same(a, r.Assignment) || (r.Stage == "closed" || r.Stage == "closing") {
		return
	}
	r.Stage = "failed"
	if e == nil && success {
		r.Stage = "complete"
	}
	if stopErr != nil && !errors.Is(stopErr, sandbox.ErrNotFound) {
		r.Stage = "cleanup_" + r.Stage
	}
	if e := b.save(a, r); e != nil {
		b.cfg.Logger.Error("host task result not persisted; recovery will fence execution", "branch_id", a.BranchID, "epoch", a.Epoch, "error", e)
	} else {
		b.cfg.Logger.Info("host task result", "branch_id", a.BranchID, "execution_id", a.ExecutionID, "epoch", a.Epoch, "state", r.Stage)
	}
}
func (b *Bridge) observeLocked(ctx context.Context, a controlplane.Assignment, r record) (controlplane.Observation, error) {
	if r.Mode == "cooperative_python_v1" && r.Stage != "closed" && r.Stage != "closing" {
		return b.observeAgentLocked(ctx, a, r)
	}
	if r.Mode == "controlled_python_v1" && r.Stage != "closed" && r.Stage != "closing" {
		return b.observeSnapshotLocked(ctx, a, r)
	}
	if r.Stage == "closed" {
		return controlplane.Observation{State: "stopped"}, nil
	}
	if r.Stage == "closing" {
		if e := b.runtime.Stop(ctx, allocation(a)); e != nil && !errors.Is(e, sandbox.ErrNotFound) {
			return controlplane.Observation{}, runtimeError(e)
		}
		r.Stage = "closed"
		if e := b.save(a, r); e != nil {
			return controlplane.Observation{}, e
		}
		return controlplane.Observation{State: "stopped"}, nil
	}
	if strings.HasPrefix(r.Stage, "cleanup_") {
		if e := b.runtime.Stop(ctx, allocation(a)); e != nil && !errors.Is(e, sandbox.ErrNotFound) {
			return controlplane.Observation{}, runtimeError(e)
		}
		r.Stage = strings.TrimPrefix(r.Stage, "cleanup_")
		if e := b.save(a, r); e != nil {
			return controlplane.Observation{}, e
		}
	}
	if r.Stage == "complete" {
		return controlplane.Observation{State: "stopped"}, nil
	}
	if r.Stage == "failed" {
		return controlplane.Observation{State: "failed"}, nil
	}
	if !time.Now().Before(r.ExpiresAt) || b.workers[workerKey(a)] == nil {
		if e := b.runtime.Stop(ctx, allocation(a)); e != nil && !errors.Is(e, sandbox.ErrNotFound) {
			return controlplane.Observation{}, runtimeError(e)
		}
		r.Stage = "failed"
		if e := b.save(a, r); e != nil {
			return controlplane.Observation{}, e
		}
		return controlplane.Observation{State: "failed"}, nil
	}
	o, e := b.runtime.Inspect(ctx, allocation(a))
	if e != nil {
		return controlplane.Observation{}, runtimeError(e)
	}
	return controlplane.Observation{State: string(o.State)}, nil
}
func (b *Bridge) Observe(ctx context.Context, a controlplane.Assignment) (controlplane.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.validate(a, false); e != nil {
		return controlplane.Observation{}, e
	}
	r, ok, e := b.existing(a)
	if e != nil {
		return controlplane.Observation{}, e
	}
	if !ok || !same(a, r.Assignment) {
		return controlplane.Observation{}, fail("not_found", "assignment absent")
	}
	return b.observeLocked(ctx, a, r)
}

// Proxy calls authorize against the durable assignment, using the host-owned
// runtime session. Guest credentials never cross this service boundary.
func (b *Bridge) proxy(ctx context.Context, a controlplane.Assignment) (context.Context, context.CancelFunc, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.validate(a, true); e != nil {
		return nil, nil, e
	}
	r, ok, e := b.existing(a)
	if e != nil {
		return nil, nil, e
	}
	if !ok || !same(a, r.Assignment) || r.Stage != "executing" && r.Stage != "controlled_running" && r.Stage != "agent_running" {
		return nil, nil, fail("failed_precondition", "assignment is not executing")
	}
	until := a.LeaseUntil
	if r.ExpiresAt.Before(until) {
		until = r.ExpiresAt
	}
	c, cancel := context.WithDeadline(ctx, until)
	return c, cancel, nil
}
func (b *Bridge) Exec(ctx context.Context, a controlplane.Assignment, r *guestv1.ExecRequest, emit func(*guestv1.ExecEvent) error) error {
	c, cancel, e := b.proxy(ctx, a)
	if e != nil {
		return e
	}
	defer cancel()
	if a.HarnessSession != nil {
		return fail("failed_precondition", "unregistered command/file mutations are unsupported for cooperative agents")
	}
	return runtimeError(b.runtime.Exec(c, allocation(a), r, emit))
}
func (b *Bridge) ReadFile(ctx context.Context, a controlplane.Assignment, r *guestv1.ReadFileRequest) (*guestv1.ReadFileResponse, error) {
	c, cancel, e := b.proxy(ctx, a)
	if e != nil {
		return nil, e
	}
	defer cancel()
	v, e := b.runtime.ReadFile(c, allocation(a), r)
	return v, runtimeError(e)
}
func (b *Bridge) WriteFile(ctx context.Context, a controlplane.Assignment, r *guestv1.WriteFileRequest) (*guestv1.WriteFileResponse, error) {
	c, cancel, e := b.proxy(ctx, a)
	if e != nil {
		return nil, e
	}
	defer cancel()
	if a.HarnessSession != nil {
		return nil, fail("failed_precondition", "unregistered command/file mutations are unsupported for cooperative agents")
	}
	v, e := b.runtime.WriteFile(c, allocation(a), r)
	return v, runtimeError(e)
}

var _ controlplane.Backend = (*Bridge)(nil)
