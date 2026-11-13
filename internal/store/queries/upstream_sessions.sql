-- name: GetLiveUpstreamSession :one
-- The live upstream session of an (instance, toolgate session) pair.
SELECT id, upstream_session_id, initialize_result FROM upstream_sessions
WHERE instance_id = @instance_id AND session_pk = @session_pk AND ended_at IS NULL;

-- name: LockLiveSessionForKeyShare :one
-- Step one of a claim: the toolgate session must be live. FOR KEY SHARE
-- orders the claim against disable and session end, which lock the row
-- FOR UPDATE, and doesn't block last_seen_at touches.
SELECT 1::int AS found FROM sessions WHERE id = @id AND ended_at IS NULL FOR KEY SHARE;

-- name: SessionHasServerForInstanceTemplate :one
-- Step two of a claim: the session has the instance's template enabled.
SELECT EXISTS (
    SELECT 1 FROM session_servers ss
    JOIN instances i ON i.template_ns = ss.template_ns AND i.template_name = ss.template_name
    WHERE ss.session_pk = @session_pk AND i.id = @instance_id
) AS enabled;

-- name: InsertUpstreamSession :one
-- Step three of a claim. No row comes back when a live row already exists
-- for the pair.
INSERT INTO upstream_sessions (instance_id, session_pk, upstream_session_id, initialize_result)
VALUES (@instance_id, @session_pk, @upstream_session_id, @initialize_result)
ON CONFLICT (instance_id, session_pk) WHERE ended_at IS NULL DO NOTHING
RETURNING id;

-- name: DeleteUpstreamSessionIfUnchanged :execrows
-- Removes a row the server no longer knows. It matches nothing when another
-- replica already removed or replaced the row.
DELETE FROM upstream_sessions WHERE id = @id AND upstream_session_id = @upstream_session_id;

-- name: EndUpstreamSessionsOfTemplate :execrows
-- Ends a session's live rows for the instances of one template (disable).
UPDATE upstream_sessions SET ended_at = now()
WHERE session_pk = @session_pk AND ended_at IS NULL
  AND instance_id IN (SELECT id FROM instances WHERE template_ns = @template_ns AND template_name = @template_name);

-- name: EndUpstreamSessionsOfSession :execrows
-- Ends all live rows of a session (session end).
UPDATE upstream_sessions SET ended_at = now()
WHERE session_pk = @session_pk AND ended_at IS NULL;

-- name: ListLiveUpstreamSessionsOfSession :many
SELECT instance_id, upstream_session_id FROM upstream_sessions
WHERE session_pk = @session_pk AND ended_at IS NULL;

-- name: EndInvalidUpstreamSessions :execrows
-- Ends the live rows of invalid pairs: the instance isn't live, the session
-- ended, or the session has no session_servers row for the instance's
-- template.
UPDATE upstream_sessions u SET ended_at = now()
WHERE u.ended_at IS NULL AND (
    NOT EXISTS (
        SELECT 1 FROM instances i
        WHERE i.id = u.instance_id AND i.desired = 'running' AND i.status NOT IN ('stopping', 'stopped'))
    OR NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = u.session_pk AND s.ended_at IS NULL)
    OR NOT EXISTS (
        SELECT 1 FROM session_servers ss
        JOIN instances i ON i.template_ns = ss.template_ns AND i.template_name = ss.template_name
        WHERE ss.session_pk = u.session_pk AND i.id = u.instance_id)
);

-- name: ListEndedUpstreamSessions :many
-- At most 256 ended rows, oldest first, with whether the instance is live
-- and whether it is remote (a remote instance has no namespace).
SELECT u.id, u.instance_id, u.upstream_session_id,
       COALESCE(i.desired = 'running' AND i.status NOT IN ('stopping', 'stopped'), false)::boolean AS instance_live,
       COALESCE(i.namespace = '', false)::boolean AS instance_remote
FROM upstream_sessions u
JOIN instances i ON i.id = u.instance_id
WHERE u.ended_at IS NOT NULL
ORDER BY u.ended_at, u.id
LIMIT 256;

-- name: DeleteUpstreamSession :exec
DELETE FROM upstream_sessions WHERE id = @id;
