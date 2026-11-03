-- idp-host schema v3: skills and their releases (ADR-0001 D17), shaped like agents.

CREATE TABLE skills (
  id                 uuid PRIMARY KEY,
  tenant_id          uuid NOT NULL REFERENCES tenants(id),
  name               text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$'),
  display_name       text NOT NULL,
  definition         jsonb NOT NULL,     -- the editable draft (agent.SkillDefinition)
  current_release_id uuid,
  created_by         text NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, name)
);

CREATE TABLE skill_releases (
  id          uuid PRIMARY KEY,
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  skill_id    uuid NOT NULL REFERENCES skills(id),
  version     int  NOT NULL CHECK (version >= 1),
  status      text NOT NULL CHECK (status IN ('CANDIDATE', 'APPROVED', 'REJECTED', 'SUPERSEDED')),
  document    text NOT NULL,
  digest      text,
  envelope    text,
  created_by  text NOT NULL,
  approved_by text,
  created_at  timestamptz NOT NULL DEFAULT now(),
  approved_at timestamptz,
  UNIQUE (skill_id, version)
);
ALTER TABLE skills ADD CONSTRAINT skills_current_release_fk FOREIGN KEY (current_release_id) REFERENCES skill_releases(id);

DO $$ DECLARE t text; BEGIN
  FOREACH t IN ARRAY ARRAY['skills', 'skill_releases'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY tenant_rows ON %I USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant())', t);
  END LOOP;
END $$;
GRANT SELECT, INSERT, UPDATE ON skills, skill_releases TO idp_host_app;
