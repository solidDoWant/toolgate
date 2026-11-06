-- name: UpsertSession :one
-- Creates the session or, for an existing one, writes this request's
-- identity snapshot and moves last_seen_at forward. It does not restart an
-- ended session (RestartSession does).
INSERT INTO sessions (principal, session_id, workspace, attestation, kind, email, name, groups)
VALUES (@principal, @session_id, @workspace, @attestation, @kind, @email, @name, @groups)
ON CONFLICT (principal, session_id) DO UPDATE
    SET email = EXCLUDED.email,
        name = EXCLUDED.name,
        groups = EXCLUDED.groups,
        last_seen_at = now()
RETURNING *;

-- name: CreateSession :one
-- Creates the session. It returns no row when the session already exists.
INSERT INTO sessions (principal, session_id, workspace, attestation, kind, email, name, groups)
VALUES (@principal, @session_id, @workspace, @attestation, @kind, @email, @name, @groups)
ON CONFLICT (principal, session_id) DO NOTHING
RETURNING *;

-- name: GetSession :one
SELECT * FROM sessions WHERE principal = @principal AND session_id = @session_id;

-- name: GetSessionByID :one
SELECT * FROM sessions WHERE id = @id;

-- name: LockSession :one
SELECT * FROM sessions WHERE id = @id FOR UPDATE;

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = now() WHERE id = @id;

-- name: RestartSession :one
UPDATE sessions
SET ended_at = NULL, workspace = @workspace, attestation = @attestation, kind = @kind, last_seen_at = now()
WHERE id = @id AND ended_at IS NOT NULL
RETURNING *;

-- name: EndIdleSession :one
UPDATE sessions
SET ended_at = now()
WHERE id = @id AND ended_at IS NULL AND last_seen_at < now() - @idle_timeout::interval
RETURNING *;

-- name: ListIdleSessions :many
SELECT * FROM sessions
WHERE ended_at IS NULL AND last_seen_at < now() - @idle_timeout::interval
ORDER BY id;

-- name: LatestSessionOfPrincipal :one
SELECT * FROM sessions
WHERE principal = @principal
ORDER BY last_seen_at DESC
LIMIT 1;

-- name: EnableServer :exec
INSERT INTO session_servers (session_pk, template_ns, template_name)
VALUES (@session_pk, @template_ns, @template_name)
ON CONFLICT DO NOTHING;

-- name: DisableServer :execrows
DELETE FROM session_servers
WHERE session_pk = @session_pk AND template_ns = @template_ns AND template_name = @template_name;

-- name: DeleteSessionServers :exec
DELETE FROM session_servers WHERE session_pk = @session_pk;

-- name: ListSessionServers :many
SELECT * FROM session_servers WHERE session_pk = @session_pk ORDER BY template_ns, template_name;

-- name: CountSessionServers :one
SELECT count(*) FROM session_servers WHERE session_pk = @session_pk;

-- name: ListLiveSessionsEnabling :many
SELECT s.* FROM sessions s
JOIN session_servers ss ON ss.session_pk = s.id
WHERE ss.template_ns = @template_ns AND ss.template_name = @template_name AND s.ended_at IS NULL
ORDER BY s.id;

-- name: CountLiveEnablers :one
-- Counts the live sessions that have the template enabled and use an
-- instance of the given scope and scope key: any session for shared, the
-- owner's sessions for per-user, and the one session of the scope key for
-- per-session.
SELECT count(*) FROM session_servers ss
JOIN sessions s ON s.id = ss.session_pk
WHERE ss.template_ns = @template_ns AND ss.template_name = @template_name
  AND s.ended_at IS NULL
  AND (
    @scope::text = 'shared'
    OR (@scope::text = 'per-user' AND s.principal = @owner_principal::text)
    OR (@scope::text = 'per-session' AND 'session:' || s.principal || ':' || s.session_id = @scope_key::text)
  );

-- name: ListOpenSessionsOfPrincipal :many
SELECT * FROM sessions
WHERE principal = @principal AND ended_at IS NULL
ORDER BY last_seen_at DESC
LIMIT @max_rows::bigint;

-- name: ListOpenSessionsWithServers :many
-- The principal's open sessions (not ended, seen within the idle timeout),
-- newest first and at most max_rows of them, each with one row per enabled
-- server (none gives one row with NULL server columns).
WITH open_sessions AS (
    SELECT id, session_id, workspace, attestation, created_at, last_seen_at
    FROM sessions
    WHERE principal = @principal AND ended_at IS NULL
      AND last_seen_at > now() - @idle_timeout::interval
    ORDER BY last_seen_at DESC, id DESC
    LIMIT @max_rows::bigint
)
SELECT s.id, s.session_id, s.workspace, s.attestation, s.created_at, s.last_seen_at,
       ss.template_name AS server_name, ss.enabled_at AS server_enabled_at
FROM open_sessions s
LEFT JOIN session_servers ss ON ss.session_pk = s.id
ORDER BY s.last_seen_at DESC, s.id DESC, ss.template_ns, ss.template_name;

-- name: ListForgettableSessions :many
-- The sessions that ended longer ago than the resume window and still have
-- enabled servers.
SELECT s.* FROM sessions s
WHERE s.ended_at IS NOT NULL AND s.ended_at < now() - @resume_window::interval
  AND EXISTS (SELECT 1 FROM session_servers ss WHERE ss.session_pk = s.id)
ORDER BY s.id;

-- name: ForgetEndedSessionServers :many
-- Deletes the enabled servers of the session while it is still ended longer
-- ago than the resume window, and returns their names.
DELETE FROM session_servers ss
USING sessions s
WHERE ss.session_pk = @id AND s.id = ss.session_pk
  AND s.ended_at IS NOT NULL AND s.ended_at < now() - @resume_window::interval
RETURNING ss.template_name;
