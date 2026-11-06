-- name: UpsertToolCatalog :exec
-- A list whose stored form is over 1 MiB is not learned.
INSERT INTO tool_catalog (template_uid, generation, template_ns, template_name, tools, instance_id)
SELECT @template_uid::text, @generation::bigint, @template_ns::text, @template_name::text, @tools::jsonb, @instance_id::text
WHERE octet_length((@tools::jsonb)::text) <= 1048576
ON CONFLICT (template_uid, generation) DO UPDATE
SET template_ns = EXCLUDED.template_ns, template_name = EXCLUDED.template_name,
    tools = EXCLUDED.tools, instance_id = EXCLUDED.instance_id, learned_at = now();

-- name: GetToolCatalog :one
SELECT * FROM tool_catalog WHERE template_uid = @template_uid AND generation = @generation;

-- name: ListToolCatalogForTemplate :many
SELECT * FROM tool_catalog WHERE template_uid = @template_uid ORDER BY generation DESC;

-- name: DeleteToolCatalogBefore :exec
DELETE FROM tool_catalog WHERE template_uid = @template_uid AND generation < @generation;

-- name: InsertToolSearch :exec
-- The document has four weighted parts (docs/design/phase-4.md §5).
INSERT INTO tool_search (template_uid, template_ns, template_name, source_generation, upstream_name, exposed_name, document)
VALUES (@template_uid, @template_ns, @template_name, @source_generation, @upstream_name, @exposed_name,
    setweight(to_tsvector('english', @name_part::text), 'A')
    || setweight(to_tsvector('english', @description_part::text), 'B')
    || setweight(to_tsvector('english', @server_part::text), 'C')
    || setweight(to_tsvector('english', @input_part::text), 'D'));

-- name: LockToolSearch :exec
SELECT pg_advisory_xact_lock(hashtextextended('toolsearch/' || @template_uid::text, 0));

-- name: ListToolSearchUIDs :many
SELECT DISTINCT template_uid FROM tool_search;

-- name: DeleteToolSearchOfTemplates :execrows
DELETE FROM tool_search WHERE template_uid = ANY(@template_uids::text[]);

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

-- name: ListToolCatalogUIDs :many
SELECT DISTINCT template_uid FROM tool_catalog;

-- name: DeleteToolCatalogOfTemplates :execrows
DELETE FROM tool_catalog WHERE template_uid = ANY(@template_uids::text[]);

-- name: DeleteToolCatalogOlderThanNewestTwo :execrows
DELETE FROM tool_catalog c
WHERE c.generation < (
    SELECT n.generation FROM tool_catalog n
    WHERE n.template_uid = c.template_uid
    ORDER BY n.generation DESC OFFSET 1 LIMIT 1
);

-- name: CountPredecessorInstances :one
-- Counts the rows of the same template and scope key, other than id, that
-- are not yet stopped or failed: an instance being replaced or stopped.
SELECT count(*) FROM instances
WHERE template_ns = @template_ns AND template_name = @template_name AND scope_key = @scope_key
  AND id <> @id AND status NOT IN ('stopped', 'failed');

-- name: DeleteTargetScopeEntries :many
-- Deletes the entries of one effect for a server of one target: a workspace
-- of the scope, or a session. With any_tool every entry of the server goes,
-- otherwise only the one for tool. Returns the deleted rows.
DELETE FROM scope_entries
WHERE scope_id = @scope_id
  AND workspace IS NOT DISTINCT FROM sqlc.narg(workspace)
  AND session_pk IS NOT DISTINCT FROM sqlc.narg(session_pk)
  AND server = @server AND effect = @effect
  AND (@any_tool::bool OR tool = @tool)
RETURNING *;

-- name: DeleteWorkspaceScopeEntriesOfEffect :exec
DELETE FROM scope_entries WHERE scope_id = @scope_id AND workspace = @workspace AND effect = @effect;

-- name: ListScopeEntriesOfSessions :many
SELECT * FROM scope_entries WHERE session_pk = ANY(@session_pks::bigint[]) ORDER BY id;

-- name: DeleteForgottenSessionDenyEntries :execrows
-- Deletes the deny entries of the session while it is still ended longer
-- ago than the resume window.
DELETE FROM scope_entries e
USING sessions s
WHERE s.id = @id AND e.session_pk = s.id AND e.effect = 'deny'
  AND s.ended_at IS NOT NULL AND s.ended_at < now() - @resume_window::interval;

-- name: DeleteEndedSessionAllowEntries :execrows
DELETE FROM scope_entries e
USING sessions s
WHERE s.id = e.session_pk AND e.effect = 'allow' AND s.ended_at IS NOT NULL;

-- name: DeleteEndedSessionFoundTools :execrows
DELETE FROM session_found_tools f
USING sessions s
WHERE s.id = f.session_pk AND s.ended_at IS NOT NULL;
