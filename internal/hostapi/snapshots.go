package hostapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	"github.com/gduchidze/hypercall/internal/controlplane"
	"github.com/gduchidze/hypercall/internal/sandbox"
	"github.com/gduchidze/hypercall/internal/snapshot"
)

func taskLimits(t controlplane.TaskSpec) sandbox.Limits {
	return sandbox.Limits{VCPUs: uint32(t.Limits.CPU), MemoryMiB: uint32(t.Limits.MemoryMB), WorkspaceBytes: int64(t.Limits.DiskMB) << 20, PIDs: 64, TTL: time.Duration(t.Limits.TimeoutSeconds) * time.Second}
}
func (b *Bridge) profile(t controlplane.TaskSpec) error {
	if t.ExecutionMode != "controlled_python_v1" {
		return nil
	}
	if !b.Supports("suspend") || t.AgentDigest != b.cfg.ControlledAgentDigest {
		return fail("failed_precondition", "controlled snapshot profile is not configured for verified guest artifact")
	}
	return nil
}
func controlledIdentity(s *guestv1.ControlledState, a controlplane.Assignment) error {
	if s == nil || !s.AtBarrier || s.Identity == nil || s.Identity.TenantId != a.Scope.TenantID || s.Identity.ProjectId != a.Scope.ProjectID || s.Identity.SandboxId != sandboxID(a) || s.Identity.ExecutionId != a.ExecutionID || s.Identity.Epoch != uint64(a.Epoch) {
		return fail("failed_precondition", "controlled barrier identity mismatch")
	}
	return nil
}
func runHash(a controlplane.Assignment) string {
	raw, _ := json.Marshal(a.Run)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func manifestBytes(m snapshot.Manifest) ([]byte, string) {
	data, _ := json.Marshal(m)
	h := sha256.Sum256(data)
	return data, "sha256:" + hex.EncodeToString(h[:])
}
func nativeScope(a controlplane.Assignment, id string) snapshot.Scope {
	return snapshot.Scope{TenantID: a.Scope.TenantID, ProjectID: a.Scope.ProjectID, CheckpointID: id}
}
func checkpointID(a controlplane.Assignment) string {
	data, _ := json.Marshal([]any{a.Scope.TenantID, a.Scope.ProjectID, a.BranchID, sandboxID(a), a.ExecutionID, a.Epoch})
	h := sha256.Sum256(data)
	return "cp-" + hex.EncodeToString(h[:])
}
func (b *Bridge) checkpoint(a controlplane.Assignment, m snapshot.Manifest) (controlplane.CheckpointSpec, error) {
	if m.Scope.TenantID != a.Scope.TenantID || m.Scope.ProjectID != a.Scope.ProjectID || "sha256:"+m.Compatibility.TemplateSHA256 != b.cfg.TemplateDigest || "sha256:"+m.Compatibility.GuestSHA256 != b.cfg.ControlledAgentDigest {
		return controlplane.CheckpointSpec{}, fail("failed_precondition", "snapshot scope/template/guest mismatch")
	}
	data, digest := manifestBytes(m)
	return controlplane.CheckpointSpec{NativeCheckpointID: m.Scope.CheckpointID, SnapshotManifest: data, BranchID: a.BranchID, ManifestDigest: digest, ManifestURI: "hypercall-snapshot://" + m.Scope.TenantID + "/" + m.Scope.ProjectID + "/" + m.Scope.CheckpointID, ProtocolVersion: "hypercall.snapshot.v1", Verified: true, Compatibility: map[string]string{"architecture": m.Compatibility.Architecture, "snapshot_format": m.Compatibility.SnapshotFormatVersion, "template_digest": b.cfg.TemplateDigest, "guest_agent_digest": b.cfg.ControlledAgentDigest, "execution_mode": "controlled_python_v1", "source_sandbox_id": sandboxID(a), "source_execution_id": a.ExecutionID, "source_epoch": strconv.FormatInt(a.Epoch, 10)}}, nil
}
func (b *Bridge) verifyCheckpoint(ctx context.Context, a controlplane.Assignment, cp controlplane.CheckpointSpec) (snapshot.Manifest, error) {
	var m snapshot.Manifest
	if !cp.Verified || cp.BranchID != a.BranchID || cp.ProtocolVersion != "hypercall.snapshot.v1" || len(cp.SnapshotManifest) > 1<<20 || decode(cp.SnapshotManifest, &m) != nil || m.Scope != nativeScope(a, cp.NativeCheckpointID) {
		return m, fail("failed_precondition", "invalid retained snapshot manifest")
	}
	parentEpoch, parseErr := strconv.ParseInt(cp.Compatibility["source_epoch"], 10, 64)
	parent := controlplane.Assignment{Scope: a.Scope, BranchID: a.BranchID, SandboxID: cp.Compatibility["source_sandbox_id"], ExecutionID: cp.Compatibility["source_execution_id"], Epoch: parentEpoch}
	if parseErr != nil || parentEpoch < 1 || allocation(parent).Validate() != nil || checkpointID(parent) != cp.NativeCheckpointID || cp.Compatibility["execution_mode"] != "controlled_python_v1" || cp.Compatibility["template_digest"] != b.cfg.TemplateDigest || cp.Compatibility["guest_agent_digest"] != b.cfg.ControlledAgentDigest || cp.Compatibility["architecture"] != m.Compatibility.Architecture || cp.Compatibility["snapshot_format"] != m.Compatibility.SnapshotFormatVersion || m.Compatibility.BarrierProtocolVersion != "controlled-python-v2-host-clock" {
		return m, fail("failed_precondition", "retained checkpoint source/profile binding mismatch")
	}
	data, digest := manifestBytes(m)
	if !bytes.Equal(data, cp.SnapshotManifest) || digest != cp.ManifestDigest || "sha256:"+m.Compatibility.TemplateSHA256 != b.cfg.TemplateDigest || "sha256:"+m.Compatibility.GuestSHA256 != b.cfg.ControlledAgentDigest {
		return m, fail("failed_precondition", "retained snapshot digest/template/guest mismatch")
	}
	ready, e := b.cfg.SnapshotStore.ReadyManifest(ctx, m.Scope)
	if e != nil {
		return m, storageError(e)
	}
	actual, actualDigest := manifestBytes(ready)
	if actualDigest != digest || !bytes.Equal(actual, data) {
		return m, fail("failed_precondition", "retained manifest differs from ready store manifest")
	}
	return m, nil
}
func storageError(e error) error {
	if e == nil {
		return nil
	}
	if errors.Is(e, snapshot.ErrNotFound) || errors.Is(e, snapshot.ErrIntegrity) || errors.Is(e, snapshot.ErrIncompatible) || errors.Is(e, snapshot.ErrDeleting) || errors.Is(e, snapshot.ErrInvalid) {
		return fail("failed_precondition", "snapshot is missing, incompatible or invalid")
	}
	return fail("unavailable", "snapshot storage unavailable")
}
func (b *Bridge) Suspend(ctx context.Context, a controlplane.Assignment) (controlplane.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.validate(a, false); e != nil {
		return controlplane.Observation{}, e
	}
	r, ok, e := b.existing(a)
	if e != nil {
		return controlplane.Observation{}, e
	}
	if !ok || !same(a, r.Assignment) || r.Mode != "controlled_python_v1" || !b.Supports("suspend") {
		return controlplane.Observation{}, fail("failed_precondition", "only configured controlled Python assignments support suspension")
	}
	if r.Stage == "suspended" {
		return controlplane.Observation{State: "suspended", Checkpoint: r.Checkpoint}, nil
	}
	if r.Stage == "closed" || r.Stage == "closing" {
		return controlplane.Observation{}, fail("aborted", "assignment permanently closed")
	}
	native := b.runtime.(sandbox.SnapshotBackend)
	if r.Stage == "controlled_running" {
		if !time.Now().Before(a.LeaseUntil) {
			return controlplane.Observation{}, fail("failed_precondition", "execution lease expired")
		}
		r.CaptureID = checkpointID(a)
		r.Stage = "capturing"
		if e = b.save(a, r); e != nil {
			return controlplane.Observation{}, e
		}
		parent := ""
		if r.Restore != nil {
			parent = r.Restore.CheckpointID
		}
		m, captureErr := native.Capture(ctx, sandbox.CaptureRequest{Allocation: allocation(a), CheckpointID: r.CaptureID, ParentCheckpointID: parent})
		if captureErr != nil {
			return controlplane.Observation{}, runtimeError(captureErr)
		}
		cp, e := b.checkpoint(a, m)
		if e != nil {
			return controlplane.Observation{}, e
		}
		r.Checkpoint = &cp
		r.Stage = "captured"
		if e = b.save(a, r); e != nil {
			return controlplane.Observation{}, e
		}
	}
	return b.finishSuspendLocked(ctx, a, r)
}
func (b *Bridge) finishSuspendLocked(ctx context.Context, a controlplane.Assignment, r record) (controlplane.Observation, error) {
	if r.Stage == "capturing" {
		m, e := b.cfg.SnapshotStore.ReadyManifest(ctx, nativeScope(a, r.CaptureID))
		if e != nil {
			return controlplane.Observation{}, storageError(e)
		}
		cp, e := b.checkpoint(a, m)
		if e != nil {
			return controlplane.Observation{}, e
		}
		r.Checkpoint = &cp
		r.Stage = "captured"
		if e = b.save(a, r); e != nil {
			return controlplane.Observation{}, e
		}
	}
	if r.Stage != "captured" || r.Checkpoint == nil {
		return controlplane.Observation{}, fail("failed_precondition", "snapshot capture is not ready")
	}
	if _, e := b.verifyCheckpoint(ctx, a, *r.Checkpoint); e != nil {
		return controlplane.Observation{}, e
	}
	// This permanent checkpoint retention pin survives cancellation/CP commit
	// ambiguity. Never release it merely because a controller RPC was lost.
	if e := b.cfg.SnapshotStore.AcquireReference(ctx, nativeScope(a, r.CaptureID), "retained-"+r.CaptureID); e != nil {
		return controlplane.Observation{}, storageError(e)
	}
	if e := b.runtime.Stop(ctx, allocation(a)); e != nil && !errors.Is(e, sandbox.ErrNotFound) {
		return controlplane.Observation{}, runtimeError(e)
	}
	r.Stage = "suspended"
	if e := b.save(a, r); e != nil {
		return controlplane.Observation{}, e
	}
	return controlplane.Observation{State: "suspended", Checkpoint: r.Checkpoint}, nil
}
func (b *Bridge) Resume(ctx context.Context, a controlplane.Assignment, cp controlplane.CheckpointSpec) (controlplane.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.validate(a, true); e != nil {
		return controlplane.Observation{}, e
	}
	t, _, e := inputs(a, b.cfg.TemplateDigest)
	if e != nil {
		return controlplane.Observation{}, e
	}
	if t.ExecutionMode != "controlled_python_v1" {
		return controlplane.Observation{}, fail("failed_precondition", "foreground tasks cannot resume snapshots")
	}
	if e = b.profile(t); e != nil {
		return controlplane.Observation{}, e
	}
	if _, e = b.verifyCheckpoint(ctx, a, cp); e != nil {
		return controlplane.Observation{}, e
	}
	if cp.Compatibility["source_sandbox_id"] == sandboxID(a) || cp.Compatibility["source_execution_id"] == a.ExecutionID {
		return controlplane.Observation{}, fail("failed_precondition", "resume requires fresh sandbox and execution identities")
	}
	r, ok, e := b.existing(a)
	if e != nil {
		return controlplane.Observation{}, e
	}
	if ok && same(a, r.Assignment) {
		if r.Stage == "closed" || r.Stage == "closing" {
			return controlplane.Observation{}, fail("aborted", "assignment permanently closed")
		}
		if r.RunHash != runHash(a) || r.RestoreCheckpoint == nil || r.RestoreCheckpoint.ManifestDigest != cp.ManifestDigest || r.RestoreCheckpoint.NativeCheckpointID != cp.NativeCheckpointID {
			return controlplane.Observation{}, fail("aborted", "resume inputs changed")
		}
		if r.Stage == "controlled_running" {
			return b.observeSnapshotLocked(ctx, a, r)
		}
		if r.Stage != "restoring" || r.Restore == nil {
			return controlplane.Observation{}, fail("failed_precondition", "resume cannot replay this assignment")
		}
	} else {
		request := sandbox.RestoreRequest{OperationID: "resume-" + a.ExecutionID, CheckpointID: cp.NativeCheckpointID, Allocation: allocation(a), Limits: taskLimits(t), AbsoluteExpiresAt: a.LeaseUntil}
		if e = request.Validate(); e != nil {
			return controlplane.Observation{}, runtimeError(e)
		}
		r = record{Assignment: a, RunHash: runHash(a), Stage: "restoring", ExpiresAt: a.LeaseUntil, Mode: t.ExecutionMode, Restore: &request, RestoreCheckpoint: &cp}
		if e = b.save(a, r); e != nil {
			return controlplane.Observation{}, e
		}
	}
	// Reuse the exact durable request: native operation hashes include TTL and the
	// absolute expiry, so retries neither change identity nor extend the lease.
	o, e := b.runtime.(sandbox.SnapshotBackend).Restore(ctx, *r.Restore)
	if e != nil {
		return controlplane.Observation{}, runtimeError(e)
	}
	if o.State != sandbox.StateRunning {
		return controlplane.Observation{}, fail("unavailable", "restored VM is not running")
	}
	state, e := b.runtime.(sandbox.SnapshotBackend).InspectControlled(ctx, allocation(a))
	if e != nil {
		return controlplane.Observation{}, runtimeError(e)
	}
	if e = controlledIdentity(state, a); e != nil {
		return controlplane.Observation{}, e
	}
	if !o.ExpiresAt.IsZero() && o.ExpiresAt.Before(r.ExpiresAt) {
		r.ExpiresAt = o.ExpiresAt
	}
	r.Stage = "controlled_running"
	if e = b.save(a, r); e != nil {
		return controlplane.Observation{}, e
	}
	return controlplane.Observation{State: "running"}, nil
}
func (b *Bridge) observeSnapshotLocked(ctx context.Context, a controlplane.Assignment, r record) (controlplane.Observation, error) {
	if !b.Supports("suspend") {
		return controlplane.Observation{}, fail("failed_precondition", "snapshot profile no longer configured")
	}
	if r.Stage == "suspended" {
		return controlplane.Observation{State: "suspended", Checkpoint: r.Checkpoint}, nil
	}
	if r.Stage == "capturing" || r.Stage == "captured" {
		return b.finishSuspendLocked(ctx, a, r)
	}
	if r.Stage == "restoring" {
		return controlplane.Observation{}, fail("unavailable", "restore requires original durable resume request")
	}
	if r.Stage == "failed" {
		return controlplane.Observation{State: "failed"}, nil
	}
	if r.Stage != "controlled_running" && r.Stage != "controlled_starting" {
		return controlplane.Observation{}, fail("failed_precondition", "unsupported controlled assignment stage")
	}
	o, e := b.runtime.Inspect(ctx, allocation(a))
	if e != nil || !time.Now().Before(r.ExpiresAt) || o.State != sandbox.StateRunning {
		if e != nil && !errors.Is(e, sandbox.ErrNotFound) {
			return controlplane.Observation{}, runtimeError(e)
		}
		if stopErr := b.runtime.Stop(ctx, allocation(a)); stopErr != nil && !errors.Is(stopErr, sandbox.ErrNotFound) {
			return controlplane.Observation{}, runtimeError(stopErr)
		}
		r.Stage = "failed"
		if e = b.save(a, r); e != nil {
			return controlplane.Observation{}, e
		}
		return controlplane.Observation{State: "failed"}, nil
	}
	state, e := b.runtime.(sandbox.SnapshotBackend).InspectControlled(ctx, allocation(a))
	if e != nil {
		return controlplane.Observation{}, runtimeError(e)
	}
	if e = controlledIdentity(state, a); e != nil {
		return controlplane.Observation{}, e
	}
	if r.Stage == "controlled_starting" {
		r.Stage = "controlled_running"
		if e = b.save(a, r); e != nil {
			return controlplane.Observation{}, e
		}
	}
	return controlplane.Observation{State: "running"}, nil
}
