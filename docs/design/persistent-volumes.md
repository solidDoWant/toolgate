# Persistent volumes design

This document decides how a template keeps volumes that outlive its instances. It adds to `docs/design/phase-1.md` to `docs/design/phase-4.md`, which still hold unless a section here says otherwise. Where they disagree, stop and ask (`HANDOFF: question`). Workers implement it and never change its rules. The operator and owner change it.

The target is the "Done when" list of the owner's issue, copied in §15. Snapshots are out of scope.

Today an instance's namespace, and every volume in it, is deleted when the instance stops (`SPEC.md` §9, "Limits"). Many MCP servers keep useful state on disk: SQLite caches, browser profiles, search indexes. With this feature, the PersistentVolume (PV, the cluster object that holds the data) stays after the namespace goes. The next instance with the same template and scope binds to the same PV.

## 1. Who does what

Phase 1's split stays (phase 1 §1). Request handling runs on every replica and never touches Kubernetes objects. The runtime reconciler runs only on the Lease holder.

**New request-handling work**, on every replica:
- the catalog checks the new template field (§2);
- `GET /api/v1/admin/volumes` and `DELETE /api/v1/admin/volumes/{id}` read and mark rows (§11). They change nothing in Kubernetes.

**New reconciler work**, on the leader only:
- claiming, checking and binding volumes when an instance starts (§4);
- recording the PV of a new volume (§4, step 8);
- draining an instance's pods before its namespace is deleted, and releasing its volumes (§5);
- the volume clean-up on each full pass (§8).

**Notifications.** No new kind. A release is written in the same transaction as the `stopping` → `stopped` write, which already notifies `instance:<id>`. Every instance notification starts a full pass (`triggersPass` in `internal/runtime/reconciler.go`), so a start that waits for the volume is stepped again.

The feature is off by default. Everything in this document applies only when the gateway setting `persistentVolumes.enabled` is `true` (§10).

## 2. Template field

### The field

```yaml
spec:
  scope: per-user
  persistentVolumes:
    - name: cache                                   # the PVC name the pod spec uses
      size: 1Gi
      storageClassName: ssd-replicated-3x-block-retain
      scope: per-user                               # optional: per-template | per-user
  resources:
    - apiVersion: apps/v1
      kind: Deployment
      spec:
        template:
          spec:
            volumes:
              - name: data
                persistentVolumeClaim:
                  claimName: cache
```

Go types in `api/v1alpha1/mcpservertemplate_types.go`:

```go
// PersistentVolumes are volumes that outlive the instance. Each is a
// PersistentVolumeClaim named Name in the instance namespace.
// +optional
// +kubebuilder:validation:MaxItems=8
// +listType=map
// +listMapKey=name
PersistentVolumes []PersistentVolume `json:"persistentVolumes,omitempty"`

type PersistentVolume struct {
	// +required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// +required
	// +kubebuilder:validation:XValidation:rule="isQuantity(self) && quantity(self).isGreaterThan(quantity('0'))",message="size must be a quantity greater than 0"
	Size string `json:"size"`
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	StorageClassName string `json:"storageClassName"`
	// +optional
	// +kubebuilder:validation:XValidation:rule="self in ['per-template', 'per-user']",message="scope must be one of per-template, per-user"
	Scope string `json:"scope,omitempty"`
}
```

The name is at most 63 characters because toolgate puts it in a label value. CEL rules on `MCPServerTemplateSpec`:
- `!has(self.persistentVolumes) || !has(self.remote)`, message "persistentVolumes cannot be set together with remote".
- `!has(self.persistentVolumes) || !has(self.__namespace__) || self.__namespace__ != 'shared'`, message "persistentVolumes needs namespace: per-instance".
- `!has(self.persistentVolumes) || !has(self.scope) || self.scope != 'per-session'`, message "a per-session template cannot have persistentVolumes".

### Volume scope

A volume's **scope ID** says which instances share it:
- `per-template`: one volume for the template. The scope ID is the empty string.
- `per-user`: one volume for each user. The scope ID is the instance's `owner_principal`, the verified principal.

A volume scope must be the one that matches the template's effective scope (`templatescope.Effective`):

| Template's effective scope | Volume scope allowed | Why the others fail |
|---|---|---|
| `shared` | `per-template` | A shared instance has no user, so `per-user` has no scope ID. |
| `per-user` | `per-user` | `per-template` is wider: two users then share data. |
| `per-session` | none | Both are wider than one session. |

An unset volume `scope` takes the allowed value. A set `scope` that differs makes the template invalid. So an admin who writes `scope: per-template` gets an invalid template, not split data, when a later edit narrows the template to `per-user`.

### Catalog checks

`catalog.Validator` gains `PersistentVolumes bool`, set from the gateway setting. `Validate` adds these problems, each reported in the `Valid` condition:
1. "persistentVolumes needs the gateway setting persistentVolumes.enabled", when the setting is off.
2. The three CRD rules above, checked again.
3. "persistentVolumes `<name>`: scope `<scope>` is wider than the template's effective scope `<scope>`", or "… needs a user, and a shared template has none", from the table above.
4. "persistentVolumes `<name>`: size must be a quantity greater than 0", checked with `resource.ParseQuantity`.
5. "persistentVolumes cannot be used with a Pod in resources. Use a Deployment or StatefulSet.", when any item of `resources` has `apiVersion: v1` and `kind: Pod`. toolgate scales controllers to zero before it deletes the namespace (§5). It cannot scale a bare Pod.
6. "resources[`i`] is a PersistentVolumeClaim named `<name>`, which persistentVolumes already makes", when a template PVC has the same name as a volume.

