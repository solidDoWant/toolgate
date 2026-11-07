-- name: UpsertPrincipalProfile :exec
-- Keeps the newest display fields seen for the principal.
INSERT INTO principal_profiles (principal, email, name, username)
VALUES (@principal, @email, @name, @username)
ON CONFLICT (principal) DO UPDATE
    SET email = EXCLUDED.email,
        name = EXCLUDED.name,
        username = EXCLUDED.username,
        updated_at = now();

-- name: ListPrincipalProfiles :many
SELECT * FROM principal_profiles WHERE principal = ANY(@principals::text[]);
