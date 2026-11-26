-- idp-host schema v14: the member's credential for the CMI (ADR-0001 D68), sealed like the gateway credential and cleared with it when
-- the run ends. It is exchanged at run creation, from the caller's token, only for a release whose knowledge tools are on and
-- only when the CMI is reached by delegation; the worker reads it so that the knowledge tools call the CMI as the member. The
-- sealed document holds the access token, its expiry and, for a session-bound exchange, the refresh token.
ALTER TABLE runs ADD COLUMN cmi_credential bytea;
