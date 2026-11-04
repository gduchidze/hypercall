//go:build linux

package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gduchidze/hypercall/internal/snapshot"
)

type snapshotChild struct {
	Result            ChildResult `json:"result"`
	Status            string      `json:"status"`
	Reference         string      `json:"reference"`
	RetryFence        bool        `json:"retry_fence"`
	ReferenceMayExist bool        `json:"reference_may_exist"`
	OwnedToken        string      `json:"owned_token,omitempty"`
}
type snapshotOperation struct {
	SchemaVersion int             `json:"schema_version"`
	Request       ForkRequest     `json:"request"`
	RequestSHA256 string          `json:"request_sha256"`
	Children      []snapshotChild `json:"children"`
}

func operationName(scope Scope, id string) string {
	data, _ := json.Marshal([]string{scope.TenantID, scope.ProjectID, id})
	return digest(data) + ".json"
}
func (b *Backend) writeSnapshotOperation(name string, op snapshotOperation) error {
	data, err := json.Marshal(op)
	if err != nil {
		return err
	}
	temp := filepath.Join("operations", name+".pending")
	f, err := b.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		_ = b.root.Remove(temp)
		return err
	}
	if err = b.root.Rename(temp, filepath.Join("operations", name)); err != nil {
		return err
	}
	dir, err := b.root.Open("operations")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (b *Backend) loadSnapshotOperation(name string) (snapshotOperation, error) {
	var op snapshotOperation
	err := decodeFile(filepath.Join(b.cfg.StateDir, "operations", name), &op)
	if err != nil {
		return op, err
	}
	data, _ := json.Marshal(op.Request)
	if op.SchemaVersion != 1 || op.Request.Validate() != nil || digest(data) != op.RequestSHA256 || len(op.Children) != len(op.Request.Children) || name != operationName(Scope{op.Request.Children[0].Allocation.TenantID, op.Request.Children[0].Allocation.ProjectID}, op.Request.OperationID) {
		return op, errors.New("invalid snapshot operation journal")
	}
	for i, c := range op.Children {
		if c.Result.Allocation != op.Request.Children[i].Allocation || !identifier.MatchString(c.Reference) || (c.RetryFence && !validToken(c.OwnedToken)) {
			return op, errors.New("invalid snapshot operation child")
		}
		switch c.Status {
		case "pending", "running", "succeeded", "failed", "interrupted":
		default:
			return op, errors.New("invalid snapshot operation state")
		}
	}
	return op, nil
}
func (b *Backend) recoverSnapshotOperations(ctx context.Context) error {
	entries, err := readDir(b.root, "operations")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".pending" {
			if err = b.root.Remove(filepath.Join("operations", entry.Name())); err != nil {
				return err
			}
			continue
		}
		op, err := b.loadSnapshotOperation(entry.Name())
		if err != nil {
			return err
		}
		changed := false
		for i := range op.Children {
			c := &op.Children[i]
			b.mu.Lock()
			active := b.active[c.Result.Allocation] != nil
			b.mu.Unlock()
			if active {
				continue
			}
			if c.Status == "running" || c.Status == "succeeded" {
				c.Status = "interrupted"
				c.Result.Interrupted = true
				c.Result.Error = "previous host process interrupted; fixed child identity will not be recreated"
				c.Result.Observation.State = StateStopped
				changed = true
			}
			if c.ReferenceMayExist {
				if b.cfg.SnapshotStore == nil {
					return errors.New("snapshot store required for interrupted operation recovery")
				}
				a := c.Result.Allocation
				err = b.cfg.SnapshotStore.ReleaseReference(ctx, snapshot.Scope{TenantID: a.TenantID, ProjectID: a.ProjectID, CheckpointID: op.Request.CheckpointID}, c.Reference)
				if err != nil && !errors.Is(err, snapshot.ErrNotFound) {
					return err
				}
				c.ReferenceMayExist = false
				changed = true
			}
		}
		if changed {
			if err = b.writeSnapshotOperation(entry.Name(), op); err != nil {
				return err
			}
		}
	}
	return nil
}
func (b *Backend) loadSnapshotOwner() error {
	const path = "host-owner.json"
	var owner struct {
		ID string `json:"id"`
	}
	err := decodeFile(filepath.Join(b.cfg.StateDir, path), &owner)
	if errors.Is(err, os.ErrNotExist) {
		entropy := make([]byte, 32)
		if _, err = rand.Read(entropy); err != nil {
			return err
		}
		owner.ID = hex.EncodeToString(entropy)
		pending := filepath.Join(b.cfg.StateDir, "host-owner.pending")
		if err = os.Remove(pending); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err = writeJSON(pending, owner, 0600); err != nil {
			return err
		}
		if err = b.root.Rename("host-owner.pending", path); err != nil {
			return err
		}
		dir, e := b.root.Open(".")
		if e != nil {
			return e
		}
		err = dir.Sync()
		_ = dir.Close()
	}
	if err != nil {
		return err
	}
	raw, e := hex.DecodeString(owner.ID)
	if e != nil || len(raw) != 32 {
		return errors.New("invalid host snapshot owner identity")
	}
	b.ownerID = owner.ID
	return nil
}
func (b *Backend) Restore(ctx context.Context, r RestoreRequest) (Observation, error) {
	if err := r.Validate(); err != nil {
		return Observation{}, err
	}
	children, err := b.Fork(ctx, ForkRequest{OperationID: r.OperationID, CheckpointID: r.CheckpointID, Children: []CreateRequest{{HarnessSession: r.HarnessSession, Allocation: r.Allocation, Limits: r.Limits, AbsoluteExpiresAt: r.AbsoluteExpiresAt}}})
	if err != nil {
		return Observation{}, err
	}
	if children[0].Error != "" {
		return children[0].Observation, fmt.Errorf("%w: %s", ErrUnavailable, children[0].Error)
	}
	return children[0].Observation, nil
}
func (b *Backend) Fork(ctx context.Context, r ForkRequest) ([]ChildResult, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	for _, c := range r.Children {
		if err := b.authorize(c.Allocation); err != nil {
			return nil, err
		}
	}
	if b.cfg.SnapshotStore == nil {
		return nil, fmt.Errorf("%w: snapshot store required", ErrUnavailable)
	}
	if err := b.lockSnapshot(ctx); err != nil {
		return nil, err
	}
	defer b.snapshotMu.Unlock()
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	name := operationName(b.cfg.Scope, r.OperationID)
	payload, _ := json.Marshal(r)
	requestHash := digest(payload)
	op, err := b.loadSnapshotOperation(name)
	if errors.Is(err, os.ErrNotExist) {
		entries, e := readDir(b.root, "operations")
		if e != nil {
			return nil, e
		}
		if len(entries) >= 4096 {
			return nil, fmt.Errorf("%w: host operation journal capacity exhausted", ErrUnavailable)
		}
		op = snapshotOperation{SchemaVersion: 1, Request: r, RequestSHA256: requestHash}
		for _, child := range r.Children {
			a := child.Allocation
			refData, _ := json.Marshal([]string{b.ownerID, name, a.SandboxID, a.ExecutionID})
			op.Children = append(op.Children, snapshotChild{Status: "pending", Reference: "host-" + digest(refData), Result: ChildResult{Allocation: a}})
		}
		if err = b.writeSnapshotOperation(name, op); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if op.RequestSHA256 != requestHash {
		return nil, ErrConflict
	}
	results := make([]ChildResult, len(op.Children))
	for i := range op.Children {
		child := &op.Children[i]
		request := r.Children[i]
		a := request.Allocation
		if child.Status == "succeeded" {
			observed, e := b.Inspect(ctx, a)
			if e != nil && !errors.Is(e, ErrNotFound) {
				return results, e
			}
			if e == nil && observed.State == StateRunning {
				child.Result.Observation = observed
				results[i] = child.Result
				continue
			}
			child.Status = "interrupted"
			child.Result.Interrupted = true
			child.Result.Error = "successful child no longer running; fixed identity will not be recreated"
			if err = b.writeSnapshotOperation(name, op); err != nil {
				return results, err
			}
		}
		if child.Status == "interrupted" {
			results[i] = child.Result
			continue
		}
		if child.Status == "running" {
			return nil, errors.New("unrecovered snapshot operation; restart owner before retry")
		}
		if err = ctx.Err(); err != nil {
			return results, err
		}
		// Failed attempts can retry only after their old cgroup has been fenced.
		if child.RetryFence {
			b.mu.Lock()
			active := b.active[a]
			b.mu.Unlock()
			if active != nil {
				if child.OwnedToken == "" || active.record.Token != child.OwnedToken {
					return results, fmt.Errorf("%w: child attempt ownership changed", ErrConflict)
				}
				if err = b.stop(active, true); err != nil {
					child.Result.Error = err.Error()
					results[i] = child.Result
					continue
				}
			}
		} else {
			b.mu.Lock()
			highest := b.fences[fenceFor(a).name()]
			b.mu.Unlock()
			if highest >= a.Epoch {
				child.Status = "interrupted"
				child.Result.Interrupted = true
				child.Result.Error = "fixed child allocation epoch already consumed"
				if err = b.writeSnapshotOperation(name, op); err != nil {
					return results, err
				}
				results[i] = child.Result
				continue
			}
		}
		child.Status = "running"
		child.ReferenceMayExist = true
		child.Result.Error = ""
		if err = b.writeSnapshotOperation(name, op); err != nil {
			return results, err
		}
		scope := snapshot.Scope{TenantID: a.TenantID, ProjectID: a.ProjectID, CheckpointID: r.CheckpointID}
		attemptErr := b.cfg.SnapshotStore.AcquireReference(ctx, scope, child.Reference)
		var verified *snapshot.Verified
		if attemptErr == nil {
			expected, e := b.compatibility(request.Limits, request.HarnessSession)
			attemptErr = e
			if attemptErr == nil {
				verified, attemptErr = b.cfg.SnapshotStore.OpenVerifiedIn(ctx, scope, expected, filepath.Join(b.cfg.StateDir, "materialized"))
			}
		}
		if attemptErr == nil && request.HarnessSession != nil {
			want := semanticFromWire(request.HarnessSession.InheritedBarrier)
			if verified.Manifest.SemanticBarrier == nil || want == nil || *want != *verified.Manifest.SemanticBarrier {
				attemptErr = snapshot.ErrIntegrity
			}
		}
		if attemptErr == nil {
			spec := &restoreSpec{checkpoint: r.CheckpointID, reference: child.Reference, memory: verified.Paths[snapshot.Memory], state: verified.Paths[snapshot.VMState], disk: verified.Paths[snapshot.Disk]}
			child.Result.Observation, attemptErr = b.create(ctx, request, spec, child.RetryFence)
			if spec.admitted {
				child.RetryFence = true
				child.OwnedToken = spec.token
			}
		}
		if verified != nil {
			attemptErr = errors.Join(attemptErr, verified.Close())
		}
		if attemptErr != nil {
			// Failure after VM readiness (for example private cache cleanup)
			// must fence that owned VM before reporting a failed child.
			b.mu.Lock()
			owned := b.active[a]
			b.mu.Unlock()
			if owned != nil && child.OwnedToken != "" && owned.record.Token == child.OwnedToken {
				attemptErr = errors.Join(attemptErr, b.stop(owned, true))
				child.Result.Observation.State = StateFailed
			}
			child.Status = "failed"
			child.Result.Error = attemptErr.Error()
			// If cleanup failed the cgroup still owns the pin; next retry fences it.
			b.mu.Lock()
			active := b.active[a] != nil
			b.mu.Unlock()
			if !active {
				releaseCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				e := b.cfg.SnapshotStore.ReleaseReference(releaseCtx, scope, child.Reference)
				cancel()
				if e != nil && !errors.Is(e, snapshot.ErrNotFound) {
					child.Result.Error = errors.Join(attemptErr, e).Error()
				} else {
					child.ReferenceMayExist = false
				}
			}
		} else {
			child.Status = "succeeded"
		}
		if err = b.writeSnapshotOperation(name, op); err != nil {
			return results, err
		}
		results[i] = child.Result
	}
	return results, nil
}

func (b *Backend) recoverMaterialized() error {
	entries, err := readDir(b.root, "materialized")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "snapshot-") {
			return errors.New("unrecognized owner materialization cache entry")
		}
		suffix := strings.TrimPrefix(entry.Name(), "snapshot-")
		if suffix == "" {
			return errors.New("invalid materialization cache entry")
		}
		for _, c := range suffix {
			if c < '0' || c > '9' {
				return errors.New("invalid materialization cache entry")
			}
		}
		if err = b.root.RemoveAll(filepath.Join("materialized", entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
