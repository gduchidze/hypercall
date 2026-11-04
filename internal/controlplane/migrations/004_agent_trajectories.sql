ALTER TABLE branches ADD COLUMN agent_gate text NOT NULL DEFAULT 'unsupported' CHECK(agent_gate IN ('unsupported','configuring','open','sealed','finalized'));
ALTER TABLE branches ADD COLUMN route_digest text NOT NULL DEFAULT '';
ALTER TABLE branches ADD COLUMN session_hash text NOT NULL DEFAULT '';
ALTER TABLE branches ADD COLUMN session_ack_hash text NOT NULL DEFAULT '';
ALTER TABLE branches ADD COLUMN agent_step bigint NOT NULL DEFAULT 0 CHECK(agent_step>=0);
ALTER TABLE branches ADD COLUMN agent_cursor bigint NOT NULL DEFAULT 0 CHECK(agent_cursor>=0);
ALTER TABLE branches ADD COLUMN agent_state_digest text NOT NULL DEFAULT '';
ALTER TABLE branches ADD COLUMN sealed_barrier jsonb;
ALTER TABLE branches ADD COLUMN trace_complete boolean NOT NULL DEFAULT false;
CREATE TABLE agent_calls (
 tenant_id text NOT NULL,project_id text NOT NULL,branch_id text NOT NULL,id text NOT NULL,
 step bigint NOT NULL CHECK(step>=1),position integer NOT NULL CHECK(position>=1),
 registered_sequence bigint NOT NULL,kind text NOT NULL,request_hash text NOT NULL,request jsonb NOT NULL,result jsonb,
 execution_id text NOT NULL,epoch bigint NOT NULL,model_digest text NOT NULL,route_digest text NOT NULL,
 adapter_version text NOT NULL,mutating boolean NOT NULL,state text NOT NULL,
 claim_token text,claim_epoch bigint NOT NULL DEFAULT 0,lease_until timestamptz,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,project_id,branch_id,id),
 UNIQUE(tenant_id,project_id,branch_id,step,position),
 FOREIGN KEY(tenant_id,project_id,branch_id) REFERENCES branches(tenant_id,project_id,id)
);
CREATE TABLE agent_steps (
 tenant_id text NOT NULL,project_id text NOT NULL,branch_id text NOT NULL,step bigint NOT NULL,
 state_digest text NOT NULL,event_sequence bigint NOT NULL,session_hash text NOT NULL,
 complete boolean NOT NULL DEFAULT true,
 PRIMARY KEY(tenant_id,project_id,branch_id,step),
 FOREIGN KEY(tenant_id,project_id,branch_id,event_sequence) REFERENCES events(tenant_id,project_id,branch_id,sequence)
);
ALTER TABLE branches ADD COLUMN harness_session jsonb;
ALTER TABLE branches ADD COLUMN session_parent_checkpoint_id text;
ALTER TABLE agent_calls ADD FOREIGN KEY(tenant_id,project_id,branch_id,registered_sequence) REFERENCES events(tenant_id,project_id,branch_id,sequence);
ALTER TABLE agent_calls ADD CHECK(kind IN ('inference','tool','step_commit','finalize'));
ALTER TABLE agent_calls ADD CHECK(state IN ('registered','executing','completed','committed','finalized','unknown'));
CREATE INDEX agent_calls_cursor ON agent_calls(tenant_id,project_id,branch_id,registered_sequence);
