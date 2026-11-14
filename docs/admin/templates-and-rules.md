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
  scope: per-session  # narrower than the per-user that the variables need
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

- `description`: the text that users and the model see in the list of servers. The gateway also puts it in the instructions it sends to the model at the start of every session, for each server the user can use. Write it for the model: what the server is for and when to use it, in one or two sentences. The gateway cuts it to 120 characters and removes line breaks and control characters, so put the key words first. The model sees at most 20 servers.
- `scope`: who shares one instance. An instance is one running copy of the server. The field is optional. See [Scope](#scope).
  - `per-session`: one instance for each Claude Code session.
  - `per-user`: one instance for each user, shared by the sessions of that user.
  - `shared`: one instance for all users.

  Each Claude Code session that uses an instance gets its own MCP session on the server. The calls of one Claude Code session always use that one MCP session, on every gateway replica. So a `per-user` or `shared` instance holds one MCP session for each Claude Code session that uses it. A server that keeps a lot of state for each session, such as a browser, uses more memory than before.
- `namespace`: `per-instance` (the default) or `shared`. See [Where instances run](#where-instances-run).
- `updatePolicy`: when toolgate replaces instances that started from an older version of the template: `manual` (default), `immediate` or `whenIdle` with `idleFor`. See [Updating outdated instances](instances.md#updating-outdated-instances).
- `idleStop`: how long a `per-user` or `shared` instance runs without calls before it stops. For a `shared` template, `0` means that the instance does not stop. [Instance stops and cleanup](instances.md) gives the full rules.
- `startTimeout`: how long an instance can take to become ready. The default is 5 minutes. While the instance starts, toolgate tries the MCP `initialize` and `tools/list` requests again every 30 seconds. Each try stops after 10 seconds. A server that does not answer stays `starting` until `startTimeout` passes, and then it fails.
- `access`: who can see and turn on the server. See [Who can use a server](#who-can-use-a-server).
- `tools`: changes for single tools, with the name of the tool as the key. Each entry can set `environments` or `blocked: true`. A blocked tool is never offered and every call to it is refused.
  - `name`: the name that users and the model see, in place of the tool's own name. It matches `^[A-Za-z0-9_.-]{1,128}$`. With the `<server>__` prefix it has 64 characters at most. It cannot be the name of another tool of the server, or the name of a hidden tool.
  - `description`: replaces the description of the tool. It has 4096 characters at most.
  - `inputDescriptions`: a description for each top-level input property of the tool, with the property name as the key. There are 64 entries at most, of 4096 characters each. A property that the tool does not have is ignored.
  - `cache`: turns on the cache for the results of the tool, with `ttl` (1s to 24h), `scope`, `ignoreArgs` and `normalize`. See [Tool result cache](tool-result-cache.md) for when this is safe and what it stores.
- `toolFilter`: hides tools. Set exactly one of `allow` and `deny`. Each is a list of tool names, as the server gives them. There are 512 names at most, of 128 characters each. With `allow`, the server offers only the listed tools. With `deny`, it offers all tools except the listed ones.
- `declaredTools`: your own list of the server's tools, with `name`, `description` and `inputSchema` (a JSON Schema object). There are 512 tools at most, and the list has 1 MiB at most. Names must be unique. The list follows the same checks as a list from a server, after the changes in `tools` and `toolFilter`.
- `start`: when an instance starts. `onCall` (the default) or `onEnable`. With `onCall`, enabling a server starts nothing when the template has a tool list that toolgate knows (declared tools, or learned from an earlier instance). The first call that needs the server starts it and waits, with progress notifications, up to `startTimeout`. If the instance failed to start, calls are refused until the user enables the server again. With `onEnable`, or when toolgate knows no tool list yet, enabling the server starts the instance and waits for it.
- `endpoint`: the Service, port and path where the instance serves MCP over HTTP. It is required, except in a remote template. The `path` is `/mcp` when you leave it out. If your server serves MCP at the root, set `path: /`.
- `resources`: the Kubernetes objects that toolgate applies for each instance.
- `healthCheck`: an optional HTTP check of a running instance. See [Health checks](#health-checks).
- `remote`: makes the template a remote server, which toolgate does not start. See [Remote servers](#remote-servers).
- `persistentVolumes`: volumes that outlive the instance. It needs the gateway setting `persistentVolumes.enabled`. See [Persistent volumes](#persistent-volumes).

**Upgrade note.** `toolFilter` and the `name`, `description` and `inputDescriptions` fields take effect only after every gateway pod runs this version. During a rolling upgrade, an older pod ignores them: it lists hidden tools, forwards calls to them and does not rename tools. Each call still gets a recorded decision. To hide a tool as a security control, use `tools.<name>.blocked`. It works on both versions.

A call of a hidden tool, or of a renamed tool by its own name, is refused with the reason `not-offered`. The gateway records the refusal and does not forward the call. Rules and approvals use the tool's own name. A rule's `tools` pattern matches both the tool's own name and its new name. The audit log shows the new name in `exposedTool` when it differs.

## Scope

The scope of a template must be as narrow as the variables that the template uses, or narrower. `shared` is the broadest scope and `per-session` is the narrowest. toolgate looks at the variables in `resources`, `endpoint.service`, `healthCheck.http.service` and `remote.headers`:

| The template uses | Broadest scope it can have |
|---|---|
| no variable, or only `${instance.*}` and `${template.name}` | `shared` |
| `${principal.*}`, `${secret:…}`, `${resolver:…}` or `${exchange:…}` | `per-user` |
| `${templateSecret:…}` | `shared` (it does not narrow the scope) |
| `${session.id}` or `${workspace.id}` | `per-session` |

If you do not set `scope`, toolgate uses the broadest scope that the table allows. For example, a template without variables is `shared`, and a template with `${secret:…}` is `per-user`.

You can set a narrower scope than the table gives. For example, set `per-user` to keep the files of each user apart on disk, or `per-session` to give each session its own copy. If you set a broader scope, the template is invalid. The reason in the `Valid` condition names the field and the variable, for example: `` `scope: shared`, but `remote.headers.Authorization` uses `${secret:…}`, which needs `per-user` or narrower``. The reason never shows a header value or a secret name.

When a template is valid, the message of its `Valid` condition gives the scope that toolgate uses and the cause, for example ``Scope per-user: `remote.headers.Authorization` uses `${secret:…}`.`` The list of servers in Claude Code and on the Servers page show this scope.

## Persistent volumes

A persistent volume keeps its data after the instance stops. The next instance with the same template and scope uses the same data. The field needs the gateway setting `persistentVolumes.enabled`. Without it, a template that sets `persistentVolumes` is invalid.

```yaml
spec:
  scope: per-user
  persistentVolumes:
    - name: cache
      size: 1Gi
      storageClassName: ssd
```

Each entry has these fields:

- `name`: the name of the PersistentVolumeClaim. The pod spec uses it in `persistentVolumeClaim.claimName`. It has 63 characters at most and matches `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`.
- `size`: a Kubernetes quantity greater than 0, such as `1Gi`.
- `storageClassName`: the storage class of the volume. It has 1 to 253 characters.
- `scope`: `per-template` or `per-user`. It is optional.

A template has 8 volumes at most. The volume scope must match the effective scope of the template:

| Scope of the template | Scope of the volume |
|---|---|
| `shared` | `per-template` |
| `per-user` | `per-user` |
| `per-session` | none, the template cannot have volumes |

If you leave out the scope of a volume, it takes the value in the table. If you set a different value, the template is invalid.

The template is also invalid in these cases:

- It sets `remote`.
- It sets `namespace: shared`.
- `resources` has a `Pod`. Use a `Deployment` or a `StatefulSet`.
- `resources` has a PersistentVolumeClaim with the same name as a volume.

toolgate does not check that a resource uses the claim.

The [persistent volumes guide](persistent-volumes.md) covers the StorageClass, clean-up and the size limit.

## Health checks

A template can add a `healthCheck`. It is a `GET` request to a Service in the instance namespace. The request passes when the status is the one you expect. Without a `healthCheck`, toolgate only does its MCP ping.

```yaml
spec:
  healthCheck:
    http:
      service: db-proxy    # a Service in the template; ${instance.id} is replaced
      port: 8080
      path: /healthz       # starts with /, at most 1024 characters, not replaced
      expectedStatus: 200  # 100 to 599
    interval: 30s          # 10s to 10m
    timeout: 5s            # 1s to 60s, less than interval
    failureThreshold: 3    # 1 to 10: failures in a row that make the instance unhealthy
    successThreshold: 1    # 1 to 10: passes in a row that make it healthy again
    onFailure: replace     # replace | reapply | none
    maxRepairs: 3          # 1 to 10: repairs allowed in 30 minutes
```

The values above are the defaults, except for `http`, which has no default. In a `namespace: shared` template, `http.service` must contain `${instance.id}`, like `endpoint.service`. The Kubernetes API server and the catalog both check these limits. A template that breaks one is invalid, and the `Valid` condition gives the reason.

An instance keeps the check it started with. A change to `healthCheck` applies to instances that start after it.

The gateway that holds the Lease runs the check. The first check runs when the instance is ready, and the next one runs every `interval`. The check does not follow redirects, does not use a proxy, and reads at most 64 KiB of the response body. The MCP ping runs apart from the check, so a ping that does not answer does not delay the check.

After `failureThreshold` failures in a row, the instance is unhealthy. Its status stays `ready`, but toolgate does these things:

- It removes the tools of the server from the tool list of each session that uses it.
- It refuses each new call to the server, with a message such as "`github` is unhealthy: GET http://db-proxy:8080/healthz answered 503, expected 200. toolgate is repairing it." The audit log gets a `tool.decision` event with the reason `not-ready`.
- It shows the reason in `toolgate_server_status` and on the Servers page of the web UI.

After `successThreshold` passes in a row, the instance is healthy again and its tools come back. Each change of health writes an `instance.health` event to the audit log.

When an instance becomes unhealthy, toolgate repairs it as `onFailure` says:

- `replace`: toolgate stops the instance and starts a new one, as a restart does. Sessions keep the server enabled, and the tools come back when the new instance is ready. The audit log gets an `instance.stop` event with the reason `health-check`.
- `reapply`: toolgate renders the objects of the instance again and applies them again. This repairs objects that someone changed or deleted. The instance keeps its ID and stays unhealthy until the check passes. If it is still unhealthy `failureThreshold × interval` later, toolgate makes one more repair. Each repair writes an `instance.reapply` event.
- `none`: toolgate makes no repair. The tools stay away until the check passes again.

`maxRepairs` limits the repairs in 30 minutes. The count continues across the new instances that `replace` starts. When one more repair would go over the limit, toolgate makes no repair. Instead the instance fails with a reason such as "Health check still failing after 3 repairs in 30 minutes: GET http://db-proxy:8080/healthz answered 503, expected 200", and the audit log gets an `instance.fail` event. Sessions keep the server enabled with no tools. The next time a user enables the server, a new instance starts with a count of zero.

With `reapply`, toolgate can only render the current version of the template. If the template changed after the instance started, toolgate replaces the instance instead, as with `replace`. This counts as one repair.

### Failed objects of a ready instance

toolgate also checks the Kubernetes objects of each ready instance, on each pass of the reconciler (every 30 seconds) and on each change to the instance. An object fails when kstatus reports that it failed. An object also fails when its `Ready` condition stays `False` for 60 seconds at the current generation of the object. For example, this happens when the `OCIRepository` of a HelmRelease points at a tag that does not exist. Pods have no generation on their `Ready` condition, so only kstatus judges them. A pod that only restarts is left to the health checks.

When an object fails, the instance becomes unhealthy, as above, with a reason such as "OCIRepository mcpserver failed: `<Ready message>`". toolgate repairs it as `onFailure` says, within the `maxRepairs` limit. A template without `healthCheck` uses `replace` with the default limit of 3. When no object fails anymore, an instance without `healthCheck` becomes healthy again. An instance with `healthCheck` becomes healthy again when its check passes.

## Who can use a server

A user can use a server only when both of these checks pass:

1. The user matches the access lists. `access.users` lists OIDC subjects or email addresses, as patterns. `access.groups` lists groups, as patterns. See [Patterns in users and groups](#patterns-in-users-and-groups). `access.serviceAccounts` lists service accounts as `system:serviceaccount:NAMESPACE:NAME`. `access.spiffeIDs` lists glob patterns of SPIFFE IDs. See [SPIFFE sign-in](spiffe-sign-in.md). A template with no access lists is available to no one.
2. The environment of the request matches `access.environments`:
   - `any`: every signed-in user and every pod.
   - `attested`: only pods that sign in with a service-account token, and workloads that sign in with a SPIFFE JWT-SVID. See [Pod sign-in](pod-sign-in.md) and [SPIFFE sign-in](spiffe-sign-in.md).
   - `opt-in`: the default. In this version it works like `attested`.

toolgate matches an email address only when the identity provider verified it.

### Patterns in users and groups

Each entry of `access.users` and `access.groups` is a glob pattern. A glob pattern is a text where `*` stands for any text. The pattern must match the whole value.

- `*` matches any text. `?` matches one character. `[abc]` matches one of the listed characters. `{a,b}` matches one of the listed texts.
- An entry of `access.users` matches the user's subject. It also matches the user's email address, but only when the identity provider verified the address.
- An entry of `access.groups` matches each group of the user.
- An entry without `*`, `?`, `[`, `]`, `{`, `}` or `\` matches only the exact value, as in earlier versions.
- To match one of these characters as itself, put `\` in front of it. In YAML, write `'\*'` in single quotes.
- An entry that is not a valid pattern makes the template invalid. The reason shows in the `Valid` condition of the template.
- Service accounts and SPIFFE workloads never match `users` or `groups`. They need `serviceAccounts` or `spiffeIDs`.

Examples:

```yaml
access:
  users: ["*"]                 # every signed-in user
  environments: any
---
access:
  users: ["*@example.com"]     # users with a verified address at example.com
---
access:
  groups: ["team-*"]           # users in a group whose name starts with team-
```

`groups: ["*"]` does not mean everyone. It matches only users who have at least one group. To give a server to everyone, use `users: ["*"]`.

Use `users: ["*"]` with care. Anyone who can sign in to the gateway can then use the server. Use it only for servers that have no per-user data and no per-user permissions, for example a server that searches public data. A pattern only decides who passes the access lists. `environments`, workspace scopes, rules, grants, approvals and instance limits still apply. A remote `per-user` server that uses `${secret:...}` still needs the secret of each user. Because `opt-in` is the default, users on their own computers cannot use a server until you set `environments: any`.

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
| `${resolver:NAME}` | A value from the resolver with that name. [Resolvers](#resolvers) tells more. |
| `${exchange:NAME}` | A token for the calling user, from the token exchange with that name. |
| `${templateSecret:NAME/KEY}` | The value of key `KEY` in the Secret `NAME` in the template secrets namespace. |

To write a literal `${`, write `$${`. An unknown variable makes the start fail.

You can use `${secret:NAME}` and `${resolver:NAME}` only in `stringData` or `data` of a `Secret`. If a user did not set the secret, the user gets a message with the command that sets it. [User secrets](user-secrets.md) tells how toolgate keeps them.

You can use `${exchange:NAME}` and `${templateSecret:NAME/KEY}` only in `stringData` or `data` of a `Secret`, and the value must be exactly that one variable, with no other text and no other variable. For example, `token: ${exchange:temporal}` is valid, and `token: "Bearer ${exchange:temporal}"` is not. toolgate updates the key later without rendering the template again, so the key must hold one value from one source. Under `stringData`, the key holds the text of the value. Under `data`, toolgate encodes the value with standard base64, so the key holds exactly the bytes of the value. This differs from `${secret:…}` and `${resolver:…}`, which toolgate puts in as they are. A template is not valid if it names an exchange that is not configured, or uses `${templateSecret:…}` when the gateway has no template secrets namespace. `NAME/KEY` must be a Secret name and a key, split at the first `/`. toolgate never reads the Secret when it checks a template, so you can create the Secret after the template. [Token exchange](token-exchange.md) and [Template secrets](template-secrets.md) tell more.

A `shared` template cannot use `${secret:…}`, `${resolver:…}`, `${exchange:…}`, `${principal.*}`, `${workspace.*}` or `${session.*}`, because one instance serves all users.

## Resolvers

A resolver is an HTTPS service that gives the gateway a short-lived value for one user, for example a GitHub App token. You list resolvers in the chart value `config.resolvers`. Each one has a `name`, an `https://` `url`, an `audience`, a `timeout` (default `10s`, at most `60s`) and an optional `caFile`. A `caFile` is a PEM file of the certificate authorities that sign the certificate of the resolver. Mount it into the gateway pods with `resources.persistence` and `advancedMounts.gateway.gateway`. A template that names a resolver that is not in the list is not valid, and its `Valid` condition gives the reason.

When the gateway starts an instance, it calls each resolver that the template names one time. The request is a `POST` with a JSON body:

```json
{"resolver": "github-app", "template": {"namespace": "...", "name": "..."}, "instance": "...",
 "principal": {"id": "...", "email": "...", "name": "...", "groups": ["..."]},
 "workspace": "...", "session": "..."}
```

The body has `workspace` and `session` only for a `per-session` template. The request has the header `Authorization: Bearer TOKEN`. The token is a Kubernetes service-account token of the gateway, with the `audience` of the resolver. The resolver must check the token with a TokenReview before it answers.

The resolver must answer HTTP 200 with `{"value": "...", "expiresAt": "RFC 3339 time"}`. The value must not be empty, and `expiresAt` must be at least 2 minutes in the future. The answer must be 1 MiB or less. The gateway does not follow redirects. If the answer is not correct, the instance fails with a reason that names the resolver and the problem, but never the value.

The gateway puts the value only into the Secret of the instance. It does not store the value anywhere else. When the earliest value of an instance is less than 60 seconds from `expiresAt`, the gateway replaces the instance with a new one, which calls the resolvers again.

## Where instances run

With `namespace: per-instance`, toolgate makes a new namespace `tg-INSTANCE-ID` for each instance. The namespace has a network policy that denies all traffic, except DNS and connections from the gateway pods. Add a `NetworkPolicy` to the template if the server needs to connect to other places, for example the GitHub API.

With `namespace: shared`, all instances go into the one namespace that `config.sharedNamespace.name` names. Only a `shared` template can use it. In this namespace, each object name and each selector must contain `${instance.id}`, so that two instances never use the same names. [The chart page](chart.md#the-shared-namespace) tells more.

## Remote servers

A remote server is an MCP server that already runs as part of another app. toolgate does not start it. The gateway connects to its URL and sends headers that it fills in for each user. The example is mail-archiver, which wants the API key of each user in the `Authorization` header:

```yaml
apiVersion: toolgate.soliddowant.dev/v1alpha1
kind: MCPServerTemplate
metadata:
  name: mail-archiver
  namespace: toolgate-catalog
spec:
  description: Search and read archived mail
  remote:
    url: http://mail-archiver.email.svc.cluster.local:5000/mcp
    headers:
      Authorization: "Bearer ${secret:mail-archiver-api-key}"
    timeout: 30s
```

A remote template has these rules:

- Its scope follows the same rules as other templates. See [Scope](#scope). With `${secret:…}` or `${principal.*}` in a header, it is `per-user` or narrower. With no header, or only fixed header values, it can be `shared`.
- It cannot have `endpoint`, `resources` or `healthCheck`. A template without `remote` must have `endpoint`.
- `remote.url` is the Streamable HTTP address of the server. It must start with `https://`. It can start with `http://` only when the host name ends in `.svc` or `.svc.cluster.local`, because that traffic stays inside the cluster. The URL cannot contain a user name or a password. Put credentials in `remote.headers`.
- `remote.headers` has 32 headers at most. A header name has 256 characters at most, and a value has 4096 characters at most.
- A header value can use only two kinds of variable: `${secret:NAME}`, a secret that the user stores with `toolgate secret set`, and `${principal.*}`, for example `${principal.email}`. Any other variable makes the template invalid. Each `${` must have a closing `}`. Write `$${` for a literal `${`.
- `remote.timeout` is the time limit for each request to the server, from 1s to 5m. It is optional.

The Kubernetes API server refuses a template that breaks the rules for `endpoint`, `resources`, `healthCheck` or `remote.timeout`. The gateway checks the URL and the header values, and puts the reason for a problem in the `Valid` condition. The reason names the header, but it never shows the value. The gateway never writes header values into a status, an event or a log.

When a user enables a remote server, the gateway first makes sure that the user set each secret that the headers name. If a secret is missing, the user gets the message `Secret NAME isn't set. Run toolgate secret set NAME.`, and nothing starts. A missing secret is not a server error.

Then the gateway connects to the URL with the headers of that user and lists the tools. For a `per-user` template, each user gets an own connection, which uses only the secrets of that user. One user's headers never go with a call of another user. The tool list is also kept for each user. For a `shared` template, all users use one connection and one tool list, because its headers contain nothing that belongs to one user. Tool names, `tools:` overrides, rules, grants, the audit log and access lists work as for servers that toolgate starts.

If the server answers 401 or 403, the user gets the message that the server refused the credential, with the `toolgate secret set` command to set the secret again. A new secret value takes effect at the next connection. A remote server counts toward no instance limit, and it gets no namespace. The connection closes when no session of the user has the server turned on and `idleStop` has passed, or when the template changes.

The gateway pods open the connection to the remote server. If the cluster has network policies, the cluster owner must allow traffic from the gateway pods to the remote host.

A remote server can give a different tool list to each user, because each user sends their own credential. For this reason, the gateway never shares the tool list that one user gets with other users:

- The catalog and tool search show the tools in `declaredTools` to every user who can use the server, before anyone enables it.
- If the template has no `declaredTools`, tool search shows no tools for the server until a user enables it. After that, the user finds the tools that the server listed for their own credential. Other users never see them.

Add `declaredTools` to a remote template if users must find its tools before they enable it.

## When a template is invalid

The gateway checks each template when you apply it and when it reads it. The result is in the `Valid` condition of the template:

```sh
kubectl --namespace toolgate-catalog get mcpservertemplate github \
  --output jsonpath='{.status.conditions[?(@.type=="Valid")]}'
```

If the status is `False`, the message gives the reason. The gateway does not offer an invalid template. If an earlier version was valid, the gateway uses that version until you fix the template.

A template is also invalid when:

- an override `name` does not match the pattern, or is too long with the `<server>__` prefix;
- two `declaredTools` have the same name, or the list is too large;
- a name in `declaredTools` collides with another tool after your changes;
- an override `name` is the name of a tool in `toolFilter.deny`.

The server name `toolgate` is reserved for the built-in server. A template with that name is always invalid, and the gateway treats it as deleted: its running instances stop. Rename such a template before you upgrade.

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
- `tools`: tool names in the form `SERVER__TOOL`. A tool of the built-in `toolgate` server also matches by its exact name. For example, `toolgate_allow` and `toolgate__allow` both match the tool `toolgate_allow`.

In each pattern, `*` matches any characters, including `/`. The `reason` is shown to the user and the model in the refusal, after the namespace and name of the rule. After `expires`, the rule no longer applies.

The gateway applies rules when it lists servers, when it lists tools, when a server is turned on, and on every call. A blocked tool does not show in the list of tools. A rule change reaches all open sessions within 5 seconds. This is also true when a rule reaches its `expires` time.

A rule that matches only `workspaces` that start with `claimed/`, or only `sessions`, does not stop a user who wants to avoid it. The user can send another value. Read [The security model](security-model.md) before you write such a rule.

If a rule is invalid, for example because a pattern is not valid, the gateway ignores it. The reason is in the `status.conditions` of the rule and in the gateway log.
