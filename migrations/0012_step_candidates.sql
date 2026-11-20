-- idp-host schema v12: the candidates that did not answer a model step (ADR-0001 D56). A JSON array of
-- {provider, model, code} in the order they were asked, codes only (never what the provider said); NULL when every
-- candidate that was asked answered (provider and model of the row are the one that did).
ALTER TABLE run_steps
  ADD COLUMN failed_candidates jsonb;
