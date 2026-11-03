CREATE TABLE projects (tenant_id text NOT NULL,id text NOT NULL,PRIMARY KEY(tenant_id,id));
CREATE TABLE resources (
 tenant_id text NOT NULL, project_id text NOT NULL, kind text NOT NULL, id text NOT NULL,
 name text NOT NULL, body jsonb NOT NULL, PRIMARY KEY(tenant_id,project_id,kind,id),
 UNIQUE(tenant_id,project_id,kind,name)
);
CREATE TABLE idempotency (
 tenant_id text NOT NULL, project_id text NOT NULL, key text NOT NULL, request_hash text NOT NULL,
 response jsonb NOT NULL, PRIMARY KEY(tenant_id,project_id,key)
);
CREATE TABLE branches (
 tenant_id text NOT NULL, project_id text NOT NULL, id text NOT NULL,
 desired text NOT NULL, observed text NOT NULL, execution_id text NOT NULL,
 epoch bigint NOT NULL DEFAULT 0, lease_until timestamptz, sequence bigint NOT NULL DEFAULT 0,
 revision bigint NOT NULL DEFAULT 1, PRIMARY KEY(tenant_id,project_id,id),
 FOREIGN KEY(tenant_id,project_id) REFERENCES projects(tenant_id,id)
);
CREATE TABLE jobs (
 id text PRIMARY KEY, tenant_id text NOT NULL, project_id text NOT NULL, branch_id text NOT NULL,
 operation_id text NOT NULL, action text NOT NULL, desired_revision bigint NOT NULL,
 owner text, claim_epoch bigint NOT NULL DEFAULT 0, lease_until timestamptz,
 available_at timestamptz NOT NULL DEFAULT clock_timestamp(), done boolean NOT NULL DEFAULT false,
 FOREIGN KEY(tenant_id,project_id,branch_id) REFERENCES branches(tenant_id,project_id,id)
);
CREATE INDEX jobs_available ON jobs(available_at) WHERE NOT done;
CREATE TABLE events (
 tenant_id text NOT NULL, project_id text NOT NULL, branch_id text NOT NULL,
 sequence bigint NOT NULL, id text NOT NULL, epoch bigint NOT NULL, type text NOT NULL,
 data jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,project_id,branch_id,sequence), UNIQUE(tenant_id,project_id,id),
 FOREIGN KEY(tenant_id,project_id,branch_id) REFERENCES branches(tenant_id,project_id,id)
);
CREATE TABLE outbox (
 id text PRIMARY KEY, tenant_id text NOT NULL, project_id text NOT NULL,
 branch_id text NOT NULL, sequence bigint NOT NULL, published boolean NOT NULL DEFAULT false,
 claim_token text, lease_until timestamptz,
 FOREIGN KEY(tenant_id,project_id,branch_id,sequence) REFERENCES events(tenant_id,project_id,branch_id,sequence)
);
