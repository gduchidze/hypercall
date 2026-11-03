package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct{ db *sql.DB }

func Open(ctx context.Context, dsn string) (*Store, error) {
	db, e := sql.Open("pgx", dsn)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(16)
	if e = db.PingContext(ctx); e != nil {
		db.Close()
		return nil, e
	}
	return &Store{db}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Migrate(ctx context.Context) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(48291401)"); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL)`); e != nil {
		return e
	}

	const latest = 3
	var future bool
	if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version>$1 OR version<1)", latest).Scan(&future); e != nil {
		return e
	}
	if future {
		return failure("failed_precondition", "database schema is newer or unsupported; use a compatible binary")
	}
	for version, name := range []string{"001_initial.sql", "002_resume.sql", "003_physical_sandbox.sql"} {
		data, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		checksum := hex.EncodeToString(sum[:])
		var existing string
		e = tx.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE version=$1", version+1).Scan(&existing)
		if e == nil && existing != checksum {
			return failure("failed_precondition", fmt.Sprintf("migration %d checksum mismatch", version+1))
		}
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if errors.Is(e, sql.ErrNoRows) {
			var later bool
			if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version>$1)", version+1).Scan(&later); e != nil {
				return e
			}
			if later {
				return failure("failed_precondition", "database migration history has a gap")
			}
			if _, e = tx.ExecContext(ctx, string(data)); e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO schema_migrations VALUES($1,$2)", version+1, checksum); e != nil {
				return e
			}
		}
	}

	return tx.Commit()
}
func hash(v any) string {
	var canonical any
	d := json.NewDecoder(bytes.NewReader(encode(v)))
	d.UseNumber()
	if err := d.Decode(&canonical); err != nil {
		panic(err)
	}
	h := sha256.Sum256(encode(canonical))
	return hex.EncodeToString(h[:])
}
func metadata(scope Scope, name string) Metadata {
	return Metadata{ID: newID(), TenantID: scope.TenantID, ProjectID: scope.ProjectID, Name: name, SchemaVersion: "hypercall/v1", Revision: 1, CreatedAt: time.Now().UTC()}
}
func makeResource(scope Scope, kind, name string, spec any) Resource {
	r := Resource{Metadata: metadata(scope, name), Kind: kind, Spec: encode(spec)}
	r.Metadata.Digest = "sha256:" + hash(spec)
	return r
}
func insertResource(ctx context.Context, tx *sql.Tx, r Resource) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO resources VALUES($1,$2,$3,$4,$5,$6)`, r.Metadata.TenantID, r.Metadata.ProjectID, r.Kind, r.Metadata.ID, r.Metadata.Name, encode(r))
	return e
}
func scanResource(row interface{ Scan(...any) error }) (Resource, error) {
	var r Resource
	var b []byte
	e := row.Scan(&b)
	if errors.Is(e, sql.ErrNoRows) {
		return r, failure("not_found", "resource not found")
	}
	if e != nil {
		return r, e
	}
	e = json.Unmarshal(b, &r)
	return r, e
}
func load(ctx context.Context, tx *sql.Tx, scope Scope, kind, id string) (Resource, error) {
	return scanResource(tx.QueryRowContext(ctx, `SELECT body FROM resources WHERE tenant_id=$1 AND project_id=$2 AND kind=$3 AND id=$4`, scope.TenantID, scope.ProjectID, kind, id))
}
func save(ctx context.Context, tx *sql.Tx, r Resource) error {
	r.Metadata.Revision++
	_, e := tx.ExecContext(ctx, `UPDATE resources SET body=$5 WHERE tenant_id=$1 AND project_id=$2 AND kind=$3 AND id=$4`, r.Metadata.TenantID, r.Metadata.ProjectID, r.Kind, r.Metadata.ID, encode(r))
	return e
}
func idem(ctx context.Context, tx *sql.Tx, scope Scope, key string, request any, out any) (bool, error) {
	if key == "" {
		return false, failure("invalid_argument", "idempotency key required")
	}
	if _, e := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", scope.TenantID+"/"+scope.ProjectID+"/"+key); e != nil {
		return false, e
	}
	var old string
	var response []byte
	e := tx.QueryRowContext(ctx, `SELECT request_hash,response FROM idempotency WHERE tenant_id=$1 AND project_id=$2 AND key=$3`, scope.TenantID, scope.ProjectID, key).Scan(&old, &response)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if old != hash(request) {
		return false, failure("already_exists", "idempotency key reused with different request")
	}
	return true, json.Unmarshal(response, out)
}
func recordIdem(ctx context.Context, tx *sql.Tx, scope Scope, key string, req, out any) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO idempotency VALUES($1,$2,$3,$4,$5)`, scope.TenantID, scope.ProjectID, key, hash(req), encode(out))
	return e
}
func (s *Store) PutResource(ctx context.Context, scope Scope, r Resource, key string) (Resource, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return Resource{}, e
	}
	defer tx.Rollback()
	req := struct {
		Kind, Name string
		Spec       json.RawMessage
	}{r.Kind, r.Metadata.Name, r.Spec}
	var out Resource
	found, e := idem(ctx, tx, scope, key, req, &out)
	if e != nil || found {
		return out, e
	}
	if _, e = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", scope.TenantID+"/"+scope.ProjectID+"/"+r.Kind+"/"+r.Metadata.Name); e != nil {
		return out, e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO projects VALUES($1,$2) ON CONFLICT DO NOTHING`, scope.TenantID, scope.ProjectID)
	if e != nil {
		return out, e
	}
	old, e := scanResource(tx.QueryRowContext(ctx, `SELECT body FROM resources WHERE tenant_id=$1 AND project_id=$2 AND kind=$3 AND name=$4`, scope.TenantID, scope.ProjectID, r.Kind, r.Metadata.Name))
	if e == nil {
		if hash(old.Spec) != hash(r.Spec) {
			return out, failure("already_exists", "named definition is immutable; create a new name")
		}
		out = old
	} else if Code(e) == "not_found" {
		out = makeResource(scope, r.Kind, r.Metadata.Name, r.Spec)
		if e = insertResource(ctx, tx, out); e != nil {
			return out, e
		}
	} else {
		return out, e
	}
	if e = recordIdem(ctx, tx, scope, key, req, out); e != nil {
		return out, e
	}
	return out, tx.Commit()
}
func (s *Store) GetResource(ctx context.Context, scope Scope, kind, id string) (Resource, error) {
	r, e := scanResource(s.db.QueryRowContext(ctx, `SELECT body FROM resources WHERE tenant_id=$1 AND project_id=$2 AND kind=$3 AND id=$4`, scope.TenantID, scope.ProjectID, kind, id))
	if e != nil {
		return r, e
	}
	if kind == "Branch" {
		var st BranchStatus
		var until sql.NullTime
		e = s.db.QueryRowContext(ctx, `SELECT desired,observed,execution_id,epoch,lease_until,sequence,revision,COALESCE(checkpoint_id,''),budget_remaining_ms,sandbox_id FROM branches WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, scope.TenantID, scope.ProjectID, id).Scan(&st.Desired, &st.Observed, &st.ExecutionID, &st.Epoch, &until, &st.LastEvent, &r.Metadata.Revision, &st.CheckpointID, &st.BudgetRemainingMS, &st.SandboxID)
		st.LeaseUntil = until.Time
		r.Status = encode(st)
	}
	return r, e
}
func (s *Store) ListResources(ctx context.Context, scope Scope, kind, after string, limit int) ([]Resource, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, e := s.db.QueryContext(ctx, `SELECT body FROM resources WHERE tenant_id=$1 AND project_id=$2 AND kind=$3 AND id>$4 ORDER BY id LIMIT $5`, scope.TenantID, scope.ProjectID, kind, after, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Resource{}
	for rows.Next() {
		r, e := scanResource(rows)
		if e != nil {
			return nil, e
		}
		if kind == "Branch" {
			r, e = s.GetResource(ctx, scope, kind, r.Metadata.ID)
			if e != nil {
				return nil, e
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func operation(scope Scope, branch, action string) Resource {
	r := makeResource(scope, "Operation", newID(), OperationSpec{BranchID: branch, Action: action})
	r.Status = encode(OperationStatus{State: "pending"})
	return r
}
func enqueue(ctx context.Context, tx *sql.Tx, scope Scope, branch string, op Resource, action string, rev int64) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO jobs(id,tenant_id,project_id,branch_id,operation_id,action,desired_revision,checkpoint_id) VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''))`, newID(), scope.TenantID, scope.ProjectID, branch, op.Metadata.ID, action, rev, selectedCheckpoint(op))
	return e
}
func appendEvent(ctx context.Context, tx *sql.Tx, scope Scope, branch string, epoch int64, typ string, data any, eventIDs ...string) (Event, error) {
	id := newID()
	if len(eventIDs) > 0 {
		id = eventIDs[0]
	}
	ev := Event{ID: id, BranchID: branch, Epoch: epoch, Type: typ, Data: encode(data)}
	e := tx.QueryRowContext(ctx, `UPDATE branches SET sequence=sequence+1 WHERE tenant_id=$1 AND project_id=$2 AND id=$3 RETURNING sequence`, scope.TenantID, scope.ProjectID, branch).Scan(&ev.Sequence)
	if e != nil {
		return ev, e
	}
	e = tx.QueryRowContext(ctx, `INSERT INTO events(tenant_id,project_id,branch_id,sequence,id,epoch,type,data) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING created_at`, scope.TenantID, scope.ProjectID, branch, ev.Sequence, ev.ID, epoch, typ, ev.Data).Scan(&ev.CreatedAt)
	if e != nil {
		return ev, e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO outbox(id,tenant_id,project_id,branch_id,sequence) VALUES($1,$2,$3,$4,$5)`, ev.ID, scope.TenantID, scope.ProjectID, branch, ev.Sequence)
	return ev, e
}
func (s *Store) Submit(ctx context.Context, scope Scope, taskID, key string) (Submission, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return Submission{}, e
	}
	defer tx.Rollback()
	req := []string{"submit", taskID}
	var out Submission
	found, e := idem(ctx, tx, scope, key, req, &out)
	if e != nil || found {
		return out, e
	}
	task, e := load(ctx, tx, scope, "Task", taskID)
	if e != nil {
		return out, e
	}
	var spec TaskSpec
	if e = json.Unmarshal(task.Spec, &spec); e != nil {
		return out, e
	}
	resolve := func(kind string, ref Ref) (Resource, error) {
		r, e := load(ctx, tx, scope, kind, ref.ID)
		if e == nil && r.Metadata.Digest != ref.Digest {
			e = failure("failed_precondition", "reference digest mismatch")
		}
		return r, e
	}
	ws, e := resolve("Workspace", spec.Workspace)
	if e != nil {
		return out, e
	}
	gw, e := resolve("Gateway", spec.Gateway)
	if e != nil {
		return out, e
	}
	models := []Resource{}
	for _, ref := range spec.Models {
		m, e := resolve("ModelConfig", ref)
		if e != nil {
			return out, e
		}
		models = append(models, m)
	}
	out.Run = makeResource(scope, "Run", newID(), RunSpec{task, ws, models, gw})
	out.Branch = makeResource(scope, "Branch", newID(), BranchSpec{RunID: out.Run.Metadata.ID, Model: models[0]})
	out.Operation = operation(scope, out.Branch.Metadata.ID, "start")
	for _, r := range []Resource{out.Run, out.Branch, out.Operation} {
		if e = insertResource(ctx, tx, r); e != nil {
			return out, e
		}
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO branches(tenant_id,project_id,id,desired,observed,execution_id,sandbox_id) VALUES($1,$2,$3,'running','pending',$4,$5)`, scope.TenantID, scope.ProjectID, out.Branch.Metadata.ID, newID(), newID())
	if e != nil {
		return out, e
	}
	if e = enqueue(ctx, tx, scope, out.Branch.Metadata.ID, out.Operation, "start", 1); e != nil {
		return out, e
	}
	if _, e = appendEvent(ctx, tx, scope, out.Branch.Metadata.ID, 0, "submitted", map[string]string{"run_id": out.Run.Metadata.ID}); e != nil {
		return out, e
	}
	if e = recordIdem(ctx, tx, scope, key, req, out); e != nil {
		return out, e
	}
	return out, tx.Commit()
}
func (s *Store) Action(ctx context.Context, scope Scope, branch, action, key string) (Resource, error) {
	return s.action(ctx, scope, branch, action, key, nil)
}
func (s *Store) action(ctx context.Context, scope Scope, branch, action, key string, admit func(RunSpec) error) (Resource, error) {
	if action != "cancel" && action != "suspend" && action != "resume" {
		return Resource{}, failure("invalid_argument", "action must be cancel, suspend or resume")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return Resource{}, e
	}
	defer tx.Rollback()
	req := []string{"action", branch, action}
	var out Resource
	found, e := idem(ctx, tx, scope, key, req, &out)
	if e != nil || found {
		return out, e
	}

	var desired, observed, checkpointID string
	var revision, budget, epoch int64
	e = tx.QueryRowContext(ctx, `SELECT desired,observed,revision,COALESCE(checkpoint_id,''),budget_remaining_ms,epoch FROM branches WHERE tenant_id=$1 AND project_id=$2 AND id=$3 FOR UPDATE`, scope.TenantID, scope.ProjectID, branch).Scan(&desired, &observed, &revision, &checkpointID, &budget, &epoch)
	if errors.Is(e, sql.ErrNoRows) {
		return out, failure("not_found", "branch not found")
	}
	if e != nil {
		return out, e
	}
	if admit != nil {
		run, e := loadRun(ctx, tx, scope, branch)
		if e != nil {
			return out, e
		}
		if e = admit(run); e != nil {
			return out, e
		}
	}
	if desired == "cancelled" || desired == "stopped" || observed == "cancelled" || observed == "failed" || observed == "interrupted" || observed == "stopped" {
		return out, failure("failed_precondition", "branch is terminal")
	}
	target := map[string]string{"cancel": "cancelled", "suspend": "suspended", "resume": "running"}[action]
	nextObserved := observed
	revision++
	if action == "suspend" {
		if desired != "running" || observed != "running" {
			return out, failure("failed_precondition", "suspend requires a running branch")
		}
		nextObserved = "suspending"
	}
	if action == "resume" {
		if desired != "suspended" || observed != "suspended" || checkpointID == "" || budget <= 0 || epoch == math.MaxInt64 {
			return out, failure("failed_precondition", "resume requires a completed suspension, retained checkpoint and remaining active budget")
		}
		run, e := loadRun(ctx, tx, scope, branch)
		if e != nil {
			return out, e
		}
		var task TaskSpec
		if e = json.Unmarshal(run.Task.Spec, &task); e != nil {
			return out, e
		}
		if budget > int64(task.Limits.TimeoutSeconds)*1000 {
			return out, failure("failed_precondition", "remaining budget exceeds admitted limit")
		}
		if _, e = loadCheckpoint(ctx, tx, scope, branch, checkpointID, run); e != nil {
			return out, e
		}
		epoch++
		nextObserved = "resuming"
		_, e = tx.ExecContext(ctx, `UPDATE branches SET execution_id=$4,epoch=$5,lease_until=clock_timestamp()+($6*interval '1 millisecond'),sandbox_id=$7 WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, scope.TenantID, scope.ProjectID, branch, newID(), epoch, budget, newID())
		if e != nil {
			return out, e
		}
	}
	_, e = tx.ExecContext(ctx, `UPDATE branches SET desired=$4,observed=$5,revision=$6 WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, scope.TenantID, scope.ProjectID, branch, target, nextObserved, revision)
	if e != nil {
		return out, e
	}
	out = operation(scope, branch, action)
	if action == "resume" {
		spec := OperationSpec{BranchID: branch, Action: action, CheckpointID: checkpointID}
		out.Spec = encode(spec)
		out.Metadata.Digest = "sha256:" + hash(spec)
	}

	if e = insertResource(ctx, tx, out); e != nil {
		return out, e
	}
	if e = enqueue(ctx, tx, scope, branch, out, action, revision); e != nil {
		return out, e
	}
	if _, e = appendEvent(ctx, tx, scope, branch, 0, action+"_requested", map[string]string{}); e != nil {
		return out, e
	}
	if e = recordIdem(ctx, tx, scope, key, req, out); e != nil {
		return out, e
	}
	return out, tx.Commit()
}
func (s *Store) Events(ctx context.Context, scope Scope, branch string, after int64, limit int) ([]Event, error) {
	if _, e := s.GetResource(ctx, scope, "Branch", branch); e != nil {
		return nil, e
	}
	if after < 0 {
		return nil, failure("invalid_argument", "negative event cursor")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, e := s.db.QueryContext(ctx, `SELECT id,sequence,epoch,type,data,created_at FROM events WHERE tenant_id=$1 AND project_id=$2 AND branch_id=$3 AND sequence>$4 ORDER BY sequence LIMIT $5`, scope.TenantID, scope.ProjectID, branch, after, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		ev := Event{BranchID: branch}
		if e = rows.Scan(&ev.ID, &ev.Sequence, &ev.Epoch, &ev.Type, &ev.Data, &ev.CreatedAt); e != nil {
			return nil, e
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func selectedCheckpoint(op Resource) string {
	var spec OperationSpec
	_ = json.Unmarshal(op.Spec, &spec)
	return spec.CheckpointID
}
func loadRun(ctx context.Context, tx *sql.Tx, scope Scope, branch string) (RunSpec, error) {
	var out RunSpec
	r, e := load(ctx, tx, scope, "Branch", branch)
	if e != nil {
		return out, e
	}
	var bs BranchSpec
	if e = json.Unmarshal(r.Spec, &bs); e != nil {
		return out, e
	}
	r, e = load(ctx, tx, scope, "Run", bs.RunID)
	if e != nil {
		return out, e
	}
	e = json.Unmarshal(r.Spec, &out)
	return out, e
}
func loadCheckpoint(ctx context.Context, tx *sql.Tx, scope Scope, branch, id string, run RunSpec) (CheckpointSpec, error) {
	var cp CheckpointSpec
	r, e := load(ctx, tx, scope, "Checkpoint", id)
	if e != nil {
		if Code(e) == "not_found" {
			e = failure("failed_precondition", "retained checkpoint is missing or outside the resource scope")
		}
		return cp, e
	}
	if r.Metadata.ID != id || r.Metadata.TenantID != scope.TenantID || r.Metadata.ProjectID != scope.ProjectID || r.Kind != "Checkpoint" || r.Metadata.Digest != "sha256:"+hash(r.Spec) {
		return cp, failure("failed_precondition", "retained checkpoint metadata or content digest is invalid")
	}
	if e = json.Unmarshal(r.Spec, &cp); e != nil {
		return cp, failure("failed_precondition", "retained checkpoint is malformed")
	}
	var task TaskSpec
	if e = json.Unmarshal(run.Task.Spec, &task); e != nil {
		return cp, e
	}
	if cp.BranchID != branch || !cp.Verified || !digestPattern.MatchString(cp.ManifestDigest) || cp.ManifestURI == "" || cp.ProtocolVersion != "hypercall.snapshot.v1" || cp.Compatibility["template_digest"] != task.TemplateDigest || cp.Compatibility["snapshot_format"] == "" || cp.Compatibility["architecture"] == "" || cp.EventPosition < 1 {
		return cp, failure("failed_precondition", "retained checkpoint is unverified or incompatible with this branch")
	}
	if e := validateNativeCheckpoint(cp); e != nil {
		return cp, e
	}
	return cp, nil
}
