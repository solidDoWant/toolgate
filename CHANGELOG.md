# Changelog

## 0.1.0

toolgate is a gateway that you run yourself, between Claude Code and MCP servers. An MCP server gives Claude tools, for example to read GitHub issues. toolgate starts each server on Kubernetes when a session needs it. It checks every tool call against the rules of the admin, and it records every decision in an audit log. This is the first release. It covers sign-in, servers in a session, templates in three scopes, user secrets, block rules, settings sync, the audit log and traces.

To install toolgate:

- Admins install the Helm chart `oci://ghcr.io/soliddowant/charts/toolgate`, version `0.1.0`. The chart runs the image `ghcr.io/soliddowant/toolgate:0.1.0`. Follow `docs/admin/quick-start.md`.
- Users download the `toolgate` command for their system from the files of this release, and put it on their `PATH`. Then they install the Claude Code plugin with `claude plugin marketplace add solidDoWant/toolgate` and `claude plugin install toolgate@toolgate`. Follow `docs/user/README.md`.

### Signing in

- Users sign in with `toolgate login`, through the device-code flow of your OIDC provider. `toolgate status` shows the email of the user. When the access token expires, the command gets a new one without a new sign-in. ([#63](https://github.com/solidDoWant/toolgate/pull/63), [#78](https://github.com/solidDoWant/toolgate/pull/78))
- The command keeps tokens in the system keychain. If no keychain is available, it keeps them in a file that only the user can read. ([#74](https://github.com/solidDoWant/toolgate/pull/74))
- Pods sign in with a projected service-account token for the audience `toolgate`. A pod in an allowed namespace has the attestation `attested`, and its workspace names its owner, for example `k8s/ci/Job/build`. ([#70](https://github.com/solidDoWant/toolgate/pull/70))
- The gateway refuses a request with no token, an expired token, or a token for another audience or issuer, with HTTP 401. ([#63](https://github.com/solidDoWant/toolgate/pull/63))
- `toolgate headers` gives Claude Code the token and the Claude Code session ID for each connection. ([#55](https://github.com/solidDoWant/toolgate/pull/55), [#89](https://github.com/solidDoWant/toolgate/pull/89))

### Servers and sessions

- Each Claude Code session gets four tools: `toolgate_list_servers`, `toolgate_enable_server`, `toolgate_disable_server` and `toolgate_server_status`. The list shows only the servers that the user, the groups of the user, or the service account can use. ([#85](https://github.com/solidDoWant/toolgate/pull/85), [#106](https://github.com/solidDoWant/toolgate/pull/106), [#107](https://github.com/solidDoWant/toolgate/pull/107))
- When you turn on a server, the session gets progress messages until the server is ready. Then the tools of the server appear, with the server name in front, for example `test__echo`. The gateway sends each call to the server and returns the result. ([#98](https://github.com/solidDoWant/toolgate/pull/98), [#102](https://github.com/solidDoWant/toolgate/pull/102), [#116](https://github.com/solidDoWant/toolgate/pull/116))
- If a server does not start within the `startTimeout` of its template, `toolgate_enable_server` fails with the reason, for example an image that cannot be pulled. `toolgate_server_status` shows the same reason. ([#98](https://github.com/solidDoWant/toolgate/pull/98), [#141](https://github.com/solidDoWant/toolgate/pull/141))
- A session ends when it has no requests for the idle timeout. A client that connects again with the same session ID gets the same servers. If a gateway replica stops, the client connects through another replica and keeps its servers. ([#120](https://github.com/solidDoWant/toolgate/pull/120), [#149](https://github.com/solidDoWant/toolgate/pull/149))
- The gateway stops reading a tool result or a tool list from a server when it is larger than the limit. ([#148](https://github.com/solidDoWant/toolgate/pull/148), [#153](https://github.com/solidDoWant/toolgate/pull/153))

### Templates and rules

- Admins describe each MCP server in Git as an `MCPServerTemplate`. A template is a list of plain Kubernetes objects. The objects can use variables such as `${principal.email}`, `${workspace.id}`, `${session.id}` and `${instance.id}`. ([#51](https://github.com/solidDoWant/toolgate/pull/51), [#64](https://github.com/solidDoWant/toolgate/pull/64), [#71](https://github.com/solidDoWant/toolgate/pull/71))
- The gateway puts a variable value only into a string in the parsed object. So a value that looks like YAML stays a string and adds no fields. A workspace or session header that does not match the allowed pattern gets HTTP 400. ([#63](https://github.com/solidDoWant/toolgate/pull/63), [#71](https://github.com/solidDoWant/toolgate/pull/71))
- A template has one of three scopes: per session, per user, or shared. The sessions of one user share a per-user instance, and all users share a shared instance. An instance stops when no session uses it. A per-user instance also stops after its `idleStop` time without calls. ([#92](https://github.com/solidDoWant/toolgate/pull/92), [#98](https://github.com/solidDoWant/toolgate/pull/98), [#99](https://github.com/solidDoWant/toolgate/pull/99), [#118](https://github.com/solidDoWant/toolgate/pull/118))
- The gateway does not offer a template that is not valid, and it writes the reason in the status of the template. These templates are not valid: a shared template that uses user values or secrets, a template that uses a secret outside a Secret object, a template with a cluster-scoped kind or an RBAC object, and a server with a tool name longer than 64 characters. ([#64](https://github.com/solidDoWant/toolgate/pull/64), [#102](https://github.com/solidDoWant/toolgate/pull/102))
- When an admin changes a template, new instances use the new version. `toolgate_server_status` shows older running instances as outdated. ([#110](https://github.com/solidDoWant/toolgate/pull/110))
- Admins can set a limit on the number of running servers for each user and for the whole gateway. When a user is at the limit, `toolgate_enable_server` fails with a message that names the limit. ([#111](https://github.com/solidDoWant/toolgate/pull/111))
- A `ToolgateRule` blocks servers or tools for a user, a group or a workspace pattern, for example `k8s/ci/*`. Open sessions get the change within 5 seconds, without a new connection. ([#72](https://github.com/solidDoWant/toolgate/pull/72), [#119](https://github.com/solidDoWant/toolgate/pull/119))
- A template with `environments: opt-in` or `attested` is offered to attested pods only, not to users on a laptop. ([#72](https://github.com/solidDoWant/toolgate/pull/72))

### Secrets

- `toolgate secret set` stores a secret for the user. The gateway keeps it encrypted, and puts the value only in the Secret objects of the instances of that user. The value is never in a log line or an audit event. `toolgate secret list` shows the names and dates, never the values. ([#73](https://github.com/solidDoWant/toolgate/pull/73), [#109](https://github.com/solidDoWant/toolgate/pull/109))
- If a template needs a secret that the user did not set, `toolgate_enable_server` fails with the name of the secret and the command that sets it. ([#98](https://github.com/solidDoWant/toolgate/pull/98))

### Claude Code plugin and CLI

- This repository is a Claude Code plugin marketplace with the `toolgate` plugin. The plugin adds the `toolgate` MCP server, a hook that runs when a session starts, and a skill that tells Claude how to use the gateway. ([#101](https://github.com/solidDoWant/toolgate/pull/101))
- Admins can send Claude Code settings to users with a `SettingsProfile`. When a session starts, the hook writes the keys of the matching profiles into `~/.claude/settings.json`. It does not change a key that the user changed, and it reports that key. It removes a key that the profile no longer sets. ([#96](https://github.com/solidDoWant/toolgate/pull/96))
- `toolgate servers` lists the servers that you can use, from a shell. `toolgate secret delete` removes a secret. `toolgate admin rewrap` encrypts all user secrets again with the primary key of the keyset, after an admin adds a new key. ([#109](https://github.com/solidDoWant/toolgate/pull/109))
- The CLI is available for Linux, macOS and Windows, on amd64 and arm64. ([#75](https://github.com/solidDoWant/toolgate/pull/75))

### Audit log and traces

- The gateway records each tool decision and each tool result in the audit log, on stdout and in PostgreSQL. Each event has the principal, workspace, session, attestation, server, tool and trace ID. ([#62](https://github.com/solidDoWant/toolgate/pull/62))
- The gateway sends OpenTelemetry traces over OTLP. If a request has a `traceparent` header, the spans of the gateway are children of that parent. The gateway also writes JSON logs and serves Prometheus metrics. ([#54](https://github.com/solidDoWant/toolgate/pull/54), [#127](https://github.com/solidDoWant/toolgate/pull/127), [#133](https://github.com/solidDoWant/toolgate/pull/133))

### Deployment

- The Helm chart installs the CRDs, two gateway replicas, narrow RBAC and an admission policy. `helm upgrade` also updates the CRDs. ([#68](https://github.com/solidDoWant/toolgate/pull/68), [#90](https://github.com/solidDoWant/toolgate/pull/90))
- The gateway applies its database migrations when it starts. ([#58](https://github.com/solidDoWant/toolgate/pull/58), [#100](https://github.com/solidDoWant/toolgate/pull/100))
- Each instance that is not shared gets its own namespace, with Pod Security `restricted` and network policies that refuse all traffic by default. ([#98](https://github.com/solidDoWant/toolgate/pull/98), [#117](https://github.com/solidDoWant/toolgate/pull/117))
- Templates with `namespace: shared` run in one shared namespace. Set its name in `config.sharedNamespace.name`. If the namespace exists already, set `config.sharedNamespace.create` to `false`, and add the labels `toolgate.soliddowant.dev/writable: "true"` and `pod-security.kubernetes.io/enforce: restricted` to it. Without these labels, the admission policy refuses every write of the gateway in that namespace. ([#118](https://github.com/solidDoWant/toolgate/pull/118), [#152](https://github.com/solidDoWant/toolgate/pull/152), [#155](https://github.com/solidDoWant/toolgate/pull/155), [#156](https://github.com/solidDoWant/toolgate/pull/156))
- The gateway image is for linux/amd64 and linux/arm64. Each binary in it is checked with `govulncheck`. ([#52](https://github.com/solidDoWant/toolgate/pull/52), [#75](https://github.com/solidDoWant/toolgate/pull/75))

### Security

- The admission policy lets the gateway write only in namespaces with the labels `toolgate.soliddowant.dev/writable: "true"` and `pod-security.kubernetes.io/enforce: restricted`, and only the kinds that a template can contain. So the gateway cannot create cluster-scoped objects, RBAC objects or pods with access to the node, even with its own service account. Do not put the `writable` label on namespaces that hold other workloads. ([#68](https://github.com/solidDoWant/toolgate/pull/68), [#155](https://github.com/solidDoWant/toolgate/pull/155), [#156](https://github.com/solidDoWant/toolgate/pull/156))
- `docs/admin/security-model.md` tells admins what claimed workspaces and session IDs cannot protect against. ([#113](https://github.com/solidDoWant/toolgate/pull/113))

### Known issues

- The gateway does not record `auth.refuse` audit events. A request with a bad token gets HTTP 401, but the audit log has no record of it. ([#146](https://github.com/solidDoWant/toolgate/issues/146))
- When a `ToolgateRule` reaches its `expires` time, open sessions are not told. The gateway allows the tools again at once, but Claude Code does not show them until the next change to the tool list. ([#138](https://github.com/solidDoWant/toolgate/issues/138))

Internal work: CI, unit, integration, envtest and kind end-to-end tests, the acceptance scoreboard, the release pipeline, the design documents and the agent scheduler. ([#50](https://github.com/solidDoWant/toolgate/pull/50), [#53](https://github.com/solidDoWant/toolgate/pull/53), [#61](https://github.com/solidDoWant/toolgate/pull/61), [#69](https://github.com/solidDoWant/toolgate/pull/69), [#76](https://github.com/solidDoWant/toolgate/pull/76), [#77](https://github.com/solidDoWant/toolgate/pull/77), [#86](https://github.com/solidDoWant/toolgate/pull/86), [#88](https://github.com/solidDoWant/toolgate/pull/88), [#94](https://github.com/solidDoWant/toolgate/pull/94), [#95](https://github.com/solidDoWant/toolgate/pull/95), [#103](https://github.com/solidDoWant/toolgate/pull/103), [#112](https://github.com/solidDoWant/toolgate/pull/112), [#115](https://github.com/solidDoWant/toolgate/pull/115), [#125](https://github.com/solidDoWant/toolgate/pull/125), [#128](https://github.com/solidDoWant/toolgate/pull/128), [#129](https://github.com/solidDoWant/toolgate/pull/129), [#132](https://github.com/solidDoWant/toolgate/pull/132), [#134](https://github.com/solidDoWant/toolgate/pull/134), [#147](https://github.com/solidDoWant/toolgate/pull/147), [#151](https://github.com/solidDoWant/toolgate/pull/151), [#154](https://github.com/solidDoWant/toolgate/pull/154))
