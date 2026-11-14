-- +goose Up

-- access_requests gains the kind key. The kind check and the shape check
-- have generated names or were replaced earlier; find them by what they test.
-- +goose StatementBegin
DO $$
DECLARE
    found record;
BEGIN
    FOR found IN
        SELECT conname FROM pg_constraint
        WHERE conrelid = 'access_requests'::regclass
          AND contype = 'c'
          AND pg_get_constraintdef(oid) ~ '\mkind\M'
    LOOP
        EXECUTE format('ALTER TABLE access_requests DROP CONSTRAINT %I', found.conname);
    END LOOP;
END
$$;
-- +goose StatementEnd

ALTER TABLE access_requests ADD CONSTRAINT access_requests_kind_check
    CHECK (kind IN ('server', 'change', 'scope', 'key'));
ALTER TABLE access_requests ADD CONSTRAINT access_requests_shape_check
    CHECK ((kind = 'server' AND session_pk IS NOT NULL AND server IS NOT NULL)
        OR (kind = 'change' AND change IS NOT NULL)
        OR (kind = 'scope' AND change IS NOT NULL)
        OR (kind = 'key' AND change IS NOT NULL));

-- workspace_keys holds approved workspace keys only. A pending key exists
-- only as a key access request. request_id is not a foreign key, because
-- access requests are deleted 7 days after they stop being live.
CREATE TABLE workspace_keys (
    id            bigserial   PRIMARY KEY,
    principal     text        NOT NULL,
    username      text        NOT NULL CHECK (username ~ '^[A-Za-z0-9._-]{1,63}$'),
    name          text        NOT NULL CHECK (name ~ '^[A-Za-z0-9._-]{1,63}$'),
    key_id        text        NOT NULL CHECK (key_id ~ '^[A-Za-z0-9_-]{43}$'),
    public_key    bytea       NOT NULL CHECK (octet_length(public_key) = 32),
    status        text        NOT NULL CHECK (status IN ('active', 'replaced', 'revoked')),
    request_id    text        NOT NULL,
    registered_at timestamptz NOT NULL,
    approved_at   timestamptz NOT NULL,
    ended_at      timestamptz,
    last_used_at  timestamptz,
    CHECK ((status = 'active') = (ended_at IS NULL))
);

CREATE UNIQUE INDEX workspace_keys_active_name_key ON workspace_keys (principal, name) WHERE status = 'active';
CREATE UNIQUE INDEX workspace_keys_active_key_id_key ON workspace_keys (key_id) WHERE status = 'active';
CREATE INDEX workspace_keys_key_id_idx ON workspace_keys (key_id);
CREATE INDEX workspace_keys_ended_idx ON workspace_keys (ended_at) WHERE status <> 'active';
