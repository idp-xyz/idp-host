-- idp-host schema v15: a child run does not hold a copy of the member's CMI credential (ADR-0001 D68, amended): it points at the run
-- whose credential it is (the root run's), so that one refresh token is renewed in one place, under that run's row lock.
ALTER TABLE runs ADD COLUMN cmi_credential_ref text;
