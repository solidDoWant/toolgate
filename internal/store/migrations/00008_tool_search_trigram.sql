-- +goose Up
-- pg_trgm ships with PostgreSQL and is a trusted extension, so the database
-- owner can create it.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- name_text holds the name words of a tool for the trigram term of the
-- search ranking. The leader rebuilds every row when it takes the Lease.
ALTER TABLE tool_search ADD COLUMN name_text text NOT NULL DEFAULT '';

-- pgvector holds the embeddings. It is not a trusted extension, so a
-- superuser must create it, unless it already exists.
CREATE EXTENSION IF NOT EXISTS vector;

-- embedding is the vector of the document from the embeddings endpoint, and
-- embedding_model the model that made it. NULL means the row is not embedded
-- (embeddings are off, or the endpoint failed). The column has no fixed
-- dimension, because it depends on the model; a search compares only rows of
-- the configured model.
ALTER TABLE tool_search ADD COLUMN embedding vector;
ALTER TABLE tool_search ADD COLUMN embedding_model text NOT NULL DEFAULT '';
