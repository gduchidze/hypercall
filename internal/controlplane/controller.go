package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	guestv1 "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
)

type Job struct {
	ID                                   string
	Scope                                Scope
	BranchID, OperationID, Action, Owner string
	ClaimEpoch, DesiredRevision          int64
	LeaseUntil                           time.Time
	Assignment                           Assignment
	CheckpointID                         string
	Checkpoint                           *CheckpointSpec
	CheckpointError                      error
}

func (s *Store) Claim(ctx context.Context, owner string, ttl time.Duration) (Job, error) {
	var j Job
	if owner == "" || ttl < time.Millisecond || ttl > time.Minute {
		return j, failure("invalid_argument", "worker and claim TTL (1ms..1m) required")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return j, e
	}
	defer tx.Rollback()
	e = tx.QueryRowContext(ctx, `UPDATE jobs SET owner=$1,claim_epoch=claim_epoch+1,lease_until=clock_timestamp()+($2*interval '1 second') WHERE id=(SELECT id FROM jobs WHERE NOT done AND available_at<=clock_timestamp() AND (lease_until IS NULL OR lease_until<clock_timestamp()) ORDER BY available_at,id FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING id,tenant_id,project_id,branch_id,operation_id,action,desired_revision,owner,claim_epoch,lease_until,COALESCE(checkpoint_id,'')`, owner, ttl.Seconds()).Scan(&j.ID, &j.Scope.TenantID, &j.Scope.ProjectID, &j.BranchID, &j.OperationID, &j.Action, &j.DesiredRevision, &j.Owner, &j.ClaimEpoch, &j.LeaseUntil, &j.CheckpointID)
	if errors.Is(e, sql.ErrNoRows) {
		return j, failure("not_found", "no work")
	}
	if e != nil {
		return j, e
	}
	var epoch int64
	var until sql.NullTime
	var execution, sandboxID string
	e = tx.QueryRowContext(ctx, `SELECT execution_id,epoch,lease_until,sandbox_id FROM branches WHERE tenant_id=$1 AND project_id=$2 AND id=$3 FOR UPDATE`, j.Scope.TenantID, j.Scope.ProjectID, j.BranchID).Scan(&execution, &epoch, &until, &sandboxID)
	if e != nil {
		return j, e
	}
	branch, e := load(ctx, tx, j.Scope, "Branch", j.BranchID)
	if e != nil {
		return j, e
	}
	var bs BranchSpec
	if e = json.Unmarshal(branch.Spec, &bs); e != nil {
		return j, e
	}
	run, e := load(ctx, tx, j.Scope, "Run", bs.RunID)
	if e != nil {
		return j, e
	}
	var rs RunSpec
	if e = json.Unmarshal(run.Spec, &rs); e != nil {
		return j, e
	}
	if epoch == 0 {
		var task TaskSpec
		if e = json.Unmarshal(rs.Task.Spec, &task); e != nil {
			return j, e
		}
		epoch = 1
		e = tx.QueryRowContext(ctx, `UPDATE branches SET epoch=$4,budget_remaining_ms=$5*1000,lease_until=clock_timestamp()+($5*interval '1 second') WHERE tenant_id=$1 AND project_id=$2 AND id=$3 RETURNING lease_until`, j.Scope.TenantID, j.Scope.ProjectID, j.BranchID, epoch, task.Limits.TimeoutSeconds).Scan(&until)
		if e != nil {
			return j, e
		}
	}
	if j.CheckpointID != "" {
		cp, err := loadCheckpoint(ctx, tx, j.Scope, j.BranchID, j.CheckpointID, rs)
		j.CheckpointError = err
		if err == nil {
			j.Checkpoint = &cp
		}
	}
	j.Assignment = Assignment{Scope: j.Scope, BranchID: j.BranchID, SandboxID: sandboxID, ExecutionID: execution, Epoch: epoch, LeaseUntil: until.Time, Run: rs}
	return j, tx.Commit()
}
func (s *Store) RenewClaim(ctx context.Context, j Job, ttl time.Duration) error {
	if ttl < time.Millisecond || ttl > time.Minute {
		return failure("invalid_argument", "claim TTL outside supported range")
	}
	r, e := s.db.ExecContext(ctx, `UPDATE jobs SET lease_until=clock_timestamp()+($4*interval '1 second') WHERE id=$1 AND owner=$2 AND claim_epoch=$3 AND NOT done AND lease_until>clock_timestamp()`, j.ID, j.Owner, j.ClaimEpoch, ttl.Seconds())
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return failure("aborted", "expired or superseded job claim")
	}
	return nil
}
func verifyClaim(ctx context.Context, tx *sql.Tx, j Job) error {
	var valid bool
	e := tx.QueryRowContext(ctx, `SELECT owner=$2 AND claim_epoch=$3 AND lease_until>clock_timestamp() AND NOT done FROM jobs WHERE id=$1 FOR UPDATE`, j.ID, j.Owner, j.ClaimEpoch).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return failure("aborted", "expired or superseded job claim")
	}
	return nil
}
func (s *Store) Finish(ctx context.Context, j Job, obs Observation) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = verifyClaim(ctx, tx, j); e != nil {
		return e
	}
	var desired, observed, execution, physicalID string
	var revision, epoch int64
	var valid bool
	e = tx.QueryRowContext(ctx, `SELECT desired,observed,revision,execution_id,epoch,sandbox_id,lease_until>clock_timestamp() FROM branches WHERE tenant_id=$1 AND project_id=$2 AND id=$3 FOR UPDATE`, j.Scope.TenantID, j.Scope.ProjectID, j.BranchID).Scan(&desired, &observed, &revision, &execution, &epoch, &physicalID, &valid)
	if e != nil {
		return e
	}
	if revision != j.DesiredRevision || epoch != j.Assignment.Epoch || execution != j.Assignment.ExecutionID || physicalID != j.Assignment.SandboxID {
		return failure("aborted", "superseded desired revision or execution epoch")
	}
	if obs.State == "running" && (!valid || desired != "running") {
		return failure("aborted", "execution lease expired or branch no longer desires running")
	}
	if obs.State != "running" && obs.State != "stopped" && obs.State != "failed" && obs.State != "suspended" {
		return failure("failed_precondition", "backend returned unsupported observation")
	}
	if (j.Action == "cancel" || j.Action == "expire") && obs.State != "stopped" {
		return failure("failed_precondition", "cancellation requires confirmed termination")
	}
	if j.Action == "suspend" && obs.State != "suspended" {
		return failure("failed_precondition", "suspend requires a verified checkpoint and fenced predecessor")
	}
	if obs.State == "suspended" {
		if j.Action != "suspend" || desired != "suspended" {
			return failure("failed_precondition", "unexpected suspended observation")
		}
		if obs.Checkpoint == nil || !obs.Checkpoint.Verified || !digestPattern.MatchString(obs.Checkpoint.ManifestDigest) || obs.Checkpoint.ManifestURI == "" || obs.Checkpoint.ProtocolVersion != "hypercall.snapshot.v1" || obs.Checkpoint.Compatibility["snapshot_format"] == "" || obs.Checkpoint.Compatibility["architecture"] == "" {
			return failure("failed_precondition", "suspension requires a verified immutable manifest")
		}
		if e = validateNativeCheckpoint(*obs.Checkpoint); e != nil {
			return e
		}
		var seq int64
		if e = tx.QueryRowContext(ctx, `SELECT sequence FROM branches WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, j.Scope.TenantID, j.Scope.ProjectID, j.BranchID).Scan(&seq); e != nil {
			return e
		}
		var task TaskSpec
		if e = json.Unmarshal(j.Assignment.Run.Task.Spec, &task); e != nil {
			return e
		}
		if obs.Checkpoint.Compatibility["template_digest"] != task.TemplateDigest {
			return failure("failed_precondition", "checkpoint template compatibility mismatch")
		}
		if obs.Checkpoint.BranchID != "" && obs.Checkpoint.BranchID != j.BranchID {
			return failure("failed_precondition", "checkpoint belongs to another branch")
		}
		if obs.Checkpoint.EventPosition != 0 && obs.Checkpoint.EventPosition != seq {
			return failure("failed_precondition", "checkpoint event position contradicts committed branch cursor")
		}
		obs.Checkpoint.BranchID = j.BranchID
		obs.Checkpoint.EventPosition = seq
		cp := makeResource(j.Scope, "Checkpoint", newID(), obs.Checkpoint)
		if e = insertResource(ctx, tx, cp); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE branches SET checkpoint_id=$4,budget_remaining_ms=GREATEST(0,FLOOR(EXTRACT(EPOCH FROM (lease_until-clock_timestamp()))*1000))::bigint WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, j.Scope.TenantID, j.Scope.ProjectID, j.BranchID, cp.Metadata.ID); e != nil {
			return e
		}
	}
	state := obs.State
	if j.Action == "cancel" {
		state = "cancelled"
	}
	if j.Action == "expire" {
		state = "interrupted"
	}
	_, e = tx.ExecContext(ctx, `UPDATE branches SET observed=$4,desired=CASE WHEN $4 IN ('stopped','failed') THEN $4 ELSE desired END WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, j.Scope.TenantID, j.Scope.ProjectID, j.BranchID, state)
	if e != nil {
		return e
	}
	op, e := load(ctx, tx, j.Scope, "Operation", j.OperationID)
	if e != nil {
		return e
	}
	if j.Action != "observe" {
		opState := "succeeded"
		if obs.State == "failed" {
			opState = "failed"
		}
		op.Status = encode(OperationStatus{State: opState})
		if e = save(ctx, tx, op); e != nil {
			return e
		}
	}
	_, e = tx.ExecContext(ctx, `UPDATE jobs SET done=true WHERE id=$1`, j.ID)
	if e != nil {
		return e
	}
	if _, e = appendEvent(ctx, tx, j.Scope, j.BranchID, epoch, state, map[string]string{"operation_id": j.OperationID}); e != nil {
		return e
	}
	if obs.State == "running" {
		_, e = tx.ExecContext(ctx, `INSERT INTO jobs(id,tenant_id,project_id,branch_id,operation_id,action,desired_revision,available_at) VALUES($1,$2,$3,$4,$5,'observe',$6,clock_timestamp()+interval '1 second')`, newID(), j.Scope.TenantID, j.Scope.ProjectID, j.BranchID, j.OperationID, j.DesiredRevision)
		if e != nil {
			return e
		}
	}
	return tx.Commit()
}

