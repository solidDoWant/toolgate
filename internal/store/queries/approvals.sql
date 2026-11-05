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
