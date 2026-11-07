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
-- The document has weighted parts; name_text
-- feeds the trigram term of the ranking.
INSERT INTO tool_search (template_uid, template_ns, template_name, source_generation, upstream_name, exposed_name, name_text, embedding, embedding_model, document)
VALUES (@template_uid, @template_ns, @template_name, @source_generation, @upstream_name, @exposed_name, @name_part::text, CAST(NULLIF(@embedding::text, '') AS vector), @embedding_model::text,
    setweight(to_tsvector('english', @name_part::text), 'A')
    || setweight(to_tsvector('english', @description_part::text), 'B')
    || setweight(to_tsvector('english', @server_part::text), 'C')
    || setweight(to_tsvector('english', @input_part::text), 'C'));

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

-- name: CountUnverifiedServers :one
SELECT count(*) FROM unverified_servers WHERE scope_id = @scope_id;

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

-- name: ListSessionAdditionsOfSessions :many
-- The session additions of the given sessions.
SELECT session_pk, server FROM session_additions
WHERE session_pk = ANY(@session_pks::bigint[])
ORDER BY session_pk, server;

-- name: SearchTools :many
-- Ranks the search rows of the candidate templates, and the caller's own
-- documents passed in own_documents, against a tsquery text: cover density
-- over the weighted document, plus a small trigram similarity between the
-- plain query and the name words. The own documents are the tools of the
-- caller's own remote servers, which have no shared rows; they are built
-- here with the same weights as InsertToolSearch.
WITH docs AS (
    SELECT tool_search.template_uid, tool_search.template_ns, tool_search.template_name,
           tool_search.upstream_name, tool_search.exposed_name, tool_search.name_text,
           tool_search.document, tool_search.embedding, tool_search.embedding_model
    FROM tool_search
    WHERE tool_search.template_uid = ANY(@template_uids::text[])
    UNION ALL
    SELECT own.template_uid, own.template_ns, own.template_name, own.upstream_name, own.exposed_name, own.name_part,
           setweight(to_tsvector('english', own.name_part), 'A')
           || setweight(to_tsvector('english', own.description_part), 'B')
           || setweight(to_tsvector('english', own.server_part), 'C')
           || setweight(to_tsvector('english', own.input_part), 'C'),
           NULL::vector, ''::text
    FROM jsonb_to_recordset(@own_documents::jsonb)
         AS own(template_uid text, template_ns text, template_name text, upstream_name text, exposed_name text,
                name_part text, description_part text, server_part text, input_part text)
)
SELECT docs.template_uid, docs.template_ns, docs.template_name,
       docs.upstream_name, docs.exposed_name
FROM docs, to_tsquery('english', @expanded::text) AS q, to_tsquery('english', @query::text) AS q0
WHERE docs.document @@ q
ORDER BY (ts_rank_cd(docs.document, q0, 1) + 0.4 * ts_rank_cd(docs.document, q, 1) + 0.3 * word_similarity(docs.name_text, @plain::text)) DESC, docs.exposed_name
LIMIT 100;

-- name: SearchToolsHybrid :many
-- Like SearchTools, and also ranks the rows embedded with the given model by
-- cosine distance to the query vector. The two ranks are fused by reciprocal
-- rank fusion with constant 60: a row missing from a list gets nothing from
-- it. The own documents have no vector, so they rank by words only.
WITH docs AS (
    SELECT tool_search.template_uid, tool_search.template_ns, tool_search.template_name,
           tool_search.upstream_name, tool_search.exposed_name, tool_search.name_text,
           tool_search.document, tool_search.embedding, tool_search.embedding_model
    FROM tool_search
    WHERE tool_search.template_uid = ANY(@template_uids::text[])
    UNION ALL
    SELECT own.template_uid, own.template_ns, own.template_name, own.upstream_name, own.exposed_name, own.name_part,
           setweight(to_tsvector('english', own.name_part), 'A')
           || setweight(to_tsvector('english', own.description_part), 'B')
           || setweight(to_tsvector('english', own.server_part), 'C')
           || setweight(to_tsvector('english', own.input_part), 'C'),
           NULL::vector, ''::text
    FROM jsonb_to_recordset(@own_documents::jsonb)
         AS own(template_uid text, template_ns text, template_name text, upstream_name text, exposed_name text,
                name_part text, description_part text, server_part text, input_part text)
), words AS (
    SELECT docs.template_uid, docs.upstream_name,
           row_number() OVER (ORDER BY (ts_rank_cd(docs.document, q0, 1) + 0.4 * ts_rank_cd(docs.document, q, 1) + 0.3 * word_similarity(docs.name_text, @plain::text)) DESC, docs.exposed_name) AS rank
    FROM docs, to_tsquery('english', @expanded::text) AS q, to_tsquery('english', @query::text) AS q0
    WHERE docs.document @@ q
), vectors AS (
    SELECT docs.template_uid, docs.upstream_name,
           row_number() OVER (ORDER BY docs.embedding <=> CAST(@query_vector::text AS vector), docs.exposed_name) AS rank
    FROM docs
    WHERE docs.embedding_model = @model::text AND docs.embedding IS NOT NULL
      AND vector_dims(docs.embedding) = @dimensions::int
)
SELECT docs.template_uid, docs.template_ns, docs.template_name,
       docs.upstream_name, docs.exposed_name
