-- idp-host schema v13: the W3C traceparent of the request that created a run (ADR-0001 D57), so that the worker's turn continues
-- that request's trace. trace_id stays as it was (the trace id alone); a run created before this migration has NULL here and its
-- turn joins the trace by trace_id. It identifies spans, nothing else: no content, no subject.
ALTER TABLE runs
  ADD COLUMN traceparent text;