// Retry retains ambiguous work. It never claims an unavailable runtime is stopped.
func (s *Store) Retry(ctx context.Context, j Job, cause error) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = verifyClaim(ctx, tx, j); e != nil {
		return e
	}
	var revision int64
	var desired string
	e = tx.QueryRowContext(ctx, `SELECT revision,desired FROM branches WHERE tenant_id=$1 AND project_id=$2 AND id=$3 FOR UPDATE`, j.Scope.TenantID, j.Scope.ProjectID, j.BranchID).Scan(&revision, &desired)
	if e != nil {
		return e
	}
	obsolete := revision != j.DesiredRevision
	_, e = tx.ExecContext(ctx, `UPDATE jobs SET done=$2,owner=NULL,lease_until=NULL,available_at=clock_timestamp()+interval '1 second' WHERE id=$1`, j.ID, obsolete)
	if e != nil {
		return e
	}
	if j.Action != "observe" {
		op, e := load(ctx, tx, j.Scope, "Operation", j.OperationID)
		if e != nil {
			return e
		}
		state := "pending"
		if obsolete {
			state = "superseded"
		}
		op.Status = encode(OperationStatus{State: state, Error: cause.Error()})
		if e = save(ctx, tx, op); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Store) AppendEvent(ctx context.Context, scope Scope, branch, execution string, epoch int64, typ string, data json.RawMessage) (Event, error) {
	return s.AppendEventID(ctx, scope, branch, execution, epoch, newID(), typ, data)
}
func (s *Store) AppendEventID(ctx context.Context, scope Scope, branch, execution string, epoch int64, eventID, typ string, data json.RawMessage) (Event, error) {
	if !json.Valid(data) || typ == "" || len(typ) > 128 || eventID == "" || len(eventID) > 128 || len(data) > 1<<20 {
		return Event{}, failure("invalid_argument", "bounded JSON event, ID and type required")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return Event{}, e
	}
	defer tx.Rollback()
	var valid bool
	e = tx.QueryRowContext(ctx, `SELECT execution_id=$4 AND epoch=$5 AND lease_until>clock_timestamp() AND desired='running' AND observed IN ('pending','running','resuming') FROM branches WHERE tenant_id=$1 AND project_id=$2 AND id=$3 FOR UPDATE`, scope.TenantID, scope.ProjectID, branch, execution, epoch).Scan(&valid)
	if errors.Is(e, sql.ErrNoRows) {
		return Event{}, failure("not_found", "branch not found")
	}
	if e != nil {
		return Event{}, e
	}
	id := branch + "/" + eventID
	old := Event{ID: id, BranchID: branch}
	e = tx.QueryRowContext(ctx, `SELECT sequence,epoch,type,data,created_at FROM events WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, scope.TenantID, scope.ProjectID, id).Scan(&old.Sequence, &old.Epoch, &old.Type, &old.Data, &old.CreatedAt)
	if e == nil {
		if old.Epoch != epoch || old.Type != typ || hash(old.Data) != hash(data) {
			return old, failure("already_exists", "event ID reused with different content")
		}
		return old, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return old, e
	}
	if !valid {
		return Event{}, failure("aborted", "stale execution epoch, lease or intent")
	}
	ev, e := appendEvent(ctx, tx, scope, branch, epoch, typ, data, id)
	if e != nil {
		return ev, e
	}
	return ev, tx.Commit()
}

type Controller struct {
	Store    *Store
	Backend  Backend
	WorkerID string
	Logger   *slog.Logger
}

func (c *Controller) Tick(ctx context.Context) error {
	if e := c.Store.ExpireExecutions(ctx); e != nil {
		return e
	}
	j, e := c.Store.Claim(ctx, c.WorkerID, 30*time.Second)
	if e != nil {
		return e
	}
	if e = c.Store.CheckIntent(ctx, j); e != nil {
		_ = c.Store.Retry(ctx, j, e)
		return e
	}
	callCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	backend := c.Backend
	if backend == nil {
		backend = UnavailableBackend{}
	}
	// A desired revision can change after claim. Fenced backend calls plus Finish's
	// revision check keep late observations from overwriting cancellation intent.
	var obs Observation
	switch j.Action {
	case "start":
		obs, e = backend.Ensure(callCtx, j.Assignment)
	case "observe":
		obs, e = backend.Observe(callCtx, j.Assignment)
	case "cancel", "expire":
		obs, e = backend.Stop(callCtx, j.Assignment)
	case "suspend":
		obs, e = backend.Suspend(callCtx, j.Assignment)
	case "resume":
		if j.CheckpointError != nil {
			e = j.CheckpointError
		} else if j.Checkpoint == nil {
			e = failure("failed_precondition", "resume job has no retained checkpoint")
		} else {
			obs, e = backend.Resume(callCtx, j.Assignment, *j.Checkpoint)
		}
	}
	if e == nil {
		e = c.Store.Finish(ctx, j, obs)
	}
	if e != nil {
		retry := c.Store.Retry(ctx, j, e)
		if retry != nil && Code(retry) != "aborted" {
			return retry
		}
		return &ReconcileError{Job: j, Cause: e}
	}
	return nil
}
func (c *Controller) Run(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	var lastWarning time.Time
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if e := c.Tick(ctx); e != nil && Code(e) != "not_found" && c.Logger != nil {
				if time.Since(lastWarning) >= 5*time.Second {
					lastWarning = time.Now()
					args := []any{"code", Code(e)}
					var reconcile *ReconcileError
					if errors.As(e, &reconcile) {
						args = append(args, "branch_id", reconcile.Job.BranchID, "operation_id", reconcile.Job.OperationID, "job_id", reconcile.Job.ID)
					}
					c.Logger.Warn("reconcile pending", args...)
				}
			}
		}
	}
}
func (s *Service) assignment(ctx context.Context, scope Scope, branch string) (Assignment, error) {
	if e := validateScope(scope); e != nil {
		return Assignment{}, e
	}
	var a Assignment
	var until sql.NullTime
	var valid bool
	e := s.store.db.QueryRowContext(ctx, `SELECT execution_id,epoch,lease_until,sandbox_id,desired='running' AND observed='running' AND lease_until>clock_timestamp() FROM branches WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, scope.TenantID, scope.ProjectID, branch).Scan(&a.ExecutionID, &a.Epoch, &until, &a.SandboxID, &valid)
	if errors.Is(e, sql.ErrNoRows) {
		return a, failure("not_found", "branch not found")
	}
	if e != nil {
		return a, e
	}
	if !valid {
		return a, failure("failed_precondition", "branch has no live running assignment")
	}
	a.Scope = scope
	a.LeaseUntil = until.Time
	a.BranchID = branch
	return a, nil
}
func (s *Service) Exec(ctx context.Context, scope Scope, branch string, r *guestv1.ExecRequest, send func(*guestv1.ExecEvent) error) error {
	a, e := s.assignment(ctx, scope, branch)
	if e != nil {
		return e
	}
	if s.backend == nil {
		return failure("unavailable", "backend unavailable")
	}
	return s.backend.Exec(ctx, a, r, send)
}
func (s *Service) ReadFile(ctx context.Context, scope Scope, branch string, r *guestv1.ReadFileRequest) (*guestv1.ReadFileResponse, error) {
	a, e := s.assignment(ctx, scope, branch)
	if e != nil {
		return nil, e
	}
	if s.backend == nil {
		return nil, failure("unavailable", "backend unavailable")
	}
	return s.backend.ReadFile(ctx, a, r)
}
func (s *Service) WriteFile(ctx context.Context, scope Scope, branch string, r *guestv1.WriteFileRequest) (*guestv1.WriteFileResponse, error) {
	a, e := s.assignment(ctx, scope, branch)
	if e != nil {
		return nil, e
	}
	if s.backend == nil {
		return nil, failure("unavailable", "backend unavailable")
	}
	return s.backend.WriteFile(ctx, a, r)
}

