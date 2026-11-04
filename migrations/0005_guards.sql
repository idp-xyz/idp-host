-- idp-host schema v5: tenant-wide blocked output patterns (ADR-0001 D28).
ALTER TABLE tenant_policies ADD COLUMN blocked_patterns jsonb NOT NULL DEFAULT '[]'::jsonb;
