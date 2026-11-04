package controlplane

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/gduchidze/hypercall/internal/harness"
)

func payloadDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type agentLock struct {
	gate, ack    string
	step, cursor int64
	stateDigest  string
}

func lockAgent(ctx context.Context, tx *sql.Tx, a Assignment) (agentLock, error) {
	var v agentLock
	var physical, execution, session string
	var epoch int64
	var alive bool
	e := tx.QueryRowContext(ctx, `SELECT sandbox_id,execution_id,epoch,session_hash,COALESCE(lease_until>clock_timestamp(),false) AND desired IN ('running','suspended'),agent_gate,session_ack_hash,agent_step,agent_cursor,agent_state_digest FROM branches WHERE tenant_id=$1 AND project_id=$2 AND id=$3 FOR UPDATE`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID).Scan(&physical, &execution, &epoch, &session, &alive, &v.gate, &v.ack, &v.step, &v.cursor, &v.stateDigest)
	if errors.Is(e, sql.ErrNoRows) {
		return v, failure("not_found", "agent branch not found")
	}
	if e != nil {
		return v, e
	}
	if !alive || physical != a.SandboxID || execution != a.ExecutionID || epoch != a.Epoch || a.HarnessSession == nil || harness.ValidateSession(a.HarnessSession) != nil || session != a.HarnessSession.SessionHash || a.HarnessSession.BranchId != a.BranchID || a.HarnessSession.ExecutionId != a.ExecutionID || a.HarnessSession.SandboxId != a.SandboxID || a.HarnessSession.Epoch != a.Epoch || a.HarnessSession.TenantId != a.Scope.TenantID || a.HarnessSession.ProjectId != a.Scope.ProjectID {
		return v, failure("aborted", "stale cooperative assignment, lease or session")
	}
	actual, e := loadRun(ctx, tx, a.Scope, a.BranchID)
	if e != nil {
		return v, e
	}
	branch, e := load(ctx, tx, a.Scope, "Branch", a.BranchID)
	if e != nil {
		return v, e
	}
	var spec BranchSpec
	if e = json.Unmarshal(branch.Spec, &spec); e != nil {
		return v, e
	}
	if hash(a.Run) != hash(actual) || hash(a.SelectedModel) != hash(spec.Model) || a.HarnessSession.ModelDigest != spec.Model.Metadata.Digest || a.HarnessSession.RouteDigest != agentRouteDigest(actual, spec.Model) {
		return v, failure("aborted", "assignment routing differs from immutable branch inputs")
	}
	if e = validateAgentRoute(actual, spec.Model); e != nil {
		return v, e
	}
	return v, nil
}
func (s *Store) AcknowledgeAgentSession(ctx context.Context, a Assignment, sessionHash string) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	v, e := lockAgent(ctx, tx, a)
	if e != nil {
		return e
	}
	if sessionHash != a.HarnessSession.SessionHash {
		return failure("aborted", "harness acknowledged another routing session")
	}
	if v.ack == sessionHash {
		return nil
	}
	if v.gate != "configuring" {
		return failure("failed_precondition", "harness session cannot reopen this gate")
	}
	_, e = tx.ExecContext(ctx, `UPDATE branches SET session_ack_hash=$4,agent_gate='open' WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID, sessionHash)
	if e != nil {
		return e
	}
	if _, e = appendEvent(ctx, tx, a.Scope, a.BranchID, a.Epoch, "agent_session_acknowledged", map[string]string{"session_hash": sessionHash, "route_digest": a.HarnessSession.RouteDigest, "model_digest": a.HarnessSession.ModelDigest}); e != nil {
		return e
	}
	rows, e := tx.QueryContext(ctx, `SELECT body FROM resources WHERE tenant_id=$1 AND project_id=$2 AND kind='Operation' AND body->'spec'->>'branch_id'=$3 AND body->'status'->>'state'='awaiting_session'`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID)
	if e != nil {
		return e
	}
	operations := []Resource{}
	for rows.Next() {
		r, e := scanResource(rows)
		if e != nil {
			rows.Close()
			return e
		}
		operations = append(operations, r)
	}
	if e = rows.Close(); e != nil {
		return e
	}
	for _, op := range operations {
		op.Status = encode(OperationStatus{State: "succeeded"})
		if e = save(ctx, tx, op); e != nil {
			return e
		}
		if e = refreshForkOperation(ctx, tx, a.Scope, op); e != nil {
			return e
		}
	}

	return tx.Commit()
}
func validateAgentRequest(a Assignment, r AgentRequest) error {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._/-]{0,127}$`).MatchString(r.ID) || r.Step < 1 || r.Step > 100000 || r.Position < 1 || r.Position > 1024 || len(r.Payload) > 64<<10 || r.PayloadDigest != payloadDigest(r.Payload) || a.HarnessSession == nil || r.SessionHash != a.HarnessSession.SessionHash {
		return failure("invalid_argument", "bounded registered action with matching payload/session digest required")
	}
	switch r.Kind {
	case "inference", "tool", "step_commit", "finalize":
	default:
		return failure("invalid_argument", "unsupported agent action")
	}
	return nil
}
func (s *Service) ProcessAgentRequest(ctx context.Context, a Assignment, r AgentRequest) (AgentResult, error) {
	if e := validateScope(a.Scope); e != nil {
		return AgentResult{}, e
	}
	if e := validateAgentRequest(a, r); e != nil {
		return AgentResult{}, e
	}
	return s.store.processAgentRequest(ctx, a, r)
}
func (s *Store) processAgentRequest(ctx context.Context, a Assignment, r AgentRequest) (AgentResult, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return AgentResult{}, e
	}
	defer tx.Rollback()
	v, e := lockAgent(ctx, tx, a)
	if e != nil {
		return AgentResult{}, e
	}
	requestHash := hash(r)
	var priorHash, state, token string
	var encoded []byte
	var claimAlive bool
	var mutating bool
	e = tx.QueryRowContext(ctx, `SELECT request_hash,state,result,COALESCE(claim_token,''),COALESCE(lease_until>clock_timestamp(),false),mutating FROM agent_calls WHERE tenant_id=$1 AND project_id=$2 AND branch_id=$3 AND id=$4`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID, r.ID).Scan(&priorHash, &state, &encoded, &token, &claimAlive, &mutating)
	if e == nil {
		if priorHash != requestHash {
			return AgentResult{}, failure("already_exists", "agent request ID reused with changed input")
		}
		if len(encoded) > 0 {
			var result AgentResult
			if e = json.Unmarshal(encoded, &result); e != nil {
				return result, e
			}
			result.Replayed = true
			return result, nil
		}
		if claimAlive {
			return AgentResult{ID: r.ID, State: "pending"}, nil
		}
		// A mutating call with lost execution ownership is never automatically replayed.
		if mutating {
			return s.completeUnknown(ctx, tx, a, r, "unknown_effect")
		}
	} else if !errors.Is(e, sql.ErrNoRows) {
		return AgentResult{}, e
	}
	if v.gate != "open" || v.ack != r.SessionHash {
		return AgentResult{}, failure("failed_precondition", "agent gateway gate is closed")
	}
	if errors.Is(e, sql.ErrNoRows) {
		if r.Kind == "finalize" {
			if r.Step != v.step || v.step < 1 {
				return AgentResult{}, failure("failed_precondition", "finalize requires the last committed step")
			}
			var uncommittedTail bool
			if e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_calls WHERE tenant_id=$1 AND project_id=$2 AND branch_id=$3 AND step>$4)`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID, r.Step).Scan(&uncommittedTail); e != nil {
				return AgentResult{}, e
			}
			if uncommittedTail {
				return AgentResult{}, failure("failed_precondition", "finalize cannot skip registered actions beyond the committed step")
			}
		} else if r.Step != v.step+1 {
			return AgentResult{}, failure("failed_precondition", "agent steps must commit in order")
		}
		var unresolved bool
		e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_calls WHERE tenant_id=$1 AND project_id=$2 AND branch_id=$3 AND state IN ('registered','executing','unknown'))`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID).Scan(&unresolved)
		if e != nil {
			return AgentResult{}, e
		}
		if unresolved {
			return AgentResult{}, failure("failed_precondition", "unresolved registered action blocks progression")
		}
		var position int
		e = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),0) FROM agent_calls WHERE tenant_id=$1 AND project_id=$2 AND branch_id=$3 AND step=$4`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID, r.Step).Scan(&position)
		if e != nil {
			return AgentResult{}, e
		}
		if int(r.Position) != position+1 {
			return AgentResult{}, failure("failed_precondition", "agent subevents must be contiguous")
		}
		adapter, e := agentAdapter(a, r)
		if e != nil {
			return AgentResult{}, e
		}
		mutating = adapter == "unknown-effect-v1"
		event, e := appendEvent(ctx, tx, a.Scope, a.BranchID, a.Epoch, "agent_action_registered", map[string]any{"request": r, "model_digest": a.HarnessSession.ModelDigest, "route_digest": a.HarnessSession.RouteDigest, "synthetic": true})
		if e != nil {
			return AgentResult{}, e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO agent_calls(tenant_id,project_id,branch_id,id,step,position,registered_sequence,kind,request_hash,request,execution_id,epoch,model_digest,route_digest,adapter_version,mutating,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'registered')`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID, r.ID, r.Step, r.Position, event.Sequence, r.Kind, requestHash, encode(r), a.ExecutionID, a.Epoch, a.HarnessSession.ModelDigest, a.HarnessSession.RouteDigest, adapter, mutating)
		if e != nil {
			return AgentResult{}, e
		}
	}
	token = newID()
	_, e = tx.ExecContext(ctx, `UPDATE agent_calls SET state='executing',claim_token=$5,claim_epoch=claim_epoch+1,lease_until=clock_timestamp()+interval '5 seconds' WHERE tenant_id=$1 AND project_id=$2 AND branch_id=$3 AND id=$4`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID, r.ID, token)
	if e != nil {
		return AgentResult{}, e
	}
	if e = tx.Commit(); e != nil {
		return AgentResult{}, e
	}
	// Dispatch starts only after durable registration. These registered adapters are
	// pure local fixtures; no HTTP/provider/model fallback is implemented.
	result, e := executeAgentFixture(a, r)
	if e != nil {
		return result, e
	}
	return s.finishAgentRequest(ctx, a, r, token, result)
}
func agentAdapter(a Assignment, r AgentRequest) (string, error) {
	if e := validateAgentRoute(a.Run, a.SelectedModel); e != nil {
		return "", e
	}
	var gateway GatewaySpec
	json.Unmarshal(a.Run.Gateway.Spec, &gateway)
	switch r.Kind {
	case "inference":
		if r.Name != "inference" {
			return "", failure("invalid_argument", "fixture inference name required")
		}
		return "text-v1", nil
	case "tool":
		if r.Name == "echo" && gateway.Routes["tool.echo"] == "fixture://echo-v1" {
			return "echo-v1", nil
		}
		if r.Name == "unknown" && gateway.Routes["tool.unknown"] == "fixture://unknown-effect-v1" {
			return "unknown-effect-v1", nil
		}
		return "", failure("invalid_argument", "unregistered fixture tool")
	case "step_commit", "finalize":
		return agentProtocol, nil
	}
	return "", failure("invalid_argument", "unsupported registered adapter")
}
func executeAgentFixture(a Assignment, r AgentRequest) (AgentResult, error) {
	result := AgentResult{ID: r.ID, State: "completed"}
	switch r.Kind {
	case "inference":
		var model ModelConfigSpec
		if e := json.Unmarshal(a.SelectedModel.Spec, &model); e != nil {
			return result, e
		}
		result.Payload = []byte(model.Parameters["fixture_response"])
	case "tool":
		if r.Name == "unknown" {
			result.State = "unknown"
			result.ErrorCode = "unknown_effect"
		} else {
			result.Payload = r.Payload
		}
	case "step_commit":
		result.State = "committed"
		result.Payload = []byte(r.PayloadDigest)
	case "finalize":
		result.State = "finalized"
		result.Payload = r.Payload
	}
	return result, nil
}
func (s *Store) completeUnknown(ctx context.Context, tx *sql.Tx, a Assignment, r AgentRequest, code string) (AgentResult, error) {
	result := AgentResult{ID: r.ID, State: "unknown", ErrorCode: code}
	event, e := appendEvent(ctx, tx, a.Scope, a.BranchID, a.Epoch, "agent_action_unknown", map[string]any{"call_id": r.ID, "reason": code, "synthetic": true})
	if e != nil {
		return result, e
	}
	result.EventSequence = event.Sequence
	_, e = tx.ExecContext(ctx, `UPDATE agent_calls SET state='unknown',result=$5,claim_token=NULL WHERE tenant_id=$1 AND project_id=$2 AND branch_id=$3 AND id=$4`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID, r.ID, encode(result))
	if e != nil {
		return result, e
	}
	return result, tx.Commit()
}
func (s *Store) finishAgentRequest(ctx context.Context, a Assignment, r AgentRequest, token string, result AgentResult) (AgentResult, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return result, e
	}
	defer tx.Rollback()
	v, e := lockAgent(ctx, tx, a)
	if e != nil {
		return result, e
	}
	var valid bool
	e = tx.QueryRowContext(ctx, `SELECT claim_token=$5 AND lease_until>clock_timestamp() AND state='executing' FROM agent_calls WHERE tenant_id=$1 AND project_id=$2 AND branch_id=$3 AND id=$4 FOR UPDATE`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID, r.ID, token).Scan(&valid)
	if e != nil {
		return result, e
	}
	if !valid {
		return result, failure("aborted", "stale agent dispatch claim")
	}
	if v.gate != "open" {
		return result, failure("aborted", "agent completion cannot cross a sealed checkpoint")
	}
	event, e := appendEvent(ctx, tx, a.Scope, a.BranchID, a.Epoch, "agent_action_"+result.State, map[string]any{"request": r, "result": result, "model_digest": a.HarnessSession.ModelDigest, "route_digest": a.HarnessSession.RouteDigest, "synthetic": true})
	if e != nil {
		return result, e
	}
	result.EventSequence = event.Sequence
	_, e = tx.ExecContext(ctx, `UPDATE agent_calls SET state=$5,result=$6,claim_token=NULL WHERE tenant_id=$1 AND project_id=$2 AND branch_id=$3 AND id=$4`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID, r.ID, result.State, encode(result))
	if e != nil {
		return result, e
	}
	if r.Kind == "step_commit" {
		_, e = tx.ExecContext(ctx, `INSERT INTO agent_steps VALUES($1,$2,$3,$4,$5,$6,$7,true)`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID, r.Step, r.PayloadDigest, event.Sequence, r.SessionHash)
		if e != nil {
			return result, e
		}
		_, e = tx.ExecContext(ctx, `UPDATE branches SET agent_step=$4,agent_cursor=$5,agent_state_digest=$6 WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID, r.Step, event.Sequence, r.PayloadDigest)
		if e != nil {
			return result, e
		}
	}
	if r.Kind == "finalize" {
		_, e = tx.ExecContext(ctx, `UPDATE branches SET trace_complete=true,agent_gate='finalized',agent_cursor=$4 WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, a.Scope.TenantID, a.Scope.ProjectID, a.BranchID, event.Sequence)
		if e != nil {
			return result, e
		}
	}
	return result, tx.Commit()
}
func (s *Service) Steps(ctx context.Context, scope Scope, branch string, after int64, limit int) ([]AgentStep, error) {
	if e := validateScope(scope); e != nil {
		return nil, e
	}
	if _, e := s.store.GetResource(ctx, scope, "Branch", branch); e != nil {
		return nil, e
	}
	if after < 0 {
		return nil, failure("invalid_argument", "negative step cursor")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, e := s.store.db.QueryContext(ctx, `SELECT step,state_digest,event_sequence,session_hash,complete FROM agent_steps WHERE tenant_id=$1 AND project_id=$2 AND branch_id=$3 AND step>$4 ORDER BY step LIMIT $5`, scope.TenantID, scope.ProjectID, branch, after, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []AgentStep{}
	for rows.Next() {
		v := AgentStep{BranchID: branch}
		if e = rows.Scan(&v.Step, &v.StateDigest, &v.EventSequence, &v.SessionHash, &v.Complete); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
