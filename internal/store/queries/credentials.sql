-- name: InsertInstanceExchange :exec
-- Inserts the row of an exchange that a pending instance uses. wanted: the row
-- waits for a first token, and holds the claim when claimed. A row that waits
-- for a volume first is inserted with refresh_after = 'infinity' instead.
INSERT INTO instance_exchanges (instance_id, exchange, refresh_after, wanted_since, claimed_at)
VALUES (
    @instance_id, @exchange,
    CASE WHEN @closed::boolean THEN 'infinity'::timestamptz ELSE now() END,
    CASE WHEN @wanted::boolean THEN now() END,
    CASE WHEN @claimed::boolean THEN now() END
)
ON CONFLICT (instance_id, exchange) DO NOTHING;

-- name: DeleteInstanceExchangesNotIn :execrows
DELETE FROM instance_exchanges
WHERE instance_id = @instance_id AND NOT (exchange = ANY (@keep::text[]));

-- name: ListInstanceExchanges :many
SELECT * FROM instance_exchanges WHERE instance_id = @instance_id ORDER BY exchange;

-- name: MarkInstanceExchangesWanted :execrows
-- Opens the rows that have no first token yet: they are wanted now, and any
-- request may claim them.
UPDATE instance_exchanges
SET wanted_since = now(), refresh_after = now()
WHERE instance_id = @instance_id AND wanted_since IS NULL AND issued_at IS NULL;

-- name: MarkInstanceExchangesWantedIn :execrows
-- Opens the listed rows that aren't waiting yet: the leader holds no token
-- for them, so a request must exchange now.
UPDATE instance_exchanges
SET wanted_since = now(), refresh_after = now()
WHERE instance_id = @instance_id AND exchange = ANY (@exchanges::text[]) AND wanted_since IS NULL;

-- name: SetInstanceExchangeRendered :exec
-- The pending to starting write of one row: the token in the Secret now,
-- where it is, and when a request must exchange again.
UPDATE instance_exchanges
SET targets = @targets, issued_at = @issued_at, expires_at = @expires_at, refresh_after = @refresh_after,
    wanted_since = NULL, claimed_at = NULL
WHERE instance_id = @instance_id AND exchange = @exchange;

-- name: ResetInstanceExchangesForRepending :exec
-- The sweep's write: every row waits for a new token. closed keeps the rows
-- shut until the volume steps are done.
UPDATE instance_exchanges
SET wanted_since = CASE WHEN @closed::boolean THEN NULL ELSE now() END,
    refresh_after = CASE WHEN @closed::boolean THEN 'infinity'::timestamptz ELSE now() END,
    claimed_at = NULL
WHERE instance_id = @instance_id;

-- name: RecordExchangeFailure :execrows
-- A refresh also pushes refresh_after to a retry in 60 seconds, but never
-- past the expiry of the token in the Secret.
UPDATE instance_exchanges
SET last_error = @last_error, last_error_at = now(), claimed_at = NULL,
    refresh_after = CASE WHEN @retry::boolean
        THEN least(now() + interval '60 seconds', coalesce(expires_at, 'infinity'::timestamptz))
        ELSE refresh_after END
WHERE instance_id = @instance_id AND exchange = @exchange;

-- name: ApplyInstanceExchange :execrows
-- Compare-and-set on the old issued_at, so a repeated or old delivery changes
-- nothing. old_issued_at is null for the first apply.
UPDATE instance_exchanges
SET issued_at = @issued_at, expires_at = @expires_at, refresh_after = @refresh_after,
    claimed_at = NULL, last_error = NULL, last_error_at = NULL
WHERE instance_id = @instance_id AND exchange = @exchange
  AND issued_at IS NOT DISTINCT FROM sqlc.narg(old_issued_at)::timestamptz;

-- name: ListExpiredInstanceExchanges :many
SELECT instance_id, exchange, expires_at FROM instance_exchanges
WHERE expires_at <= now();

-- name: FirstExpiredInstanceExchange :one
-- The name of an exchange of the instance whose token in the Secret has
-- expired at now, or no row.
SELECT exchange FROM instance_exchanges
WHERE instance_id = @instance_id AND expires_at <= @now::timestamptz
ORDER BY exchange LIMIT 1;

-- name: ClaimInstanceExchanges :many
-- Claims the due rows of the configured exchanges. Each exchange has its own
-- claim timeout, so a row expires after the timeout of its own exchange. A row
-- of an exchange that is not in the list is never claimed.
UPDATE instance_exchanges AS e SET claimed_at = now()
FROM (SELECT unnest(@exchanges::text[]) AS exchange, unnest(@claim_timeouts::interval[]) AS claim_timeout) AS t
WHERE e.instance_id = @instance_id AND e.exchange = t.exchange
  AND e.refresh_after <= now()
  AND (e.claimed_at IS NULL OR e.claimed_at < now() - t.claim_timeout)
RETURNING e.exchange;

-- name: InsertInstanceTemplateSecret :exec
INSERT INTO instance_template_secrets (
    instance_id, target_secret, target_key, target_field,
    source_namespace, source_name, source_key, source_version
) VALUES (
    @instance_id, @target_secret, @target_key, @target_field,
    @source_namespace, @source_name, @source_key, @source_version
)
ON CONFLICT (instance_id, target_secret, target_key) DO UPDATE
SET target_field = excluded.target_field, source_namespace = excluded.source_namespace,
    source_name = excluded.source_name, source_key = excluded.source_key,
    source_version = excluded.source_version, last_error = NULL;

-- name: DeleteInstanceTemplateSecrets :exec
DELETE FROM instance_template_secrets WHERE instance_id = @instance_id;

-- name: ListInstanceTemplateSecrets :many
SELECT * FROM instance_template_secrets WHERE instance_id = @instance_id
ORDER BY target_secret, target_key;

-- name: SetTemplateSecretVersion :execrows
-- Compare-and-set on the old source_version.
UPDATE instance_template_secrets
SET source_version = @source_version, last_error = NULL
WHERE instance_id = @instance_id AND target_secret = @target_secret AND target_key = @target_key
  AND source_version = @old_source_version;

-- name: SetTemplateSecretError :execrows
UPDATE instance_template_secrets
SET last_error = sqlc.narg(last_error)
WHERE instance_id = @instance_id AND target_secret = @target_secret AND target_key = @target_key
  AND last_error IS DISTINCT FROM sqlc.narg(last_error);

-- name: ListLiveInstancesWithTemplateSecrets :many
SELECT i.* FROM instances i
WHERE i.desired = 'running' AND i.status IN ('starting', 'ready')
  AND EXISTS (SELECT 1 FROM instance_template_secrets s WHERE s.instance_id = i.id)
ORDER BY i.id;
