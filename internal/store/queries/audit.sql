-- name: InsertAuditEvent :exec
INSERT INTO audit_events (
    type, principal, workspace, session, attestation, server, tool,
    trace_id, request_id, details, payload, payload_ref
) VALUES (
    @type, @principal, @workspace, @session, @attestation, @server, @tool,
    @trace_id, @request_id, @details, @payload, @payload_ref
);
