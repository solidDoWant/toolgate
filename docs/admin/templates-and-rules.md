# Templates and rules

For: admins who write the catalog of MCP servers and the rules that refuse calls.

The catalog is the list of MCP servers that users can turn on. Each entry is an `MCPServerTemplate` object. A `ToolgateRule` object refuses calls that match it. Both kinds are in the API group `toolgate.soliddowant.dev`, version `v1alpha1`.

Keep these objects in Git, and let Flux apply them. toolgate reads them and writes only their `status`. The gateway reads them only in the namespaces in `config.catalogNamespaces`. If that list is empty, it reads the release namespace. See the [chart values reference](chart-values.md).

## A first template

This template gives each Claude Code session its own copy of a server. Users in the group `dev` can use it from any machine.

```yaml
apiVersion: toolgate.soliddowant.dev/v1alpha1
kind: MCPServerTemplate
metadata:
  name: github
  namespace: toolgate-catalog
spec:
  description: GitHub issues, pull requests and repositories
  scope: per-session
  startTimeout: 5m
  access:
    groups: [dev]
    environments: any
  tools:
    delete_repository: {blocked: true}
  endpoint: {service: github-mcp, port: 8080, path: /mcp}
  resources:
    - apiVersion: v1
      kind: Secret
      metadata: {name: github-token}
      stringData:
        token: ${secret:github-token}
    - apiVersion: v1
      kind: Pod
      metadata:
        name: github-mcp
        labels: {app: github-mcp}
      spec:
        securityContext:
          runAsNonRoot: true
          seccompProfile: {type: RuntimeDefault}
        containers:
          - name: server
            image: ghcr.io/example/github-mcp:1.0.0
            ports: [{containerPort: 8080}]
            env:
              - name: GITHUB_TOKEN
                valueFrom: {secretKeyRef: {name: github-token, key: token}}
              - name: USER_EMAIL
                value: ${principal.email}
            securityContext:
              allowPrivilegeEscalation: false
              capabilities: {drop: [ALL]}
    - apiVersion: v1
      kind: Service
      metadata: {name: github-mcp}
      spec:
        selector: {app: github-mcp}
        ports: [{port: 8080}]
```

The name of the template is the name of the server. It has 31 characters at most: a lowercase letter, then lowercase letters, digits and `-`. Two templates in different namespaces cannot have the same name. If they do, both are invalid until you rename one.

## The fields of a template

- `description`: the text that users and the model see in the list of servers.
- `scope`: who shares one instance. An instance is one running copy of the server.
  - `per-session`: one instance for each Claude Code session.
  - `per-user`: one instance for each user, shared by the sessions of that user.
  - `shared`: one instance for all users.