// CheckIntent avoids starting already superseded jobs. The backend still must
// tombstone stopped epochs because intent can change while a request is in flight.
func (s *Store) CheckIntent(ctx context.Context, j Job) error {
	var revision int64
	e := s.db.QueryRowContext(ctx, `SELECT revision FROM branches WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, j.Scope.TenantID, j.Scope.ProjectID, j.BranchID).Scan(&revision)
	if e != nil {
		return e
	}
	if revision != j.DesiredRevision {
		return failure("aborted", "superseded desired revision")
	}
	return nil
}

// ExpireExecutions never resurrects a timed-out assignment. Cleanup is durable;
// uncertainty remains visible until the backend confirms termination.
func (s *Store) ExpireExecutions(ctx context.Context) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	rows, e := tx.QueryContext(ctx, `SELECT tenant_id,project_id,id,revision,epoch FROM branches WHERE (desired='running' OR (desired='suspended' AND observed!='suspended')) AND observed NOT IN ('stopped','failed','cancelled','interrupted') AND lease_until<=clock_timestamp() FOR UPDATE SKIP LOCKED`)
	if e != nil {
		return e
	}
	type expired struct {
		s          Scope
		id         string
		rev, epoch int64
	}
	all := []expired{}
	for rows.Next() {
		var x expired
		if e = rows.Scan(&x.s.TenantID, &x.s.ProjectID, &x.id, &x.rev, &x.epoch); e != nil {
			rows.Close()
			return e
		}
		all = append(all, x)
	}
	if e = rows.Close(); e != nil {
		return e
	}
	for _, x := range all {
		x.rev++
		_, e = tx.ExecContext(ctx, `UPDATE branches SET desired='stopped',observed='interrupted',revision=$4 WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, x.s.TenantID, x.s.ProjectID, x.id, x.rev)
		if e != nil {
			return e
		}
		op := operation(x.s, x.id, "expire")
		if e = insertResource(ctx, tx, op); e != nil {
			return e
		}
		if e = enqueue(ctx, tx, x.s, x.id, op, "expire", x.rev); e != nil {
			return e
		}
		if _, e = appendEvent(ctx, tx, x.s, x.id, x.epoch, "execution_expired", map[string]string{"cleanup": "pending"}); e != nil {
			return e
		}
	}
	return tx.Commit()
}

// ReconcileError carries diagnostic identities without exposing input/credentials.
type ReconcileError struct {
	Job   Job
	Cause error
}

func (e *ReconcileError) Error() string {
	return fmt.Sprintf("reconcile job %s: %v", e.Job.ID, e.Cause)
}
func (e *ReconcileError) Unwrap() error { return e.Cause }
