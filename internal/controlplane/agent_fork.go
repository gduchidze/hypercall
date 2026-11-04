package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
)

func (s *Service) Fork(ctx context.Context, scope Scope, checkpointID string, models []Ref, key string) (ForkResult, error) {
	if e := validateScope(scope); e != nil {
		return ForkResult{}, e
	}
	if len(models) < 1 || len(models) > 2 {
		return ForkResult{}, failure("invalid_argument", "fork supports one or two declared candidates")
	}
	tx, e := s.store.db.BeginTx(ctx, nil)
	if e != nil {
		return ForkResult{}, e
	}
	defer tx.Rollback()
	request := struct {
		Checkpoint string
		Models     []Ref
	}{checkpointID, models}
	var out ForkResult
	found, e := idem(ctx, tx, scope, key, request, &out)
	if e != nil || found {
		return out, e
	}
	cpResource, e := load(ctx, tx, scope, "Checkpoint", checkpointID)
	if e != nil {
		return out, e
	}
	var checkpoint CheckpointSpec
	if e = json.Unmarshal(cpResource.Spec, &checkpoint); e != nil {
		return out, e
	}
	var desired, observed string
	e = tx.QueryRowContext(ctx, `SELECT desired,observed FROM branches WHERE tenant_id=$1 AND project_id=$2 AND id=$3 FOR UPDATE`, scope.TenantID, scope.ProjectID, checkpoint.BranchID).Scan(&desired, &observed)
	if e != nil {
		return out, e
	}
	if desired != "suspended" || observed != "suspended" {
		return out, failure("failed_precondition", "fork requires a confirmed suspended source branch")
	}
	run, e := loadRun(ctx, tx, scope, checkpoint.BranchID)
	if e != nil {
		return out, e
	}
	if !isCooperative(run) || checkpoint.Barrier == nil || checkpoint.BudgetRemainingMS <= 0 {
		return out, failure("failed_precondition", "fork requires a retained semantic checkpoint and remaining budget")
	}
	if checkpoint, e = loadCheckpoint(ctx, tx, scope, checkpoint.BranchID, checkpointID, run); e != nil {
		return out, e
	}
	if capable, ok := s.backend.(interface{ SupportsRun(string, RunSpec) bool }); ok {
		if !capable.SupportsRun("resume", run) {
			return out, failure("failed_precondition", "backend does not support cooperative restore")
		}
	} else if capable, ok := s.backend.(interface{ Supports(string) bool }); !ok || !capable.Supports("resume") {
		return out, failure("failed_precondition", "backend does not support restore")
	}
	selected := []Resource{}
	for _, ref := range models {
		var match *Resource
		for _, candidate := range run.Models {
			if candidate.Metadata.ID == ref.ID && candidate.Metadata.Digest == ref.Digest {
				copy := candidate
				match = &copy
				break
			}
		}
		if match == nil {
			return out, failure("failed_precondition", "fork model is outside the immutable resolved allowlist")
		}
		if e = validateAgentRoute(run, *match); e != nil {
			return out, e
		}
		selected = append(selected, *match)
	}
	source, e := load(ctx, tx, scope, "Branch", checkpoint.BranchID)
	if e != nil {
		return out, e
	}
	var sourceSpec BranchSpec
	if e = json.Unmarshal(source.Spec, &sourceSpec); e != nil {
		return out, e
	}
	out.Operation = operation(scope, checkpoint.BranchID, "fork")
	ids := []string{}
	for _, model := range selected {
		branch := makeResource(scope, "Branch", newID(), BranchSpec{RunID: sourceSpec.RunID, Model: model, ParentCheckpointID: checkpointID, ParentBranchID: checkpoint.BranchID, ForkOperationID: out.Operation.Metadata.ID})
		out.Children = append(out.Children, branch)
		ids = append(ids, branch.Metadata.ID)
	}
	aggregateSpec := OperationSpec{BranchID: checkpoint.BranchID, Action: "fork", CheckpointID: checkpointID, ChildBranchIDs: ids}
	out.Operation.Spec = encode(aggregateSpec)
	out.Operation.Metadata.Digest = "sha256:" + hash(aggregateSpec)
	if e = insertResource(ctx, tx, out.Operation); e != nil {
		return out, e
	}
	for _, branch := range out.Children {
		if e = insertResource(ctx, tx, branch); e != nil {
			return out, e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO branches(tenant_id,project_id,id,desired,observed,execution_id,sandbox_id,epoch,lease_until,checkpoint_id,budget_remaining_ms,session_parent_checkpoint_id,agent_step,agent_state_digest,agent_gate) VALUES($1,$2,$3,'running','resuming',$4,$5,1,clock_timestamp()+($6*interval '1 millisecond'),$7,$6,$7,$8,$9,'configuring')`, scope.TenantID, scope.ProjectID, branch.Metadata.ID, newID(), newID(), checkpoint.BudgetRemainingMS, checkpointID, checkpoint.Barrier.Step, checkpoint.Barrier.StateDigest)
		if e != nil {
			return out, e
		}
		childOp := operation(scope, branch.Metadata.ID, "resume")
		spec := OperationSpec{BranchID: branch.Metadata.ID, Action: "resume", CheckpointID: checkpointID, ParentOperationID: out.Operation.Metadata.ID}
		childOp.Spec = encode(spec)
		childOp.Metadata.Digest = "sha256:" + hash(spec)
		if e = insertResource(ctx, tx, childOp); e != nil {
			return out, e
		}
		if e = enqueue(ctx, tx, scope, branch.Metadata.ID, childOp, "resume", 1); e != nil {
			return out, e
		}
		if _, e = appendEvent(ctx, tx, scope, branch.Metadata.ID, 1, "branch_fork_admitted", map[string]any{"parent_branch_id": checkpoint.BranchID, "parent_checkpoint_id": checkpointID, "prefix_step": checkpoint.Barrier.Step, "model_digest": specModel(branch), "experiment_scope": "continuation", "replay": false}); e != nil {
			return out, e
		}
	}
	if e = recordIdem(ctx, tx, scope, key, request, out); e != nil {
		return out, e
	}
	return out, tx.Commit()
}
func specModel(branch Resource) string {
	var spec BranchSpec
	json.Unmarshal(branch.Spec, &spec)
	return spec.Model.Metadata.Digest
}

func refreshForkOperation(ctx context.Context, tx *sql.Tx, scope Scope, child Resource) error {
	var childSpec OperationSpec
	if e := json.Unmarshal(child.Spec, &childSpec); e != nil {
		return e
	}
	if childSpec.ParentOperationID == "" {
		return nil
	}
	parent, e := scanResource(tx.QueryRowContext(ctx, `SELECT body FROM resources WHERE tenant_id=$1 AND project_id=$2 AND kind='Operation' AND id=$3 FOR UPDATE`, scope.TenantID, scope.ProjectID, childSpec.ParentOperationID))
	if e != nil {
		return e
	}
	var parentSpec OperationSpec
	if e = json.Unmarshal(parent.Spec, &parentSpec); e != nil {
		return e
	}
	rows, e := tx.QueryContext(ctx, `SELECT body FROM resources WHERE tenant_id=$1 AND project_id=$2 AND kind='Operation' AND body->'spec'->>'parent_operation_id'=$3`, scope.TenantID, scope.ProjectID, parent.Metadata.ID)
	if e != nil {
		return e
	}
	succeeded, failed, total := 0, 0, 0
	for rows.Next() {
		op, e := scanResource(rows)
		if e != nil {
			rows.Close()
			return e
		}
		var status OperationStatus
		if e = json.Unmarshal(op.Status, &status); e != nil {
			rows.Close()
			return e
		}
		total++
		if status.State == "succeeded" {
			succeeded++
		}
		if status.State == "failed" || status.State == "superseded" {
			failed++
		}
	}
	if e = rows.Close(); e != nil {
		return e
	}
	state := "pending"
	message := ""
	if total != len(parentSpec.ChildBranchIDs) {
		message = "child operation records incomplete"
	} else if succeeded == total {
		state = "succeeded"
	} else if failed == total {
		state = "failed"
	} else if succeeded+failed == total {
		state = "partial_failure"
	} else if failed > 0 || succeeded > 0 {
		state = "partial"
	}
	parent.Status = encode(OperationStatus{State: state, Error: message})
	return save(ctx, tx, parent)
}
