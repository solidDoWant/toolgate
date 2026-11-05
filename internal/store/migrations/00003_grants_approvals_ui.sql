-- +goose Up

-- grants lets a principal use opt-in servers or tools outside their
-- environment until expires_at. tool is empty for a whole-server grant.
-- session_id, when set, narrows the grant to one X-Toolgate-Session value.
CREATE TABLE grants (
    id            text        PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    principal     text        NOT NULL,
    template_ns   text        NOT NULL,
    template_name text        NOT NULL,
    tool          text        NOT NULL DEFAULT '',
    session_id    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    revoked_at    timestamptz,
    revoked_by    text,
    CHECK (expires_at > created_at AND expires_at <= created_at + interval '7 days')
);

CREATE INDEX grants_principal_live_idx ON grants (principal) WHERE revoked_at IS NULL;

-- temporary_blocks are admin-made blocks. match has the fields of
-- ToolgateRule.spec.match. Rows are never changed after insert.
CREATE TABLE temporary_blocks (
    id         text        PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    match      jsonb       NOT NULL,
    reason     text        NOT NULL CHECK (reason <> '' AND char_length(reason) <= 1024),
    created_by text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    CHECK (expires_at > created_at AND expires_at <= created_at + interval '30 days')
);

CREATE INDEX temporary_blocks_expires_idx ON temporary_blocks (expires_at);

-- approvals ties a person's decision to one exact call by the SHA-256 of its
-- canonical arguments.
CREATE TABLE approvals (
    id               text        PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    principal        text        NOT NULL,
    session_pk       bigint      NOT NULL REFERENCES sessions (id),
    workspace        text        NOT NULL,
    template_ns      text        NOT NULL,
    template_name    text        NOT NULL,
    tool             text        NOT NULL,
    arguments_sha256 bytea       NOT NULL CHECK (octet_length(arguments_sha256) = 32),
    status           text        NOT NULL DEFAULT 'pending'
                                 CHECK (status IN ('pending', 'approved', 'denied', 'expired')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    decided_at       timestamptz,
    decided_by       text,
    used_at          timestamptz,
    CHECK (expires_at > created_at)
);

CREATE INDEX approvals_principal_pending_idx ON approvals (principal) WHERE status = 'pending';
CREATE INDEX approvals_lookup_idx ON approvals (session_pk, template_ns, template_name, tool, arguments_sha256);

-- restart_requested_at and restart_requested_by are set by a restart from
-- the web UI. resolved_expires_at is the earliest expiresAt of the resolver
-- values the instance's render used, or null.
ALTER TABLE instances ADD COLUMN restart_requested_at timestamptz;
ALTER TABLE instances ADD COLUMN restart_requested_by text;
ALTER TABLE instances ADD COLUMN resolved_expires_at  timestamptz;

-- ui_sessions are web UI sign-ins. id_hash is the SHA-256 of the cookie
-- value, so a leaked row isn't a working cookie. A session is valid while
-- now() < expires_at and last_seen_at > now() - 1 h.
CREATE TABLE ui_sessions (
    id_hash      bytea       PRIMARY KEY CHECK (octet_length(id_hash) = 32),
    principal    text        NOT NULL,
    email        text        NOT NULL DEFAULT '',
    name         text        NOT NULL DEFAULT '',
    groups       text[]      NOT NULL DEFAULT '{}',
    csrf_token   bytea       NOT NULL CHECK (octet_length(csrf_token) = 32),
    auth_time    timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL DEFAULT now() + interval '8 hours',
    CHECK (expires_at <= created_at + interval '8 hours')
);

-- ui_logins holds a web UI sign-in in progress for 10 minutes. It is taken
-- with DELETE ... RETURNING, so each state works once.
CREATE TABLE ui_logins (
    state_hash      bytea       PRIMARY KEY CHECK (octet_length(state_hash) = 32),
    nonce           text        NOT NULL,
    pkce_verifier   text        NOT NULL,
    return_to       text        NOT NULL DEFAULT '',
    step_up         boolean     NOT NULL DEFAULT false,
    ui_session_hash bytea,
    created_at      timestamptz NOT NULL DEFAULT now()
);
