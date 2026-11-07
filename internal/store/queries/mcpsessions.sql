-- name: InsertMCPSession :one
-- Stores an MCP session at initialize, with its binding.
INSERT INTO mcp_sessions (id, session_pk, principal, session_id, workspace, attestation, protocol_version, initialize_params)
VALUES (@id, @session_pk, @principal, @session_id, @workspace, @attestation, @protocol_version, @initialize_params)
RETURNING *;

-- name: GetMCPSession :one
SELECT * FROM mcp_sessions WHERE id = @id;

-- name: SetMCPSessionInitializedParams :execrows
-- Writes the notifications/initialized params of a live MCP session.
UPDATE mcp_sessions SET initialized_params = @initialized_params
WHERE id = @id AND ended_at IS NULL;

-- name: TouchMCPSession :execrows
-- Moves last_seen_at of a live MCP session forward. No row means the
-- session ended.
UPDATE mcp_sessions SET last_seen_at = now()
WHERE id = @id AND ended_at IS NULL;

-- name: ListIdleConnectorMCPSessions :many
-- Lists the live connector MCP sessions (their toolgate session ID starts
-- with "mcp.") unused for the idle timeout. Helper sessions end only with
-- their toolgate session, never by idleness.
SELECT * FROM mcp_sessions
WHERE ended_at IS NULL AND session_id LIKE 'mcp.%' AND last_seen_at < now() - @idle_timeout::interval
ORDER BY id;

-- name: EndIdleConnectorMCPSession :one
-- Ends the connector MCP session only while it is still idle, so a touch
-- that commits first keeps it.
UPDATE mcp_sessions SET ended_at = now()
WHERE id = @id AND ended_at IS NULL AND session_id LIKE 'mcp.%' AND last_seen_at < now() - @idle_timeout::interval
RETURNING *;

-- name: EndLiveMCPSession :one
UPDATE mcp_sessions SET ended_at = now()
WHERE id = @id AND ended_at IS NULL
RETURNING *;

-- name: EndSessionMCPSessions :many
-- Ends the live MCP sessions of a toolgate session that is ending.
UPDATE mcp_sessions SET ended_at = now()
WHERE session_pk = @session_pk AND ended_at IS NULL
RETURNING id;

-- name: DeleteStaleMCPSessions :execrows
-- Deletes MCP sessions that ended more than an hour ago. A request for one
-- gets the same 404 as for an ended one.
DELETE FROM mcp_sessions WHERE ended_at <= now() - interval '1 hour';
