-- +goose Up

-- kind is the kind of the identity the session was created or restarted
-- with: user, service-account or spiffe. Identities rebuilt from a session
-- snapshot take their kind from it, never from the attestation or the
-- principal's form.
ALTER TABLE sessions ADD COLUMN kind text;

-- Existing rows follow from the attestation and the principal's form, which
-- is exact for them: before this migration no user was attested, only a
-- JWT-SVID was attested with a spiffe:// principal, and only a TokenReview
-- token with a service-account one.
UPDATE sessions SET kind = CASE
    WHEN attestation = 'attested' AND principal LIKE 'spiffe://%' THEN 'spiffe'
    WHEN attestation = 'attested' AND principal LIKE 'system:serviceaccount:%' THEN 'service-account'
    ELSE 'user'
END;

-- An older replica inserts sessions without a kind during a rolling upgrade.
-- The trigger fills it in by the same rule, which is exact for that replica's
-- sessions for the same reason.
-- +goose StatementBegin
CREATE FUNCTION toolgate_session_kind_from_attestation() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.kind := CASE
        WHEN NEW.attestation = 'attested' AND NEW.principal LIKE 'spiffe://%' THEN 'spiffe'
        WHEN NEW.attestation = 'attested' AND NEW.principal LIKE 'system:serviceaccount:%' THEN 'service-account'
        ELSE 'user'
    END;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER sessions_kind_default
    BEFORE INSERT ON sessions
    FOR EACH ROW
    WHEN (NEW.kind IS NULL)
    EXECUTE FUNCTION toolgate_session_kind_from_attestation();

ALTER TABLE sessions ALTER COLUMN kind SET NOT NULL;
ALTER TABLE sessions ADD CONSTRAINT sessions_kind_check
    CHECK (kind IN ('user', 'service-account', 'spiffe'));

-- user_scopes holds a principal's defaults for sessions with and without a
-- verified workspace. A row is created on the principal's first scoping
-- write and never deleted; a missing row means both defaults are none.
CREATE TABLE user_scopes (
    id                 bigserial   PRIMARY KEY,
    principal          text        NOT NULL UNIQUE,
    verified_default   text        NOT NULL DEFAULT 'none' CHECK (verified_default IN ('none', 'all')),
    unverified_default text        NOT NULL DEFAULT 'none' CHECK (unverified_default IN ('none', 'all')),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- workspace_lists is a principal's server list for one verified workspace.
CREATE TABLE workspace_lists (
    id         bigserial   PRIMARY KEY,
    scope_id   bigint      NOT NULL REFERENCES user_scopes (id),
    workspace  text        NOT NULL CHECK (char_length(workspace) <= 256 AND workspace NOT LIKE 'claimed/%'),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (scope_id, workspace)
);

-- workspace_list_servers are the server names on a list.
CREATE TABLE workspace_list_servers (
    list_id  bigint      NOT NULL REFERENCES workspace_lists (id) ON DELETE CASCADE,
    server   text        NOT NULL,
    added_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (list_id, server)
);

-- session_additions are servers allowed for one session only, on top of its
-- workspace's list.
CREATE TABLE session_additions (
    session_pk bigint      NOT NULL REFERENCES sessions (id),
    server     text        NOT NULL,
    request_id text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_pk, server)
);

-- access_requests are a user's requests to enable a server outside the
-- workspace's list (kind server) or to widen access from the CLI (kind
-- change). Their statuses follow approvals.
CREATE TABLE access_requests (
    id            text        PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    principal     text        NOT NULL,
    kind          text        NOT NULL CHECK (kind IN ('server', 'change')),
    session_pk    bigint      REFERENCES sessions (id),
    workspace     text        NOT NULL,
    verified      boolean     NOT NULL,
    server        text,
    change        jsonb,
    status        text        NOT NULL DEFAULT 'pending'
                              CHECK (status IN ('pending', 'approved', 'denied', 'expired')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    decided_at    timestamptz,
    decided_by    text,
    decided_scope text        CHECK (decided_scope IN ('session', 'workspace')),
    CHECK (expires_at > created_at),
    CHECK ((kind = 'server' AND session_pk IS NOT NULL AND server IS NOT NULL)
        OR (kind = 'change' AND change IS NOT NULL))
);

CREATE INDEX access_requests_principal_pending_idx ON access_requests (principal) WHERE status = 'pending';
CREATE INDEX access_requests_session_pending_idx ON access_requests (session_pk, server) WHERE status = 'pending';
