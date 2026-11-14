-- name: GetWorkspaceKeyByKeyID :one
-- Returns the active row of a key ID, or else the newest ended row.
SELECT * FROM workspace_keys
WHERE key_id = @key_id
ORDER BY (status = 'active') DESC, ended_at DESC NULLS FIRST
LIMIT 1;

-- name: HasPendingKeyRequest :one
-- Reports whether a principal has a live key request for a key ID.
SELECT EXISTS (
    SELECT 1 FROM access_requests
    WHERE principal = @principal AND kind = 'key' AND status = 'pending'
      AND expires_at > now() AND change->>'keyID' = @key_id::text
);

-- name: TouchWorkspaceKey :execrows
-- Records a use of an active key, at most once a minute.
UPDATE workspace_keys SET last_used_at = now()
WHERE id = @id AND status = 'active'
  AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: ListActiveWorkspaceKeysOfPrincipal :many
SELECT * FROM workspace_keys
WHERE principal = @principal AND status = 'active'
ORDER BY name;

-- name: ListPendingKeyRequestsOfPrincipal :many
-- Lists a principal's live key requests, for the key list.
SELECT * FROM access_requests
WHERE principal = @principal AND kind = 'key' AND status = 'pending' AND expires_at > now()
ORDER BY change->>'name', created_at, id
LIMIT 100;

-- name: CountActiveWorkspaceKeysOfPrincipal :one
SELECT count(*) FROM workspace_keys WHERE principal = @principal AND status = 'active';

-- name: GetActiveWorkspaceKeyByName :one
SELECT * FROM workspace_keys WHERE principal = @principal AND name = @name AND status = 'active';

-- name: GetActiveWorkspaceKeyByKeyID :one
SELECT * FROM workspace_keys WHERE key_id = @key_id AND status = 'active';

-- name: GetPendingKeyRequestOfPrincipal :one
-- Returns a live key request of a principal with the same name and key ID.
SELECT * FROM access_requests
WHERE principal = @principal AND kind = 'key' AND status = 'pending' AND expires_at > now()
  AND change->>'name' = @name::text AND change->>'keyID' = @key_id::text
ORDER BY created_at, id
LIMIT 1;

-- name: InsertKeyRequestWithinLimit :one
-- Inserts a pending key request unless the principal already has
-- pending_limit pending access requests; then it returns no row. Run it
-- after LockPrincipalAccessRequests in the same transaction.
INSERT INTO access_requests (id, principal, kind, workspace, verified, change, expires_at)
SELECT @id, @principal, 'key', @workspace, @verified, @change, now() + @approval_timeout::interval
WHERE (
    SELECT count(*) FROM access_requests WHERE principal = @principal AND status = 'pending'
) < @pending_limit::bigint
RETURNING *;

-- name: InsertWorkspaceKey :one
-- Inserts an active key; approved_at is now.
INSERT INTO workspace_keys (principal, username, name, key_id, public_key, status, request_id, registered_at, approved_at)
VALUES (@principal, @username, @name, @key_id, @public_key, 'active', @request_id, @registered_at, now())
RETURNING *;

-- name: ReplaceWorkspaceKey :one
-- Ends the active key of a principal and name as replaced. It returns no row
-- when there is none.
UPDATE workspace_keys SET status = 'replaced', ended_at = now()
WHERE principal = @principal AND name = @name AND status = 'active'
RETURNING *;

-- name: RevokeWorkspaceKey :one
-- Ends the active key of a principal and name as revoked. It returns no row
-- when there is none.
UPDATE workspace_keys SET status = 'revoked', ended_at = now()
WHERE principal = @principal AND name = @name AND status = 'active'
RETURNING *;

-- name: DenyPendingKeyRequestsByName :many
-- Denies every pending key request of a principal for a key name, and
-- returns them.
UPDATE access_requests
SET status = 'denied', decided_at = now(), decided_by = @principal
WHERE principal = @principal AND kind = 'key' AND status = 'pending' AND change->>'name' = @name::text
RETURNING *;
