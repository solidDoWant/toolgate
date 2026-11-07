-- +goose Up

-- principal_profiles holds the newest display fields toolgate has seen for
-- a principal, from verified token claims at sign-in and session start. The
-- email is only a value the IdP verified. It is for display only.
CREATE TABLE principal_profiles (
    principal  text        PRIMARY KEY,
    email      text        NOT NULL DEFAULT '',
    name       text        NOT NULL DEFAULT '',
    username   text        NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now()
);
