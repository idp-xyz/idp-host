-- idp-host schema v11: what a model step spent before its first output and what the provider said about reuse of the prompt
-- (ADR-0001 D54). All columns are nullable and NULL means "not known": a non-streamed answer has no first-output time, a
-- provider that does not report reuse has no cached_tokens. Zero is a measurement, NULL is not.
ALTER TABLE run_steps
  ADD COLUMN first_output_ms       double precision,  -- request sent to the first output of any kind (text, reasoning, tool call)
  ADD COLUMN first_text_ms         double precision,  -- request sent to the first text fragment
  ADD COLUMN after_first_ms        double precision,  -- first output to the end of the stream
  ADD COLUMN cached_tokens         int,               -- prompt tokens the provider served from its cache (a share of prompt_tokens)
  ADD COLUMN cache_creation_tokens int,               -- prompt tokens the provider wrote to its cache (Anthropic)
  ADD COLUMN server_prompt_n       int,               -- llama.cpp timings.prompt_n: prompt tokens it processed
  ADD COLUMN server_prompt_ms      double precision,  -- timings.prompt_ms
  ADD COLUMN server_cache_n        int,               -- timings.cache_n: prompt tokens it reused
  ADD COLUMN server_predicted_n    int,               -- timings.predicted_n
  ADD COLUMN server_predicted_ms   double precision;  -- timings.predicted_ms
