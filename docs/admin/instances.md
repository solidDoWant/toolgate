# Instance stops and cleanup

For: people who deploy and operate toolgate.

An instance is one running copy of an MCP server from an `MCPServerTemplate`. This page tells you when toolgate stops an instance, how it removes it, and which metrics show problems.

## When toolgate stops an instance

The gateway replica that holds the leader Lease checks every instance every 30 seconds, and after each change in the database. It stops an instance when one of these conditions is true:

- A `per-session` instance: its session no longer has the server enabled, or its session ended. A session ends when it has no requests for `sessionIdleTimeout` (2 hours by default, chart value `config.sessionIdleTimeout`).
- A `per-user` instance: no live session of its owner has the server enabled, and it had no calls for `idleStop`. If it had no calls at all, the time counts from when it became ready.
- A `shared` instance: no live session has the server enabled, and it had no calls for `idleStop`. If `idleStop` is `0` or not set, a shared instance does not stop for this reason.
- The template was deleted, or deleted and created again.
- The `scope` of the template changed.
- The `access` of the template no longer includes the owner. toolgate uses the email and groups from the most recent request of the owner.

An ended session keeps its list of enabled servers for `sessionResumeWindow` (24 hours by default, from 1 hour to 7 days, chart value `config.sessionResumeWindow`). Its `per-session` instances stop and do not run during that time. After that time, the gateway deletes the list and records a `session.forget` audit event. A later request with the same session ID starts the session again with no servers enabled.

A request for an ended session within that time resumes it. If the request has the same workspace and attestation as the session, the session gets its servers back and the gateway writes a `session.resume` audit event with the list. The gateway decides policy again for each server and starts its instance, but it does not wait for the instance. If policy now refuses a server, or an instance limit is reached, the gateway removes the server and writes a `server.disable` audit event with the reason (`limit` for a limit). If the workspace or the attestation is different, the gateway deletes the list and writes `session.start`.

An invalid template does not stop its instances. They continue on the last valid generation.

## Updating outdated instances

An instance that started from an older version of its template is outdated. By default, an outdated instance keeps running. It is replaced only when a user or admin restarts it in the web UI, or when it stops. The template field `updatePolicy` changes this:

```yaml
spec:
  updatePolicy:
    mode: whenIdle   # manual (default), immediate or whenIdle
    idleFor: 5m      # only with whenIdle, at least 1m
```

- `manual`: toolgate does not replace outdated instances.
- `immediate`: when the template changes, toolgate replaces every ready outdated instance. Use it for servers that lose nothing when they restart. If the new template is bad, every new instance fails its readiness check. Fix the template to recover.
- `whenIdle`: toolgate replaces a ready outdated instance when it had no calls for `idleFor` and no call is running. Use it for servers that keep state, such as a browser. Set `idleFor` shorter than `idleStop` to update between tasks. An instance in steady use is not replaced until it goes quiet.

A replacement works like a restart from the web UI. The sessions keep the server enabled, and the new tools arrive with `list_changed`. The stop conditions above are checked first, and the audit event `instance.stop` has the reason `template-update`.

`updatePolicy` covers template changes only. It does not react to a changed secret. For a template that mounts a secret that changes, use a Deployment in `resources`. A tool such as Reloader or a Kyverno policy can then restart the Deployment when the secret changes.

## A deleted instance object

If an instance fails its health checks three times in a row and one of its objects is missing from the cluster, for example a Pod that someone deleted, toolgate applies the instance's objects again instead of marking the instance failed. It records the `instance.reapply` audit event. This counts as a repair, so the same limit applies: at most three repairs in 30 minutes. After that, the instance fails. If the instance has persistent volumes, toolgate also applies their claims again, so a claim that someone deleted comes back bound to the same volume. If a recorded volume is missing, has wrong labels, or is bound to another claim, the repair fails and escalates like any other.

## How toolgate removes an instance

toolgate deletes the namespace `tg-<instance id>` of the instance. Kubernetes then deletes all the objects in it. toolgate deletes only a namespace that has the label `toolgate.soliddowant.dev/instance` with the ID of the instance.

If persistent volumes are on and the instance holds at least one volume, toolgate first drains the instance. It records a volume that has no recorded PV yet. It scales every Deployment and StatefulSet in the namespace to zero. It waits until no pod that mounts a held claim is running. Only then does it delete the namespace. The wait has a bound of 2 minutes, counted from the moment the instance began to stop. When the bound passes, toolgate writes a warning to its log, counts the event `drain_timeout` in `toolgate_persistent_volume_events_total` and deletes the namespace. The data stays safe, because Kubernetes keeps a claim until the pods that use it are gone. While the drain waits, the reason of the instance is `Waiting for pods to stop before the namespace is deleted.` Instances that hold no volume are not drained.

