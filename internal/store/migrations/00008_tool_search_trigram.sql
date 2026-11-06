-- +goose Up
-- pg_trgm ships with PostgreSQL and is a trusted extension, so the database
-- owner can create it.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- name_text holds the name words of a tool for the trigram term of the
-- search ranking. The leader rebuilds every row when it takes the Lease.
ALTER TABLE tool_search ADD COLUMN name_text text NOT NULL DEFAULT '';

-- embedding is the vector of the document from the embeddings endpoint, and
-- embedding_model the model that made it. An empty vector means the row is
-- not embedded (embeddings are off, or the endpoint failed).
ALTER TABLE tool_search ADD COLUMN embedding real[] NOT NULL DEFAULT '{}';
ALTER TABLE tool_search ADD COLUMN embedding_model text NOT NULL DEFAULT '';
