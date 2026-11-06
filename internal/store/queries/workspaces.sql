-- name: LockUserScope :one
-- Locks the principal's user_scopes row for a scoping write. Run it after
-- EnsureUserScope in the same transaction, so the row exists.
SELECT * FROM user_scopes WHERE principal = @principal FOR UPDATE;

-- name: SetUserScopeDefaults :exec
UPDATE user_scopes
SET verified_default = @verified_default, unverified_default = @unverified_default, updated_at = now()
WHERE id = @id;

-- name: TouchUserScope :exec
UPDATE user_scopes SET updated_at = now() WHERE id = @id;

-- name: GetWorkspaceList :one
SELECT * FROM workspace_lists WHERE scope_id = @scope_id AND workspace = @workspace;

-- name: CountWorkspaceLists :one
SELECT count(*) FROM workspace_lists WHERE scope_id = @scope_id;

-- name: InsertWorkspaceList :one
INSERT INTO workspace_lists (scope_id, workspace) VALUES (@scope_id, @workspace)
RETURNING *;

-- name: TouchWorkspaceList :exec
UPDATE workspace_lists SET updated_at = now() WHERE id = @id;

-- name: DeleteWorkspaceList :exec
-- Deletes a list; its servers go with it.
DELETE FROM workspace_lists WHERE id = @id;

-- name: ListWorkspaceListServerNames :many
SELECT server FROM workspace_list_servers WHERE list_id = @list_id ORDER BY server;

-- name: AddWorkspaceListServers :many
-- Adds the servers that aren't on the list yet and returns them.
INSERT INTO workspace_list_servers (list_id, server)
SELECT @list_id, unnest(@servers::text[])
ON CONFLICT DO NOTHING
RETURNING server;

-- name: RemoveWorkspaceListServers :many
-- Removes the servers that are on the list and returns them.
DELETE FROM workspace_list_servers
WHERE list_id = @list_id AND server = ANY(@servers::text[])
RETURNING server;

-- name: RemoveWorkspaceSessionAdditions :exec
-- Removes the servers' session additions of the principal's open sessions
-- in the workspace.
DELETE FROM session_additions sa
USING sessions s
WHERE s.id = sa.session_pk AND s.principal = @principal AND s.workspace = @workspace
  AND s.ended_at IS NULL AND sa.server = ANY(@servers::text[]);

-- name: LockOpenSessionOfPrincipal :one
-- Locks the principal's open session with the client session ID.
SELECT id, workspace, attestation FROM sessions
WHERE principal = @principal AND session_id = @session_id AND ended_at IS NULL
FOR UPDATE;

-- name: RemoveSessionAdditions :many
-- Removes the servers' additions of one session and returns them.
DELETE FROM session_additions
WHERE session_pk = @session_pk AND server = ANY(@servers::text[])
RETURNING server;

-- name: ListWorkspaceListsOfPrincipal :many
-- The principal's lists with their servers, one row per server, or one row
-- with a NULL server for an empty list, ordered by workspace.
SELECT wl.workspace, wl.updated_at, wls.server
FROM user_scopes us
JOIN workspace_lists wl ON wl.scope_id = us.id
LEFT JOIN workspace_list_servers wls ON wls.list_id = wl.id
WHERE us.principal = @principal
ORDER BY wl.workspace, wls.server;

-- name: LockPrincipalAccessRequests :exec
-- Takes the transaction advisory lock that serializes access-request
-- inserts for one principal. It is separate from the approvals lock.
SELECT pg_advisory_xact_lock(hashtextextended('toolgate-access-request/' || @principal::text, 0));

-- name: InsertChangeRequestWithinLimit :one
-- Inserts a pending change request unless the principal already has
-- pending_limit pending access requests; then it returns no row. Run it
-- after LockPrincipalAccessRequests in the same transaction.
INSERT INTO access_requests (id, principal, kind, workspace, verified, change, expires_at)
SELECT @id, @principal, 'change', @workspace, @verified, @change, now() + @approval_timeout::interval
WHERE (
    SELECT count(*) FROM access_requests WHERE principal = @principal AND status = 'pending'
) < @pending_limit::bigint
RETURNING *;

-- name: GetAccessRequestOfPrincipal :one
SELECT * FROM access_requests WHERE id = @id AND principal = @principal;

-- name: ApproveAccessRequest :one
-- Approves a pending access request that hasn't expired. It returns no row
-- otherwise.
UPDATE access_requests
SET status = 'approved', decided_at = now(), decided_by = @decided_by, decided_scope = sqlc.narg(decided_scope)
WHERE id = @id AND principal = @principal AND status = 'pending' AND expires_at > now()
RETURNING *;

-- name: DenyAccessRequest :one
-- Denies a pending access request that hasn't expired. It returns no row
-- otherwise.
UPDATE access_requests
SET status = 'denied', decided_at = now(), decided_by = @decided_by
WHERE id = @id AND principal = @principal AND status = 'pending' AND expires_at > now()
RETURNING *;

-- name: ListOverdueAccessRequests :many
-- Lists pending access requests whose expires_at is more than grace past,
-- for the leader's pass.
SELECT ar.id, ar.principal, ar.kind, ar.workspace, ar.verified, ar.server, s.session_id
FROM access_requests ar
LEFT JOIN sessions s ON s.id = ar.session_pk
WHERE ar.status = 'pending' AND ar.expires_at < now() - @grace::interval
ORDER BY ar.expires_at
LIMIT 1000;

-- name: ExpireAccessRequest :one
-- Marks a pending access request expired. It returns no row when it isn't
-- pending any more.
UPDATE access_requests SET status = 'expired'
WHERE id = @id AND status = 'pending'
RETURNING *;