FROM docs
LEFT JOIN words USING (template_uid, upstream_name)
LEFT JOIN vectors USING (template_uid, upstream_name)
WHERE words.rank IS NOT NULL OR vectors.rank IS NOT NULL
ORDER BY (COALESCE(1.0 / (60 + words.rank), 0) + COALESCE(1.0 / (60 + vectors.rank), 0)) DESC, docs.exposed_name
LIMIT 100;

-- name: ListToolSearchUIDsNeedingEmbedding :many
-- The templates that have a row not embedded with the given model, or whose
-- vector has a different length than the given one.
SELECT DISTINCT template_uid FROM tool_search
WHERE embedding_model <> @model::text OR embedding IS NULL
   OR (@dimensions::int > 0 AND vector_dims(embedding) <> @dimensions::int);

-- name: CountToolSearchDimensionMismatches :one
-- The rows of the templates embedded with the given model whose vector has a
-- different length than the given one.
SELECT count(*) FROM tool_search
WHERE template_uid = ANY(@template_uids::text[]) AND embedding_model = @model::text
  AND embedding IS NOT NULL AND vector_dims(embedding) <> @dimensions::int;

-- name: QueryIsEmpty :one
SELECT numnode(to_tsquery('english', @query::text)) = 0;

-- name: TouchSessionFoundTool :exec
INSERT INTO session_found_tools (session_pk, template_ns, template_name, upstream_name, found_at)
VALUES (@session_pk, @template_ns, @template_name, @upstream_name, clock_timestamp())
ON CONFLICT (session_pk, template_ns, template_name, upstream_name) DO UPDATE SET found_at = clock_timestamp();

-- name: TrimSessionFoundTools :exec
-- Keeps the newest @keep found tools of a session.
DELETE FROM session_found_tools AS t
WHERE t.session_pk = @session_pk
  AND (t.template_ns, t.template_name, t.upstream_name) NOT IN (
    SELECT f.template_ns, f.template_name, f.upstream_name FROM session_found_tools f
    WHERE f.session_pk = @session_pk
    ORDER BY f.found_at DESC, f.template_name, f.upstream_name
    LIMIT @keep::int);

-- name: DeleteSessionFoundTool :exec
DELETE FROM session_found_tools
WHERE session_pk = @session_pk AND template_ns = @template_ns AND template_name = @template_name AND upstream_name = @upstream_name;

-- name: ListFoundToolSessions :many
SELECT DISTINCT session_pk FROM session_found_tools WHERE template_ns = @template_ns AND template_name = @template_name;

-- name: SetSessionFoundToolSearch :exec
-- Marks a found tool as returned by the given search at the given rank.
UPDATE session_found_tools SET search_id = @search_id, rank = @rank
WHERE session_pk = @session_pk AND template_ns = @template_ns AND template_name = @template_name AND upstream_name = @upstream_name;

-- name: SetSessionLastSearch :exec
UPDATE sessions SET last_search_id = @search_id WHERE id = @id;

-- name: TakeOpenSearch :one
-- Deletes and returns the open search of a session, if any.
DELETE FROM session_open_searches WHERE session_pk = @session_pk
RETURNING search_id, degraded, embedding_model;

-- name: InsertOpenSearch :exec
INSERT INTO session_open_searches (session_pk, search_id, degraded, embedding_model)
VALUES (@session_pk, @search_id, @degraded, @embedding_model)
ON CONFLICT (session_pk) DO UPDATE
SET search_id = EXCLUDED.search_id, searched_at = now(), degraded = EXCLUDED.degraded, embedding_model = EXCLUDED.embedding_model;

-- name: TakeSearchHit :one
-- Deletes the session's open search when the tool belongs to it, and returns
-- the search and the rank of the tool in it. No row when the tool is not in
-- the latest search or that search already has a hit.
DELETE FROM session_open_searches AS o
WHERE o.session_pk = @session_pk AND EXISTS (
    SELECT 1 FROM session_found_tools f
    WHERE f.session_pk = o.session_pk AND f.search_id = o.search_id
      AND f.template_ns = @template_ns AND f.template_name = @template_name AND f.upstream_name = @upstream_name)
RETURNING o.search_id, o.degraded, o.embedding_model, (
    SELECT f.rank FROM session_found_tools f
    WHERE f.session_pk = o.session_pk AND f.search_id = o.search_id
      AND f.template_ns = @template_ns AND f.template_name = @template_name AND f.upstream_name = @upstream_name)::int AS rank;

-- name: TakeStaleOpenSearches :many
-- Deletes and returns the open searches of ended sessions and the ones older
-- than the given age: the searches with no follow-up call.
DELETE FROM session_open_searches AS o
USING sessions AS s
WHERE s.id = o.session_pk AND (s.ended_at IS NOT NULL OR o.searched_at < now() - @max_age::interval)
RETURNING o.search_id, o.degraded, o.embedding_model;
