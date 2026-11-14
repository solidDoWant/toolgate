-- +goose Up

CREATE TABLE tool_result_cache (
    key          bytea       PRIMARY KEY CHECK (octet_length(key) = 32),
    template_uid text        NOT NULL,
    generation   bigint      NOT NULL,
    tool         text        NOT NULL,
    scope        text        NOT NULL CHECK (scope IN ('shared', 'per-user', 'per-session')),
    scope_id     text        NOT NULL,
    session_pk   bigint,
    result       bytea       NOT NULL,
    size_bytes   integer     NOT NULL,
    stored_at    timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    CHECK ((scope = 'per-session') = (session_pk IS NOT NULL))
);

CREATE INDEX tool_result_cache_expires_idx ON tool_result_cache (expires_at);
CREATE INDEX tool_result_cache_template_idx ON tool_result_cache (template_uid, generation) INCLUDE (size_bytes, expires_at);
CREATE INDEX tool_result_cache_session_idx ON tool_result_cache (session_pk) WHERE session_pk IS NOT NULL;

-- +goose Down
DROP TABLE tool_result_cache;
