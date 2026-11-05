-- name: UpsertSession :one
-- Creates the session or, for an existing one, writes this request's
-- identity snapshot and moves last_seen_at forward. It does not restart an
-- ended session (RestartSession does).
INSERT INTO sessions (principal, session_id, workspace, attestation, email, name, groups)
VALUES (@principal, @session_id, @workspace, @attestation, @email, @name, @groups)
ON CONFLICT (principal, session_id) DO UPDATE
    SET email = EXCLUDED.email,
        name = EXCLUDED.name,
        groups = EXCLUDED.groups,
        last_seen_at = now()
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
SET ended_at = NULL, workspace = @workspace, attestation = @attestation, last_seen_at = now()
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
