-- Preserve the allocation identity used by pre-v3 hosts for retained branches.
-- Every new admission/restore supplies an independent physical sandbox ID.
ALTER TABLE branches ADD COLUMN sandbox_id text;
UPDATE branches SET sandbox_id=id;
ALTER TABLE branches ALTER COLUMN sandbox_id SET NOT NULL;
ALTER TABLE branches ADD CHECK(sandbox_id ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$');
CREATE UNIQUE INDEX branches_physical_sandbox ON branches(tenant_id,project_id,sandbox_id);
