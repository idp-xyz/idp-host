-- idp-host schema v17 (ADR-0001 D70-D76). REQUIRES DOWNTIME: stop the API and the workers, run `idp-host migrate`, start them again (the
-- backfill below reads every tool result of every thread, see the RUNBOOK note for the measured time).
--
-- (1) citation_counters is the citation counter of a THREAD (D70): the model reads the whole thread, so every run that writes into it
-- (follow-up runs, the sub-agent runs of each, theirs in turn) takes the numbers of its sources from one counter, the one of the thread
-- of the ROOT run of the tree (a run without a parent). next is the number the next source gets; known maps what has been numbered
-- already (a web address by its idp-web number, a knowledge passage by document and place) to its number, so that reading it again keeps
-- the number. The row is locked only by the transaction that allocates numbers and writes the answer they appear in.
-- (2) credential_ref and credential_renewing_until are for the gateway credential what cmi_credential_ref and cmi_renewing_until are for
-- the member's CMI credential (D68, D69): a sub-agent run points at the run that holds the credential instead of holding a copy, and one
-- run at a time renews it by a lease on the holder's row, with nothing locked across the call to the authorization server (D76). The
-- *_renew_token columns say who holds the lease: a lease is given back only by the run that took it.
CREATE TABLE citation_counters (
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  thread_id text NOT NULL,
  next      integer NOT NULL DEFAULT 1 CHECK (next >= 1),
  known     jsonb NOT NULL DEFAULT '{}'::jsonb,
  PRIMARY KEY (tenant_id, thread_id),
  FOREIGN KEY (tenant_id, thread_id) REFERENCES threads (tenant_id, id)
);
ALTER TABLE citation_counters ENABLE ROW LEVEL SECURITY;
ALTER TABLE citation_counters FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON citation_counters USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT, INSERT, UPDATE ON citation_counters TO idp_host_app;

ALTER TABLE runs ADD COLUMN credential_ref text;
ALTER TABLE runs ADD COLUMN credential_renewing_until timestamptz;
ALTER TABLE runs ADD COLUMN credential_renew_token text;
ALTER TABLE runs ADD COLUMN cmi_renew_token text;

-- Threads that are in use continue from their current maximum: the counter of a thread is one past the largest number any run of any tree
-- rooted in it was given or read, whether
--   - a knowledge number counted in the events (D67: firstIndex + the items numbered by context.resolved, context.package, context.stale),
--   - a web number idp-web gave that a run recorded (the n of a source in run_sources: not all of them, a run recorded at most 100), or
--   - a number at the head of a block of any tool result in the thread (a message of role tool: "[n] " at the start of the text or after a
--     line break, which is how idp-web and the knowledge tools wrote them), which is what the model actually read.
-- A number the model has already read in an earlier tool result is therefore never given again. The numbers a child run gave itself from 1
-- before this version can still repeat those of its parent: what the model read cannot be corrected afterwards.
-- The update is made tenant by tenant, as the row-level security of these tables requires (the schema owner is not exempt).
DO $$
DECLARE
  t uuid;
BEGIN
  FOR t IN SELECT id FROM tenants LOOP
    PERFORM set_config('app.tenant_id', t::text, true);
    INSERT INTO citation_counters (tenant_id, thread_id, next)
    WITH RECURSIVE tree AS (
      SELECT id, thread_id AS root_thread FROM runs WHERE tenant_id = t AND parent_run_id IS NULL
      UNION ALL
      SELECT r.id, tr.root_thread FROM runs r JOIN tree tr ON r.parent_run_id = tr.id WHERE r.tenant_id = t
    ), by_events AS (
      SELECT tr.root_thread AS thread_id,
             max((e.payload->'value'->>'firstIndex')::int
                 + coalesce((e.payload->'value'->>'itemCount')::int, (e.payload->'value'->>'numbered')::int, 0)) AS next
      FROM tree tr JOIN run_events e ON e.tenant_id = t AND e.run_id = tr.id AND e.type = 'CUSTOM'
        AND e.payload->>'name' IN ('idp.host/context.resolved', 'idp.host/context.package', 'idp.host/context.stale')
      WHERE e.payload->'value'->>'firstIndex' IS NOT NULL
      GROUP BY tr.root_thread
    ), by_sources AS (
      SELECT tr.root_thread AS thread_id, max((i->>'n')::int) + 1 AS next
      FROM tree tr JOIN run_sources s ON s.tenant_id = t AND s.run_id = tr.id
        CROSS JOIN LATERAL jsonb_array_elements(coalesce(s.doc->'items', '[]'::jsonb)) AS i
      GROUP BY tr.root_thread
    ), by_messages AS (
      SELECT tr.root_thread AS thread_id, max(m[1]::int) + 1 AS next
      FROM tree tr JOIN runs r ON r.tenant_id = t AND r.id = tr.id
        JOIN thread_messages tm ON tm.tenant_id = t AND tm.thread_id = r.thread_id AND tm.message->>'role' = 'tool'
        CROSS JOIN LATERAL regexp_matches(coalesce(tm.message->>'content', ''), '(?:^|\n)\[([0-9]{1,6})\] ', 'g') AS m
      GROUP BY tr.root_thread
    ), roots AS (
      SELECT DISTINCT root_thread AS thread_id FROM tree
    )
    SELECT t, roots.thread_id, greatest(1, coalesce(ev.next, 1), coalesce(so.next, 1), coalesce(me.next, 1))
    FROM roots
      LEFT JOIN by_events ev ON ev.thread_id = roots.thread_id
      LEFT JOIN by_sources so ON so.thread_id = roots.thread_id
      LEFT JOIN by_messages me ON me.thread_id = roots.thread_id
    WHERE greatest(coalesce(ev.next, 1), coalesce(so.next, 1), coalesce(me.next, 1)) > 1
    ON CONFLICT (tenant_id, thread_id) DO UPDATE SET next = EXCLUDED.next;
  END LOOP;
END $$;
