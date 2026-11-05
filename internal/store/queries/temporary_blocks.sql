-- name: LockTemporaryBlockInsert :exec
-- Takes the one global transaction advisory lock that serializes temporary
-- block inserts, so two inserts can't both pass the live-block count.
SELECT pg_advisory_xact_lock(hashtextextended('toolgate-temporary-block-insert', 0));

-- name: InsertTemporaryBlockWithinLimit :one
-- Inserts the block unless live_limit blocks are already live; then it
-- returns no row. Run it after LockTemporaryBlockInsert in the same
-- transaction.
INSERT INTO temporary_blocks (id, match, reason, created_by, expires_at)
SELECT @id, @match, @reason, @created_by, now() + @lifetime::interval
WHERE (SELECT count(*) FROM temporary_blocks WHERE expires_at > now()) < @live_limit::bigint
RETURNING *;

-- name: ListLiveTemporaryBlocks :many
SELECT * FROM temporary_blocks WHERE expires_at > now() ORDER BY created_at, id;
