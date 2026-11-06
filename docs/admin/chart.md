# The Helm chart

For: admins who install toolgate and need to know what its chart is allowed to do in the cluster.

The chart is in `deploy/charts/toolgate`. [The quick start](quick-start.md) shows an install, and [the values reference](chart-values.md) lists every value.

## What the chart installs

The chart installs these objects:

- The three toolgate CRDs: `MCPServerTemplate`, `ToolgateRule` and `SettingsProfile`.
- A Deployment with two gateway pods, a Service for them, and a second Service for their metrics.
- A migration Job with its own ServiceAccount and NetworkPolicy, only in the split database mode (see below).
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
- Create, patch, delete, get, list and watch these kinds: Pod, Deployment, StatefulSet, Service, ConfigMap, ServiceAccount, NetworkPolicy, PersistentVolumeClaim and CiliumNetworkPolicy.
- Create, patch and delete Secrets. The gateway cannot read Secrets.
- List and watch Events, to find the reason when a server does not start.
- Create TokenReviews, to check the tokens of pods that sign in.
- Read the toolgate custom resources, and write their `status` only.

The Role in the release namespace allows the gateway to use its Lease and to read ConfigMaps there.

## Other kinds in templates

A template can contain any namespaced kind. The RBAC of the gateway decides which kinds the gateway can create. To allow more kinds, add rules to the chart value `rbac.extraRules`. The chart adds these rules to the ClusterRole of the gateway. You can also bind your own roles to the service account of the gateway. For each extra kind, grant `create`, `patch`, `delete`, `get`, `list` and `watch`. The gateway reads the objects to find out when a server is ready.

If RBAC refuses an object of a template, the instance fails. The reason is the message of the Kubernetes API server.

The gateway reads the list of kinds in the cluster one time, when it starts. If you install a CRD after the gateway starts, restart the gateway. Until then, a template with a kind of that CRD is invalid.

A template cannot contain a cluster-scoped kind, for example a ClusterRole. When an instance stops, the gateway deletes its namespace. A cluster-scoped object is not in the namespace, so it stays in the cluster.

This example allows templates that install a Helm chart with Flux:

```yaml
rbac:
  extraRules:
    - apiGroups: ["helm.toolkit.fluxcd.io"]
      resources: ["helmreleases"]
      verbs: ["create", "patch", "delete", "get", "list", "watch"]
    - apiGroups: ["source.toolkit.fluxcd.io"]
      resources: ["ocirepositories", "helmrepositories"]
      verbs: ["create", "patch", "delete", "get", "list", "watch"]
```

