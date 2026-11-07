-- +goose Up
-- mcp_sessions holds one row for each MCP session, so every gateway replica
-- can serve it. id is the Mcp-Session-Id. principal, session_id, workspace
-- and attestation are the binding: the verified identity at initialize. A
-- session_id starting with "mcp." is a connector session, whose toolgate
-- session ends with it. ended_at is set when the MCP session ends; later
-- requests for it get 404.
CREATE TABLE mcp_sessions (
    id                 text        PRIMARY KEY,
    session_pk         bigint      NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    principal          text        NOT NULL,
    session_id         text        NOT NULL,
    workspace          text        NOT NULL,
    attestation        text        NOT NULL CHECK (attestation IN ('attested', 'user')),
    protocol_version   text        NOT NULL,
    initialize_params  jsonb       NOT NULL,
    initialized_params jsonb,
    created_at         timestamptz NOT NULL DEFAULT now(),
    last_seen_at       timestamptz NOT NULL DEFAULT now(),
    ended_at           timestamptz
);

CREATE INDEX mcp_sessions_session_pk_idx ON mcp_sessions (session_pk);

-- The leader's idle pass reads only live rows.
CREATE INDEX mcp_sessions_live_last_seen_idx ON mcp_sessions (last_seen_at) WHERE ended_at IS NULL;

