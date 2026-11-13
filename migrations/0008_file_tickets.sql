-- idp-host schema v8: the member's file tickets of a run (ADR-0001 D37), sealed like the gateway credential and
-- cleared with it when the run ends.
ALTER TABLE runs ADD COLUMN file_tickets bytea;
