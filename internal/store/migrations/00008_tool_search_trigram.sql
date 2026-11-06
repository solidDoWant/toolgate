-- +goose Up
-- pg_trgm ships with PostgreSQL and is a trusted extension, so the database
-- owner can create it.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- name_text holds the name words of a tool for the trigram term of the
-- search ranking. The leader rebuilds every row when it takes the Lease.
ALTER TABLE tool_search ADD COLUMN name_text text NOT NULL DEFAULT '';
