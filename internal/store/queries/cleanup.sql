-- name: DeleteInvalidUISessions :execrows
DELETE FROM ui_sessions
WHERE expires_at <= now() OR last_seen_at <= now() - interval '1 hour';

-- name: DeleteStaleUILogins :execrows
DELETE FROM ui_logins WHERE created_at <= now() - interval '10 minutes';

-- name: DeleteStaleGrants :execrows
-- Deletes grants that stopped being live (revoked or expired) more than
-- 7 days ago. LEAST ignores a null revoked_at.
DELETE FROM grants WHERE LEAST(revoked_at, expires_at) <= now() - interval '7 days';

-- name: DeleteStaleTemporaryBlocks :execrows
DELETE FROM temporary_blocks WHERE expires_at <= now() - interval '7 days';

-- name: DeleteStaleApprovals :execrows
-- Deletes approvals that stopped being live (expired, denied or used) more
-- than 7 days ago.
DELETE FROM approvals
WHERE LEAST(CASE WHEN status = 'denied' THEN decided_at END, used_at, expires_at) <= now() - interval '7 days';

-- name: DeleteStaleAccessRequests :execrows
-- Deletes access requests that stopped being live (decided or expired) more
-- than 7 days ago. LEAST ignores a null decided_at.
DELETE FROM access_requests WHERE LEAST(decided_at, expires_at) <= now() - interval '7 days';

-- name: DeleteStaleWorkspaceKeys :execrows
-- Deletes workspace keys that were revoked or replaced more than 7 days ago.
DELETE FROM workspace_keys WHERE status <> 'active' AND ended_at <= now() - interval '7 days';
