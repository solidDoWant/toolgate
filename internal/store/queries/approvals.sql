-- name: LockPrincipalApprovals :exec
-- Takes the transaction advisory lock that serializes approval inserts for
-- one principal, so two inserts can't both pass the pending count.
SELECT pg_advisory_xact_lock(hashtextextended('toolgate-approval/' || @principal::text, 0));

-- name: InsertApprovalWithinLimit :one
-- Inserts a pending approval unless the principal already has
-- pending_limit pending approvals; then it returns no row. Run it after
-- LockPrincipalApprovals in the same transaction.
INSERT INTO approvals (id, principal, session_pk, workspace, template_ns, template_name, tool, arguments_sha256, expires_at)
SELECT @id, @principal, @session_pk, @workspace, @template_ns, @template_name, @tool, @arguments_sha256, now() + @approval_timeout::interval
WHERE (
    SELECT count(*) FROM approvals WHERE principal = @principal AND status = 'pending'
) < @pending_limit::bigint
RETURNING *;

-- name: GetApproval :one
SELECT * FROM approvals WHERE id = @id;

-- name: ApproveApproval :one
-- Approves a pending approval that hasn't expired, and starts its usable
-- window. It returns no row when the approval isn't pending any more or
-- has expired; the caller re-reads it to report the real status.
UPDATE approvals
SET status = 'approved', decided_at = now(), decided_by = @decided_by,
    expires_at = now() + @approval_timeout::interval
WHERE id = @id AND status = 'pending' AND expires_at > now()
RETURNING *;

-- name: DenyApproval :one
-- Denies a pending approval that hasn't expired. It returns no row
-- otherwise.
UPDATE approvals
SET status = 'denied', decided_at = now(), decided_by = @decided_by
WHERE id = @id AND status = 'pending' AND expires_at > now()
RETURNING *;

-- name: ExpireApproval :one
-- Marks a pending approval expired. It returns no row when the approval
-- isn't pending any more.
UPDATE approvals
SET status = 'expired'
WHERE id = @id AND status = 'pending'
RETURNING *;

-- name: UseApproval :one
-- Uses an approved approval once, while it is usable. It returns no row
-- when the approval was already used, isn't approved or has expired.
UPDATE approvals
SET used_at = now()
WHERE id = @id AND status = 'approved' AND used_at IS NULL AND expires_at > now()
RETURNING *;

-- name: FindUsableApproval :one
-- Returns the oldest usable approved approval of one exact call.
SELECT * FROM approvals
WHERE principal = @principal AND session_pk = @session_pk
  AND template_ns = @template_ns AND template_name = @template_name
  AND tool = @tool AND arguments_sha256 = @arguments_sha256
  AND status = 'approved' AND used_at IS NULL AND expires_at > now()
ORDER BY created_at, id
LIMIT 1;

-- name: FindPendingApproval :one
-- Returns the oldest pending approval of one exact call that hasn't
-- expired.
SELECT * FROM approvals
WHERE principal = @principal AND session_pk = @session_pk
  AND template_ns = @template_ns AND template_name = @template_name
  AND tool = @tool AND arguments_sha256 = @arguments_sha256
  AND status = 'pending' AND expires_at > now()
ORDER BY created_at, id
LIMIT 1;

-- name: ListOverdueApprovals :many
-- Lists pending approvals whose expires_at is more than 1 minute past, with
-- their session's client session ID, for the leader's pass.
SELECT a.id, a.principal, a.workspace, a.template_name, a.tool, s.session_id, s.attestation
FROM approvals a
JOIN sessions s ON s.id = a.session_pk
WHERE a.status = 'pending' AND a.expires_at < now() - interval '1 minute'
ORDER BY a.expires_at
LIMIT 1000;

-- name: GetMatchingApproval :one
-- Returns the approval with this ID only when every field of the call
-- matches, so a client-sent ID can't name another call's approval. usable
-- is computed with PostgreSQL's clock.
SELECT sqlc.embed(approvals),
       (status = 'approved' AND used_at IS NULL AND expires_at > now())::boolean AS usable
FROM approvals
WHERE id = @id AND principal = @principal AND session_pk = @session_pk
  AND template_ns = @template_ns AND template_name = @template_name
  AND tool = @tool AND arguments_sha256 = @arguments_sha256;

-- name: ListPendingApprovalsOfPrincipal :many
-- Lists a principal's pending approvals that haven't expired, newest
-- first, with their session's client session ID, for the Approvals page.
SELECT sqlc.embed(approvals), s.session_id
FROM approvals
JOIN sessions s ON s.id = approvals.session_pk
WHERE approvals.principal = @principal AND approvals.status = 'pending' AND approvals.expires_at > now()
ORDER BY approvals.created_at DESC, approvals.id DESC
LIMIT 100;

-- name: GetApprovalOfPrincipal :one
-- Returns one approval only when it belongs to principal, with its
-- session's client session ID. past_expiry is computed with PostgreSQL's
-- clock.
SELECT sqlc.embed(approvals), s.session_id,
       (approvals.expires_at <= now())::boolean AS past_expiry
FROM approvals
JOIN sessions s ON s.id = approvals.session_pk
WHERE approvals.id = @id AND approvals.principal = @principal;

-- name: GetApprovalForWait :one
-- Returns an approval and whether its expires_at has passed, by
-- PostgreSQL's clock, for a call waiting on it.
SELECT sqlc.embed(approvals), (expires_at <= now())::boolean AS overdue
FROM approvals
WHERE id = @id;

-- name: ExpireDueApproval :one
-- Marks a pending approval expired once its expires_at has passed. It
-- returns no row when the approval isn't pending any more or hasn't
-- expired yet.
UPDATE approvals
SET status = 'expired'
WHERE id = @id AND status = 'pending' AND expires_at <= now()
RETURNING *;
