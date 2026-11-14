-- name: InsertPersistentVolume :execrows
INSERT INTO persistent_volumes (id, template_ns, template_name, template_uid, volume_name, scope, scope_id, storage_class, size_bytes)
VALUES (@id, @template_ns, @template_name, @template_uid, @volume_name, @scope, @scope_id, @storage_class, @size_bytes)
ON CONFLICT DO NOTHING;

-- name: LockPersistentVolume :one
SELECT * FROM persistent_volumes
WHERE template_uid = @template_uid AND volume_name = @volume_name AND scope_id = @scope_id
FOR UPDATE;

-- name: SetPersistentVolumeHolder :execrows
UPDATE persistent_volumes SET instance_id = @instance_id, last_used_at = now() WHERE id = @id;

-- name: ClearPersistentVolumeUnusedMark :execrows
UPDATE persistent_volumes
SET delete_reason = '', delete_requested_at = NULL, problem = ''
WHERE id = @id AND delete_reason = 'unused' AND reclaim_patched_at IS NULL;

-- name: RecordPersistentVolume :execrows
-- Records the PV of a new volume. It changes nothing unless the volume is
-- still unrecorded and still held by the instance.
UPDATE persistent_volumes SET pv_name = @pv_name, size_bytes = @size_bytes
WHERE id = @id AND pv_name IS NULL AND instance_id = @instance_id;

-- name: ClearPersistentVolumePV :execrows
UPDATE persistent_volumes SET pv_name = NULL
WHERE id = @id AND pv_name = @old_pv_name;

-- name: SetPersistentVolumeProblem :exec
UPDATE persistent_volumes SET problem = @problem WHERE id = @id;

-- name: SumPersistentVolumeBytesForUser :one
SELECT COALESCE(sum(size_bytes), 0)::bigint FROM persistent_volumes
WHERE scope = 'per-user' AND scope_id = @scope_id;

-- name: ListPersistentVolumesHeldBy :many
SELECT * FROM persistent_volumes WHERE instance_id = @instance_id ORDER BY volume_name;

-- name: MarkPersistentVolumeForDeletion :execrows
-- Decides a deletion. The clean-up passes any_holder = false so a held volume is
-- never marked; an admin sets any_holder to mark any row.
UPDATE persistent_volumes
SET delete_reason = @delete_reason, delete_requested_at = now()
WHERE id = @id AND delete_reason = '' AND (@any_holder::boolean OR instance_id IS NULL);

-- name: SetPersistentVolumeReclaimPatched :execrows
UPDATE persistent_volumes SET reclaim_patched_at = now()
WHERE id = @id AND delete_reason <> '' AND instance_id IS NULL AND reclaim_patched_at IS NULL;

-- name: DeletePersistentVolume :execrows
DELETE FROM persistent_volumes WHERE id = @id;

-- name: ListPersistentVolumes :many
SELECT * FROM persistent_volumes
WHERE id > @after_id AND (@owner::text = '' OR (scope = 'per-user' AND scope_id = @owner))
ORDER BY id
LIMIT @page_size;

-- name: ListPersistentVolumesToDelete :many
SELECT * FROM persistent_volumes WHERE delete_reason <> '' AND instance_id IS NULL ORDER BY id;

-- name: CountPersistentVolumes :many
SELECT scope,
       CASE WHEN instance_id IS NOT NULL THEN 'in_use' WHEN delete_reason <> '' THEN 'deleting' ELSE 'idle' END AS state,
       count(*)::bigint AS volumes,
       COALESCE(sum(size_bytes), 0)::bigint AS bytes
FROM persistent_volumes
GROUP BY 1, 2;

-- name: ReleasePersistentVolumes :execrows
UPDATE persistent_volumes SET instance_id = NULL, last_used_at = now() WHERE instance_id = @instance_id;

-- name: GetPersistentVolume :one
SELECT * FROM persistent_volumes WHERE id = @id;
