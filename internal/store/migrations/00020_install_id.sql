-- +goose Up

-- install holds one row: the ID that marks every Kubernetes object this
-- install creates, so several installs can share a cluster.
CREATE TABLE install (
    only_row   boolean     PRIMARY KEY DEFAULT true CHECK (only_row),
    id         text        NOT NULL CHECK (id ~ '^[0-9a-f]{32}$'),
    created_at timestamptz NOT NULL DEFAULT now()
);
