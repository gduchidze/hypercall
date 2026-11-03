-- Keep 001 byte-for-byte stable: an older binary must reject this schema.
ALTER TABLE branches ADD COLUMN checkpoint_kind text NOT NULL DEFAULT 'Checkpoint' CHECK(checkpoint_kind='Checkpoint');
ALTER TABLE branches ADD COLUMN checkpoint_id text;
ALTER TABLE branches ADD COLUMN budget_remaining_ms bigint NOT NULL DEFAULT 0 CHECK(budget_remaining_ms BETWEEN 0 AND 3600000);
ALTER TABLE branches ADD FOREIGN KEY(tenant_id,project_id,checkpoint_kind,checkpoint_id) REFERENCES resources(tenant_id,project_id,kind,id);
ALTER TABLE jobs ADD COLUMN checkpoint_kind text NOT NULL DEFAULT 'Checkpoint' CHECK(checkpoint_kind='Checkpoint');
ALTER TABLE jobs ADD COLUMN checkpoint_id text;
ALTER TABLE jobs ADD FOREIGN KEY(tenant_id,project_id,checkpoint_kind,checkpoint_id) REFERENCES resources(tenant_id,project_id,kind,id);
