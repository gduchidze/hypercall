package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/gduchidze/hypercall/internal/snapshot"
	"google.golang.org/protobuf/encoding/protojson"
	"unicode/utf8"

	harnessv1 "github.com/gduchidze/hypercall/gen/go/hypercall/harness/v1"
	"github.com/gduchidze/hypercall/internal/harness"
)

const agentProtocol = "hypercall.harness.v1"

func isCooperative(run RunSpec) bool {
	var task TaskSpec
	return json.Unmarshal(run.Task.Spec, &task) == nil && task.ExecutionMode == "cooperative_python_v1"
}
func validateAgentRoute(run RunSpec, selected Resource) error {
	if !isCooperative(run) {
		return failure("failed_precondition", "cooperative instrumentation is required")
	}
	var model ModelConfigSpec
	var gateway GatewaySpec
	if strict(selected.Spec, &model) != nil || strict(run.Gateway.Spec, &gateway) != nil {
		return failure("failed_precondition", "invalid resolved fixture routing")
	}
	value, exists := model.Parameters["fixture_response"]
	if model.Provider != "fixture" || model.Revision != "text-v1" || len(model.Parameters) != 1 || !exists || len(value) > 64<<10 || !utf8.ValidString(value) || model.ArtifactDigest != payloadDigest([]byte(value)) {
		return failure("failed_precondition", "cooperative inference requires the pinned text-v1 fixture")
	}
	if gateway.Routes["inference"] != "fixture://text-v1" || gateway.Routes["tool.echo"] != "fixture://echo-v1" {
		return failure("failed_precondition", "registered inference and echo fixtures required")
	}
	for name, route := range gateway.Routes {
		if name == "inference" && route == "fixture://text-v1" || name == "tool.echo" && route == "fixture://echo-v1" || name == "tool.unknown" && route == "fixture://unknown-effect-v1" {
			continue
		}
		return failure("failed_precondition", "unsupported cooperative fixture route")
	}
	if len(gateway.SecretRefs) != 0 || len(gateway.AllowedHosts) != 0 {
		return failure("failed_precondition", "offline fixture routing cannot have provider secrets or egress")
	}
	return nil
}
func agentRouteDigest(run RunSpec, selected Resource) string {
	return "sha256:" + hash([]string{selected.Metadata.Digest, run.Gateway.Metadata.Digest, "fixture-adapters-v1"})
}
func fillAgentAssignment(ctx context.Context, tx *sql.Tx, a *Assignment) error {
	branch, e := load(ctx, tx, a.Scope, "Branch", a.BranchID)
	if e != nil {
		return e
	}
	var spec BranchSpec
	if e = json.Unmarshal(branch.Spec, &spec); e != nil {
		return e
	}
	a.SelectedModel = spec.Model
	a.ParentBranchID = spec.ParentBranchID
	a.ParentCheckpointID = spec.ParentCheckpointID
	if !isCooperative(a.Run) {
		return nil
	}
	if e = validateAgentRoute(a.Run, a.SelectedModel); e != nil {
		return e
	}

	var stored, barrier []byte
	var parentCheckpoint sql.NullString
	e = tx.QueryRowContext(ctx, `SELECT harness_session,session_parent_checkpoint_id,sealed_barrier FROM branches WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID).Scan(&stored, &parentCheckpoint, &barrier)
	if e != nil {
		return e
	}
	if len(barrier) > 0 {
		var b AgentBarrier
		if e = json.Unmarshal(barrier, &b); e != nil {
			return e
		}
		a.Barrier = &b
	}
	if parentCheckpoint.Valid {
		a.ParentCheckpointID = parentCheckpoint.String
	}
	if len(stored) > 0 {
		a.HarnessSession = &harnessv1.HarnessSession{}
		if e = protojson.Unmarshal(stored, a.HarnessSession); e != nil {
			return e
		}
		if e = harness.ValidateSession(a.HarnessSession); e != nil {
			return e
		}
		if a.HarnessSession.InheritedBarrier != nil {
			a.ParentBranchID = a.HarnessSession.InheritedBarrier.SourceBranchId
		}
		if a.HarnessSession.ExecutionId != a.ExecutionID || a.HarnessSession.Epoch != a.Epoch || a.HarnessSession.SandboxId != a.SandboxID || a.HarnessSession.ModelDigest != a.SelectedModel.Metadata.Digest {
			return failure("aborted", "persisted harness session disagrees with current assignment")
		}
		return nil
	}
	session := &harnessv1.HarnessSession{TenantId: a.Scope.TenantID, ProjectId: a.Scope.ProjectID, BranchId: a.BranchID, SandboxId: a.SandboxID, ExecutionId: a.ExecutionID, Epoch: a.Epoch, CallNamespace: a.ExecutionID, RouteDigest: agentRouteDigest(a.Run, a.SelectedModel), ModelDigest: a.SelectedModel.Metadata.Digest, ProtocolVersion: agentProtocol}
	if a.ParentCheckpointID != "" {
		cp, e := loadCheckpoint(ctx, tx, a.Scope, a.BranchID, a.ParentCheckpointID, a.Run)
		if e != nil {
			return e
		}
		var manifest snapshot.Manifest
		if e = json.Unmarshal(cp.SnapshotManifest, &manifest); e != nil || manifest.SemanticBarrier == nil {
			return failure("failed_precondition", "semantic restore requires an immutable native barrier")
		}
		b := manifest.SemanticBarrier
		session.ParentCheckpointId = cp.NativeCheckpointID
		session.InheritedBarrier = &harnessv1.SemanticBarrier{ProtocolVersion: b.ProtocolVersion, Step: b.Step, EventSequence: b.EventSequence, StateDigest: b.StateDigest, SourceSessionHash: b.SourceSessionHash, SourceBranchId: b.SourceBranchID, SourceExecutionId: b.SourceExecutionID, SourceEpoch: b.SourceEpoch}
		a.ParentBranchID = cp.BranchID
	}
	a.HarnessSession, e = harness.SealSession(session)
	if e != nil {
		return e
	}
	raw, e := protojson.Marshal(a.HarnessSession)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `UPDATE branches SET harness_session=$4,session_hash=$5,route_digest=$6,session_ack_hash='',agent_gate='configuring',sealed_barrier=NULL WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID, raw, a.HarnessSession.SessionHash, a.HarnessSession.RouteDigest)
	return e
}