An instance of a `namespace: shared` template runs in the shared namespace (see [The shared namespace](chart.md#the-shared-namespace)). toolgate does not delete the shared namespace. It deletes the objects in it that have the labels `app.kubernetes.io/managed-by: toolgate` and `toolgate.soliddowant.dev/instance` with the ID of the instance. toolgate cannot read Secrets. Before it applies the objects of a shared instance, toolgate creates a ConfigMap named `tg-<instance id>` in the shared namespace. This ConfigMap is the anchor of the instance. Each object of the instance has an owner reference to the anchor. When toolgate deletes the anchor, the Kubernetes garbage collector deletes the other objects of the instance, Secrets included. The record of the instance can show `stopped` a short time before the garbage collector deletes the Secrets.

Do not make a ConfigMap with a name that starts with `tg-` in the shared namespace. If the ConfigMap `tg-<instance id>` exists without the label of that instance, the instance fails.

A failed instance is removed at once. The database keeps the record of a stopped instance for 7 days, and then deletes it.

## Labels and annotations on instance objects

toolgate marks the namespace `tg-<instance id>` and every object it makes for an instance (pods, Deployments, Services, PVCs, network policies and the rest) with the keys below. In the shared namespace, the objects carry them too, because the namespace cannot say whose they are. Keys from the template are kept, but a template cannot change a key of toolgate.

Labels (you can select on them):

| Label | Value |
| --- | --- |
| `toolgate.soliddowant.dev/scope` | `per-session`, `per-user` or `shared`. |
| `toolgate.soliddowant.dev/template-namespace` | The namespace of the template. |
| `toolgate.soliddowant.dev/owner` | The first 16 hex characters of the SHA-256 of the owner's `sub`. Per-user and per-session only. |
| `toolgate.soliddowant.dev/attestation` | `attested` or `user`. Per-session only. |

Annotations (full values):

| Annotation | Value |
| --- | --- |
| `toolgate.soliddowant.dev/owner-subject` | The full `sub` of the owner. Per-user and per-session only. |
| `toolgate.soliddowant.dev/owner-email`, `toolgate.soliddowant.dev/owner-name` | The verified email and name of the owner, if the identity provider gives them. |
| `toolgate.soliddowant.dev/workspace` | The workspace ID of the session. Per-session only. |
| `toolgate.soliddowant.dev/workspace-verified` | `true` when the platform verified the workspace, `false` when the client only claimed it. |
| `toolgate.soliddowant.dev/session` | The toolgate session ID. Per-session only. |
| `toolgate.soliddowant.dev/template-generation`, `toolgate.soliddowant.dev/template-uid` | The generation and UID of the template that the instance started from. |
| `toolgate.soliddowant.dev/created-by` | The version of the gateway. |

Shared instances get only the scope and template keys. The values come from the verified identity of the session, never from a client header.

To list the namespaces of one user, or of one scope:

```sh
kubectl get ns -l toolgate.soliddowant.dev/owner=<owner id>
kubectl get ns -l toolgate.soliddowant.dev/scope=per-user
```

The owner ID is the same as the short owner ID that toolgate shows in its UI and CLI. To keep personal data off cluster objects, set the chart value `config.instanceOwnerDetails` to `false`. The gateway then leaves out the email and the name. The subject stays.

toolgate sets these keys when it creates an object. Instances that were created before an upgrade get them when they start again.

## Recovery after a restart or a leader change

The replica that holds the leader Lease compares Kubernetes with the database every 30 seconds. It does this also when it takes the Lease after a restart or after the previous holder stopped. It lists the instance namespaces first, and then reads the instance records:

- If a namespace has the labels `app.kubernetes.io/managed-by: toolgate` and `toolgate.soliddowant.dev/instance`, but no running instance has that ID, toolgate deletes the namespace.
- If the namespace of a starting or ready instance is missing, toolgate starts the instance again from the current version of its template.
- If an instance was stopping, toolgate deletes its namespace again. The sweep skips the namespace of a stopping instance, so the drain is not cut short. Teardown deletes it.
- In the shared namespace, toolgate deletes each object with the label `toolgate.soliddowant.dev/instance` when no running instance has that ID. The garbage collector then deletes the Secrets that the deleted anchor owns. toolgate never deletes the shared namespace.

Do not put the label `toolgate.soliddowant.dev/instance` on namespaces that you make yourself. toolgate deletes them.

## Upstream sessions

A server can keep one MCP session (a connection with its own state) for each user session. The leader ends these sessions every 30 seconds, and after each change in the database. It ends a session when its instance is not running, its user session ended, or the user session no longer has the server enabled.

The leader then sends one `DELETE` request for each ended session to a running instance or a remote server. It sends no `DELETE` to a stopped instance, because its pods are going away. For a remote server, it uses the headers of the instance's owner. If it cannot fill in the headers, for example because the secret is deleted, it sends nothing. The leader does not retry a failed `DELETE`. It counts each one in `toolgate_upstream_session_events_total` with the event `delete_sent` or `delete_failed`.

If a server answers a call with HTTP 404 for its session, the server no longer knows that session, for example after a restart. toolgate then opens a new session and sends the call one more time. A second 404 fails the call. The state that the server kept for the old session is lost.

Each replica keeps its own connection to each session that it uses. A replica closes a connection when it served no call for 30 minutes, when the user session ends, or when the user disables the server. Closing a connection does not end the session on the server.

## Instance limits

Two chart values limit the number of instances. Only instances that are starting or ready count:

- `config.limits.instancesPerUser` (default 20) is the maximum for one user. Per-session and per-user instances count for the user who started them. Shared instances do not count for a user.
- `config.limits.instances` (default 200) is the maximum for the gateway. All instances count, including shared instances.

Both values must be at least 1. If a value is lower, `helm install` and `helm upgrade` fail.

A limit applies only when an enable must start a new instance. If the instance already runs, the enable works at the limit. For example, a user at the limit can use a running per-user server from a second session.

If a limit refuses an enable, the user gets a message that names the limit. The audit log gets a `server.enable` event with `decision: refuse` and `reason: limit`. The field `limit` tells which limit refused it: `per-user` or `gateway`.

## Start reasons for persistent volumes

A template with `persistentVolumes` (see [Persistent volumes](persistent-volumes.md)) can make an instance wait or fail before its objects are applied. A waiting instance stays `pending`, and its reason tells you what it waits for. These are the wait reasons:

- `Volume <name> is still held by instance <id>, which is <status>.` The old instance did not stop yet.
- `Volume <name> is still bound to <namespace>/<claim>.` The PV is still bound to the claim of an old instance.
- `Volume <name> is being deleted.` An admin or the clean-up marked the volume for deletion.

If a wait lasts longer than the `startTimeout` of the template, the instance fails with `Timed out waiting for volume <name>: <wait reason>`. The time counts from the start of the attempt.

These reasons make the instance fail at once:

- `persistentVolumes needs the gateway setting persistentVolumes.enabled`: the feature is off.
- `StorageClass <class> for volume <name> doesn't exist.`
- `StorageClass <class> for volume <name> has reclaimPolicy <policy>.` and the rest of that message: the class does not keep the data.
- `PersistentVolume <pv> has reclaim policy <policy>, so toolgate won't bind it.`
- `PersistentVolume <pv> doesn't carry toolgate's labels for volume <name>, so toolgate won't use it.`
- `PersistentVolume <pv> wasn't provisioned for volume <name>.` or `PersistentVolume <pv> belongs to another volume.`: the claim bound to a PV that toolgate did not make for it.
- `<server> needs a <size> volume <name>, but your volumes already use <used> of your <limit> limit.` and the rest of that message: the size limit for one user. The audit log also gets an `instance.fail` event.

## Start order with volumes and token exchange

A template can have both `persistentVolumes` and `${exchange:…}` (see [Token exchange](token-exchange.md)). Its instance then starts in this order:

1. toolgate claims the volumes and checks them. If a volume is not free, the instance waits with one of the wait reasons above.
2. When every volume is free, toolgate asks for the exchanged tokens. The request of the user runs the exchange and sends the tokens to the leader.
3. toolgate renders the template, creates the claims for the volumes and applies the objects.

No exchange runs while the instance waits for a volume. So an exchanged token never expires while the instance waits.

Both waits share one `startTimeout`. The time counts from the start of the attempt, not from the end of the volume wait. If the time passes in the volume wait, the reason is `Timed out waiting for volume <name>: <wait reason>`. If the time passes in the token wait, the reason is the token reason of [Token exchange](token-exchange.md).

## Metrics

The gateway serves these metrics on its metrics port. Only the replica that holds the leader Lease sets them. The other replicas report `0`.

| Metric | Labels | What it counts |
|---|---|---|
| `toolgate_instances` | `scope`, `status` | Instances that are not stopped. |
| `toolgate_instance_namespaces_stuck` | none | Instance namespaces that stayed `Terminating` for more than 10 minutes. |

A namespace stays `Terminating` when a finalizer does not complete. When this happens for more than 10 minutes, toolgate writes a warning to its log and stores the namespace conditions as the reason of the instance. The instance does not block other instances.

These Prometheus alert rules are examples. Change the numbers to fit your `limits.instances` value:

```yaml
groups:
  - name: toolgate-instances
    rules:
      - alert: ToolgateNamespaceStuck
        expr: max(toolgate_instance_namespaces_stuck) > 0
        for: 15m
        annotations:
          summary: An instance namespace did not finish deleting.
      - alert: ToolgateManyInstances
        expr: sum(toolgate_instances) > 150
        for: 10m
        annotations:
          summary: The gateway is near its limit of 200 instances.
```

If `ToolgateNamespaceStuck` fires, find the namespace with `kubectl get namespace -l toolgate.soliddowant.dev/instance`. Then read its conditions with `kubectl describe namespace <name>`.
