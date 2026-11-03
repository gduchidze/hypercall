package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Delivery struct {
	Scope Scope
	Event Event
	Token string
}

// ClaimOutbox has no global cursor: rows can commit out of order, and unpublished
// rows must remain discoverable. Consumers deduplicate by the immutable event ID.
func (s *Store) ClaimOutbox(ctx context.Context, ttl time.Duration) (Delivery, error) {
	var d Delivery
	if ttl < time.Millisecond || ttl > time.Minute {
		return d, failure("invalid_argument", "outbox lease must be 1ms..1m")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return d, e
	}
	defer tx.Rollback()
	d.Token = newID()
	e = tx.QueryRowContext(ctx, `UPDATE outbox SET claim_token=$1,lease_until=clock_timestamp()+($2*interval '1 second') WHERE id=(SELECT id FROM outbox WHERE NOT published AND (lease_until IS NULL OR lease_until<clock_timestamp()) ORDER BY branch_id,sequence FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING tenant_id,project_id,branch_id,sequence,id`, d.Token, ttl.Seconds()).Scan(&d.Scope.TenantID, &d.Scope.ProjectID, &d.Event.BranchID, &d.Event.Sequence, &d.Event.ID)
	if errors.Is(e, sql.ErrNoRows) {
		return d, failure("not_found", "no unpublished events")
	}
	if e != nil {
		return d, e
	}
	e = tx.QueryRowContext(ctx, `SELECT epoch,type,data,created_at FROM events WHERE tenant_id=$1 AND project_id=$2 AND branch_id=$3 AND sequence=$4`, d.Scope.TenantID, d.Scope.ProjectID, d.Event.BranchID, d.Event.Sequence).Scan(&d.Event.Epoch, &d.Event.Type, &d.Event.Data, &d.Event.CreatedAt)
	if e != nil {
		return d, e
	}
	return d, tx.Commit()
}
func (s *Store) AckOutbox(ctx context.Context, d Delivery) error {
	r, e := s.db.ExecContext(ctx, `UPDATE outbox SET published=true WHERE id=$1 AND tenant_id=$2 AND project_id=$3 AND claim_token=$4 AND lease_until>clock_timestamp() AND NOT published`, d.Event.ID, d.Scope.TenantID, d.Scope.ProjectID, d.Token)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return failure("aborted", "expired or superseded outbox delivery")
	}
	return nil
}
