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

An invalid template does not stop its instances. They continue on the last valid generation.

## How toolgate removes an instance

toolgate deletes the namespace `tg-<instance id>` of the instance. Kubernetes then deletes all the objects in it. toolgate deletes only a namespace that has the label `toolgate.soliddowant.dev/instance` with the ID of the instance.

An instance of a `namespace: shared` template runs in the shared namespace (see [The shared namespace](chart.md#the-shared-namespace)). toolgate does not delete the shared namespace. It deletes the objects in it that have the labels `app.kubernetes.io/managed-by: toolgate` and `toolgate.soliddowant.dev/instance` with the ID of the instance. toolgate cannot read Secrets. Before it applies the objects of a shared instance, toolgate creates a ConfigMap named `tg-<instance id>` in the shared namespace. This ConfigMap is the anchor of the instance. Each object of the instance has an owner reference to the anchor. When toolgate deletes the anchor, the Kubernetes garbage collector deletes the other objects of the instance, Secrets included. The record of the instance can show `stopped` a short time before the garbage collector deletes the Secrets.

Do not make a ConfigMap with a name that starts with `tg-` in the shared namespace. If the ConfigMap `tg-<instance id>` exists without the label of that instance, the instance fails.

A failed instance is removed at once. The database keeps the record of a stopped instance for 7 days, and then deletes it.

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
