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
