-- idp-host schema v6: evaluation gate (ADR-0001 D26). An evaluation runs every case of a release's
-- evaluation set as an ordinary run of that release and records the verdicts; a required release
-- cannot be approved before its latest evaluation passed.

CREATE TABLE evaluations (
  id          uuid PRIMARY KEY,
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  release_id  uuid NOT NULL REFERENCES agent_releases(id),
  status      text NOT NULL CHECK (status IN ('RUNNING', 'PASSED', 'FAILED')),
  total       int  NOT NULL,
  passed      int  NOT NULL DEFAULT 0,
  results     jsonb NOT NULL DEFAULT '[]'::jsonb,   -- one entry per case: name, runId, status, passed, reason
  created_by  text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz
);
CREATE INDEX evaluations_by_release ON evaluations (tenant_id, release_id, created_at DESC);
ALTER TABLE evaluations ENABLE ROW LEVEL SECURITY;
ALTER TABLE evaluations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON evaluations USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT, INSERT, UPDATE ON evaluations TO idp_host_app;

-- a candidate release is sealed provisionally (no approver) so its evaluation runs can load it
ALTER TABLE agent_releases ADD COLUMN candidate_digest text;
ALTER TABLE agent_releases ADD COLUMN candidate_envelope text;

ALTER TABLE runs ADD COLUMN purpose text;
ALTER TABLE runs ADD COLUMN evaluation_id uuid REFERENCES evaluations(id);
