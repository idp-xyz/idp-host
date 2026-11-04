-- idp-host schema v7: the CMI package a run's context came from (ADR-0001 D32).
ALTER TABLE runs ADD COLUMN context_package_id text;