The pod spec refers to a volume by `persistentVolumeClaim.claimName: <name>`. toolgate does not check that a resource uses the claim, because a HelmRelease's values can't be read reliably. A StatefulSet's `volumeClaimTemplates` make ordinary PVCs, which do not outlive the instance.

### What toolgate renders

For each volume, toolgate makes one PersistentVolumeClaim (PVC) in the instance namespace. It is not part of `Render`'s template objects. The starter builds it as a typed object (§4, step 6):

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: cache                                   # the volume name
  namespace: tg-<instance id>
  labels:
    app.kubernetes.io/managed-by: toolgate
    toolgate.soliddowant.dev/instance: <instance id>
    toolgate.soliddowant.dev/template: <template name>
    toolgate.soliddowant.dev/volume: cache
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  storageClassName: <the row's storage_class>
  resources:
    requests:
      storage: <the row's size_bytes>
  volumeName: <the row's pv_name>               # only when the row has one
```

The instance metadata annotations (`NewInstanceMetadata`) go on it too, like on every other object. It is applied with server-side apply, field manager `toolgate`.

**Labels on the PV.** toolgate never creates a PV. After the first binding, it adds these labels to the provisioned PV (§4, step 8):
- `app.kubernetes.io/managed-by: toolgate`;
- `toolgate.soliddowant.dev/template: <template name>`;
- `toolgate.soliddowant.dev/template-uid: <template uid>`;
- `toolgate.soliddowant.dev/volume: <volume name>`;
- `toolgate.soliddowant.dev/volume-id: <row id>`.

No principal goes in a label. The row ID ties the PV to its row, and the row holds the scope ID.

## 3. State

One additive migration, `00008_persistent_volumes.sql` (phase 1 §7, "Database migrations").

**`persistent_volumes`**, one row per (template UID, volume name, scope ID):

| Column | Type | Meaning |
|---|---|---|
| `id` | `text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$')` | From `crypto/rand`, like instance IDs. |
| `template_ns`, `template_name` | `text NOT NULL` | Where the template was when the row was made. |
| `template_uid` | `text NOT NULL` | A re-created template has a new UID, so it gets new volumes. |
| `volume_name` | `text NOT NULL` | The template's `persistentVolumes[].name`. |
| `scope` | `text NOT NULL CHECK (scope IN ('per-template', 'per-user'))` | |
| `scope_id` | `text NOT NULL` | `''` for `per-template`, the principal for `per-user`. A check enforces `(scope = 'per-template') = (scope_id = '')`. |
| `storage_class` | `text NOT NULL` | The class the volume was made with. It never changes. |
| `size_bytes` | `bigint NOT NULL CHECK (size_bytes > 0)` | The requested size, then the PV's capacity once it is recorded. |
| `pv_name` | `text` | Null until the first PV is recorded. |
| `instance_id` | `text REFERENCES instances (id) ON DELETE SET NULL` | The holder: the instance that uses the volume now. Null when no instance holds it. |
| `created_at` | `timestamptz NOT NULL DEFAULT now()` | |
| `last_used_at` | `timestamptz NOT NULL DEFAULT now()` | Set when the volume is claimed and when it is released. |
| `delete_reason` | `text NOT NULL DEFAULT '' CHECK (delete_reason IN ('', 'template-deleted', 'unused', 'admin'))` | Not empty once a deletion is decided. |
| `delete_requested_at` | `timestamptz` | When the deletion was decided. |
| `reclaim_patched_at` | `timestamptz` | When the leader set the PV's reclaim policy to `Delete` (§8). |
| `problem` | `text NOT NULL DEFAULT ''` | The last reason the leader did not use or delete the PV, for the admin list. At most 1024 bytes. |

Indexes:
- `UNIQUE (template_uid, volume_name, scope_id)`;
- `(instance_id) WHERE instance_id IS NOT NULL`, for the release;
- `(scope_id) WHERE scope = 'per-user'`, for the size limit;
- `(last_used_at) WHERE instance_id IS NULL AND delete_reason = ''`, for the unused clean-up.

**Instances.** `instances.stopping_at timestamptz`. `SetInstanceStopping` sets it to `now()`. The drain bound (§5) counts from it, so a new leader keeps the same bound. A null value, on a row that became `stopping` before the upgrade, counts as a bound that has passed.

**Row limits.** At most 8 volumes per template. Rows grow with users × templates × 8, and the clean-up removes unused ones. The admin list pages by 200 rows.

## 4. Starting an instance

These steps run in `Starter.startPending` for a template with `persistentVolumes`, after `Render` succeeds and before `applyAll` applies the template objects. Each step is safe to repeat, because a new leader repeats the whole step.

1. **Claim** all the template's volumes in one transaction, in volume-name order. Two instances that lock in the same order cannot deadlock. For each volume:
   - Insert the row with `ON CONFLICT DO NOTHING`, then `SELECT … FOR UPDATE` it. A new row gets `size_bytes` from the template's `size` and `storage_class` from its `storageClassName`.
   - If `delete_reason` is set, the volume is being deleted. Roll back and **wait** (below).
   - If `instance_id` is this instance, the claim is already held. Go on.
   - If `instance_id` is another instance whose row exists and is not `stopped`, roll back and **wait**.
   - Otherwise set `instance_id` to this instance and `last_used_at = now()`.
   - **Size limit** (§8), only for a new `per-user` row: take `pg_advisory_xact_lock(hashtext('toolgate-volume-limit:' || scope_id))` first, so that two starts for one user can't both pass. If the user's other rows plus this size pass `persistentVolumes.limitPerUser`, roll back and fail the instance.

   A claim takes every volume or none.

