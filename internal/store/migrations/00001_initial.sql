-- +goose Up

-- sessions holds one row per (principal, session ID). The identity snapshot
-- (email, name, groups) copies the claims of the most recent verified
-- request; it is used for rendering and re-checks and only narrows access.
CREATE TABLE sessions (
    id           bigserial   PRIMARY KEY,
    principal    text        NOT NULL,
    session_id   text        NOT NULL,
    workspace    text        NOT NULL,
    attestation  text        NOT NULL CHECK (attestation IN ('attested', 'user')),
    email        text        NOT NULL DEFAULT '',
    name         text        NOT NULL DEFAULT '',
    groups       text[]      NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    ended_at     timestamptz,
    UNIQUE (principal, session_id)
);

CREATE INDEX sessions_principal_live_idx ON sessions (principal) WHERE ended_at IS NULL;

-- session_servers is each session's enabled set.
CREATE TABLE session_servers (
    session_pk    bigint      NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    template_ns   text        NOT NULL,
    template_name text        NOT NULL,
    enabled_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_pk, template_ns, template_name)
);

CREATE INDEX session_servers_template_idx ON session_servers (template_ns, template_name);

-- instances holds one row per start attempt of a template for a scope key.
CREATE TABLE instances (
    id                 text        PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    template_ns        text        NOT NULL,
    template_name      text        NOT NULL,
    template_uid       text        NOT NULL,
    generation         bigint      NOT NULL,
    scope              text        NOT NULL CHECK (scope IN ('per-session', 'per-user', 'shared')),
    scope_key          text        NOT NULL,
    owner_principal    text        NOT NULL DEFAULT '',
    render_session     bigint      REFERENCES sessions (id) ON DELETE SET NULL,
    desired            text        NOT NULL DEFAULT 'running' CHECK (desired IN ('running', 'stopped')),
    status             text        NOT NULL DEFAULT 'pending'
                                   CHECK (status IN ('pending', 'starting', 'ready', 'failed', 'stopping', 'stopped')),
    reason             text        NOT NULL DEFAULT '',
    namespace          text        NOT NULL,
    tools              jsonb,
    secrets_used       jsonb       NOT NULL DEFAULT '{}',
    created_at         timestamptz NOT NULL DEFAULT now(),
    attempt_started_at timestamptz,
    ready_at           timestamptz,
    failed_at          timestamptz,
    last_used_at       timestamptz,
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- At most one live instance per template and scope key.
CREATE UNIQUE INDEX instances_live_idx ON instances (template_ns, template_name, scope_key)
    WHERE desired = 'running' AND status NOT IN ('stopping', 'stopped');

CREATE INDEX instances_template_scope_idx ON instances (template_ns, template_name, scope_key, created_at DESC);
CREATE INDEX instances_status_idx ON instances (status);
CREATE INDEX instances_owner_live_idx ON instances (owner_principal)
    WHERE desired = 'running' AND status NOT IN ('stopping', 'stopped');

-- user_secrets holds envelope-encrypted secrets. key_id is the Tink key ID
-- that encrypted the ciphertext.
CREATE TABLE user_secrets (
    principal  text        NOT NULL,
    name       text        NOT NULL,
    ciphertext bytea       NOT NULL,
    key_id     bigint      NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (principal, name)
);

-- audit_events is partitioned by month on occurred_at. Partitions are
-- created by the gateway (store.EnsureAuditPartitions).
CREATE TABLE audit_events (
    id          bigint      GENERATED ALWAYS AS IDENTITY,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    type        text        NOT NULL,
    principal   text        NOT NULL DEFAULT '',
    workspace   text        NOT NULL DEFAULT '',
    session     text        NOT NULL DEFAULT '',
    attestation text        NOT NULL DEFAULT '',
    server      text        NOT NULL DEFAULT '',
    tool        text        NOT NULL DEFAULT '',
    trace_id    text        NOT NULL DEFAULT '',
    request_id  text        NOT NULL DEFAULT '',
    details     jsonb       NOT NULL DEFAULT '{}',
    payload     jsonb,
    payload_ref text        NOT NULL DEFAULT '',
    PRIMARY KEY (occurred_at, id)
) PARTITION BY RANGE (occurred_at);

CREATE INDEX audit_events_principal_idx ON audit_events (principal, occurred_at);
