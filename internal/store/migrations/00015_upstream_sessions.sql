-- +goose Up
-- upstream_sessions holds the MCP session that a server made for one
-- (instance, toolgate session) pair. A row is live while ended_at is null.
CREATE TABLE upstream_sessions (
    id                  bigserial   PRIMARY KEY,
    instance_id         text        NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    session_pk          bigint      NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    upstream_session_id text        NOT NULL CHECK (char_length(upstream_session_id) BETWEEN 1 AND 1024),
    initialize_result   jsonb       NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    ended_at            timestamptz
);

-- At most one live upstream session per pair.
CREATE UNIQUE INDEX upstream_sessions_live_idx ON upstream_sessions (instance_id, session_pk)
    WHERE ended_at IS NULL;

-- The disable and session-end transactions end a session's live rows.
CREATE INDEX upstream_sessions_session_live_idx ON upstream_sessions (session_pk)
    WHERE ended_at IS NULL;

-- The sweep reads ended rows, oldest first.
CREATE INDEX upstream_sessions_ended_idx ON upstream_sessions (ended_at)
    WHERE ended_at IS NOT NULL;
