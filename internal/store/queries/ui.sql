-- name: InsertUISession :one
INSERT INTO ui_sessions (id_hash, principal, email, name, groups, csrf_token, auth_time)
VALUES (@id_hash, @principal, @email, @name, @groups, @csrf_token, sqlc.narg(auth_time))
RETURNING *;

-- name: GetValidUISession :one
-- Returns the web UI session while it is valid.
SELECT * FROM ui_sessions
WHERE id_hash = @id_hash AND expires_at > now() AND last_seen_at > now() - interval '1 hour';

-- name: TouchUISession :exec
-- Moves last_seen_at forward, at most once a minute.
UPDATE ui_sessions SET last_seen_at = now()
WHERE id_hash = @id_hash AND last_seen_at <= now() - interval '1 minute';

-- name: DeleteUISession :exec
DELETE FROM ui_sessions WHERE id_hash = @id_hash;

-- name: InsertUILogin :exec
INSERT INTO ui_logins (state_hash, nonce, pkce_verifier, return_to, step_up, ui_session_hash)
VALUES (@state_hash, @nonce, @pkce_verifier, @return_to, @step_up, sqlc.narg(ui_session_hash));

-- name: TakeUILogin :one
-- Takes the sign-in in progress for the state, so each state works once.
-- It returns no row when the state is unknown, already taken or older than
-- 10 minutes.
DELETE FROM ui_logins
WHERE state_hash = @state_hash AND created_at > now() - interval '10 minutes'
RETURNING *;

-- name: CountUILogins :one
-- Counts the sign-ins in progress, to refuse new ones over the limit.
SELECT count(*) FROM ui_logins;

-- name: UpdateUISessionSignIn :execrows
-- Records a step-up sign-in on a valid web UI session of the same
-- principal: the new auth_time and identity snapshot.
UPDATE ui_sessions
SET email = @email, name = @name, groups = @groups, auth_time = @auth_time
WHERE id_hash = @id_hash AND principal = @principal
  AND expires_at > now() AND last_seen_at > now() - interval '1 hour';