2. **Check each new volume**, that is, a row with `pv_name` null. Read the StorageClass `storage_class` with the uncached API reader.
   - Not found: fail with "StorageClass `<class>` for volume `<name>` doesn't exist."
   - `reclaimPolicy` is not `Retain`: fail with "StorageClass `<class>` for volume `<name>` has reclaimPolicy `<policy>`. Persistent volumes need Retain, or the data is deleted when the instance stops."

3. **Check each recorded volume**, that is, a row with `pv_name` set. Read the PV with the uncached API reader.
   - **Not found.** Clear `pv_name` (compare-and-set on the old value), write `volume.lost`, count `lost`, and treat the volume as new (step 2). The next instance starts with an empty volume, as the issue asks.
   - **Wrong labels.** If `toolgate.soliddowant.dev/volume-id` is not the row's `id`, or `toolgate.soliddowant.dev/template-uid` is not the row's `template_uid`, or `app.kubernetes.io/managed-by` is not `toolgate`: fail with "PersistentVolume `<pv>` doesn't carry toolgate's labels for volume `<name>`, so toolgate won't use it." Set `problem`. toolgate changes nothing on that PV.
   - **Reclaim policy.** If `spec.persistentVolumeReclaimPolicy` is not `Retain`: fail with "PersistentVolume `<pv>` has reclaim policy `<policy>`, so toolgate won't bind it." Set `problem`.
   - **Phase `Bound`**, with `spec.claimRef` naming this instance's claim (`tg-<id>`, volume name): go on.
   - **Phase `Bound`** to any other claim: **wait**, with the reason "Volume `<name>` is still bound to `<namespace>/<claim>`."
   - **Phase `Released` or `Available`:** go on to step 5.
   - **Phase `Pending` or `Failed`:** fail with "PersistentVolume `<pv>` is `<phase>`: `<status.message>`."

4. **Prepare the namespace** as today (`prepareNamespace`).

5. **Pre-bind each recorded PV** whose `claimRef` doesn't name this instance's claim yet. Patch `spec.claimRef` to `{apiVersion: v1, kind: PersistentVolumeClaim, namespace: tg-<id>, name: <volume name>}`, with no `uid` and no `resourceVersion`. Use a merge patch with an optimistic lock (`client.MergeFromWithOptions(old, client.MergeFromWithOptimisticLock{})`). A conflict ends the step, and the next pass starts again at step 3. This is the issue's "clear `spec.claimRef`" step. toolgate never sets `claimRef` to empty (§16, choice 2).

6. **Apply the PVCs** (§2), with `volumeName` set for a recorded volume. A new volume has no `volumeName`, so the class's provisioner makes a PV.

7. **Apply the template objects** (`applyAll`) and `MarkStarting`, as today.

