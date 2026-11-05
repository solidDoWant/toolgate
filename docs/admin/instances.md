# Instance stops and cleanup

For: people who deploy and operate toolgate.

An instance is one running copy of an MCP server from an `MCPServerTemplate`. This page tells you when toolgate stops an instance, how it removes it, and which metrics show problems.

## When toolgate stops an instance

The gateway replica that holds the leader Lease checks every instance every 30 seconds, and after each change in the database. It stops an instance when one of these conditions is true:

- A `per-session` instance: its session no longer has the server enabled. A session that ends loses all its enabled servers.
- A `per-user` instance: no live session of its owner has the server enabled, and it had no calls for `idleStop`. If it had no calls at all, the time counts from when it became ready.
- A `shared` instance: no live session has the server enabled, and it had no calls for `idleStop`. If `idleStop` is `0` or not set, a shared instance does not stop for this reason.
- The template was deleted, or deleted and created again.
- The `scope` of the template changed.
- The `access` of the template no longer includes the owner. toolgate uses the email and groups from the most recent request of the owner.

An invalid template does not stop its instances. They continue on the last valid generation.

## How toolgate removes an instance

toolgate deletes the namespace `tg-<instance id>` of the instance. Kubernetes then deletes all the objects in it. toolgate deletes only a namespace that has the label `toolgate.soliddowant.dev/instance` with the ID of the instance.

A failed instance is removed at once. The database keeps the record of a stopped instance for 7 days, and then deletes it.

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
