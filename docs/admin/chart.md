# The Helm chart

For: admins who install toolgate and need to know what its chart is allowed to do in the cluster.

The chart is in `deploy/charts/toolgate`. [The quick start](quick-start.md) shows an install, and [the values reference](chart-values.md) lists every value.

## What the chart installs

The chart installs these objects:

- The three toolgate CRDs: `MCPServerTemplate`, `ToolgateRule` and `SettingsProfile`.
- A Deployment with two gateway pods, a Service for them, and a second Service for their metrics.
- A PodDisruptionBudget that keeps at least one gateway pod running when Kubernetes drains a node. A PodDisruptionBudget is a limit on how many pods Kubernetes can evict at the same time.
- A ServiceMonitor for the metrics Service, if you set `config.serviceMonitor.enabled` to `true`. The Prometheus Operator must be installed.
- A ServiceAccount for the gateway.
- A ClusterRole and a Role for that service account.
- A ValidatingAdmissionPolicy and its binding. An admission policy is a set of rules that the Kubernetes API server applies to each write before it stores the object.
- The shared namespace for instances, if you set `config.sharedNamespace.name`.

## The layout of the values

The values have two top-level blocks:

- `config` holds the settings of toolgate: the image, the number of replicas, the database, the identity provider, logging, traces, the shared namespace and the CRDs. Most installs change only this block.
- `resources` describes the Deployment, the Services, the ServiceAccount, the PodDisruptionBudget and the ServiceMonitor. It uses the values format of the [bjw-s `common` library chart](https://bjw-s-labs.github.io/helm-charts/docs/common-library/). You can change any part of these objects here, for example probes, node selectors, tolerations, affinity, resource requests or an extra container. String values can contain Helm templates, such as `"{{ .Values.config.replicas }}"`.

For example, these values add a toleration to the gateway pods:

```yaml
resources:
  controllers:
    gateway:
      pod:
        tolerations:
          - key: dedicated
            operator: Equal
            value: toolgate
            effect: NoSchedule
```

By default, the memory request of the gateway container is equal to its memory limit, and the container has no CPU limit.

The ClusterRole, the Role, their bindings and the admission policy are not in `resources`. Values cannot change them. Only `config.serviceAccount.name` and `config.sharedNamespace` change what they refer to.

## The pods

Every gateway pod serves requests. Only one pod at a time runs the reconciler, the part of the gateway that creates and deletes the objects of MCP servers. The pods decide which pod that is with a Kubernetes Lease named `toolgate-runtime` in the release namespace. If that pod stops or loses the Lease, another pod takes it over within about 15 seconds.

## Why the gateway has a ClusterRole

toolgate starts each MCP server in its own namespace, named `tg-` and an instance ID. It creates these namespaces when a user enables a server. A Role works only in one namespace that exists already, so the gateway needs a ClusterRole.

The ClusterRole allows these actions:

- Create, delete, get, list and watch Namespaces.
- Create, patch, delete, get, list and watch the kinds that a template can contain: Pod, Deployment, StatefulSet, Service, ConfigMap, ServiceAccount, NetworkPolicy, PersistentVolumeClaim and CiliumNetworkPolicy.
- Create, patch and delete Secrets. The gateway cannot read Secrets.
- List and watch Events, to find the reason when a server does not start.
- Create TokenReviews, to check the tokens of pods that sign in.
- Read the toolgate custom resources, and write their `status` only.

The Role in the release namespace allows the gateway to use its Lease and to read ConfigMaps there.

## What the admission policy prevents

The ClusterRole alone lets the gateway write in every namespace. The admission policy limits this. It applies to every create, update and delete by the gateway's service account, for all resources. It refuses these writes:

- An object of a kind that is not in the list above. So the gateway cannot create RBAC objects, webhooks or cluster-scoped objects, even if an RBAC rule allowed it.
- An object in a namespace that does not have the label `app.kubernetes.io/managed-by: toolgate`.
- A new namespace whose name does not start with `tg-`, or that does not have that label.
- The deletion of a namespace that does not have that label.

The policy has two exceptions, because the gateway needs them to work:

- The Lease objects in the release namespace.
- The `status` of the toolgate custom resources, in the namespaces where you keep them.

The policy refuses a write when it cannot evaluate its rules (`failurePolicy: Fail`).

So if an attacker controls the gateway, the attacker can write only in toolgate's own namespaces, and only the kinds that a template can contain. The attacker can still read and change the MCP servers that toolgate runs, and create Secrets in their namespaces. The labels on an object do not matter: the policy checks the label of the namespace, and only an admin or the gateway can label a namespace.

Do not put the label `app.kubernetes.io/managed-by: toolgate` on namespaces that hold other workloads. The gateway can write in every namespace with that label.

## The shared namespace

Templates with `namespace: shared` run in one shared namespace. Set its name in `config.sharedNamespace.name`. The chart then creates the namespace with toolgate's label. If the namespace exists already, set `config.sharedNamespace.create` to `false` and add the label `app.kubernetes.io/managed-by: toolgate` to it yourself. The name must not start with `tg-`. The chart gives the name to the gateway in the setting `sharedNamespace` (see [Configuration](configuration.md)).

If `config.sharedNamespace.name` is empty, every `namespace: shared` template is invalid, and users do not see it.

toolgate does not rename the objects of a template. Several instances share this namespace, so a `namespace: shared` template must give each instance its own names. A template is invalid if one of these rules fails:

- Each `metadata.name` contains `${instance.id}`, and `endpoint.service` contains it too.
- The selectors contain `toolgate.soliddowant.dev/instance: ${instance.id}`. This applies to `spec.selector` of a Service, `spec.selector.matchLabels` and `spec.template.metadata.labels` of a Deployment or StatefulSet, and `spec.podSelector.matchLabels` of a NetworkPolicy.
- The template contains no `CiliumNetworkPolicy`.

## CRD upgrades

The chart installs the CRDs as normal templates, not from the Helm `crds/` directory. So `helm upgrade` also updates the CRDs when a new chart version changes them.

If you install the CRDs yourself, set `config.crds.enabled` to `false`. Then you must update them yourself before each upgrade.

## Uninstall

By default, the CRDs stay in the cluster after `helm uninstall`, because `config.crds.keep` is `true`. Your templates, rules and settings profiles stay too. If you set `config.crds.keep` to `false`, `helm uninstall` deletes the CRDs and all objects of these kinds.

If you uninstall the chart, the namespaces of running MCP servers stay. To delete them, run this command:

```sh
kubectl delete namespace -l toolgate.soliddowant.dev/instance
```

Instance namespaces always have this label. The shared namespace never has it.
