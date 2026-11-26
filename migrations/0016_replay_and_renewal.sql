-- idp-host schema v16: (1) request_raw_sha256 is the digest of what the caller sent (the request without what the HOST derives from
-- the gateway catalogue and the CMI), so that an idempotent replay is found before anything is exchanged, listed, rewritten or
-- retrieved (ADR-0001 D69); a run created before it has NULL and is found by the older digest. (2) cmi_renewing_until is the short lease on
-- the renewal of a member's CMI credential (D68): one run renews it at a time, without any row lock held across the call to the
-- authorization server.
ALTER TABLE runs ADD COLUMN request_raw_sha256 text;
ALTER TABLE runs ADD COLUMN cmi_renewing_until timestamptz;
