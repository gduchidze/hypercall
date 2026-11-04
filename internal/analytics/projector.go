package analytics

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	cp "github.com/gduchidze/hypercall/internal/controlplane"
)

const Consumer = "clickhouse-v1"
const projectionLease = 30 * time.Second

// Sources are retained authority, independent of other outbox consumers. There
// is deliberately no monotonic cursor: earlier rows may commit after later ones.
const sources = `WITH source AS (
 SELECT 'branch:'||id AS event_id,tenant_id,project_id,type AS kind,branch_id AS resource_id,created_at AS occurred_at,data::text AS payload_json FROM events
 UNION ALL
 SELECT 'evaluation:'||id,tenant_id,project_id,'evaluation.completed',evaluation_id,created_at,report::text FROM eval_outbox
)`

type Projector struct {
	db   *sql.DB
	sink Sink
}

func NewProjector(db *sql.DB, sink Sink) *Projector { return &Projector{db: db, sink: sink} }

// ProjectOnce claims one source event, writes it, then records a fenced receipt.
// The sink must tolerate duplicates, including insertion followed by a lost ACK.
// A sink outage never changes the authoritative events, reports or lifecycle.
func (p *Projector) ProjectOnce(ctx context.Context) (bool, error) {
	if p.db == nil || p.sink == nil {
		return false, errors.New("projection requires a database and sink")
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return false, err
	}
	token := hex.EncodeToString(random[:])
	var e Event
	err := p.db.QueryRowContext(ctx, sources+`, candidate AS (
 SELECT s.* FROM source s LEFT JOIN projection_deliveries d ON d.consumer=$1 AND d.tenant_id=s.tenant_id AND d.project_id=s.project_id AND d.event_id=s.event_id
 WHERE d.delivered_at IS NULL AND (d.lease_until IS NULL OR d.lease_until<clock_timestamp())
 ORDER BY s.occurred_at,s.tenant_id,s.project_id,s.event_id LIMIT 1
), claimed AS (
 INSERT INTO projection_deliveries(consumer,tenant_id,project_id,event_id,claim_token,lease_until)
 SELECT $1,tenant_id,project_id,event_id,$2,clock_timestamp()+($3*interval '1 second') FROM candidate
 ON CONFLICT(consumer,tenant_id,project_id,event_id) DO UPDATE SET claim_token=EXCLUDED.claim_token,lease_until=EXCLUDED.lease_until
 WHERE projection_deliveries.delivered_at IS NULL AND (projection_deliveries.lease_until IS NULL OR projection_deliveries.lease_until<clock_timestamp())
 RETURNING tenant_id,project_id,event_id
)
 SELECT s.event_id,s.tenant_id,s.project_id,s.kind,s.resource_id,s.occurred_at,s.payload_json FROM candidate s JOIN claimed d USING(tenant_id,project_id,event_id)`, Consumer, token, projectionLease.Seconds()).Scan(&e.EventID, &e.TenantID, &e.ProjectID, &e.Kind, &e.ResourceID, &e.OccurredAt, &e.PayloadJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim projection: %w", err)
	}
	e.SchemaVersion = 1
	if err = p.sink.Write(ctx, e); err != nil {
		// Release promptly on a known failure. A canceled context may leave this
		// leased; expiry still guarantees eventual retry without requiring cleanup.
		_, _ = p.db.ExecContext(ctx, `UPDATE projection_deliveries SET claim_token=NULL,lease_until=NULL WHERE consumer=$1 AND tenant_id=$2 AND project_id=$3 AND event_id=$4 AND claim_token=$5 AND delivered_at IS NULL`, Consumer, e.TenantID, e.ProjectID, e.EventID, token)
		return true, fmt.Errorf("project event: %w", err)
	}
	result, err := p.db.ExecContext(ctx, `UPDATE projection_deliveries SET delivered_at=clock_timestamp(),claim_token=NULL,lease_until=NULL WHERE consumer=$1 AND tenant_id=$2 AND project_id=$3 AND event_id=$4 AND claim_token=$5 AND lease_until>clock_timestamp() AND delivered_at IS NULL`, Consumer, e.TenantID, e.ProjectID, e.EventID, token)
	if err != nil {
		return true, fmt.Errorf("acknowledge projection: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return true, err
	}
	if count != 1 {
		return true, errors.New("projection claim expired or was superseded")
	}
	return true, nil
}

type Status struct {
	Pending              int64      `json:"pending"`
	OldestPendingSeconds float64    `json:"oldest_pending_seconds"`
	LastDeliveredAt      *time.Time `json:"last_delivered_at,omitempty"`
}

// Status remains available during an analytics outage and includes leased work.
// Scope must come from authorization. A zero backlog has zero pending age.
func (p *Projector) Status(ctx context.Context, scope cp.Scope) (Status, error) {
	var s Status
	if p.db == nil || scope.TenantID == "" || scope.ProjectID == "" {
		return s, errors.New("database and tenant/project scope are required")
	}
	err := p.db.QueryRowContext(ctx, sources+`
 SELECT count(*) FILTER(WHERE d.delivered_at IS NULL),
 COALESCE(GREATEST(0,extract(epoch FROM clock_timestamp()-min(s.occurred_at) FILTER(WHERE d.delivered_at IS NULL))),0),max(d.delivered_at)
 FROM source s LEFT JOIN projection_deliveries d ON d.consumer=$1 AND d.tenant_id=s.tenant_id AND d.project_id=s.project_id AND d.event_id=s.event_id
 WHERE s.tenant_id=$2 AND s.project_id=$3`, Consumer, scope.TenantID, scope.ProjectID).Scan(&s.Pending, &s.OldestPendingSeconds, &s.LastDeliveredAt)
	return s, err
}

// Rebuild invalidates only this consumer's receipts. Subsequent ProjectOnce
// calls replay all retained sources; FINAL keeps retries logically idempotent.
// It never truncates ClickHouse or changes another consumer's delivery state.
func (p *Projector) Rebuild(ctx context.Context) error {
	if p.db == nil {
		return errors.New("projection requires a database")
	}
	_, err := p.db.ExecContext(ctx, `DELETE FROM projection_deliveries WHERE consumer=$1`, Consumer)
	return err
}
