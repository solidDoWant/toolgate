-- +goose Up

-- mcp_revision is the MCP protocol revision the reconciler negotiated with
-- the instance. An empty value means the row was made ready by an older
-- leader.
ALTER TABLE instances ADD COLUMN mcp_revision text NOT NULL DEFAULT '';

-- tool_catalog is the upstream tool list of a template generation, as an
-- instance served it, before curation. instance_id names the instance it was
-- learned from; that instance may be gone.
CREATE TABLE tool_catalog (
    template_uid  text        NOT NULL,
    generation    bigint      NOT NULL,
    template_ns   text        NOT NULL,
    template_name text        NOT NULL,
    tools         jsonb       NOT NULL CHECK (octet_length(tools::text) <= 1048576),
    instance_id   text        NOT NULL,
    learned_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (template_uid, generation)
);

-- tool_search holds one searchable row for each tool of a template.
CREATE TABLE tool_search (
    template_uid      text        NOT NULL,
    template_ns       text        NOT NULL,
    template_name     text        NOT NULL,
    source_generation bigint      NOT NULL,
    upstream_name     text        NOT NULL,
    exposed_name      text        NOT NULL,
    document          tsvector    NOT NULL,
    rebuilt_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (template_uid, upstream_name)
);

CREATE INDEX tool_search_document_idx ON tool_search USING GIN (document);

-- session_found_tools are the tools a session found by search.
CREATE TABLE session_found_tools (
    session_pk    bigint      NOT NULL REFERENCES sessions (id),
    template_ns   text        NOT NULL,
    template_name text        NOT NULL,
    upstream_name text        NOT NULL,
    found_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_pk, template_ns, template_name, upstream_name)
);

-- scope_entries are tool-level and session-level allow and deny entries of a
-- principal. tool is an upstream tool name, or empty for the whole server.
CREATE TABLE scope_entries (
    id         bigserial   PRIMARY KEY,
    scope_id   bigint      NOT NULL REFERENCES user_scopes (id),
    workspace  text        CHECK (char_length(workspace) <= 256 AND workspace NOT LIKE 'claimed/%'),
    session_pk bigint      REFERENCES sessions (id),
    server     text        NOT NULL,
    tool       text        NOT NULL DEFAULT '',
    effect     text        NOT NULL CHECK (effect IN ('allow', 'deny')),
    created_at timestamptz NOT NULL DEFAULT now(),
    request_id text,
    CHECK ((workspace IS NOT NULL) <> (session_pk IS NOT NULL))
);

CREATE UNIQUE INDEX scope_entries_unique_idx
    ON scope_entries (scope_id, coalesce(workspace, ''), coalesce(session_pk, 0), server, tool, effect);

CREATE INDEX scope_entries_session_idx ON scope_entries (session_pk) WHERE session_pk IS NOT NULL;

-- unverified_servers is the allow list that user_scopes.unverified_list
-- turns on for sessions without a verified workspace.
CREATE TABLE unverified_servers (
    scope_id bigint      NOT NULL REFERENCES user_scopes (id),
    server   text        NOT NULL,
    added_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope_id, server)
);

-- The default "list" is stored as unverified_default = 'none' with
-- unverified_list = true, so an older replica still reads a value it knows.
ALTER TABLE user_scopes ADD COLUMN unverified_list boolean NOT NULL DEFAULT false;

-- access_requests gains the kind scope. The kind check and the shape check
-- have generated names; find them by what they test.
-- +goose StatementBegin
DO $$
DECLARE
    found record;
BEGIN
    FOR found IN
        SELECT conname FROM pg_constraint
        WHERE conrelid = 'access_requests'::regclass
          AND contype = 'c'
          AND pg_get_constraintdef(oid) ~ '\mkind\M'
    LOOP
        EXECUTE format('ALTER TABLE access_requests DROP CONSTRAINT %I', found.conname);
    END LOOP;
END
$$;
-- +goose StatementEnd

ALTER TABLE access_requests ADD CONSTRAINT access_requests_kind_check
    CHECK (kind IN ('server', 'change', 'scope'));
ALTER TABLE access_requests ADD CONSTRAINT access_requests_shape_check
    CHECK ((kind = 'server' AND session_pk IS NOT NULL AND server IS NOT NULL)
        OR (kind = 'change' AND change IS NOT NULL)
        OR (kind = 'scope' AND change IS NOT NULL));
