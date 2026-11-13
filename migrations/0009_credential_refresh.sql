-- idp-host schema v9: the refresh token of a session-bound delegated credential (ADR-0001 D38), sealed like the
-- credential and cleared with it when the run ends, so that a run renews its gateway credential within the user's session.
ALTER TABLE runs ADD COLUMN credential_refresh bytea;
