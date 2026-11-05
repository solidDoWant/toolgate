-- +goose Up

-- trace_context is the W3C trace context (traceparent and tracestate, as a
-- JSON object) of the enable request that inserted the row, or empty. The
-- reconciler's render and apply spans are children of it.
ALTER TABLE instances ADD COLUMN trace_context TEXT NOT NULL DEFAULT '';
