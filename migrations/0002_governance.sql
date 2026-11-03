-- idp-host schema v2: tenant-level governance (ADR-0001 D15).
-- A policy per tenant (defaults apply when absent); run token accounting for the per-run cap;
-- an index for the rolling usage window.

CREATE TABLE tenant_policies (
  tenant_id           uuid PRIMARY KEY REFERENCES tenants(id),
  max_concurrent_runs int    NOT NULL DEFAULT 10 CHECK (max_concurrent_runs >= 0),  -- 0 = unlimited
  tokens_per_day      bigint NOT NULL DEFAULT 0  CHECK (tokens_per_day >= 0),       -- 0 = unlimited
  max_tokens_per_run  bigint NOT NULL DEFAULT 0  CHECK (max_tokens_per_run >= 0),   -- 0 = unlimited; a release's limits.maxTokensPerRun overrides
  updated_by          text,
  updated_at          timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE tenant_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON tenant_policies USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT, INSERT, UPDATE ON tenant_policies TO idp_host_app;

ALTER TABLE runs ADD COLUMN tokens_used bigint NOT NULL DEFAULT 0;
CREATE INDEX run_steps_usage ON run_steps (tenant_id, started_at);
CREATE INDEX runs_active ON runs (tenant_id) WHERE status IN ('QUEUED', 'RUNNING', 'AWAITING_INPUT');
