-- idp-host schema v1 (ADR-0001 D2, D3, D7, D8, D12).
-- Run by the schema owner (`idp-host migrate`). The service connects as a login role that is a
-- member of idp_host_app: not a superuser, no BYPASSRLS, so the policies below always apply.

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'idp_host_app') THEN
    CREATE ROLE idp_host_app NOLOGIN NOBYPASSRLS;
  END IF;
END $$;

-- The tenant of the current transaction (set_config('app.tenant_id', ..., true)); NULL when unset,
-- so a connection that forgot to bind a tenant sees nothing.
CREATE FUNCTION current_tenant() RETURNS uuid LANGUAGE sql STABLE AS $$
  SELECT nullif(current_setting('app.tenant_id', true), '')::uuid
$$;

CREATE TABLE tenants (
  id         uuid PRIMARY KEY,
  slug       text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE agents (
  id                 uuid PRIMARY KEY,
  tenant_id          uuid NOT NULL REFERENCES tenants(id),
  name               text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$'),
  display_name       text NOT NULL,
  definition         jsonb NOT NULL,     -- the editable draft (agent.Definition)
  current_release_id uuid,               -- the release new runs use; NULL = not published
  created_by         text NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, name)
);

CREATE TABLE agent_releases (
  id          uuid PRIMARY KEY,
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  agent_id    uuid NOT NULL REFERENCES agents(id),
  version     int  NOT NULL CHECK (version >= 1),
  status      text NOT NULL CHECK (status IN ('CANDIDATE', 'APPROVED', 'REJECTED', 'SUPERSEDED')),
  document    text NOT NULL,  -- canonical JSON as compiled; sealed into the envelope on approval
  digest      text,
  envelope    text,           -- byte-exact (jsonb would rewrite numbers and break the digest)
  created_by  text NOT NULL,
  approved_by text,
  created_at  timestamptz NOT NULL DEFAULT now(),
  approved_at timestamptz,
  UNIQUE (agent_id, version)
);
ALTER TABLE agents ADD CONSTRAINT agents_current_release_fk FOREIGN KEY (current_release_id) REFERENCES agent_releases(id);

CREATE TABLE sessions (
  id         uuid PRIMARY KEY,
  tenant_id  uuid NOT NULL REFERENCES tenants(id),
  owner      text NOT NULL,
  title      text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- Thread and run ids may be chosen by the caller (AG-UI threadId / runId), so they are text and
-- unique per tenant.
CREATE TABLE threads (
  tenant_id  uuid NOT NULL REFERENCES tenants(id),
  id         text NOT NULL CHECK (id ~ '^[A-Za-z0-9_.:-]{1,128}$'),
  session_id uuid REFERENCES sessions(id),
  owner      text NOT NULL,
  agent_id   uuid REFERENCES agents(id),  -- the agent of the first run; later runs must match
  message_seq bigint NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id)
);

CREATE TABLE thread_messages (
  tenant_id  uuid NOT NULL REFERENCES tenants(id),
  thread_id  text NOT NULL,
  seq        bigint NOT NULL,
  id         text NOT NULL,     -- AG-UI message id
  run_id     text,              -- the run that produced it; NULL for caller-supplied history
  message    jsonb NOT NULL,    -- agui.Message
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, thread_id, seq),
  UNIQUE (tenant_id, thread_id, id),
  FOREIGN KEY (tenant_id, thread_id) REFERENCES threads (tenant_id, id)
);

CREATE TABLE runs (
  tenant_id             uuid NOT NULL REFERENCES tenants(id),
  id                    text NOT NULL CHECK (id ~ '^[A-Za-z0-9_.:-]{1,128}$'),
  thread_id             text NOT NULL,
  agent_id              uuid NOT NULL REFERENCES agents(id),
  release_id            uuid NOT NULL REFERENCES agent_releases(id),
  release_digest        text NOT NULL,
  owner                 text NOT NULL,
  status                text NOT NULL CHECK (status IN ('QUEUED', 'RUNNING', 'AWAITING_INPUT', 'COMPLETED', 'FAILED', 'CANCELED')),
  idempotency_key       text NOT NULL,
  request_sha256        text NOT NULL,
  input                 jsonb NOT NULL,   -- frontend tools, context, forwardedProps of the request
  step_count            int NOT NULL DEFAULT 0,
  event_seq             int NOT NULL DEFAULT 0,
  fencing_epoch         bigint NOT NULL DEFAULT 0,
  lease_owner           text,
  lease_until           timestamptz,
  cancel_requested      boolean NOT NULL DEFAULT false,
  failure_code          text,
  failure_message       text,
  execution_ref         text,             -- reserved for EER (ADR-0001 D10)
  credential            bytea,            -- encrypted delegated gateway token (D8)
  credential_expires_at timestamptz,
  trace_id              text,
  deadline_at           timestamptz NOT NULL,
  created_at            timestamptz NOT NULL DEFAULT now(),
  started_at            timestamptz,
  finished_at           timestamptz,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, thread_id) REFERENCES threads (tenant_id, id),
  UNIQUE (tenant_id, thread_id, owner, idempotency_key)
);
CREATE INDEX runs_claimable ON runs (created_at) WHERE status IN ('QUEUED', 'RUNNING');
CREATE INDEX runs_waiting ON runs (tenant_id, status) WHERE status = 'AWAITING_INPUT';

