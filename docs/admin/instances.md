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

## How toolgate removes an instance

toolgate deletes the namespace `tg-<instance id>` of the instance. Kubernetes then deletes all the objects in it. toolgate deletes only a namespace that has the label `toolgate.soliddowant.dev/instance` with the ID of the instance.

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
- If an instance was stopping, toolgate deletes its namespace again.
- In the shared namespace, toolgate deletes each object with the label `toolgate.soliddowant.dev/instance` when no running instance has that ID. The garbage collector then deletes the Secrets that the deleted anchor owns. toolgate never deletes the shared namespace.

Do not put the label `toolgate.soliddowant.dev/instance` on namespaces that you make yourself. toolgate deletes them.

## Instance limits

Two chart values limit the number of instances. Only instances that are starting or ready count:

- `config.limits.instancesPerUser` (default 20) is the maximum for one user. Per-session and per-user instances count for the user who started them. Shared instances do not count for a user.
- `config.limits.instances` (default 200) is the maximum for the gateway. All instances count, including shared instances.

Both values must be at least 1. If a value is lower, `helm install` and `helm upgrade` fail.

A limit applies only when an enable must start a new instance. If the instance already runs, the enable works at the limit. For example, a user at the limit can use a running per-user server from a second session.

If a limit refuses an enable, the user gets a message that names the limit. The audit log gets a `server.enable` event with `decision: refuse` and `reason: limit`. The field `limit` tells which limit refused it: `per-user` or `gateway`.

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
