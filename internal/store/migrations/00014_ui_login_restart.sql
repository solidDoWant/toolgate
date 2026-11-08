-- +goose Up

-- restarted marks a web UI sign-in that the gateway started again after the
-- previous one failed in a way a new sign-in can fix. A restarted sign-in
-- that fails the same way shows the error page, so the browser never loops
-- between the gateway and the identity provider.
ALTER TABLE ui_logins ADD COLUMN restarted boolean NOT NULL DEFAULT false;