CREATE TABLE run_events (
  id        bigserial PRIMARY KEY,
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  run_id    text NOT NULL,
  seq       int NOT NULL,
  type      text NOT NULL,
  payload   jsonb NOT NULL,
  at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, run_id, seq),
  FOREIGN KEY (tenant_id, run_id) REFERENCES runs (tenant_id, id)
);
CREATE INDEX run_events_by_run ON run_events (tenant_id, run_id, id);

CREATE TABLE run_steps (
  tenant_id         uuid NOT NULL REFERENCES tenants(id),
  run_id            text NOT NULL,
  step              int NOT NULL,
  provider          text NOT NULL,
  model             text NOT NULL,
  prompt_tokens     int NOT NULL DEFAULT 0,
  completion_tokens int NOT NULL DEFAULT 0,
  latency_ms        bigint NOT NULL DEFAULT 0,
  stop_reason       text,
  error_code        text,
  started_at        timestamptz NOT NULL DEFAULT now(),
  finished_at       timestamptz,
  PRIMARY KEY (tenant_id, run_id, step),
  FOREIGN KEY (tenant_id, run_id) REFERENCES runs (tenant_id, id)
);

CREATE TABLE tool_calls (
  tenant_id               uuid NOT NULL REFERENCES tenants(id),
  run_id                  text NOT NULL,
  id                      text NOT NULL,      -- the model's tool call id
  step                    int NOT NULL,
  message_id              text NOT NULL,      -- the assistant message carrying the call
  tool                    text NOT NULL,      -- exposed name as the gateway lists it
  arguments               text NOT NULL,      -- canonical JSON
  arguments_sha256        text NOT NULL,
  state                   text NOT NULL CHECK (state IN ('PENDING_CONFIRMATION', 'CONFIRMED', 'REJECTED', 'EXPIRED', 'EXECUTING', 'DONE', 'FAILED', 'FRONTEND', 'DENIED')),
  confirmation_expires_at timestamptz,
  decided_by              text,
  decided_at              timestamptz,
  result                  text,
  is_error                boolean NOT NULL DEFAULT false,
  created_at              timestamptz NOT NULL DEFAULT now(),
  finished_at             timestamptz,
  PRIMARY KEY (tenant_id, run_id, id),
  FOREIGN KEY (tenant_id, run_id) REFERENCES runs (tenant_id, id)
);

CREATE TABLE audit_events (
  id          bigserial PRIMARY KEY,
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  at          timestamptz NOT NULL DEFAULT now(),
  subject     text NOT NULL,
  action      text NOT NULL,
  resource_id text,
  detail      jsonb
);

-- Row-level security: every tenant table is restricted to the transaction's tenant, for the
-- table owner too (FORCE). Only runs has a second policy: a worker transaction that declares
-- app.role = 'worker' may see and update runs of every tenant, which it needs to claim the next
-- one; it then binds the run's tenant before touching anything else.
DO $$ DECLARE t text; BEGIN
  FOREACH t IN ARRAY ARRAY['agents', 'agent_releases', 'sessions', 'threads', 'thread_messages', 'runs', 'run_events', 'run_steps', 'tool_calls', 'audit_events'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY tenant_rows ON %I USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant())', t);
  END LOOP;
END $$;
CREATE POLICY worker_claim ON runs
  USING (current_setting('app.role', true) = 'worker')
  WITH CHECK (current_setting('app.role', true) = 'worker');

GRANT SELECT ON tenants TO idp_host_app;
GRANT SELECT, INSERT, UPDATE ON agents, agent_releases, sessions, threads, thread_messages, runs, run_steps, tool_calls TO idp_host_app;
GRANT SELECT, INSERT ON run_events, audit_events TO idp_host_app;  -- append-only for the service
GRANT USAGE ON SEQUENCE run_events_id_seq, audit_events_id_seq TO idp_host_app;
