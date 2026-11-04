-- Derived data only: authoritative event/report retention remains in Postgres.
-- Run against the selected database with a migration/admin credential.
CREATE TABLE IF NOT EXISTS hypercall_events
(
    event_id String,
    tenant_id String,
    project_id String,
    kind LowCardinality(String),
    resource_id String,
    occurred_at DateTime64(6, 'UTC'),
    payload_json String,
    schema_version UInt32
)
ENGINE = ReplacingMergeTree
ORDER BY (tenant_id, project_id, event_id);
-- Query through FINAL: physical duplicates are expected after ambiguous delivery.
-- A time partition would break deduplication across replayed copies; omit it.
