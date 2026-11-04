package controlplane

import (
	"context"
	"database/sql"
	"time"
)

type ProjectionStatus struct {
	Pending              int64
	OldestPendingSeconds float64
	LastDeliveredAt      *time.Time
}

func (s *Service) ProjectionStatus(ctx context.Context, scope Scope) (ProjectionStatus, error) {
	var out ProjectionStatus
	if err := validateScope(scope); err != nil {
		return out, err
	}
	var delivered sql.NullTime
	err := s.store.db.QueryRowContext(ctx, `WITH source AS (SELECT 'branch:'||id AS id,created_at FROM events WHERE tenant_id=$1 AND project_id=$2 UNION ALL SELECT 'evaluation:'||id,created_at FROM eval_outbox WHERE tenant_id=$1 AND project_id=$2) SELECT count(*) FILTER(WHERE p.delivered_at IS NULL),COALESCE(EXTRACT(EPOCH FROM clock_timestamp()-min(s.created_at) FILTER(WHERE p.delivered_at IS NULL)),0),max(p.delivered_at) FROM source s LEFT JOIN projection_deliveries p ON p.consumer='clickhouse-v1' AND p.tenant_id=$1 AND p.project_id=$2 AND p.event_id=s.id`, scope.TenantID, scope.ProjectID).Scan(&out.Pending, &out.OldestPendingSeconds, &delivered)
	if delivered.Valid {
		out.LastDeliveredAt = &delivered.Time
	}
	return out, err
}
