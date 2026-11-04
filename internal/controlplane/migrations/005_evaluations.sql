CREATE TABLE evaluations (
 tenant_id text NOT NULL,
 project_id text NOT NULL,
 id text NOT NULL,
 operation_id text NOT NULL,
 input_digest text NOT NULL,
 plan jsonb NOT NULL,
 observations jsonb NOT NULL,
 state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','running','completed')),
 report jsonb,
 owner text,
 claim_epoch bigint NOT NULL DEFAULT 0,
 attempts integer NOT NULL DEFAULT 0,
 lease_until timestamptz,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 completed_at timestamptz,
 PRIMARY KEY(tenant_id,project_id,id),
 FOREIGN KEY(tenant_id,project_id) REFERENCES projects(tenant_id,id)
);
CREATE INDEX evaluations_pending ON evaluations(state,lease_until,created_at);
CREATE TABLE eval_outbox (
 id text PRIMARY KEY,
 tenant_id text NOT NULL,
 project_id text NOT NULL,
 evaluation_id text NOT NULL,
 report jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(tenant_id,project_id,evaluation_id) REFERENCES evaluations(tenant_id,project_id,id)
);
CREATE TABLE projection_deliveries (
 consumer text NOT NULL,
 tenant_id text NOT NULL,
 project_id text NOT NULL,
 event_id text NOT NULL,
 claim_token text,
 lease_until timestamptz,
 delivered_at timestamptz,
 PRIMARY KEY(consumer,tenant_id,project_id,event_id)
);
CREATE INDEX projection_deliveries_pending ON projection_deliveries(consumer,delivered_at,lease_until);
