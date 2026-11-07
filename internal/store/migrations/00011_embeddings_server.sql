-- +goose Up
-- embeddings_server holds one row shared by every gateway replica for the
-- bundled embeddings server that the leader scales to zero. last_demand_at is
-- when a replica last needed embeddings; any replica moves it forward. state
-- is what the leader last saw of the server's Deployment (down, starting or
-- ready); only the leader writes it. A new leader starts from this row.
CREATE TABLE embeddings_server (
    id             boolean PRIMARY KEY DEFAULT true CHECK (id),
    last_demand_at timestamptz NOT NULL DEFAULT now(),
    state          text NOT NULL DEFAULT 'down' CHECK (state IN ('down', 'starting', 'ready'))
);

INSERT INTO embeddings_server DEFAULT VALUES;
