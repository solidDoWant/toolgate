-- name: InsertUISession :one
INSERT INTO ui_sessions (id_hash, principal, email, name, groups, csrf_token, auth_time)
VALUES (@id_hash, @principal, @email, @name, @groups, @csrf_token, sqlc.narg(auth_time))
RETURNING *;

-- name: GetValidUISession :one
-- Returns the web UI session while it is valid.
SELECT * FROM ui_sessions
WHERE id_hash = @id_hash AND expires_at > now() AND last_seen_at > now() - interval '1 hour';

-- name: TouchUISession :exec
-- Moves last_seen_at forward, at most once a minute.
UPDATE ui_sessions SET last_seen_at = now()
WHERE id_hash = @id_hash AND last_seen_at <= now() - interval '1 minute';

-- name: DeleteUISession :exec
DELETE FROM ui_sessions WHERE id_hash = @id_hash;

-- name: InsertUILogin :exec
INSERT INTO ui_logins (state_hash, nonce, pkce_verifier, return_to, step_up, ui_session_hash)
VALUES (@state_hash, @nonce, @pkce_verifier, @return_to, @step_up, sqlc.narg(ui_session_hash));

-- name: TakeUILogin :one
-- Takes the sign-in in progress for the state, so each state works once.
-- It returns no row when the state is unknown, already taken or older than
-- 10 minutes.
DELETE FROM ui_logins
WHERE state_hash = @state_hash AND created_at > now() - interval '10 minutes'
RETURNING *;

-- name: CountUILogins :one
-- Counts the sign-ins in progress, to refuse new ones over the limit.
SELECT count(*) FROM ui_logins;

-- name: UpdateUISessionSignIn :execrows
-- Records a step-up sign-in on a valid web UI session of the same
-- principal: the new auth_time and identity snapshot.
UPDATE ui_sessions
SET email = @email, name = @name, groups = @groups, auth_time = @auth_time
WHERE id_hash = @id_hash AND principal = @principal
  AND expires_at > now() AND last_seen_at > now() - interval '1 hour';

-- name: ListRecentDecisions :many
-- Lists the newest tool.decision events of one session.
SELECT id, occurred_at, principal, workspace, session, attestation, server, tool, details
FROM audit_events
WHERE type = 'tool.decision' AND principal = @principal AND session = @session
  AND occurred_at >= @since::timestamptz
ORDER BY occurred_at DESC, id DESC
LIMIT @page_size;

-- name: ListSessionsForUI :many
-- Lists sessions for the Sessions page, newest activity first: open ones and
-- those that ended after @ended_after (the start of the resume window). An
-- empty principal or workspace matches every one. @only_open leaves ended
-- sessions out, and @seen_after leaves out sessions with no request since.
-- The client is the clientInfo of the session's newest MCP session.
SELECT s.*,
       COALESCE(m.initialize_params -> 'clientInfo' ->> 'name', '')::text AS client_name,
       COALESCE(m.initialize_params -> 'clientInfo' ->> 'version', '')::text AS client_version
FROM sessions s
LEFT JOIN LATERAL (
    SELECT initialize_params FROM mcp_sessions
    WHERE session_pk = s.id
    ORDER BY created_at DESC
    LIMIT 1
) m ON true
WHERE (s.ended_at IS NULL OR (NOT @only_open::boolean AND s.ended_at > @ended_after::timestamptz))
  AND (@principal::text = '' OR s.principal = @principal)
  AND (@workspace::text = '' OR s.workspace = @workspace)
  AND s.last_seen_at > @seen_after::timestamptz
ORDER BY s.last_seen_at DESC, s.id DESC
LIMIT @page_size;

-- name: ListSessionWorkspaces :many
-- Lists the workspaces of the sessions the Sessions page can show, for its
-- workspace filter. An empty principal matches every one.
SELECT DISTINCT workspace FROM sessions
WHERE (ended_at IS NULL OR ended_at > @ended_after::timestamptz)
  AND (@principal::text = '' OR principal = @principal)
ORDER BY workspace
LIMIT 200;

-- name: ListSessionPrincipals :many
-- Lists the principals of the sessions the Sessions page can show, for the
-- admin's user filter.
SELECT DISTINCT principal FROM sessions
WHERE ended_at IS NULL OR ended_at > @ended_after::timestamptz
ORDER BY principal
LIMIT 200;

-- name: ListServersForSessions :many
-- Lists the enabled servers of the given sessions, for the Sessions page.
SELECT session_pk, template_ns, template_name FROM session_servers
WHERE session_pk = ANY(@session_pks::bigint[])
ORDER BY session_pk, template_ns, template_name;

-- name: CountSessionActivity :many
-- Counts, for each of the given sessions: the ready instances of its enabled
-- servers that it uses (shared ones, its owner's per-user ones and its own
-- per-session ones), the tool decisions and refusals recorded for it since
-- @since, and its unexpired pending approvals. Audit events only count for
-- the session's own principal.
SELECT s.id AS session_pk,
       (SELECT count(*) FROM session_servers ss
        JOIN instances i ON i.template_ns = ss.template_ns AND i.template_name = ss.template_name
        WHERE ss.session_pk = s.id AND i.desired = 'running' AND i.status = 'ready'
          AND (i.scope = 'shared'
               OR (i.scope = 'per-user' AND i.owner_principal = s.principal)
               OR (i.scope = 'per-session' AND i.scope_key = 'session:' || s.principal || ':' || s.session_id)))::bigint AS running_instances,
       (SELECT count(*) FROM audit_events a
        WHERE a.type = 'tool.decision' AND a.principal = s.principal AND a.session = s.session_id
          AND a.occurred_at >= @since::timestamptz)::bigint AS tool_calls,
       (SELECT count(*) FROM audit_events a
        WHERE a.type = 'tool.decision' AND a.principal = s.principal AND a.session = s.session_id
          AND a.occurred_at >= @since::timestamptz
          AND a.details ->> 'decision' IN ('deny', 'refuse'))::bigint AS refusals,
       (SELECT count(*) FROM approvals p
        WHERE p.session_pk = s.id AND p.principal = s.principal AND p.status = 'pending'
          AND p.expires_at > now())::bigint AS pending_approvals
FROM sessions s
WHERE s.id = ANY(@session_pks::bigint[]);
