-- idp-host schema v17 (ADR-0001 D70-D76).
-- (1) citation_next is the citation counter of a run tree (D70): the number the next source of the tree gets. Only the row of the
-- ROOT run is used (a run without a parent); the sub-agent runs of the tree allocate from it, in one short transaction that holds
-- the root's row. Numbers are given gaplessly, so every number below citation_next was given to some source of the tree.
-- (2) credential_ref and credential_renewing_until are for the gateway credential what cmi_credential_ref and cmi_renewing_until
-- are for the member's CMI credential (D68, D69): a sub-agent run points at the run that holds the credential instead of holding a
-- copy, and one run at a time renews it, by a lease on the holder's row, with nothing locked across the call to the authorization
-- server (D76).
ALTER TABLE runs ADD COLUMN citation_next integer NOT NULL DEFAULT 1 CHECK (citation_next >= 1);
ALTER TABLE runs ADD COLUMN credential_ref text;
ALTER TABLE runs ADD COLUMN credential_renewing_until timestamptz;

-- Runs that are already in flight (or finished) continue from their current maximum: the root of every tree gets one past the
-- largest number any run of the tree was given, whether a knowledge number counted in the events (D67: firstIndex + the items
-- numbered by context.resolved, context.package and context.stale) or a web number idp-web gave (the n of a source of the run).
-- A number the model has already read in an earlier tool result is therefore never given again. The numbers a child run gave
-- itself from 1 before this version can still repeat those of its parent: what the model read cannot be corrected afterwards.
-- The update is made tenant by tenant, as the row-level security of these tables requires (the schema owner is not exempt).
DO $$
DECLARE
  t uuid;
BEGIN
  FOR t IN SELECT id FROM tenants LOOP
    PERFORM set_config('app.tenant_id', t::text, true);
    WITH RECURSIVE tree AS (
      SELECT id, id AS root FROM runs WHERE tenant_id = t AND parent_run_id IS NULL
      UNION ALL
      SELECT r.id, tr.root FROM runs r JOIN tree tr ON r.parent_run_id = tr.id WHERE r.tenant_id = t
    ), by_events AS (
      SELECT tr.root, max((e.payload->'value'->>'firstIndex')::int
                          + coalesce((e.payload->'value'->>'itemCount')::int, (e.payload->'value'->>'numbered')::int, 0)) AS next
      FROM tree tr JOIN run_events e ON e.tenant_id = t AND e.run_id = tr.id AND e.type = 'CUSTOM'
        AND e.payload->>'name' IN ('idp.host/context.resolved', 'idp.host/context.package', 'idp.host/context.stale')
      WHERE e.payload->'value'->>'firstIndex' IS NOT NULL
      GROUP BY tr.root
    ), by_web AS (
      SELECT tr.root, max((i->>'n')::int) + 1 AS next
      FROM tree tr JOIN run_sources s ON s.tenant_id = t AND s.run_id = tr.id
        CROSS JOIN LATERAL jsonb_array_elements(coalesce(s.doc->'items', '[]'::jsonb)) AS i
      GROUP BY tr.root
    )
    UPDATE runs r SET citation_next = greatest(1, coalesce(ev.next, 1), coalesce(w.next, 1))
    FROM (SELECT DISTINCT root FROM tree) roots
      LEFT JOIN by_events ev ON ev.root = roots.root
      LEFT JOIN by_web w ON w.root = roots.root
    WHERE r.tenant_id = t AND r.id = roots.root;
  END LOOP;
END $$;
