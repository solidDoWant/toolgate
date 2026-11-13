-- +goose Up

-- instances.uses_exchange is true when the instance's generation uses an
-- exchanged token. credential_expired is true when the instance stopped
-- because such a token expired.
ALTER TABLE instances
    ADD COLUMN uses_exchange boolean NOT NULL DEFAULT false,
    ADD COLUMN credential_expired boolean NOT NULL DEFAULT false;

-- instance_exchanges has one row for each token exchange that an instance
-- uses. It never holds a token.
CREATE TABLE instance_exchanges (
    instance_id   text        NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    exchange      text        NOT NULL,
    targets       jsonb,
    issued_at     timestamptz,
    expires_at    timestamptz,
    refresh_after timestamptz NOT NULL DEFAULT now(),
    wanted_since  timestamptz,
    claimed_at    timestamptz,
    last_error    text,
    last_error_at timestamptz,
    PRIMARY KEY (instance_id, exchange)
);

-- instance_template_secrets has one row for each value that the render
-- copied from the template secrets namespace. It never holds a value.
CREATE TABLE instance_template_secrets (
    instance_id      text NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    target_secret    text NOT NULL,
    target_key       text NOT NULL,
    target_field     text NOT NULL CHECK (target_field IN ('data', 'stringData')),
    source_namespace text NOT NULL,
    source_name      text NOT NULL,
    source_key       text NOT NULL,
    source_version   text NOT NULL DEFAULT '',
    last_error       text,
    PRIMARY KEY (instance_id, target_secret, target_key)
);

CREATE INDEX instance_template_secrets_source_idx ON instance_template_secrets (source_name);
