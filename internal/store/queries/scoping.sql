-- name: GetUserScope :one
-- The principal's scoping defaults. No row means both defaults are none.
SELECT * FROM user_scopes WHERE principal = @principal;

-- name: GetWorkspaceListServers :many
-- The principal's list for the workspace, one row per server, or one row
-- with a NULL server for an empty list. No row means there is no list.
SELECT wl.id AS list_id, wls.server
FROM user_scopes us
JOIN workspace_lists wl ON wl.scope_id = us.id
LEFT JOIN workspace_list_servers wls ON wls.list_id = wl.id
WHERE us.principal = @principal AND wl.workspace = @workspace
ORDER BY wls.server;

-- name: ListSessionAdditions :many
-- The servers added to the principal's session for that session only.
SELECT sa.server
FROM session_additions sa
JOIN sessions s ON s.id = sa.session_pk
WHERE s.principal = @principal AND s.session_id = @session_id
ORDER BY sa.server;

-- name: EnsureUserScope :exec
-- Creates the principal's row with the given defaults, or leaves an existing
-- row as it is.
INSERT INTO user_scopes (principal, verified_default, unverified_default)
VALUES (@principal, @verified_default, @unverified_default)
ON CONFLICT (principal) DO NOTHING;
