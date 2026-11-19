-- idp-host schema v10: the web sources a run's tools returned (ADR-0001 D49). One row per run holds the list (numbers, titles,
-- addresses, read times, digests, whether the final answer cited them) and the counts of what was not recorded. A table of
-- its own, not a column of runs: a list of runs reads many rows and does not need the sources, which may be 100 entries long.
-- Titles and addresses only: no page text and no search excerpt is stored (the search services' terms).
CREATE TABLE run_sources (
  tenant_id  uuid NOT NULL REFERENCES tenants(id),
  run_id     text NOT NULL,
  doc        jsonb NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, run_id),
  FOREIGN KEY (tenant_id, run_id) REFERENCES runs (tenant_id, id)
);
ALTER TABLE run_sources ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_sources FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON run_sources USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT, INSERT, UPDATE ON run_sources TO idp_host_app;
