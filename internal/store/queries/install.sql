-- name: InsertInstallID :exec
-- Stores the candidate ID unless the install already has one.
INSERT INTO install (id) VALUES (@id) ON CONFLICT DO NOTHING;

-- name: GetInstallID :one
SELECT id FROM install WHERE only_row;
