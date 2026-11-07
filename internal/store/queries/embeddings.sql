-- name: RecordEmbeddingsDemand :exec
-- Moves the last embeddings demand time forward to @at, never back.
UPDATE embeddings_server SET last_demand_at = @at::timestamptz WHERE last_demand_at < @at::timestamptz;

-- name: GetEmbeddingsServer :one
SELECT last_demand_at, state FROM embeddings_server;

-- name: SetEmbeddingsServerState :exec
UPDATE embeddings_server SET state = @state WHERE state <> @state;

-- name: LiveToolSearchSessionExists :one
SELECT EXISTS (SELECT 1 FROM sessions WHERE ended_at IS NULL AND tool_search)::boolean;
