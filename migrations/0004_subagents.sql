-- idp-host schema v4: sub-agent runs (ADR-0001 D22). A run may be the child of another run's tool
-- call; a parent rests in AWAITING_SUBAGENT while its children run.

ALTER TABLE runs ADD COLUMN parent_run_id text;
ALTER TABLE runs ADD COLUMN depth int NOT NULL DEFAULT 0 CHECK (depth >= 0);
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check CHECK (status IN ('QUEUED', 'RUNNING', 'AWAITING_INPUT', 'AWAITING_SUBAGENT', 'COMPLETED', 'FAILED', 'CANCELED'));
ALTER TABLE runs ADD CONSTRAINT runs_parent_fk FOREIGN KEY (tenant_id, parent_run_id) REFERENCES runs (tenant_id, id);
CREATE INDEX runs_children ON runs (tenant_id, parent_run_id) WHERE parent_run_id IS NOT NULL;

-- a waiting parent is active: it holds its place in the tenant's concurrency and its thread
DROP INDEX runs_active;
CREATE INDEX runs_active ON runs (tenant_id) WHERE status IN ('QUEUED', 'RUNNING', 'AWAITING_INPUT', 'AWAITING_SUBAGENT');

ALTER TABLE tool_calls ADD COLUMN child_run_id text;
ALTER TABLE tool_calls DROP CONSTRAINT tool_calls_state_check;
ALTER TABLE tool_calls ADD CONSTRAINT tool_calls_state_check CHECK (state IN ('PENDING_CONFIRMATION', 'CONFIRMED', 'REJECTED', 'EXPIRED', 'EXECUTING', 'DONE', 'FAILED', 'FRONTEND', 'DENIED', 'DELEGATED'));
