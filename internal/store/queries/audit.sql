-- name: InsertAuditEvent :exec
INSERT INTO audit_events (
    type, principal, workspace, session, attestation, server, tool,
    trace_id, request_id, details, payload, payload_ref
) VALUES (
    @type, @principal, @workspace, @session, @attestation, @server, @tool,
    @trace_id, @request_id, @details, @payload, @payload_ref
);

-- name: DeleteAuditEventsBefore :execrows
-- Deletes at most batch_size audit events that occurred before cutoff,
-- oldest first, so retention never holds a long lock on the table.
DELETE FROM audit_events AS target
WHERE (target.occurred_at, target.id) IN (
    SELECT old.occurred_at, old.id FROM audit_events AS old
    WHERE old.occurred_at < @cutoff
    ORDER BY old.occurred_at
    LIMIT @batch_size
);

-- name: ListExpiredAuditEvents :many
-- Lists at most batch_size audit events that occurred before cutoff and
-- after the (after_occurred_at, after_id) cursor, in cursor order.
SELECT occurred_at, id, payload_ref FROM audit_events
WHERE occurred_at < @cutoff
  AND (occurred_at, id) > (@after_occurred_at::timestamptz, @after_id::bigint)
ORDER BY occurred_at, id
LIMIT @batch_size;

-- name: DeleteAuditEventsByKey :execrows
-- Deletes the audit events with the given (occurred_at, id) keys.
DELETE FROM audit_events
WHERE (occurred_at, id) IN (
    SELECT unnest(@occurred_ats::timestamptz[]), unnest(@ids::bigint[])
);

-- name: ListAuditEventsPage :many
-- Lists one page of audit events for the web UI, newest first. An empty
-- filter matches everything. A null cursor starts at the newest event.
SELECT id, occurred_at, type, principal, workspace, session, attestation,
       server, tool, trace_id, request_id, details
FROM audit_events
WHERE occurred_at >= @from_time::timestamptz
  AND occurred_at < @to_time::timestamptz
  AND (@principal::text = '' OR principal = @principal)
  AND (@workspace::text = '' OR workspace = @workspace)
  AND (@session::text = '' OR session = @session)
  AND (@server::text = '' OR server = @server)
  AND (@tool::text = '' OR tool = @tool)
  AND (sqlc.narg(before_at)::timestamptz IS NULL
       OR (occurred_at, id) < (sqlc.narg(before_at)::timestamptz, @before_id::bigint))
ORDER BY occurred_at DESC, id DESC
LIMIT @page_size;

-- name: GetAuditEvent :one
-- Returns one audit event by its primary key, without its payload.
SELECT id, occurred_at, type, principal, workspace, session, attestation,
       server, tool, trace_id, request_id, details
FROM audit_events
WHERE occurred_at = @occurred_at AND id = @id;
