# Template secrets

For: admins who give MCP servers a value that all users share, such as an API key of a team account.

A template secret is a value from a Kubernetes Secret that you keep in one namespace. A template reads it as `${templateSecret:NAME/KEY}`. `NAME` is the name of the Secret and `KEY` is a key in its `data`. The gateway copies the value into the Secret of each instance. A template cannot name another namespace.

The gateway reads template secrets only when you set `config.templateSecretsNamespace`. If the value is empty, the feature is off.

## Set up the namespace

1. Create the namespace, for example `toolgate-template-secrets`. The chart does not create it. Flux can create it from your Git repository.
2. Do not put the label `toolgate.soliddowant.dev/writable` on the namespace. Without the label, the admission policy refuses every write of the gateway there. The gateway can then read the namespace but never change it.
3. Put your Secrets in the namespace. You can store them with Flux and SOPS.
4. Set `config.templateSecretsNamespace` in the chart values to the name of the namespace, and upgrade the release.

The chart then adds a `Role` and a `RoleBinding` named `RELEASE-template-secrets` in that namespace. They let the gateway `get`, `list` and `watch` Secrets there. The `ClusterRole` of the gateway does not change. It still has no read access to Secrets. So Kubernetes RBAC, not the gateway, limits the reads to this one namespace.

The name must follow these rules. The chart refuses to render, and the gateway refuses to start, when one rule fails:

- It is a valid namespace name.
- It does not start with `tg-`, the prefix of instance namespaces.
- It is not the namespace of the gateway. That namespace holds the keys, the database credentials and the client secrets of the gateway.
- It is not `config.sharedNamespace.name`, where the shared instances run.
- It is not `kube-system`.
- It is not one of `config.catalogNamespaces`, where template authors write.

## Example

This Secret holds a team API key:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: keys
  namespace: toolgate-template-secrets
stringData:
  api-key: example-value
```

A `shared` template uses the key in a Secret of its own instance:

```yaml
apiVersion: toolgate.soliddowant.dev/v1alpha1
kind: MCPServerTemplate
metadata:
  name: team-search
  namespace: toolgate-catalog
spec:
  scope: shared
  # endpoint and the other fields are as in any template
  resources:
    - apiVersion: v1
      kind: Secret
      metadata: {name: team-search-key-${instance.id}}
      stringData:
        api-key: ${templateSecret:keys/api-key}
```

The variable must be the whole value, with no other text around it. It can be used only in the `data` or `stringData` values of a Secret.

## Changes to a source Secret

The gateway watches the namespace. When you change a source Secret, the gateway patches the new value into the Secret of each running instance that uses it. It does not restart anything.

The kubelet updates files from a mounted Secret within about a minute. It does not update environment variables or `subPath` mounts. Those change only when the pod restarts.

To restart a pod when a value changes, use the reloader of your cluster. For example, a reloader controller can watch an annotation on a Deployment and restart it when a Secret changes. Use a Deployment, not a bare Pod, for a template that needs a restart.

## Threat model

Anyone who can write a template in a catalog namespace can read every Secret in the template secrets namespace. The template can name any Secret there, and the value goes into an instance that the template controls. Keep only Secrets that your template authors may use in this namespace. Do not put other Secrets there.

A person who can read Secrets in the instance namespaces can also read the copied values. Give this permission only to people who must have it.

The gateway never logs a value. Status messages name the Secret and the key but never the value.

## Move from a Kyverno copy policy

If a Kyverno policy copies Secrets into each instance namespace today, move in these steps:

1. Put the source Secrets in the template secrets namespace.
2. Replace the copied Secrets in each template with a Secret that uses `${templateSecret:NAME/KEY}`.
3. Make sure that instances start and work.
4. Delete the Kyverno policy and the copies that it made.
