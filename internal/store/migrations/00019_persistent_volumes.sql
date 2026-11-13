-- +goose Up

-- persistent_volumes has one row per (template UID, volume name, scope ID):
-- a volume that outlives the instances that use it.
CREATE TABLE persistent_volumes (
    id                  text        PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    template_ns         text        NOT NULL,
    template_name       text        NOT NULL,
    template_uid        text        NOT NULL,
    volume_name         text        NOT NULL,
    scope               text        NOT NULL CHECK (scope IN ('per-template', 'per-user')),
    scope_id            text        NOT NULL,
    storage_class       text        NOT NULL,
    size_bytes          bigint      NOT NULL CHECK (size_bytes > 0),
    pv_name             text,
    instance_id         text        REFERENCES instances (id) ON DELETE SET NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    last_used_at        timestamptz NOT NULL DEFAULT now(),
    delete_reason       text        NOT NULL DEFAULT '' CHECK (delete_reason IN ('', 'template-deleted', 'unused', 'admin')),
    delete_requested_at timestamptz,
    reclaim_patched_at  timestamptz,
    problem             text        NOT NULL DEFAULT '' CHECK (octet_length(problem) <= 1024),
    CHECK ((scope = 'per-template') = (scope_id = ''))
);

CREATE UNIQUE INDEX persistent_volumes_key_idx ON persistent_volumes (template_uid, volume_name, scope_id);
CREATE INDEX persistent_volumes_instance_idx ON persistent_volumes (instance_id) WHERE instance_id IS NOT NULL;
CREATE INDEX persistent_volumes_user_idx ON persistent_volumes (scope_id) WHERE scope = 'per-user';
CREATE INDEX persistent_volumes_unused_idx ON persistent_volumes (last_used_at) WHERE instance_id IS NULL AND delete_reason = '';

-- stopping_at is when the instance became stopping. The volume drain bound
-- counts from it. It is null for rows that were stopping before this column.
ALTER TABLE instances ADD COLUMN stopping_at timestamptz;
