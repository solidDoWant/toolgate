-- name: UpsertToolCatalog :exec
INSERT INTO tool_catalog (template_uid, generation, template_ns, template_name, tools, instance_id)
VALUES (@template_uid, @generation, @template_ns, @template_name, @tools, @instance_id)
ON CONFLICT (template_uid, generation) DO UPDATE
SET template_ns = EXCLUDED.template_ns, template_name = EXCLUDED.template_name,
    tools = EXCLUDED.tools, instance_id = EXCLUDED.instance_id, learned_at = now();

-- name: GetToolCatalog :one
SELECT * FROM tool_catalog WHERE template_uid = @template_uid AND generation = @generation;

-- name: ListToolCatalogForTemplate :many
SELECT * FROM tool_catalog WHERE template_uid = @template_uid ORDER BY generation DESC;

-- name: DeleteToolCatalogBefore :exec
DELETE FROM tool_catalog WHERE template_uid = @template_uid AND generation < @generation;

-- name: UpsertToolSearch :exec
INSERT INTO tool_search (template_uid, template_ns, template_name, source_generation, upstream_name, exposed_name, document)
VALUES (@template_uid, @template_ns, @template_name, @source_generation, @upstream_name, @exposed_name, to_tsvector('simple', @document::text))
ON CONFLICT (template_uid, upstream_name) DO UPDATE
SET template_ns = EXCLUDED.template_ns, template_name = EXCLUDED.template_name,
    source_generation = EXCLUDED.source_generation, exposed_name = EXCLUDED.exposed_name,
    document = EXCLUDED.document, rebuilt_at = now();

-- name: ListToolSearchForTemplate :many
SELECT template_uid, template_ns, template_name, source_generation, upstream_name, exposed_name, rebuilt_at
FROM tool_search WHERE template_uid = @template_uid ORDER BY upstream_name;

-- name: DeleteToolSearchForTemplate :exec
DELETE FROM tool_search WHERE template_uid = @template_uid;

-- name: DeleteToolSearchTool :exec
DELETE FROM tool_search WHERE template_uid = @template_uid AND upstream_name = @upstream_name;

-- name: AddSessionFoundTool :exec
INSERT INTO session_found_tools (session_pk, template_ns, template_name, upstream_name)
VALUES (@session_pk, @template_ns, @template_name, @upstream_name)
ON CONFLICT DO NOTHING;

-- name: ListSessionFoundTools :many
SELECT * FROM session_found_tools WHERE session_pk = @session_pk
ORDER BY template_ns, template_name, upstream_name;

-- name: CountSessionFoundTools :one
SELECT count(*) FROM session_found_tools WHERE session_pk = @session_pk;

-- name: DeleteSessionFoundTools :exec
DELETE FROM session_found_tools WHERE session_pk = @session_pk;

-- name: InsertScopeEntry :one
-- Inserts an entry unless the same one exists; returns the new row, or no row.
INSERT INTO scope_entries (scope_id, workspace, session_pk, server, tool, effect, request_id)
VALUES (@scope_id, sqlc.narg(workspace), sqlc.narg(session_pk), @server, @tool, @effect, sqlc.narg(request_id))
ON CONFLICT DO NOTHING
RETURNING *;

-- name: ListScopeEntries :many
SELECT * FROM scope_entries WHERE scope_id = @scope_id ORDER BY id;

-- name: ListScopeEntriesForSession :many
SELECT * FROM scope_entries WHERE session_pk = @session_pk ORDER BY id;

-- name: CountAllowScopeEntries :one
SELECT count(*) FROM scope_entries WHERE scope_id = @scope_id AND effect = 'allow';

-- name: DeleteScopeEntry :exec
DELETE FROM scope_entries WHERE id = @id;

-- name: DeleteSessionScopeEntries :exec
-- Deletes a session's entries of one effect.
DELETE FROM scope_entries WHERE session_pk = @session_pk AND effect = @effect;

-- name: AddUnverifiedServers :many
-- Adds the servers that aren't on the list yet and returns them.
INSERT INTO unverified_servers (scope_id, server)
SELECT @scope_id, unnest(@servers::text[])
ON CONFLICT DO NOTHING
RETURNING server;

-- name: ListUnverifiedServers :many
SELECT server FROM unverified_servers WHERE scope_id = @scope_id ORDER BY server;

-- name: DeleteUnverifiedServers :exec
DELETE FROM unverified_servers WHERE scope_id = @scope_id AND server = ANY(@servers::text[]);

-- name: SetUserScopeUnverifiedList :exec
UPDATE user_scopes SET unverified_list = @unverified_list, updated_at = now() WHERE id = @id;
