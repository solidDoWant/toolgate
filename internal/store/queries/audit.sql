-- name: InsertAuditEvent :exec
-- occurred_at is the time of the insert, not of the transaction's start,
-- so the audit tail's commit window only has to cover insert to commit.
INSERT INTO audit_events (
    occurred_at, type, principal, workspace, session, attestation, server, tool,
    trace_id, request_id, details, payload, payload_ref
) VALUES (
    clock_timestamp(), @type, @principal, @workspace, @session, @attestation, @server, @tool,
    @trace_id, @request_id, @details, @payload, @payload_ref
);

-- name: ListAuditTail :many
-- Lists at most max_rows audit events after the (after_at, after_id) cursor,
-- oldest first, for the audit tail. An empty principal matches every
-- principal. Rows from the last 5 seconds are left for a later query, so a
-- row that commits after a newer one is still read in key order.
SELECT id, occurred_at, type, principal, workspace, session, attestation,
       server, tool, trace_id, request_id, details
FROM audit_events
WHERE (occurred_at, id) > (@after_at::timestamptz, @after_id::bigint)
  AND occurred_at <= now() - interval '5 seconds'
  AND (@principal::text = '' OR principal = @principal)
ORDER BY occurred_at, id
LIMIT @max_rows;

-- name: ListAuditTailBacklog :many
-- Lists the newest max_rows audit events since since, newest first, for the
-- start of an audit tail. It leaves out the same last 5 seconds as
-- ListAuditTail. An empty principal matches every principal.
SELECT id, occurred_at, type, principal, workspace, session, attestation,
       server, tool, trace_id, request_id, details
FROM audit_events
WHERE occurred_at > @since::timestamptz
  AND occurred_at <= now() - interval '5 seconds'
  AND (@principal::text = '' OR principal = @principal)
ORDER BY occurred_at DESC, id DESC
LIMIT @max_rows;

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
-- types is the allowed event types (empty: all). outcome is empty,
-- refused, error or approval.
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
  AND (cardinality(@types::text[]) = 0 OR type = ANY(@types::text[]))
  AND (@outcome::text = ''
       OR (@outcome::text = 'refused'
           AND ((type = 'tool.decision' AND details->>'decision' = 'refuse')
                OR type IN ('auth.refuse', 'access.deny', 'approval.deny')))
       OR (@outcome::text = 'error'
           AND ((type = 'tool.result' AND details->>'outcome' IS DISTINCT FROM 'ok')
                OR type = 'instance.fail'))
       OR (@outcome::text = 'approval' AND type LIKE 'approval.%'))
  AND (sqlc.narg(before_at)::timestamptz IS NULL
       OR (occurred_at, id) < (sqlc.narg(before_at)::timestamptz, @before_id::bigint))
ORDER BY occurred_at DESC, id DESC
LIMIT @page_size;

-- name: CountAuditEvents :one
-- Counts the events a filter matches, for the match count of the audit page.
SELECT count(*)::bigint
FROM audit_events
WHERE occurred_at >= @from_time::timestamptz
  AND occurred_at < @to_time::timestamptz
  AND (@principal::text = '' OR principal = @principal)
  AND (@workspace::text = '' OR workspace = @workspace)
  AND (@session::text = '' OR session = @session)
  AND (@server::text = '' OR server = @server)
  AND (@tool::text = '' OR tool = @tool)
  AND (cardinality(@types::text[]) = 0 OR type = ANY(@types::text[]))
  AND (@outcome::text = ''
       OR (@outcome::text = 'refused'
           AND ((type = 'tool.decision' AND details->>'decision' = 'refuse')
                OR type IN ('auth.refuse', 'access.deny', 'approval.deny')))
       OR (@outcome::text = 'error'
           AND ((type = 'tool.result' AND details->>'outcome' IS DISTINCT FROM 'ok')
                OR type = 'instance.fail'))
       OR (@outcome::text = 'approval' AND type LIKE 'approval.%'))
;

-- name: ListAuditFilterOptions :many
-- Lists the distinct values of one filter field among the events of the
-- time range that the principal filter allows, for the pickers of the audit
-- page. kind is server, tool, workspace, session or principal.
SELECT value::text AS value
FROM (
    SELECT DISTINCT CASE @kind::text
        WHEN 'server' THEN server
        WHEN 'tool' THEN tool
        WHEN 'workspace' THEN workspace
        WHEN 'session' THEN session
        ELSE principal END AS value
    FROM audit_events
    WHERE occurred_at >= @from_time::timestamptz
      AND occurred_at < @to_time::timestamptz
      AND (@principal::text = '' OR principal = @principal)
) AS options
WHERE value <> ''
ORDER BY value
LIMIT 200;

-- name: ListSessionPageIDs :many
-- Returns the row IDs of the sessions with the given session IDs, for links
-- from the audit page. A non-empty principal limits the result to that
-- principal's sessions.
SELECT id, session_id
FROM sessions
WHERE session_id = ANY(@session_ids::text[])
  AND (@principal::text = '' OR principal = @principal);

-- name: GetAuditEvent :one
-- Returns one audit event by its primary key, without its payload.
SELECT id, occurred_at, type, principal, workspace, session, attestation,
       server, tool, trace_id, request_id, details
FROM audit_events
WHERE occurred_at = @occurred_at AND id = @id;

-- name: GetAuditEventPayload :one
-- Returns the payload of one audit event: inline JSON, or the S3 key in
-- payload_ref. Both are empty when the event has none.
SELECT principal, payload, payload_ref
FROM audit_events
WHERE occurred_at = @occurred_at AND id = @id;

-- name: GetApprovalRequestPayload :one
-- Returns the payload of the approval.request event of an approval. since
-- bounds the search to the partitions from when the approval was created.
SELECT payload, payload_ref
FROM audit_events
WHERE type = 'approval.request'
  AND occurred_at >= @since::timestamptz
  AND details->>'approval' = @approval_id::text
ORDER BY occurred_at, id
LIMIT 1;

-- name: ListRelatedAuditEvents :many
-- Lists the events of the same request or trace as an event, oldest first,
-- within a day of it. A non-empty principal limits the result to that
-- principal's events.
SELECT id, occurred_at, type, principal, workspace, session, attestation,
       server, tool, trace_id, request_id, details
FROM audit_events
WHERE occurred_at >= @from_time::timestamptz
  AND occurred_at < @to_time::timestamptz
  AND (@principal::text = '' OR principal = @principal)
  AND ((@request_id::text <> '' AND request_id = @request_id)
       OR (@trace_id::text <> '' AND trace_id = @trace_id))
ORDER BY occurred_at, id
LIMIT 50;