An instance with a HelmRelease is ready when Flux reports the HelmRelease ready and the server answers MCP requests. If the install keeps failing, the `Ready` condition of the HelmRelease stays `False`. After 60 seconds, the instance fails. The reason is the message of that condition. The same rule applies to every kind with a `Ready` condition. If an object fails after the instance is ready, the instance becomes unhealthy and toolgate repairs it. See [Failed objects of a ready instance](templates-and-rules.md#failed-objects-of-a-ready-instance). If the instance does not become ready within `startTimeout`, the reason is the `Ready` message of the first object that is not ready. When the instance stops, the gateway deletes its namespace. Kubernetes keeps the namespace until helm-controller uninstalls the chart. If the uninstall cannot finish, the namespace stays in `Terminating`.

Flux's helm-controller installs the chart with its own service account, not with the service account of the gateway. This service account often has `cluster-admin`. So a template with a HelmRelease can install anything that helm-controller can install. This is also true if an attacker controls the gateway. You decide if you trust this.

To limit helm-controller, start it with `--default-service-account` and `--no-cross-namespace-refs` (Flux multi-tenancy). Then grant the gateway `rolebindings` with the six verbs above. Each template then contains a RoleBinding for the helm service account in its own namespace. Kubernetes lets the gateway grant only the permissions that the gateway has itself. toolgate does not set or check `spec.serviceAccountName` of a HelmRelease.

## What the admission policy prevents

The ClusterRole alone lets the gateway write in every namespace. The admission policy limits this. It applies to every create, update and delete by the gateway's service account, for all resources. It refuses these writes:

- An object outside a namespace. So the gateway cannot create cluster-scoped objects, such as a ClusterRole or a webhook, even if an RBAC rule allows it.
- An object in a namespace that does not have the label `toolgate.soliddowant.dev/writable: "true"`.
- An object in a namespace that does not have the label `pod-security.kubernetes.io/enforce: restricted`.
- A new namespace whose name does not start with `tg-`, or that does not have both labels.
- Any change to an existing namespace. The gateway only creates and deletes namespaces. So it cannot remove or change a Pod Security label.
- The deletion of a namespace that does not have the label `toolgate.soliddowant.dev/instance`. Only the namespaces that the gateway creates for single servers have this label. So the gateway cannot delete the shared namespace.

The policy has two exceptions, because the gateway needs them to work:

- The Lease objects in the release namespace.
- The `status` of the toolgate custom resources, in the namespaces where you keep them.

The policy refuses a write when it cannot evaluate its rules (`failurePolicy: Fail`).

Pod Security admission is a part of Kubernetes. It checks each pod in a namespace against the level in the namespace's `pod-security.kubernetes.io/enforce` label. The level `restricted` refuses privileged pods, pods in the node's network or process namespaces, and `hostPath` volumes. Every namespace that the gateway can write in has this level. So the gateway cannot start a pod that gets access to the node. This also applies to pods that a Deployment or a StatefulSet creates.

So if an attacker controls the gateway, the attacker can write only in toolgate's own namespaces, and only the kinds that the RBAC of the gateway allows. The attacker can still read and change the MCP servers that toolgate runs, and create Secrets in their namespaces. The labels on an object do not matter: the policy checks the label of the namespace, and only an admin or the gateway can label a namespace.

Do not put the label `toolgate.soliddowant.dev/writable: "true"` on namespaces that hold other workloads. The gateway can write in every namespace with that label.

The policy does not use the label `app.kubernetes.io/managed-by`, because Helm 4 sets that label to `Helm` on every object that it installs.

## The shared namespace

Templates with `namespace: shared` run in one shared namespace. Set its name in `config.sharedNamespace.name`. The chart then creates the namespace with the labels `toolgate.soliddowant.dev/writable: "true"` and `pod-security.kubernetes.io/enforce: restricted`. If the namespace exists already, set `config.sharedNamespace.create` to `false` and add these two labels to it yourself. If a label is missing, the admission policy refuses every write of the gateway in the namespace. The name must not start with `tg-`. The chart gives the name to the gateway in the setting `sharedNamespace` (see [Configuration](configuration.md)).

If `config.sharedNamespace.name` is empty, every `namespace: shared` template is invalid, and users do not see it.

toolgate does not rename the objects of a template. Several instances share this namespace, so a `namespace: shared` template must give each instance its own names. A template is invalid if one of these rules fails:

- Each `metadata.name` contains `${instance.id}`, and `endpoint.service` contains it too.
- The selectors contain `toolgate.soliddowant.dev/instance: ${instance.id}`. This applies to `spec.selector` of a Service, `spec.selector.matchLabels` and `spec.template.metadata.labels` of a Deployment or StatefulSet, and `spec.podSelector.matchLabels` of a NetworkPolicy.
- The template contains only these kinds: Pod, Deployment, StatefulSet, Service, ConfigMap, Secret, ServiceAccount, NetworkPolicy and PersistentVolumeClaim. toolgate cannot check the names and selectors of other kinds, such as `CiliumNetworkPolicy` or `HelmRelease`.

## Database rights and the migration Job

The gateway keeps its state in PostgreSQL and creates its tables itself. You can run it in one of two modes.

**Single role (the default).** `config.database.migrateOnStart` is `true`. The gateway connects with one database role, and creates and updates its tables when it starts. That role needs the right to create tables. The chart installs no Job.

**Split roles.** The gateway connects with a role that can only read and write data. A second role owns the tables and runs the migrations. To use it:

1. Create the two roles, a schema and the default privileges. This SQL does it, as a PostgreSQL superuser, in the database for toolgate. Give both roles a way to sign in that fits your setup, such as a password, and make the connection string of each:

   ```sql
   CREATE ROLE toolgate_migrator LOGIN;
   CREATE ROLE toolgate_app LOGIN;
   CREATE SCHEMA toolgate AUTHORIZATION toolgate_migrator;
   ALTER ROLE toolgate_migrator SET search_path = toolgate;
   ALTER ROLE toolgate_app SET search_path = toolgate;
   GRANT USAGE ON SCHEMA toolgate TO toolgate_app;
   ALTER DEFAULT PRIVILEGES FOR ROLE toolgate_migrator IN SCHEMA toolgate
     GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO toolgate_app;
   ALTER DEFAULT PRIVILEGES FOR ROLE toolgate_migrator IN SCHEMA toolgate
     GRANT USAGE, SELECT ON SEQUENCES TO toolgate_app;
   ALTER DEFAULT PRIVILEGES FOR ROLE toolgate_migrator IN SCHEMA toolgate
     GRANT EXECUTE ON FUNCTIONS TO toolgate_app;
   ```

2. Put each connection string in a Secret in the release namespace.
3. Set these values:

   ```yaml
   config:
     database:
       existingSecret: toolgate-app        # the toolgate_app role
       migrateOnStart: false
       migrationSecret: toolgate-migrator  # the toolgate_migrator role
       migrationEgress:                    # NetworkPolicy egress rules that reach PostgreSQL
         - to:
             - namespaceSelector:
                 matchLabels:
                   kubernetes.io/metadata.name: databases
           ports:
             - protocol: TCP
               port: 5432
   ```

If you already have a database that one role owns, move it to the split mode with these steps, with the same default privileges as above. Use `REASSIGN OWNED BY old_role TO toolgate_migrator`, then `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA toolgate TO toolgate_app` and `GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA toolgate TO toolgate_app`. Do not grant `EXECUTE` on a function by name: the audit partition function does not exist until the Job runs, and the default privilege for functions covers it.

When `migrationSecret` is set and `migrateOnStart` is `false`, the chart adds three objects: a ServiceAccount with no permissions, a NetworkPolicy and a Job named `<release>-migrate` that runs `toolgate migrate`. The NetworkPolicy allows no ingress, and allows egress to DNS (port 53) and to `migrationEgress`. The chart fails with a message that names `config.database.migrationEgress` if you leave it empty.

These are Helm hooks (`pre-install` and `pre-upgrade`). Helm and the Flux helm-controller run them before they apply the other objects, and wait for the Job. So the new gateway pods start only after the migrations. If the Job fails, the install or upgrade fails and the old pods keep serving. Helm deletes the objects after the Job succeeds.

- **Hooks need Helm or Flux.** `helm template | kubectl apply` does not run hooks, so nothing migrates. In that case run `toolgate migrate` yourself before each upgrade.
- **The deadline.** The Job stops after 240 seconds. The default timeout of the helm-controller is 5 minutes, so the Job fails before Flux gives up. If you set a longer `spec.timeout` on the HelmRelease, you must also raise `activeDeadlineSeconds` under `resources.controllers.migrate.job`.
- **Mounts.** The Job mounts nothing from the release, because the release's ConfigMap and Secrets do not exist yet when it runs. If the Job needs a file, such as a client certificate for PostgreSQL, mount it with `resources.persistence.<name>.advancedMounts.migrate`. Mount files for the gateway, such as a resolver `caFile`, with `advancedMounts.gateway.gateway`. A `globalMounts` entry also mounts the file into the Job.
- **No migration Secret.** If `migrateOnStart` is `false` and `migrationSecret` is empty, nothing migrates. The gateway refuses to start while the schema is older than it needs, and its log says so. Run `toolgate migrate` yourself.

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
