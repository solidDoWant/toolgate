-- name: GetToolResultCache :one
-- Every key column is checked again, so a bug in the key hash cannot give a
-- wrong hit. The age comes from the database's clock.
SELECT result, (now() - stored_at)::interval AS age FROM tool_result_cache
WHERE key = @key AND template_uid = @template_uid AND generation = @generation
  AND tool = @tool AND scope_id = @scope_id AND expires_at > now();

-- name: LockLiveSessionForResultCache :one
-- Step one of a per-session store: the session must be live. FOR KEY SHARE
-- waits for an end transaction that holds the row and does not conflict with
-- last_seen_at touches.
SELECT 1::int AS found FROM sessions WHERE id = @id AND ended_at IS NULL FOR KEY SHARE;

-- name: LockToolResultCacheTemplate :exec
-- Serializes the stores of one template, so the size check is exact.
SELECT pg_advisory_xact_lock(hashtextextended('toolresultcache/' || @template_uid::text, 0));

-- name: SumToolResultCacheTemplateBytes :one
SELECT coalesce(sum(size_bytes), 0)::bigint AS total FROM tool_result_cache
WHERE template_uid = @template_uid AND expires_at > now() AND key <> @key;

-- name: UpsertToolResultCache :exec
INSERT INTO tool_result_cache (key, template_uid, generation, tool, scope, scope_id, session_pk, result, size_bytes, stored_at, expires_at)
VALUES (@key, @template_uid, @generation, @tool, @scope, @scope_id, @session_pk, @result, @size_bytes, now(), now() + @ttl::interval)
ON CONFLICT (key) DO UPDATE SET
    result = EXCLUDED.result, size_bytes = EXCLUDED.size_bytes,
    stored_at = now(), expires_at = EXCLUDED.expires_at;

-- name: DeleteExpiredToolResultCache :execrows
DELETE FROM tool_result_cache WHERE key IN (
    SELECT key FROM tool_result_cache WHERE expires_at <= now() LIMIT 1000);

-- name: ListToolResultCacheTemplates :many
SELECT DISTINCT template_uid, generation FROM tool_result_cache;

-- name: DeleteToolResultCacheTemplate :execrows
-- Deletes up to 1000 entries of a template UID whose generation is not keep.
-- keep 0 matches no generation, so every entry goes.
DELETE FROM tool_result_cache WHERE key IN (
    SELECT inner_cache.key FROM tool_result_cache AS inner_cache
    WHERE inner_cache.template_uid = @template_uid AND inner_cache.generation <> @keep LIMIT 1000);

-- name: DeleteEndedSessionToolResultCache :execrows
DELETE FROM tool_result_cache WHERE key IN (
    SELECT key FROM tool_result_cache
    WHERE session_pk IN (SELECT id FROM sessions WHERE ended_at IS NOT NULL) LIMIT 1000);

-- name: DeleteSessionToolResultCache :exec
DELETE FROM tool_result_cache WHERE session_pk = @id;
