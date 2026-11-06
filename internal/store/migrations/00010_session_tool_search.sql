-- +goose Up
-- tool_search is the effective tool search mode of the session's latest
-- request (the X-Toolgate-Tool-Search header, else the gateway setting). The
-- open-session gauge groups by it.
ALTER TABLE sessions ADD COLUMN tool_search boolean NOT NULL DEFAULT false;
