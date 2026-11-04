package controlplane

import (
	"context"
	"encoding/json"
	"github.com/gduchidze/hypercall/internal/snapshot"
)

func (s *Store) SealAgentBarrier(ctx context.Context, a Assignment, barrier AgentBarrier) (AgentBarrier, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return AgentBarrier{}, e
	}
	defer tx.Rollback()
	v, e := lockAgent(ctx, tx, a)
	if e != nil {
		return AgentBarrier{}, e
	}
	if barrier.ProtocolVersion != agentProtocol || barrier.SessionHash != a.HarnessSession.SessionHash || barrier.Step != v.step || barrier.Step < 1 || barrier.StateDigest != v.stateDigest {
		return AgentBarrier{}, failure("failed_precondition", "barrier does not match the last committed instrumented step")
	}
	var saved []byte
	if e = tx.QueryRowContext(ctx, `SELECT sealed_barrier FROM branches WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID).Scan(&saved); e != nil {
		return AgentBarrier{}, e
	}
	if v.gate == "sealed" && len(saved) > 0 {
		var sealed AgentBarrier
		if e = json.Unmarshal(saved, &sealed); e != nil {
			return sealed, e
		}
		if barrier.EventSequence != v.cursor && barrier.EventSequence != sealed.EventSequence {
			return sealed, failure("failed_precondition", "barrier cursor changed")
		}
		return sealed, nil
	}
	if v.gate != "open" || barrier.EventSequence != v.cursor {
		return AgentBarrier{}, failure("failed_precondition", "barrier requires the exact committed cursor and open acknowledged session")
	}
	var unresolved bool
	e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_calls WHERE tenant_id=$1 AND project_id=$2 AND branch_id=$3 AND (state IN ('registered','executing','unknown') OR step>$4))`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID, barrier.Step).Scan(&unresolved)
	if e != nil {
		return AgentBarrier{}, e
	}
	if unresolved {
		return AgentBarrier{}, failure("failed_precondition", "unknown or in-flight effects make this step uncheckpointable")
	}
	event, e := appendEvent(ctx, tx, a.Scope, a.BranchID, a.Epoch, "agent_barrier_sealed", barrier)
	if e != nil {
		return AgentBarrier{}, e
	}
	barrier.EventSequence = event.Sequence
	_, e = tx.ExecContext(ctx, `UPDATE branches SET agent_gate='sealed',sealed_barrier=$4 WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID, encode(barrier))
	if e != nil {
		return AgentBarrier{}, e
	}
	return barrier, tx.Commit()
}
func (s *Service) Calls(ctx context.Context, scope Scope, branch string, after int64, limit int) ([]AgentCall, error) {
	if e := validateScope(scope); e != nil {
		return nil, e
	}
	if _, e := s.store.GetResource(ctx, scope, "Branch", branch); e != nil {
		return nil, e
	}
	if after < 0 {
		return nil, failure("invalid_argument", "negative call cursor")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, e := s.store.db.QueryContext(ctx, `SELECT request,result,execution_id,epoch,model_digest,route_digest,adapter_version,mutating,created_at,registered_sequence FROM agent_calls WHERE tenant_id=$1 AND project_id=$2 AND branch_id=$3 AND registered_sequence>$4 AND kind IN ('inference','tool') ORDER BY registered_sequence LIMIT $5`, scope.TenantID, scope.ProjectID, branch, after, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []AgentCall{}
	for rows.Next() {
		var call AgentCall
		var request, result []byte
		if e = rows.Scan(&request, &result, &call.ExecutionID, &call.Epoch, &call.ModelDigest, &call.RouteDigest, &call.AdapterVersion, &call.Mutating, &call.CreatedAt, &call.RegisteredSequence); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(request, &call.Request); e != nil {
			return nil, e
		}
		if len(result) > 0 {
			if e = json.Unmarshal(result, &call.Result); e != nil {
				return nil, e
			}
		} else {
			call.Result = AgentResult{ID: call.Request.ID, State: "pending"}
		}
		out = append(out, call)
	}
	return out, rows.Err()
}

func validateSemanticCheckpoint(a Assignment, cp CheckpointSpec) error {
	if a.Barrier == nil || cp.Barrier == nil || hash(a.Barrier) != hash(cp.Barrier) || cp.EventPosition != a.Barrier.EventSequence || cp.RouteDigest != a.HarnessSession.RouteDigest {
		return failure("failed_precondition", "checkpoint does not preserve the sealed harness marker")
	}
	var manifest snapshot.Manifest
	if json.Unmarshal(cp.SnapshotManifest, &manifest) != nil || manifest.SemanticBarrier == nil {
		return failure("failed_precondition", "native manifest lacks semantic barrier binding")
	}
	b := manifest.SemanticBarrier
	if b.Validate() != nil || b.ProtocolVersion != a.Barrier.ProtocolVersion || b.Step != a.Barrier.Step || b.EventSequence != a.Barrier.EventSequence || b.StateDigest != a.Barrier.StateDigest || b.SourceSessionHash != a.HarnessSession.SessionHash || b.SourceBranchID != a.BranchID || b.SourceExecutionID != a.ExecutionID || b.SourceEpoch != a.Epoch || manifest.Scope.TenantID != a.Scope.TenantID || manifest.Scope.ProjectID != a.Scope.ProjectID || manifest.Scope.CheckpointID != cp.NativeCheckpointID {
		return failure("failed_precondition", "native artifacts contradict the committed harness marker")
	}
	return nil
}
