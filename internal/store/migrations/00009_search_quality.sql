-- +goose Up
-- Search quality in use: each search gets an id; the found tools keep the id
-- and rank of the search that last returned them.
ALTER TABLE sessions ADD COLUMN last_search_id uuid;
ALTER TABLE session_found_tools ADD COLUMN search_id uuid;
ALTER TABLE session_found_tools ADD COLUMN rank integer;

-- session_open_searches holds the latest search of a session while it has no
-- hit and no newer search. A hit, a newer search or the no-call mark deletes
-- it. The query text is never stored.
CREATE TABLE session_open_searches (
    session_pk      bigint      PRIMARY KEY REFERENCES sessions (id) ON DELETE CASCADE,
    search_id       uuid        NOT NULL,
    searched_at     timestamptz NOT NULL DEFAULT now(),
    degraded        boolean     NOT NULL,
    embedding_model text        NOT NULL DEFAULT ''
);