- `namespace`: `per-instance` (the default) or `shared`. See [Where instances run](#where-instances-run).
- `idleStop`: how long a `per-user` or `shared` instance runs without calls before it stops. For a `shared` template, `0` means that the instance does not stop. [Instance stops and cleanup](instances.md) gives the full rules.
- `startTimeout`: how long an instance can take to become ready. The default is 5 minutes. While the instance starts, toolgate tries the MCP `initialize` and `tools/list` requests again every 30 seconds. Each try stops after 10 seconds. A server that does not answer stays `starting` until `startTimeout` passes, and then it fails.
- `access`: who can see and turn on the server. See [Who can use a server](#who-can-use-a-server).
- `tools`: changes for single tools, with the name of the tool as the key. Each entry can set `environments` or `blocked: true`. A blocked tool is never offered and every call to it is refused.
- `endpoint`: the Service, port and path where the instance serves MCP over HTTP.
- `resources`: the Kubernetes objects that toolgate applies for each instance.

## Who can use a server

A user can use a server only when both of these checks pass:

1. The user matches the access lists. `access.users` lists OIDC subjects or email addresses. `access.groups` lists groups. `access.serviceAccounts` lists service accounts as `system:serviceaccount:NAMESPACE:NAME`. A template with no access lists is available to no one.
2. The environment of the request matches `access.environments`:
   - `any`: every signed-in user and every pod.
   - `attested`: only pods that sign in with a service-account token. See [Pod sign-in](pod-sign-in.md).
   - `opt-in`: the default. In this version it works like `attested`.

toolgate matches an email address only when the identity provider verified it. Because `opt-in` is the default, users on their own computers cannot use a server until you set `environments: any`.

## The objects in a template

A template can contain objects of any namespaced kind that the cluster has. A cluster-scoped kind, or a kind whose CRD is not installed, makes the template invalid. The RBAC of the gateway decides which kinds it can create. See [Other kinds in templates](chart.md#other-kinds-in-templates). toolgate puts each object in the namespace of the instance, and adds the labels `toolgate.soliddowant.dev/instance`, `toolgate.soliddowant.dev/template` and `app.kubernetes.io/managed-by: toolgate`.

The namespace of an instance enforces the Pod Security level `restricted`. Your pods must set the security fields in the example above, or Kubernetes refuses them.

## Variables

A variable is a value that toolgate puts into the template when it starts an instance. toolgate first reads the YAML, and then puts the values only into text values. A value cannot add fields to an object.

| Variable | Value |
|---|---|
| `${principal.id}` | The user's subject at the identity provider, or the service account. |
| `${principal.email}` | The user's email address. The start fails if the identity provider did not verify it. |
| `${principal.name}` | The user's name. |
| `${workspace.id}` | The workspace of the request. |
| `${session.id}` | The Claude Code session ID. |
| `${instance.id}` | The ID of the instance. |
| `${instance.namespace}` | The namespace of the instance. |
| `${template.name}` | The name of the template. |
| `${secret:NAME}` | The user's secret with that name. |

To write a literal `${`, write `$${`. An unknown variable makes the start fail.

You can use `${secret:NAME}` only in `stringData` or `data` of a `Secret`. If a user did not set the secret, the user gets a message with the command that sets it. [User secrets](user-secrets.md) tells how toolgate keeps them.

A `shared` template cannot use `${secret:…}`, `${principal.*}`, `${workspace.*}` or `${session.*}`, because one instance serves all users.

## Where instances run

With `namespace: per-instance`, toolgate makes a new namespace `tg-INSTANCE-ID` for each instance. The namespace has a network policy that denies all traffic, except DNS and connections from the gateway pods. Add a `NetworkPolicy` to the template if the server needs to connect to other places, for example the GitHub API.

With `namespace: shared`, all instances go into the one namespace that `config.sharedNamespace.name` names. Only a `shared` template can use it. In this namespace, each object name and each selector must contain `${instance.id}`, so that two instances never use the same names. [The chart page](chart.md#the-shared-namespace) tells more.

## When a template is invalid

The gateway checks each template when you apply it and when it reads it. The result is in the `Valid` condition of the template:

```sh
kubectl --namespace toolgate-catalog get mcpservertemplate github \
  --output jsonpath='{.status.conditions[?(@.type=="Valid")]}'
```

If the status is `False`, the message gives the reason. The gateway does not offer an invalid template. If an earlier version was valid, the gateway uses that version until you fix the template.

A running instance keeps the version of the template that it started with. A change applies to instances that start after it.

## Rules

A `ToolgateRule` refuses the requests that match it. Rules only refuse. A rule can never give access.

```yaml
apiVersion: toolgate.soliddowant.dev/v1alpha1
kind: ToolgateRule
metadata:
  name: no-repo-delete-from-ci
  namespace: toolgate-catalog
spec:
  match:
    workspaces: ["k8s/ci/*"]
    tools: ["github__delete_*"]
  reason: No repository deletion from CI
  expires: 2026-12-31T00:00:00Z
```

A request matches when it matches every field in `match`. A field that you leave out matches all requests. The fields are:

- `principals`: subjects, emails or service accounts.
- `groups`: groups of the user.
- `workspaces`: workspaces, such as `k8s/ci/*` or `claimed/laptop-*`.
- `sessions`: Claude Code session IDs.
- `attestation`: `user` or `attested`.
- `servers`: server names.
- `tools`: tool names in the form `SERVER__TOOL`.

In each pattern, `*` matches any characters, including `/`. The `reason` is shown to the user and the model in the refusal, after the namespace and name of the rule. After `expires`, the rule no longer applies.

The gateway applies rules when it lists servers, when it lists tools, when a server is turned on, and on every call. A blocked tool does not show in the list of tools. A rule change reaches all open sessions within 5 seconds. This is also true when a rule reaches its `expires` time.

A rule that matches only `workspaces` that start with `claimed/`, or only `sessions`, does not stop a user who wants to avoid it. The user can send another value. Read [The security model](security-model.md) before you write such a rule.

If a rule is invalid, for example because a pattern is not valid, the gateway ignores it. The reason is in the `status.conditions` of the rule and in the gateway log.