8. **Record each new PV.** On each `checkStarting` step, for each held row with `pv_name` null:
   - Read the PVC. If it is not `Bound`, wait. Classes with `volumeBindingMode: WaitForFirstConsumer` bind only after the pod is scheduled.
   - Read the PV that `spec.volumeName` names. If it carries a `toolgate.soliddowant.dev/volume-id` label of another row, fail with "PersistentVolume `<pv>` belongs to another volume." This stops a claim from taking another user's PV.
   - If its reclaim policy is not `Retain`, fail as in step 3.
   - Add the labels of §2 with a merge patch with an optimistic lock.
   - Then write `pv_name` and `size_bytes` (the PV's `spec.capacity.storage`) with a compare-and-set on `pv_name IS NULL AND instance_id = <this instance>`. Write `volume.create` and count `created`.

   **An instance is not `ready` until every row it holds has `pv_name`.** `checkStarting` checks this before the MCP `initialize`. So a ready instance never has an unrecorded PV.

**Waiting.** A wait leaves the row `pending`, sets `instances.reason` to the wait reason, and returns. A wait ends when the next pass finds the cause gone. `startPending` now checks `timedOut` for a `pending` row that waits. After `startTimeout`, counted from `attempt_started_at`, the instance fails with "Timed out waiting for volume `<name>`: `<wait reason>`". A call waiting for the instance gets that reason, as today.

**A recorded volume ignores template changes to its size and class.** The PVC asks for the row's `size_bytes` and `storage_class`, so it binds to the existing PV. A new size or class applies to new volumes only (§13).

## 5. Stopping an instance

`Teardown.Advance` changes for a `stopping` row with its own namespace that holds at least one volume. Rows that hold none behave as today. The steps run before the namespace is deleted:

1. **Record unrecorded PVs.** Run §4 step 8 for each held row with `pv_name` null. A PVC that never bound has nothing to record. Teardown does not fail the instance here, because it is already stopping. A label conflict sets `problem` and skips that row.
2. **Scale to zero.** While `now() - stopping_at` is less than `VolumeDrainTimeout` (2 minutes, scaled by `internal/testtime`):
   - set `spec.replicas` to 0 with a merge patch on each Deployment and StatefulSet in the namespace whose `spec.replicas` is not 0. This covers objects that a HelmRelease made, because the whole namespace belongs to the instance;
   - list the Pods in the namespace. If a Pod that is not `Succeeded` or `Failed` mounts one of the held claims, set the reason "Waiting for pods to stop before the namespace is deleted." and return. The next pass checks again.
3. **Delete the namespace** as today. After `VolumeDrainTimeout`, toolgate deletes it even if pods remain, logs a warning, and counts `drain_timeout`. The PVC's `kubernetes.io/pvc-protection` finalizer still keeps the PVC until its pods are gone, so the data stays safe.
4. **Release.** In every transaction that sets a row `stopped`, run `UPDATE persistent_volumes SET instance_id = NULL, last_used_at = now() WHERE instance_id = $1`. The PVC is gone at this point, so the PV is `Released`, or becomes `Released` within the PV controller's next sync.

**The sweep** (`internal/runtime/sweep.go`) deletes namespaces that belong to no live instance. A `stopping` row is not live, so today the sweep can delete a namespace during the drain. The sweep now skips a namespace whose instance row is `stopping`. Teardown deletes it.

**Replacement and restart.** A replace (phase 2 §6) inserts the new row while the old one is `stopping`. The new row waits at §4 step 1 until the old row is `stopped`. So a restart of a template with volumes takes about the drain time plus the namespace deletion longer than before.

## 6. One holder at a time

A volume is ReadWriteOnce, and two instances must never write it at once. The rule: **only the instance in `instance_id` may bind the volume, and a claim changes `instance_id` only under the row lock, only when the holder is null or the holder's row is `stopped` or gone.**

Cases where two instances want one (template, volume, scope ID):
- **A replace or restart.** The old row is `stopping`, and the new one is `pending`. The new one waits until the old one releases (§5, step 4).
- **A health-check repair** with `replace`. Same as above.
- **A per-user template, two sessions of one user.** They share one instance (phase 1's advisory lock and `instances_live_idx`), so there is no second claimer.
- **A shared template.** One live instance at a time, as above.
- **A stop and a start with no overlap** (idle stop, then enable). The new row waits until the old row is `stopped`.

The second instance waits, bounded by `startTimeout`. It never takes the volume early and never gets a different volume. The Kubernetes binding gives a second guard: a PV pre-bound to one claim (§4, step 5) binds to no other claim.

## 7. Crash recovery

- **The MCP server crashes.** Its pod restarts in the same namespace with the same PVC. If the instance fails or is replaced, the volume follows §5 and §6. The volume is never deleted. The next instance binds to it.
- **The leader dies at any step.** The new leader steps the same rows again. Every step is safe to repeat:
  - a claim already held by this instance is kept (§4, step 1);
  - a PV already bound or pre-bound to this claim is left alone (§4, steps 3 and 5);
  - server-side apply of the PVC is the same as before;
  - recording repeats the label patch, then the compare-and-set (§4, step 8). A PV that is labelled but not yet recorded gets recorded by the next step;
  - the drain bound counts from `stopping_at`, which is in PostgreSQL.
- **The leader dies after a pre-bind, and the instance then fails.** The PV stays `Available` with a `claimRef` to a claim in a namespace that is gone. Only that claim name in that namespace can bind it, and instance IDs are never used again. The next start pre-binds it again (§4, steps 3 and 5).
- **The instance namespace is deleted by hand.** The PVC goes, and the PV becomes `Released`. If the row was `starting` or `ready`, the sweep sends it back to `pending` (`Sweep.restart`). It still holds the volume, and §4 binds the PV again. If the PV was never recorded, it is lost to toolgate (§13).

## 8. Clean-up

The leader runs `VolumeCleaner` on each full pass, after `Templates` is loaded (`AfterLoaded`). It is a `Pass` wrapper, like `CatalogCleaner`.

**Deciding.** Each of these sets `delete_reason`, `delete_requested_at = now()` and writes `volume.delete`. Each takes the row lock and acts only when `instance_id` is null:
- **Template deleted.** `Templates.Running(template_ns/template_name)` finds no template, or one with a different UID. Reason `template-deleted`. An invalid template keeps its last valid generation (`SPEC.md` §8), so it counts as present.
- **Unused.** `last_used_at` is older than `persistentVolumes.unusedRetention`, and the setting is not 0. Reason `unused`.
- **Admin.** `DELETE /api/v1/admin/volumes/{id}` sets the reason `admin` on any row, held or not (§11). A held volume is deleted after its holder releases it.

A held row is never deleted. A start that finds `delete_reason` set waits (§4, step 1). After the deletion, it inserts a new row, and the provisioner makes a new, empty volume.

**Deleting.** For each row with `delete_reason` set and `instance_id` null:
1. If `pv_name` is null, delete the row.
2. Read the PV. If it is not found, delete the row.
3. If its labels don't match the row (§4, step 3), toolgate never touches it. Set `problem` to "PersistentVolume `<pv>` doesn't carry toolgate's labels for this volume, so toolgate didn't delete it.", delete the row, and count `delete_failed`. The admin finds it in the log.
4. If it is `Bound`, it is bound to a claim that isn't toolgate's live claim. Set `problem` and try again on the next pass.
5. If `reclaim_patched_at` is null, set `spec.persistentVolumeReclaimPolicy: Delete` with a merge patch with an optimistic lock, and set `reclaim_patched_at`. For a `Released` PV, the PV controller then hands it to its provisioner, which deletes the storage and the PV.
6. If the PV is `Available`, or `reclaim_patched_at` is more than 10 minutes old (`VolumeReclaimWait`, scaled), delete the PV object with a UID precondition. On Kubernetes 1.33 and later, a CSI provisioner deletes the storage of a deleted PV that has the `Delete` policy.
7. When a read finds the PV gone, delete the row and count `deleted`.

**Per-user size limit.** The setting `persistentVolumes.limitPerUser` (default `10Gi`, `0` for no limit) bounds the sum of `size_bytes` of a user's `per-user` rows, rows being deleted included. It applies only when a start makes a new row (§4, step 1). When the new volume would pass the limit, the instance fails with:

> "`<server>` needs a `<size>` volume `<name>`, but your volumes already use `<used>` of your `<limit>` limit. Ask an admin to delete a volume you no longer need (toolgate admin volumes)."

It writes `instance.fail` with this reason and counts `limit_refused`. The session gets the reason, like any start failure. An existing volume always binds, even when the user is over the limit. Lowering the limit deletes nothing. `per-template` volumes count toward no user.

## 9. Permissions

**ClusterRole rules**, added by the chart only when `config.persistentVolumes.enabled` is `true`, through a helper `toolgate.persistentVolumes`, like `toolgate.embeddingsScaleToZero`:

```yaml
- apiGroups: [""]
  resources: ["persistentvolumes"]
  verbs: ["get", "patch", "delete"]
- apiGroups: ["storage.k8s.io"]
  resources: ["storageclasses"]
  verbs: ["get"]
```

toolgate reads PVs and StorageClasses one at a time with the manager's uncached API reader (`mgr.GetAPIReader()`), so it needs no `list` or `watch`. Without the feature, the gateway has no rights on PVs. The existing rules already cover PVCs, Pods, and patches of Deployments and StatefulSets.

**Admission policy.** The ValidatingAdmissionPolicy denies every cluster-scoped write except Namespaces. Kubernetes RBAC can't narrow PV rights by label, so the policy does it. With the feature on, the chart adds the variable `isOwnVolume` to the OR list of the first two validations, and two more validations:

```yaml
- name: isVolume
  expression: request.kind.group == "" && request.kind.kind == "PersistentVolume" && variables.subResource == ""
- name: isOwnVolume
  expression: >-
    variables.isVolume && (request.operation == "UPDATE" || request.operation == "DELETE") && (
      oldObject.metadata.labels[?"toolgate.soliddowant.dev/volume-id"].orValue("") != "" ||
      (request.operation == "UPDATE" && has(oldObject.spec.claimRef) &&
       oldObject.spec.claimRef.namespace.startsWith("tg-")))
validations:
  # Keep the labels that tie a PV to its row, and point claims only at instance namespaces.
  - expression: >-
      !variables.isVolume || request.operation != "UPDATE" || (
      object.metadata.labels[?"app.kubernetes.io/managed-by"].orValue("") == "toolgate" &&
      object.metadata.labels[?"toolgate.soliddowant.dev/volume-id"].orValue("") != "" &&
      (oldObject.metadata.labels[?"toolgate.soliddowant.dev/volume-id"].orValue("") == "" ||
       object.metadata.labels["toolgate.soliddowant.dev/volume-id"] ==
         oldObject.metadata.labels["toolgate.soliddowant.dev/volume-id"]) &&
      (!has(object.spec.claimRef) || object.spec.claimRef.namespace.startsWith("tg-")))
    message: toolgate's service account may change only PersistentVolumes it labelled, and may bind them only to claims in tg-* namespaces
    reason: Forbidden
  # Delete only after the reclaim policy is Delete, so that deleting frees the storage.
  - expression: >-
      !variables.isVolume || request.operation != "DELETE" ||
      oldObject.spec.persistentVolumeReclaimPolicy == "Delete"
    message: toolgate's service account may delete a PersistentVolume only after setting its reclaim policy to Delete
    reason: Forbidden
```

The first labelling (§4, step 8) is the only change to a PV without toolgate's label. It is allowed only while the PV is bound to a claim in a `tg-` namespace, which only toolgate makes. In code, toolgate also checks the row's labels before each change (§4, step 3), so it never changes a PV it didn't record and label.

## 10. Settings

Flat settings in `internal/config`, in the generated reference:
| Setting | Default | Values | Environment variable |
|---|---|---|---|
| `persistentVolumes.enabled` | `false` | `true` or `false` | `TOOLGATE_PERSISTENT_VOLUMES_ENABLED` |
| `persistentVolumes.unusedRetention` | `720h` | `0` never deletes unused volumes. Otherwise at least `1h`. | `TOOLGATE_PERSISTENT_VOLUMES_UNUSED_RETENTION` |
| `persistentVolumes.limitPerUser` | `10Gi` | A Kubernetes quantity. `0` means no limit. | `TOOLGATE_PERSISTENT_VOLUMES_LIMIT_PER_USER` |

The chart sets them from `config.persistentVolumes.enabled`, `.unusedRetention` and `.limitPerUser`.

**Turning the feature off.** Templates with `persistentVolumes` become invalid. Their running instances keep running, as for any invalid template. The reconciler does no volume work: no claims, no records, no drain and no clean-up. Rows and PVs stay. Turning it on again goes on from the rows.

## 11. API and CLI

Both routes need an admin: a user identity whose groups hold `adminGroup` (`audittail.IsAdmin`). Anyone else gets 403. Without an `adminGroup`, they answer 403 to everyone.

**`GET /api/v1/admin/volumes?owner=<principal>&after=<id>`** returns at most 200 rows, ordered by `id`:

```json
{
  "volumes": [{
    "id": "3f…",
    "server": "eparts",
    "templateNamespace": "toolgate-catalog",
    "volume": "cache",
    "scope": "per-user",
    "owner": "alice-sub",
    "storageClass": "ssd-replicated-3x-block-retain",
    "sizeBytes": 1073741824,
    "persistentVolume": "pvc-81c…",
    "inUseBy": "9a…",
    "createdAt": "2026-11-13T10:00:00Z",
    "lastUsedAt": "2026-11-13T12:00:00Z",
    "deleteReason": "",
    "problem": ""
  }],
  "next": "3f…"
}
```

`owner` is empty for `per-template` rows. `inUseBy` and `persistentVolume` are null when unset. `next` is absent on the last page.

**`DELETE /api/v1/admin/volumes/{id}`** sets `delete_reason = 'admin'` and `delete_requested_at` when `delete_reason` is empty, and writes `volume.delete` with the admin as principal. It answers 202 with the row. A row already being deleted answers 202 and changes nothing. An unknown ID answers 404. The leader deletes the PV (§8). A held volume is deleted after its instance stops, and the answer's `inUseBy` says which instance.

The OpenAPI document is regenerated.

**CLI.** These call the API with the signed-in user's token, like `toolgate sessions`. Unlike `toolgate admin rewrap`, they need no gateway configuration file.
- `toolgate admin volumes list [--owner PRINCIPAL] [--json]` prints a table: ID, server, volume, scope, owner, size, in use by, last used, deletion. It follows `next` to the end, at most 50 pages.
- `toolgate admin volumes delete ID...` deletes each ID and prints the result. It exits 1 if any ID is unknown.

## 12. Metrics and audit

**Metrics.** All labels are fixed enums. The series go in the budget of `internal/metricsbudget`.
- `toolgate_persistent_volumes{scope, state}`: gauge, set on each pass. `scope` is `per-template` or `per-user`. `state` is `in_use` (holder set), `idle` or `deleting`. Six series.
- `toolgate_persistent_volume_bytes{scope}`: gauge, the sum of `size_bytes`. Two series.
- `toolgate_persistent_volume_events_total{event}`: counter. `event` is `created`, `rebound`, `lost`, `deleted`, `delete_failed`, `drain_timeout` or `limit_refused`. Seven series.

`docs/admin/metrics.md` lists them with an alert on `delete_failed` and `drain_timeout`.

**Audit events**, each with `server` set to the template name and `principal` set to the owner (empty for `per-template`), unless stated:
- `volume.create`: a new PV was recorded. Details: `volume`, `scope`, `persistentVolume`, `sizeBytes`, `instance`.
- `volume.lost`: a recorded PV was missing at start, so the instance gets a new volume. Details: `volume`, `persistentVolume`, `instance`.
- `volume.delete`: a deletion was decided. Details: `volume`, `persistentVolume`, `reason` (`template-deleted`, `unused` or `admin`), `owner`. For `admin`, `principal` is the admin.

Rebinding writes no event: `instance.start` already records the start. A refusal at the limit is an `instance.fail` with its reason.

## 13. Interaction table

Earlier phases' rows still hold. Rows continue the numbering. Each row is one event combined with one operation in flight, and its covering test proves its rule. Tests are named in §14.

| # | Event | In flight | Rule | Test |
|---|---|---|---|---|
| 279 | Template `size` grows or shrinks | Volume recorded | The PVC asks for the row's size and binds to the old PV. New volumes get the new size. No expansion. | integration |
| 280 | Template `storageClassName` changes | Volume recorded | The row keeps its class, and the PVC uses it. New volumes use the new class. | integration |
| 281 | Template `scope` changes | Instances running | Instances stop (`SPEC.md` §9). New instances use rows of the new scope ID. The old rows age out by `unusedRetention`. | integration |
| 282 | A volume is removed from the template, or renamed | Instances running | Old instances keep their claim until they stop. The row is released and ages out by `unusedRetention`. | integration |
| 283 | Template deleted | Instance running and holding | The instance stops (`SPEC.md` §9). After the release, the clean-up deletes the volume with `template-deleted`. | e2e |
| 284 | Template deleted and re-created with the same name | — | New UID, so new rows and empty volumes. The old rows are deleted as `template-deleted`. | integration |
| 285 | Instance stops (idle, disable, session end) | Pods writing | Scale to zero, wait for the pods up to 2 min, delete the namespace, release in the `stopped` write. | e2e |
| 286 | Restart or replace | Old instance holds | New row waits in `pending` until the old row is `stopped`, then claims. Bounded by `startTimeout`. | integration, `-count=50 -race` |
| 287 | Two claims for one volume at once | Each other | The row lock orders them. One holds. The other waits. | integration, `-count=50 -race` |
| 288 | Instance fails at start | Volume claimed, maybe bound | Teardown records an unrecorded bound PV, drains, deletes the namespace and releases. | integration |
| 289 | MCP server pod crashes | Instance `ready` | Same namespace, same PVC, data kept. If the instance is replaced, row 286 applies. | e2e |
| 290 | Leader dies during claim, pre-bind, PVC apply or record | — | The new leader repeats the step. Each step is safe to repeat (§7). | integration |
| 291 | Leader dies during the drain | Pods stopping | The new leader keeps the bound from `stopping_at`. | integration |
| 292 | Recorded PV deleted by hand | No instance | Next start: `volume.lost`, `pv_name` cleared, a new empty volume. | e2e |
| 293 | Recorded PV deleted by hand | Instance `ready` | Kubernetes keeps the PV until the PVC is gone (`kubernetes.io/pv-protection`). After the stop, row 292 applies. | integration |
| 294 | Recorded PV still `Bound` to an old claim | Start | Wait with the claim in the reason. After `startTimeout`, fail. | integration |
| 295 | Recorded PV lost its labels, or has another row's ID | Start or clean-up | Start fails. Clean-up forgets the row. toolgate never changes that PV. | integration |
| 296 | Recorded PV's reclaim policy changed to `Delete` | Start | Start fails. toolgate doesn't bind it. | integration |
| 297 | StorageClass missing | Start of a new volume | Fail with the class in the reason. A recorded volume doesn't need its class. | integration |
| 298 | StorageClass has `reclaimPolicy: Delete` | Start of a new volume | Fail with the Retain reason. Nothing is provisioned. | e2e |
| 299 | Feature turned off | Instances running, rows present | Templates become invalid, and running instances keep running. No volume work, and nothing deleted. | integration |
| 300 | User at the size limit | Start of a new per-user volume | Fail with the limit reason, `instance.fail`, `limit_refused`. Existing volumes still bind. | integration |
| 301 | Two new volumes for one user at once, near the limit | Each other | The per-user advisory lock orders them. Only the one that fits starts. | integration, `-count=50 -race` |
| 302 | Clean-up decides a deletion | Start for the same row | Both take the row lock. Clean-up only marks a row with no holder. A start that finds the mark waits, then gets a new volume. | integration, `-count=50 -race` |
| 303 | Admin deletes a held volume | Instance running | 202. Deleted after the release. | integration |
| 304 | Clean-up running | Leader change | The new leader reads `delete_reason` and `reclaim_patched_at` and goes on. | integration |
| 305 | PV deleted by hand | Clean-up | Not found, so the row is deleted. | integration |
| 306 | Sweep pass | Instance `stopping`, drain under way | The sweep skips the namespace of a `stopping` row. | integration |
| 307 | Namespace stuck `Terminating` | New start waits | The old row stays `stopping`, so the new start fails after `startTimeout` with the wait reason. | integration |
| 308 | Dynamic PVC binds to a PV with another row's ID | Record | Fail. The PV is not recorded. | unit |
| 309 | Non-admin calls the volume API | — | 403. | integration |

## 14. Tests

Test names describe behaviour. Timing-sensitive tests pass `-count=50 -race`.

**Unit** (`internal/catalog`, `internal/runtime`):
- `TestValidatePersistentVolumes`: each problem of §2, feature on and off, every scope pair in the table.
- `TestRenderVolumeClaim`: labels, annotations, namespace, size and class from the row, `volumeName` only when recorded.
- `TestVolumeStartChecks`: each phase and label case of §4 step 3 against a fake client, with the exact reasons.
- `TestRecordRefusesAnotherVolumesPV` (row 308).

**Integration** (PostgreSQL, envtest). envtest runs no controllers, so the tests set PV phases and PVC binding by hand through the status subresource:
- `TestVolumeClaimIsExclusive` (rows 286, 287), `-count=50 -race`.
- `TestVolumeLimitPerUser` (rows 300, 301), `-count=50 -race`.
- `TestVolumeCleanupRacesStart` (row 302), `-count=50 -race`.
- `TestVolumeCleanup`: template deleted, unused, admin, held, missing PV, wrong labels, the reclaim wait (rows 283, 295, 303, 305).
- `TestVolumeTemplateChanges` (rows 279 to 282, 284).
- `TestVolumeStepsRepeatAfterLeaderChange` (rows 290, 291, 304): stop after each step and run a fresh starter or teardown.
- `TestTeardownDrainsVolumePods`: scale patch, the wait, the 2 min bound (row 291), the release in the `stopped` write.
- `TestSweepSkipsStoppingNamespaces` (row 306).
- `TestVolumeFeatureOff` (row 299).
- `TestAdminVolumesAPI`: list, owner filter, paging, delete, 404, 403 for a non-admin (rows 303, 309).
- `TestCRDValidatesPersistentVolumes` (`api/v1alpha1`, envtest): the three CEL rules, the name pattern and the size rule.
- `test/chart`: the ClusterRole has no PV or StorageClass rules by default, and exactly §9's rules when enabled. `admission_integration_test.go` gains cases: an unlabelled PV can't be patched or deleted, a PV bound in another namespace can't be labelled, a labelled PV can't lose its ID or point at a claim outside `tg-`, and a `Retain` PV can't be deleted.

**e2e** (kind). kind's default class `standard` uses the local-path provisioner with `reclaimPolicy: Delete`. The harness adds a class `toolgate-retain` with provisioner `rancher.io/local-path`, `reclaimPolicy: Retain` and `volumeBindingMode: WaitForFirstConsumer`. The local-path provisioner keeps a `Retain` PV's directory after release, and deletes it when the policy is `Delete` and the PV is `Released`. The cluster has one node, so the PV's node affinity always fits. The test MCP server gains two tools, `write_note` and `read_note`, which write and read one file in the directory named by `--data-dir`. The chart runs with `persistentVolumes.enabled: true` and the time scale of `internal/testtime`.
- `TestPersistentVolumeKeepsDataAcrossInstances`: write a note, let the instance stop, start again, read the note. The same PV name is in the row both times.
- `TestPersistentVolumeSurvivesCrash`: write a note, force-delete the MCP server pod with no grace period, and read the note. Then write again, kill the leader gateway pod while the instance stops, start again, and read the note.
- `TestPersistentVolumeIsolation`: a per-user template for two users, and a second template with the same volume name. Each reads only its own note.
- `TestPersistentVolumeCleanup`: delete a template, and the PV is gone. Let a volume pass a short `unusedRetention`, and its PV is gone. `toolgate admin volumes delete`, and its PV is gone.
- `TestPersistentVolumeNeedsRetainClass`: a template with class `standard` fails to start with the Retain reason (row 298).
- `TestPersistentVolumeLostIsReplaced`: delete the recorded PV by hand, start, and get an empty volume with `volume.lost` (row 292).

## 15. Acceptance

These are the issue's "Done when" items, as Given/When/Then.

1. **A template can declare persistent volumes with a size, class and scope. A scope wider than the template's is invalid.**
   Given a template with `persistentVolumes` and the feature on, when its volume scope matches the template's effective scope, then the template is `Valid`. When it is wider, then `Valid` is `False` and names the volume and both scopes.
   Tests: `internal/catalog: TestValidatePersistentVolumes`, `api/v1alpha1: TestCRDValidatesPersistentVolumes`.
2. **Data written by one instance is there for the next instance with the same (template, scope ID), including after a crash.**
   Given an instance that wrote a note to its volume, when the instance stops or crashes and a new instance starts for the same template and scope ID, then the new instance reads the note.
   Tests: `test/e2e: TestPersistentVolumeKeepsDataAcrossInstances`, `test/e2e: TestPersistentVolumeSurvivesCrash`.
3. **Another template or user never gets that volume.**
   Given two users of a per-user template and a second template with the same volume name, when each writes and then reads a note, then each reads only its own.
   Tests: `test/e2e: TestPersistentVolumeIsolation`, `internal/runtime: TestRecordRefusesAnotherVolumesPV`.
4. **Clean-up deletes volumes of deleted templates and long-unused volumes.**
   Given recorded volumes, when their template is deleted or they are unused for `unusedRetention`, then their PVs and rows are gone.
   Tests: `test/e2e: TestPersistentVolumeCleanup`, `internal/runtime: TestVolumeCleanup`.
5. **Docs: admin guide (the Retain StorageClass, RBAC, clean-up), and which servers benefit.**
   A new page `docs/admin/persistent-volumes.md` covers: making a Retain StorageClass, the chart value and the permissions it adds, the clean-up rules and settings, the admin commands, the size limit, what happens when a PV is changed by hand, and the kinds of servers that benefit (servers with caches, browser profiles or search indexes). `docs/admin/templates-and-rules.md` gains the field. `docs/admin/metrics.md`, the generated configuration and chart references, and `docs/README.md` are updated.
   Test: review.

## 16. Choices made

The issue leaves these open, or conflicts with the code. Each is the smallest option that meets the acceptance.

1. **Volume scope must match the template's scope.** A shared instance has no user, so `per-user` is impossible there. `per-template` on a per-user template is wider. So each template scope allows one volume scope, and per-session templates can't have volumes. The `scope` field stays, because the issue asks for it, and because it turns a later scope change into an invalid template rather than silently new volumes.
2. **Pre-bind, never clear.** The issue says to clear `spec.claimRef`. A PV with no `claimRef` is `Available` to any pending claim of its class in the cluster, including another user's dynamic claim. That would break acceptance item 3. So toolgate sets `claimRef` to the new claim's namespace and name, with no UID. Kubernetes then binds the PV to that claim only.
3. **`namespace: per-instance` only.** The PVC name is the volume name, which the pod spec uses as is. In a shared namespace, two instances of one template would need the same name.
4. **No bare Pods.** The issue says workloads are controllers. toolgate can scale a controller to zero, but not a Pod.
5. **`storageClassName` is required.** A cluster's default class usually has `reclaimPolicy: Delete`, which deletes the data at the first stop.
6. **The Retain check is a start failure, not a `Valid` reason.** `Valid` is decided from the template alone, on every replica, which don't read cluster objects. A class can also change after validation. A recorded volume checks its PV's own policy instead, so it doesn't depend on the class.
7. **Size and class changes apply to new volumes only.** Moving data between classes, or expanding a volume, is new work. An admin who wants the change deletes the volume.
8. **PV rights are `get`, `patch` and `delete`, without `list` and `watch`.** toolgate reads only PVs it recorded, by name. The issue lists `list` and `watch` too. They aren't needed, so they are left out. A StorageClass `get` is added for the Retain check.
9. **The admission policy gains PV rules.** The issue doesn't mention it, but the policy denies every cluster-scoped write today, so it must change.
10. **Clean-up sets the reclaim policy to `Delete` before deleting.** Deleting a `Retain` PV object leaves its storage behind. Setting `Delete` on a `Released` PV makes the provisioner delete the storage.
11. **Admin deletion of a held volume is deferred** until the holder stops. It never pulls a volume from under a running server.
12. **The drain is bounded at 2 minutes**, then the namespace is deleted anyway. A HelmRelease can scale its Deployment back up, and Kubernetes' PVC protection keeps the data safe. 2 minutes leaves room within the default 5 minute `startTimeout` for a restart.
13. **Waiting for the old holder counts toward `startTimeout`.** No new timeout.
14. **The limit defaults to `10Gi`, and applies only to new per-user volumes.** It never deletes data. `per-template` volumes count toward no user.
15. **The admin commands use the API**, so an admin needs only a sign-in, not the gateway's configuration.
16. **A deleted template's volumes are deleted at once**, with no grace time, as the issue says.
17. **An instance isn't `ready` until its PVs are recorded**, so a ready instance never has a PV that toolgate can lose.

**Principle 4 (secrets).** A server can write what it receives, its own tokens included, to its volume. Before this feature, that disk went away at each stop. Now it stays until clean-up. `docs/admin/persistent-volumes.md` and `docs/admin/security-model.md` say so. toolgate itself still puts no secret in a volume.

## 17. Not in scope

- Snapshots, and rollback to a snapshot.
- Per-session volumes.
- Volume expansion, and moving a volume to another class.
- Access modes other than ReadWriteOnce, and block volume mode.
- Volumes for remote templates and for templates in the shared namespace.
- Finding PVs that toolgate labelled but has no row for, for example after a database restore. The admin docs show a `kubectl` query by label.
- A web UI page for volumes, and a user command to list one's own volumes.
- Copying data between templates or users.
