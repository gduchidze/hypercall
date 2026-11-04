package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gduchidze/hypercall/internal/eval"
)

type Evaluation struct {
	ID          string     `json:"id"`
	OperationID string     `json:"operation_id"`
	InputDigest string     `json:"input_digest"`
	Plan        *eval.Plan `json:"plan"`
	State       string     `json:"state"`
	Attempts    int        `json:"attempts"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

func (s *Service) SubmitEvaluation(ctx context.Context, scope Scope, yaml string, observations []eval.Observation, key string) (Evaluation, error) {
	if err := validateScope(scope); err != nil {
		return Evaluation{}, err
	}
	plan, err := eval.Compile([]byte(yaml))
	if err != nil {
		return Evaluation{}, failure("invalid_argument", err.Error())
	}
	if len(observations) > 10000 {
		return Evaluation{}, failure("invalid_argument", "at most 10000 observations")
	}
	requested := append([]eval.Observation(nil), observations...)
	request := evaluationRequest(plan, requested)
	if prior, found, e := s.store.existingEvaluation(ctx, scope, key, request); e != nil || found {
		return prior, e
	}
	if plan.Config.BaselineReportID != "" {
		baseline, e := s.GetReport(ctx, scope, plan.Config.BaselineReportID)
		if e != nil {
			return Evaluation{}, e
		}
		if baseline.Completed != baseline.Expected {
			return Evaluation{}, failure("invalid_argument", "baseline report is incomplete")
		}
		for _, candidate := range plan.Config.Candidates {
			found := false
			for _, prior := range baseline.Candidates {
				found = found || prior.ID == candidate.ID
			}
			if !found {
				return Evaluation{}, failure("invalid_argument", "baseline must include each candidate identity")
			}
		}
		plan.Baseline = baseline
	}
	if prefix := plan.Config.SharedPrefix; prefix != nil && prefix.Cost != nil && prefix.Cost.Kind != "synthetic" {
		return Evaluation{}, failure("invalid_argument", "public evaluation accepts synthetic prefix cost only; real prefix metering is unavailable")
	}
	observations = append([]eval.Observation(nil), observations...)
	seenCalls := map[string]bool{}
	commonCheckpoint, commonInputs := "", ""
	// Branch-backed evidence is resolved from the authoritative completed ledger.
	// Client values cannot invent output, trace finalization, models or metering.
	for i, o := range observations {
		if o.Source != "branch" {
			continue
		}
		if o.Output != nil || o.State != "" || o.TraceComplete || o.Measurements != nil || o.Fixture || o.AdapterVersion != "" {
			return Evaluation{}, failure("invalid_argument", "branch evidence accepts references only")
		}
		branch, e := s.GetResource(ctx, scope, "Branch", o.BranchID)
		if e != nil {
			return Evaluation{}, e
		}
		var spec BranchSpec
		var status BranchStatus
		if json.Unmarshal(branch.Spec, &spec) != nil || json.Unmarshal(branch.Status, &status) != nil {
			return Evaluation{}, failure("failed_precondition", "invalid branch metadata")
		}
		var cell *eval.Cell
		for j := range plan.Cells {
			if plan.Cells[j].ID == o.CellID {
				cell = &plan.Cells[j]
				break
			}
		}
		if cell == nil || cell.ModelDigest != spec.Model.Metadata.Digest {
			return Evaluation{}, failure("invalid_argument", "branch model differs from planned candidate")
		}
		if (plan.Config.Scope == "whole_task" && spec.ParentCheckpointID != "") || (plan.Config.Scope == "continuation" && spec.ParentCheckpointID == "") {
			return Evaluation{}, failure("invalid_argument", "branch does not match experiment scope")
		}
		identity := o.BranchID
		if seenCalls[identity] {
			return Evaluation{}, failure("invalid_argument", "one branch execution cannot populate multiple matrix cells")
		}
		seenCalls[identity] = true
		run, e := s.store.loadEvaluationRun(ctx, scope, o.BranchID)
		if e != nil {
			return Evaluation{}, e
		}
		var task TaskSpec
		if json.Unmarshal(run.Task.Spec, &task) != nil {
			return Evaluation{}, failure("failed_precondition", "branch task metadata invalid")
		}
		if task.DatasetDigest != plan.Config.WorkloadDigest {
			return Evaluation{}, failure("invalid_argument", "evaluation workload_digest must match branch dataset digest")
		}
		var gateway GatewaySpec
		_ = json.Unmarshal(run.Gateway.Spec, &gateway)
		delete(gateway.Routes, "inference")
		inputs := hash(struct {
			Agent, Workspace, Template, Mode string
			Command                          []string
			Env                              map[string]string
			Limits                           Limits
			ToolPolicy                       GatewaySpec
		}{task.AgentDigest, run.Workspace.Metadata.Digest, task.TemplateDigest, task.ExecutionMode, task.Command, task.Env, task.Limits, gateway})
		if commonInputs != "" && commonInputs != inputs {
			return Evaluation{}, failure("invalid_argument", "branches differ in fixed agent, workspace or template inputs")
		}
		commonInputs = inputs
		if plan.Config.Scope == "continuation" {
			if commonCheckpoint != "" && commonCheckpoint != spec.ParentCheckpointID {
				return Evaluation{}, failure("invalid_argument", "continuations must share one retained checkpoint")
			}
			commonCheckpoint = spec.ParentCheckpointID
			if plan.Config.SharedPrefix != nil && plan.Config.SharedPrefix.ID != commonCheckpoint {
				return Evaluation{}, failure("invalid_argument", "shared prefix identity differs from retained checkpoint")
			}
		}
		calls, e := s.evaluationCalls(ctx, scope, o.BranchID)
		if e != nil {
			return Evaluation{}, e
		}
		found := false
		for _, call := range calls {
			if call.Request.ID != o.CallID {
				continue
			}
			found = true
			if call.Request.Kind != "inference" || call.Result.State != "completed" || call.Result.ErrorCode != "" || !utf8.Valid(call.Result.Payload) {
				return Evaluation{}, failure("failed_precondition", "selected inference must have a committed UTF-8 response")
			}
			output := string(call.Result.Payload)
			observations[i].Output = &output
			observations[i].State = "complete"
			observations[i].TraceComplete = status.TraceComplete
			if status.Error != "" || status.Observed == "failed" || status.Observed == "cancelled" || status.Observed == "interrupted" {
				observations[i].State = "execution_error"
			}
			var model ModelConfigSpec
			_ = json.Unmarshal(spec.Model.Spec, &model)
			observations[i].Fixture = model.Provider == "fixture" || strings.Contains(call.AdapterVersion, "fixture")
			observations[i].AdapterVersion = call.AdapterVersion
		}
		if !found {
			return Evaluation{}, failure("not_found", "selected inference call not found")
		}
	}
	// Validate observation identities and source types without invoking scorers.
	if commonInputs != "" {
		plan.EvidenceDigest = "sha256:" + hash([]string{commonInputs, commonCheckpoint})
	}
	if plan.Baseline != nil && plan.Baseline.CompatibilityDigest != eval.PlanCompatibilityDigest(plan) {
		return Evaluation{}, failure("invalid_argument", "baseline report uses incompatible scope, workload, cases, repetitions, cache policy or retained checkpoint/fixed inputs")
	}
	if serialized, err := json.Marshal(plan); err != nil || len(serialized) > 3<<20 {
		return Evaluation{}, failure("invalid_argument", "resolved plan and baseline exceed 3MiB public inspection envelope")
	}
	if err = eval.ValidateObservations(plan, observations); err != nil {
		return Evaluation{}, failure("invalid_argument", err.Error())
	}
	return s.store.submitEvaluation(ctx, scope, plan, observations, requested, key)
}
func evaluationRequest(plan *eval.Plan, observations []eval.Observation) any {
	return struct {
		Action       string
		Config       eval.Config
		Observations []eval.Observation
	}{"evaluate", plan.Config, observations}
}
func (s *Store) existingEvaluation(ctx context.Context, scope Scope, key string, request any) (Evaluation, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Evaluation{}, false, err
	}
	defer tx.Rollback()
	var out Evaluation
	found, err := idem(ctx, tx, scope, key, request, &out)
	return out, found, err
}
func (s *Store) loadEvaluationRun(ctx context.Context, scope Scope, id string) (RunSpec, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunSpec{}, err
	}
	defer tx.Rollback()
	return loadRun(ctx, tx, scope, id)
}
func (s *Service) evaluationCalls(ctx context.Context, scope Scope, branch string) ([]AgentCall, error) {
	out := []AgentCall{}
	cursor := int64(0)
	for len(out) < 10000 {
		page, err := s.Calls(ctx, scope, branch, cursor, 1000)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < 1000 {
			return out, nil
		}
		cursor = page[len(page)-1].RegisteredSequence
	}
	return nil, failure("resource_exhausted", "evaluation trace exceeds 10000 call bound")
}
func (s *Store) submitEvaluation(ctx context.Context, scope Scope, plan *eval.Plan, observations, requested []eval.Observation, key string) (Evaluation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Evaluation{}, err
	}
	defer tx.Rollback()
	request := evaluationRequest(plan, requested)
	var out Evaluation
	found, err := idem(ctx, tx, scope, key, request, &out)
	if err != nil || found {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO projects VALUES($1,$2) ON CONFLICT DO NOTHING`, scope.TenantID, scope.ProjectID); err != nil {
		return out, err
	}
	out = Evaluation{ID: newID(), InputDigest: "sha256:" + hash(request), Plan: plan, State: "pending"}
	op := operation(scope, "", "evaluate")
	op.Spec = encode(OperationSpec{Action: "evaluate", EvaluationID: out.ID})
	out.OperationID = op.Metadata.ID
	if err = insertResource(ctx, tx, op); err != nil {
		return out, err
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO evaluations(tenant_id,project_id,id,operation_id,input_digest,plan,observations) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`, scope.TenantID, scope.ProjectID, out.ID, out.OperationID, out.InputDigest, encode(plan), encode(observations)).Scan(&out.CreatedAt)
	if err != nil {
		return out, err
	}
	if err = recordIdem(ctx, tx, scope, key, request, out); err != nil {
		return out, err
	}
	return out, tx.Commit()
}
func (s *Service) GetEvaluation(ctx context.Context, scope Scope, id string) (Evaluation, error) {
	var out Evaluation
	if err := validateScope(scope); err != nil {
		return out, err
	}
	var plan []byte
	var completed sql.NullTime
	err := s.store.db.QueryRowContext(ctx, `SELECT id,operation_id,input_digest,plan,state,attempts,created_at,completed_at FROM evaluations WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, scope.TenantID, scope.ProjectID, id).Scan(&out.ID, &out.OperationID, &out.InputDigest, &plan, &out.State, &out.Attempts, &out.CreatedAt, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return out, failure("not_found", "evaluation not found")
	}
	if err != nil {
		return out, err
	}
	if completed.Valid {
		out.CompletedAt = &completed.Time
	}
	err = json.Unmarshal(plan, &out.Plan)
	return out, err
}
func (s *Service) GetReport(ctx context.Context, scope Scope, id string) (*eval.Report, error) {
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	var data []byte
	err := s.store.db.QueryRowContext(ctx, `SELECT report FROM evaluations WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, scope.TenantID, scope.ProjectID, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, failure("not_found", "evaluation not found")
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, failure("failed_precondition", "evaluation report is not complete")
	}
	var report eval.Report
	err = json.Unmarshal(data, &report)
	return &report, err
}
func (s *Service) WorkEvaluation(ctx context.Context, owner string, ttl time.Duration, scorer eval.Scorer) error {
	if owner == "" || ttl < time.Millisecond || ttl > time.Minute {
		return failure("invalid_argument", "owner and lease 1ms..1m required")
	}
	var scope Scope
	var id, op string
	var epoch int64
	var attempts int
	var planJSON, input []byte
	err := s.store.db.QueryRowContext(ctx, `UPDATE evaluations SET state='running',owner=$1,claim_epoch=claim_epoch+1,attempts=attempts+1,lease_until=clock_timestamp()+($2*interval '1 second') WHERE (tenant_id,project_id,id)=(SELECT tenant_id,project_id,id FROM evaluations WHERE state='pending' OR (state='running' AND lease_until<clock_timestamp()) ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING tenant_id,project_id,id,operation_id,claim_epoch,attempts,plan,observations`, owner, ttl.Seconds()).Scan(&scope.TenantID, &scope.ProjectID, &id, &op, &epoch, &attempts, &planJSON, &input)
	if errors.Is(err, sql.ErrNoRows) {
		return failure("not_found", "no evaluation work")
	}
	if err != nil {
		return err
	}
	var plan eval.Plan
	var observations []eval.Observation
	if err = json.Unmarshal(planJSON, &plan); err != nil {
		return err
	}
	if err = json.Unmarshal(input, &observations); err != nil {
		return err
	}
	workCtx, cancel := context.WithCancel(ctx)
	budget := plan.Config.Policy.TimeoutSeconds
	if budget == 0 {
		budget = 300
	}
	budgetCtx, finishBudget := context.WithTimeout(workCtx, time.Duration(budget)*time.Second)
	defer finishBudget()
	defer cancel()
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		ticker := time.NewTicker(max(ttl/3, time.Millisecond))
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				result, e := s.store.db.ExecContext(workCtx, `UPDATE evaluations SET lease_until=clock_timestamp()+($6*interval '1 second') WHERE tenant_id=$1 AND project_id=$2 AND id=$3 AND owner=$4 AND claim_epoch=$5 AND lease_until>clock_timestamp() AND state='running'`, scope.TenantID, scope.ProjectID, id, owner, epoch, ttl.Seconds())
				if e != nil {
					cancel()
					return
				}
				n, _ := result.RowsAffected()
				if n != 1 {
					cancel()
					return
				}
			}
		}
	}()
	report, err := eval.Evaluate(budgetCtx, &plan, observations, scorer)
	if err != nil && errors.Is(budgetCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		failed := append([]eval.Observation(nil), observations...)
		for i := range failed {
			failed[i].State = "evaluation_error"
			failed[i].Output = nil
		}
		report, err = eval.Evaluate(ctx, &plan, failed, nil)
	}
	cancel()
	<-renewed
	if err != nil {
		return fmt.Errorf("evaluate retained work: %w", err)
	}
	report.EvaluationAttempts = attempts
	if attempts > 1 {
		hasScorer := false
		for _, cell := range plan.Cells {
			for _, a := range cell.Assertions {
				hasScorer = hasScorer || a.Type == "python" || a.Type == "llm-judge"
			}
		}
		if hasScorer {
			report.TotalCostMicroUSD = nil
			report.ScoringCostMicroUSD = nil
			for i := range report.Candidates {
				report.Candidates[i].CostMicroUSD = nil
			}
			report.Pareto = []string{}
			if plan.Config.Policy.MaxCostMicroUSD != nil {
				report.CI.Pass = false
				report.CI.Reasons = append(report.CI.Reasons, "measurement_missing")
			}
		}
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE evaluations SET state='completed',report=$6,completed_at=clock_timestamp(),lease_until=NULL WHERE tenant_id=$1 AND project_id=$2 AND id=$3 AND owner=$4 AND claim_epoch=$5 AND state='running' AND lease_until>clock_timestamp()`, scope.TenantID, scope.ProjectID, id, owner, epoch, encode(report))
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return failure("aborted", "evaluation ownership expired or was superseded")
	}
	operation, err := load(ctx, tx, scope, "Operation", op)
	if err != nil {
		return err
	}
	operation.Status = encode(OperationStatus{State: "succeeded"})
	if err = save(ctx, tx, operation); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO eval_outbox(id,tenant_id,project_id,evaluation_id,report) VALUES($1,$2,$3,$4,$5)`, newID(), scope.TenantID, scope.ProjectID, id, encode(report)); err != nil {
		return err
	}
	return tx.Commit()
}
