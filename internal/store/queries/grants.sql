-- name: LockPrincipalGrants :exec
-- Takes the transaction advisory lock that serializes grant inserts for one
-- principal, so two inserts can't both pass the live-grant count.
SELECT pg_advisory_xact_lock(hashtextextended('toolgate-grant/' || @principal::text, 0));

-- name: InsertGrantWithinLimit :one
-- Inserts the grant unless the principal already has live_limit live
-- grants; then it returns no row. Run it after LockPrincipalGrants in the
-- same transaction: as its own statement it counts the rows committed
-- before the lock was granted.
INSERT INTO grants (id, principal, template_ns, template_name, tool, session_id, expires_at)
SELECT @id, @principal, @template_ns, @template_name, @tool, sqlc.narg(session_id), now() + @lifetime::interval
WHERE (
    SELECT count(*) FROM grants
    WHERE principal = @principal AND revoked_at IS NULL AND expires_at > now()
) < @live_limit::bigint
RETURNING *;

-- name: RevokeGrant :one
-- Revokes the grant once. It returns no row when the grant doesn't exist
-- or was already revoked.
UPDATE grants
SET revoked_at = now(), revoked_by = @revoked_by
WHERE id = @id AND revoked_at IS NULL
RETURNING *;

-- name: GetGrant :one
SELECT * FROM grants WHERE id = @id;

-- name: ListLiveGrantsOfPrincipal :many
SELECT * FROM grants
WHERE principal = @principal AND revoked_at IS NULL AND expires_at > now()
ORDER BY created_at, id;

-- name: NextPolicyExpiry :one
-- Returns the milliseconds from PostgreSQL's now() to the earliest future
-- expires_at of the live grants and temporary blocks, or -1 when there is
-- none. PostgreSQL computes the delay, so replica clock skew doesn't matter.
SELECT COALESCE(ceil(EXTRACT(EPOCH FROM (min(live.expires_at) - now())) * 1000), -1)::bigint AS delay_ms
FROM (
    SELECT expires_at FROM grants WHERE revoked_at IS NULL AND expires_at > now()
    UNION ALL
    SELECT expires_at FROM temporary_blocks WHERE expires_at > now()
) AS live;
