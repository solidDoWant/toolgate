-- name: LockInstanceScope :exec
-- Takes the transaction-level advisory lock that serializes instance
-- creation and stop decisions for one template and scope key.
SELECT pg_advisory_xact_lock(hashtextextended(@template_ns::text || '/' || @template_name::text || '/' || @scope_key::text, 0));

-- name: LockInstanceInsert :exec
-- Serializes instance inserts gateway-wide, so two enables can't both pass
-- the instance limits. Taken after LockInstanceScope.
SELECT pg_advisory_xact_lock(hashtextextended('toolgate-instance-insert', 0));

-- name: CountLiveInstances :one
SELECT count(*) FROM instances
WHERE desired = 'running' AND status NOT IN ('stopping', 'stopped');

-- name: CountLiveInstancesOfOwner :one
SELECT count(*) FROM instances
WHERE owner_principal = @owner_principal AND owner_principal <> ''
  AND desired = 'running' AND status NOT IN ('stopping', 'stopped');

-- name: GetLiveInstance :one
SELECT * FROM instances
WHERE template_ns = @template_ns AND template_name = @template_name AND scope_key = @scope_key
  AND desired = 'running' AND status NOT IN ('stopping', 'stopped');

-- name: GetNewestInstance :one
SELECT * FROM instances
WHERE template_ns = @template_ns AND template_name = @template_name AND scope_key = @scope_key
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: GetInstance :one
SELECT * FROM instances WHERE id = @id;

-- name: InsertInstance :one
INSERT INTO instances (
    id, template_ns, template_name, template_uid, generation, scope, scope_key,
    owner_principal, render_session, namespace, trace_context, attempt_started_at
) VALUES (
    @id, @template_ns, @template_name, @template_uid, @generation, @scope, @scope_key,
    @owner_principal, @render_session, @namespace, @trace_context, now()
)
RETURNING *;

-- name: ListInstancesNotStopped :many
SELECT * FROM instances WHERE status <> 'stopped' ORDER BY id;

-- name: SetInstanceStarting :execrows
-- Records the generation the objects were rendered and applied with.
UPDATE instances
SET status = 'starting', generation = @generation, updated_at = now()
WHERE id = @id AND status = 'pending';

-- name: SetInstanceReady :execrows
UPDATE instances
SET status = 'ready', tools = @tools, ready_at = now(), reason = '', updated_at = now()
WHERE id = @id AND status = 'starting';

-- name: SetInstanceTools :execrows
UPDATE instances
SET tools = @tools, updated_at = now()
WHERE id = @id AND status = 'ready';

-- name: SetInstanceFailed :execrows
UPDATE instances
SET status = 'failed', desired = 'stopped', reason = @reason, failed_at = now(), updated_at = now()
WHERE id = @id AND status = @from_status;

-- name: SetInstancePending :execrows
-- Sends a starting or ready instance whose objects are missing back to
-- pending, at the template's current generation. The start attempt begins
-- again, so startTimeout is measured from now.
UPDATE instances
SET status = 'pending', generation = @generation, attempt_started_at = now(), updated_at = now()
WHERE id = @id AND status = @from_status;

-- name: SetInstanceSecretsUsed :execrows
UPDATE instances
SET secrets_used = @secrets_used, updated_at = now()
WHERE id = @id AND status = 'pending';

-- name: SetInstanceDesiredStopped :execrows
UPDATE instances
SET desired = 'stopped', updated_at = now()
WHERE id = @id AND desired = 'running';

-- name: SetInstanceStopping :execrows
UPDATE instances
SET status = 'stopping', updated_at = now()
WHERE id = @id AND status = @from_status AND desired = 'stopped';

-- name: SetInstanceStopped :execrows
UPDATE instances
SET status = 'stopped', updated_at = now()
WHERE id = @id AND status = 'stopping';

-- name: SetInstanceReason :execrows
UPDATE instances
SET reason = @reason, updated_at = now()
WHERE id = @id AND status = @from_status;

-- name: TouchInstance :exec
UPDATE instances SET last_used_at = now() WHERE id = @id;

-- name: DeleteStoppedInstancesBefore :execrows
DELETE FROM instances WHERE status = 'stopped' AND updated_at < @before;

-- name: ListLiveInstancesForUI :many
-- The live instances the web UI's Servers page shows: those owned by a
-- principal and, for admins, the shared ones.
SELECT * FROM instances
WHERE ((owner_principal = @owner_principal AND owner_principal <> '') OR (@include_shared::boolean AND scope = 'shared'))
  AND desired = 'running' AND status NOT IN ('stopping', 'stopped')
ORDER BY template_name, template_ns, created_at, id;

-- name: RequestInstanceRestart :execrows
-- Records a restart request from the web UI. Only a live, ready instance
-- matches; the reconciler replaces it.
UPDATE instances
SET restart_requested_at = now(), restart_requested_by = @requested_by, updated_at = now()
WHERE id = @id AND desired = 'running' AND status = 'ready';

-- name: GetInstanceForUpdate :one
SELECT * FROM instances WHERE id = @id FOR UPDATE;

-- name: InsertReplacementInstance :one
-- Inserts the pending row that replaces an instance. last_used_at is carried
-- over from the old row, so a replacement doesn't count as use.
INSERT INTO instances (
    id, template_ns, template_name, template_uid, generation, scope, scope_key,
    owner_principal, render_session, namespace, trace_context, attempt_started_at, last_used_at
) VALUES (
    @id, @template_ns, @template_name, @template_uid, @generation, @scope, @scope_key,
    @owner_principal, @render_session, @namespace, @trace_context, now(), @last_used_at
)
RETURNING *;
