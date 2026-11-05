-- name: PutUserSecret :one
INSERT INTO user_secrets (principal, name, ciphertext, key_id)
VALUES (@principal, @name, @ciphertext, @key_id)
ON CONFLICT (principal, name) DO UPDATE
    SET ciphertext = EXCLUDED.ciphertext, key_id = EXCLUDED.key_id, updated_at = now()
RETURNING *;

-- name: GetUserSecret :one
SELECT * FROM user_secrets WHERE principal = @principal AND name = @name;

-- name: ListUserSecretNames :many
SELECT name, created_at, updated_at FROM user_secrets WHERE principal = @principal ORDER BY name;

-- name: DeleteUserSecret :execrows
DELETE FROM user_secrets WHERE principal = @principal AND name = @name;
