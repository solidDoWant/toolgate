# toolgate specification

toolgate is a self-hosted gateway between AI coding harnesses (first of all Claude Code) and MCP servers. A harness makes one MCP connection to toolgate. Within a session, the model lists the MCP servers it may use and turns them on and off with tool calls. toolgate starts the servers for that session or user in Kubernetes, checks every tool call against the owner's rules, and records everything.

This file is the authoritative reference. Each phase is done when its checklist in `docs/acceptance/` passes (§17).

## 1. Purpose and reference behaviour

**The owner's goal:** run MCP servers that are built for one user at a time, including servers that need that user's credentials when they start. Each user gets their own instance, with their own secrets. A session can turn servers on and off as it needs them. The owner can block servers or single tools for chosen users, workspaces or sessions without touching the client. Everything is deployed and configured through GitOps.

**Reference products.** Where this spec doesn't decide a behaviour, decide it in this order:
1. What obot (obot-platform/obot) does for its Kubernetes MCP runtime: per-user server instances, a catalog, credentials supplied per user.
2. What the MCP specification and Claude Code's documented behaviour support.
3. The simplest behaviour that keeps principle 1 (§2).

Record the choice on the issue.

**Users of toolgate:**
- **Users** run Claude Code on a laptop, in a Coder workspace, in a pod or in CI. They sign in once and use MCP servers.
- **Admins** deploy toolgate with Helm and Flux, and write catalog entries and rules in git.
- **The owner** is both.

## 2. Principles

These are non-negotiable. Reviews and tests enforce them, and only the owner can change them.

1. **No tool call is forwarded without an authorization decision, and every decision is recorded.** There is no path from a client to an MCP server that skips the policy engine or the audit log.
2. **Trust only what is verified.** Fields a client merely claims (§6) can narrow access, never widen it. Attestation decides what a request may reach.
3. **Git is the source of truth for admin configuration.** Catalog entries, standing rules and settings profiles are Kubernetes objects applied by Flux. toolgate reads them and never writes them.
4. **Secrets stay secret.** User secrets are encrypted at rest, appear in plain text only inside Kubernetes Secret objects created for an instance, and are never logged, never placed in a template's other fields, and never returned by any API after they are set.
5. **Done means the owner's checklist passes.** Nobody raises the bar beyond the checklist and principles 1–4.

## 3. Target environment

- Kubernetes 1.35 or later, with Flux v2 (helm-controller, source-controller) installed in the cluster.
- PostgreSQL 16 or later, reached with TLS. Client-certificate authentication must work (the owner's cluster uses CloudNativePG with client certificates).
- S3-compatible object storage (the owner uses Ceph RGW).
- An OIDC identity provider. The owner uses Authentik. Device-code and authorization-code flows with PKCE are both required.
- Network policies: plain Kubernetes NetworkPolicy always works. CiliumNetworkPolicy is allowed in templates (§9) when the cluster has Cilium.
- Clients: Claude Code 2.1.300 or later. Any MCP client using Streamable HTTP can connect, but only Claude Code is tested.

The **only** backends are PostgreSQL and S3. No Redis, no message queue, no second database.

## 4. Architecture

One Go binary, `toolgate`, has two roles:
- **`toolgate serve`** is the gateway: a stateless HTTP server, run with two or more replicas.
- **The client commands** are the CLI, and the hooks and helpers of the Claude Code plugin (§13).

Gateway components (packages under `internal/`):

| Component | Job |
|---|---|
| Auth | Verifies the bearer token (user OIDC token, toolgate session token, Kubernetes service-account token, or SPIFFE JWT-SVID). Produces the request's identity (§6). |
| MCP endpoint | Streamable HTTP MCP server. Serves the meta tools (§7) and every enabled server's tools, renamed. Forwards calls. |
| Session manager | The enabled-server set per session, in PostgreSQL. Idle timeouts. |
| Policy engine | Decides every enable, list and call (§10). |
| Runtime manager | Renders templates, applies them to Kubernetes, watches readiness, tears instances down (§9). |
| Secret broker | Stores user secrets and resolves `${secret:…}` and `${resolver:…}` (§11). |
| Catalog reader | Watches the custom resources (§8) through the Kubernetes API. |
| Audit | Writes every decision and call (§12). |
| Web UI and API | Phase 2. Server-rendered pages for the things that need a browser or a person (§14). |
| OTLP ingest | Phase 3. Receives Claude Code's telemetry and stamps it with verified identity (§12). |

**Replicas share nothing in memory except caches.** A client's open MCP stream lives on one replica. A change that affects a session (rule change, instance ready, grant) is written to PostgreSQL and announced with `LISTEN/NOTIFY`. The replica holding the stream then acts on it: it sends `notifications/tools/list_changed` and refuses calls the change no longer allows.

## 5. Protocols

- **MCP.** Streamable HTTP transport, revision 2025-11-25. Revision 2026-07-28 removes protocol sessions (`Mcp-Session-Id`, `initialize`), `ping` and the GET stream, and replaces them with per-request `_meta`, `server/discover` and `subscriptions/listen`. So supporting it means handling requests statelessly, which suits toolgate because session state is already in PostgreSQL, keyed by `X-Toolgate-Session`. It isn't planned yet. The approval handlers already use the multi round-trip pattern that 2026-07-28 requires.
- **Upstream MCP servers** are reached over Streamable HTTP inside the cluster. A stdio-only server is wrapped in its template by an adapter container that exposes it over HTTP (the template author's choice; toolgate does not run stdio processes itself).
- **Client authentication:** `Authorization: Bearer <token>` on every request.
- **Identity headers:** `X-Toolgate-Session` (session ID) and `X-Toolgate-Workspace` (claimed workspace ID). Both must match `^[A-Za-z0-9._-]{1,128}$`, or the request is refused. The session header is required. Without a workspace header, the workspace is `default`.
- **Admin and user API** (phase 2): JSON over HTTP under `/api/v1/`, described by an OpenAPI document generated from the Go handlers and published in `docs/dev/`.

## 6. Identity and attestation

Every authenticated request has an **identity**:

| Field | Source | Verified? |
|---|---|---|
| `principal` | User: the OIDC `sub`, with `email` and `groups`. Pod: `system:serviceaccount:<ns>:<name>`. SPIFFE workload: the SPIFFE ID. | Always |
| `workspace` | Pod: `k8s/<namespace>/<owner kind>/<owner name>`, read from the token's pod claims. SPIFFE workload: the SPIFFE ID's path, such as `/owner/alice/workspace/dev`. User: the claimed header, prefixed `claimed/`. | Workload only |
| `session` | The `X-Toolgate-Session` header | Never |
| `attestation` | `attested` or `user` | Computed |

**Attestation levels:**
- **`attested`:** the token was issued by a platform that vouches for the environment. Phase 1: a Kubernetes projected service-account token with audience `toolgate`, checked with TokenReview, from a namespace listed in the gateway's configuration. Phase 3 adds SPIFFE JWT-SVIDs from allowed trust domains (the SPIFFE ID's path becomes the workspace), which covers Coder workspaces and other workloads running a SPIRE agent.
- **`user`:** a person signed in to the IdP, and the machine only describes itself.
- Anything else is refused with 401.

**Users and groups** come from the OIDC ID token or access token claims (`email`, `groups`). The claim names are configurable. `email` is used only when the `email_verified` claim is `true`, because an unverified email is a claim the user chose. Service accounts have no groups, but templates and rules can name them directly.

**Client sign-in (users):** the CLI runs the OAuth 2.0 device authorization grant against the IdP with PKCE, using `golang.org/x/oauth2`. It stores the refresh token in the OS keychain (`github.com/zalando/go-keyring`), with a 0600 file fallback when no keychain exists. `toolgate headers` prints a fresh access token. The gateway accepts the IdP's access tokens directly, checked against the IdP's JWKS with `github.com/coreos/go-oidc/v3`, one of the configured audiences, and expiry. `oidcAudience` is a list of 1 to 8 audiences (a single string is a one-item list), all at the one issuer: each is a client whose access tokens the gateway accepts, so each must be a client made for toolgate, and every client must give a person the same `sub`. A token for none of them gets 401 and `auth.refuse` with "audience not allowed". toolgate issues no tokens of its own in phase 1.

## 7. MCP surface

**Meta tools**, always present:
- `toolgate_list_servers()`: the catalog entries this identity may enable, with name, description, scope, whether each is enabled in this session, its status (`stopped`, `starting`, `ready`, `failed`, `blocked`) and why it is blocked if it is.
- `toolgate_enable_server(name)`: checks policy, starts or reuses the instance, waits for it to be ready while sending MCP progress notifications, then adds its tools and sends `notifications/tools/list_changed`. It returns the tools added. A timeout or failure returns an error naming the reason (image pull, readiness timeout, policy). It is idempotent.
- `toolgate_disable_server(name)`: removes the server's tools from this session and sends `list_changed`. A per-session instance stops when no session uses it.
- `toolgate_server_status(name)`: status, the reason for the last failure, and the time the instance became ready.

**Proxied tools** are named `<server>__<tool>`. A server name matches `^[a-z][a-z0-9-]{0,30}$` and a tool keeps its own name. If the result is over 64 characters, the call is refused and the server is marked `failed`, because Claude Code's own `mcp__toolgate__` prefix makes long names unusable. Descriptions and input schemas pass through unchanged. Prompts and resources are out of scope (§16).

**A call** is: identity → policy (§10) → audit `decision` event → forward to the instance → audit `result` event. An upstream error is returned to the client as a tool error with the upstream message. A policy refusal is a tool error that names the rule that refused it, so the model and the user can see why.

**Sessions.** A session is the pair (principal, session ID). It's created by the first request carrying them. Its enabled set survives reconnects and gateway restarts. It ends after `sessionIdleTimeout` (default 2 h) without requests, and then its per-session instances stop. Another principal using the same session ID gets a different session.

## 8. Custom resources

All are in API group `toolgate.soliddowant.dev`, version `v1alpha1`. The chart installs the CRDs. toolgate only reads them and writes their `status`.

- **`MCPServerTemplate`** (namespaced, in the namespaces the gateway is configured to watch): one catalog entry (§9).
- **`ToolgateRule`**: a standing block rule (§10).
- **`SettingsProfile`**: Claude Code settings handed to clients (§13).

Every resource is validated by CEL rules in its CRD and again when toolgate reads it. An invalid resource is skipped, with the reason in its `status.conditions` and in the log. For an `MCPServerTemplate`, the condition is `Valid`. Its status is `False` and its message gives the reason. A previously valid version keeps working until a valid replacement arrives.

## 9. Catalog entries and instances

```yaml
apiVersion: toolgate.soliddowant.dev/v1alpha1
kind: MCPServerTemplate
metadata: {name: github, namespace: toolgate-catalog}
spec:
  description: GitHub issues, PRs and repositories
  scope: per-session            # per-session | per-user | shared
  namespace: per-instance       # per-instance | shared
  idleStop: 30m
  startTimeout: 5m
  access:
    users: []                   # OIDC subjects or emails
    groups: [dev]
    serviceAccounts: []         # system:serviceaccount:ns:name
    spiffeIDs: []               # globs, such as spiffe://td/owner/*/workspace/*
    environments: opt-in        # any | opt-in (default) | attested
  tools:                        # optional per-tool overrides
    delete_repository: {environments: attested, approval: required}
    list_secrets: {blocked: true}
  endpoint: {service: github-mcp, port: 8080, path: /mcp}
  healthCheck:                  # optional; HTTP GET against a Service of the instance
    http: {service: github-mcp, port: 8080, path: /healthz, expectedStatus: 200}
    interval: 30s               # 10s-10m; timeout 1s-60s, less than interval
    timeout: 5s
    failureThreshold: 3         # 1-10
    successThreshold: 1         # 1-10
    onFailure: replace          # replace (default) | reapply | none
    maxRepairs: 3               # 1-10 repairs in 30 minutes
  resources: [ ... Kubernetes objects ... ]
```

**Scopes:**
- `per-session`: one instance per session.
- `per-user`: one instance per principal, shared by that principal's sessions.
- `shared`: one instance for everyone. A shared template must not use `${secret:…}`, `${resolver:…}`, `${principal.*}`, `${workspace.*}` or `${session.*}`. That is a validation error.

**Instance namespaces.** With `per-instance`, toolgate creates the namespace `tg-<instance id>` and prepares it before applying the template:
- Pod Security `restricted` enforced;
- a default-deny NetworkPolicy for ingress and egress, plus rules allowing DNS and ingress from the gateway's pods;
- the default service account with `automountServiceAccountToken: false`;
- labels naming the template, instance and owner principal (hashed).

With `shared`, all instances go in one configured namespace and are told apart by labels and by `${instance.id}` in their names and selectors. toolgate never renames objects: the catalog refuses a template that doesn't tell its instances apart, and every `shared` template when no shared namespace is configured (`docs/design/phase-1.md` §7). Only `scope: shared` templates may use it; for other scopes it is a validation error, so an instance holding one user's secrets always has its own namespace. Deleting a per-instance namespace is the teardown.

**Resources** is a list of Kubernetes objects. Phase 1 allows these kinds: `Pod`, `Deployment`, `StatefulSet`, `Service`, `ConfigMap`, `Secret`, `ServiceAccount`, `NetworkPolicy`, `PersistentVolumeClaim`, and `CiliumNetworkPolicy` when that CRD exists. Phase 2 drops the fixed list: a template may contain any namespaced kind, such as Flux's `HelmRelease` and `OCIRepository`, and toolgate has no special handling for them. What the gateway can create is decided by the RBAC the cluster admin grants its service account. The chart's default ClusterRole covers phase 1's kinds, the chart value `rbac.extraRules` adds more, and an apply that RBAC refuses fails the instance with the API server's message. Cluster-scoped kinds are a validation error, because teardown deletes the instance namespace and such an object would leak. The gateway needs a ClusterRole, because instance namespaces are created on the fly, and it never reads Secrets. The chart also installs a ValidatingAdmissionPolicy that applies to every write by the gateway's service account: it denies any write outside namespaces labelled as toolgate's, so a compromised gateway is confined to those namespaces whatever RBAC it holds there. Kinds like `HelmRelease`, whose controllers act with their own permissions rather than the gateway's, can do whatever that controller can; allowing them is the admin's trust decision. The exact rules are in `docs/design/phase-1.md` and `docs/design/phase-2.md`.

**Rendering rules:**
1. Parse the YAML first. Substitute variables only inside string scalars of the parsed tree. A value never adds keys or changes structure.
2. Variables: `${principal.id}`, `${principal.email}`, `${principal.name}`, `${workspace.id}`, `${session.id}`, `${instance.id}`, `${instance.namespace}`, `${template.name}`, `${secret:<name>}`, `${resolver:<name>}`. `$${` escapes a literal `${`. An unknown variable, or a variable that doesn't exist for this scope, fails the render.
3. Claimed values (`workspace.id` on `user` attestation, `session.id`) are already pattern-checked (§5). When rendered into an object name or label, they are hashed to fit Kubernetes length rules.
4. `${secret:…}` and `${resolver:…}` are allowed only in `stringData` or `data` of a `Secret`. Anywhere else, the template fails validation.
5. Every rendered object gets the labels `toolgate.soliddowant.dev/instance`, `toolgate.soliddowant.dev/template` and `app.kubernetes.io/managed-by: toolgate`. Its namespace is forced to the instance namespace.
6. Objects are applied with server-side apply, field manager `toolgate`.

**Readiness.** An instance is `ready` when every object is current by kstatus (`sigs.k8s.io/cli-utils/pkg/kstatus`) and an MCP `initialize` against `endpoint` succeeds. It is `failed` when `startTimeout` passes or kstatus reports a failure, with the reason taken from the objects' conditions and events. One rule comes before kstatus: an object whose `Ready` condition has stayed `False` for 60 s at its current generation counts as failed, with the `Ready` message as the reason. This covers HelmReleases whose install keeps failing while helm-controller retries.

**Health.** The leader runs each instance's `healthCheck`, apart from the MCP ping. After `failureThreshold` failures in a row, a `ready` instance becomes unhealthy and stays `ready`. Its tools leave every session's tool list, calls are refused with reason `not-ready`, and `toolgate_server_status` shows the reason. After `successThreshold` passes in a row, it becomes healthy again and its tools come back. Each change writes `instance.health`. The reconciler also judges the objects of each `ready` instance, on each pass and each notification, with the readiness rule above. A failed object makes the instance unhealthy, with the reason "`<Kind>` `<name>` failed: `<message>`". When no object has failed, an instance without `healthCheck` becomes healthy again. With `healthCheck`, the check decides.

**Repair.** The reconciler repairs an unhealthy instance as `healthCheck.onFailure` says. An instance without `healthCheck` uses `replace` with the default bound. `replace` (the default) replaces it with a new instance, as a restart does, and writes `instance.stop` with reason `health-check`. `reapply` renders the instance again, applies its objects again, and writes `instance.reapply`. If the template changed after the instance started, `reapply` replaces the instance instead. If the instance is still unhealthy `failureThreshold × interval` after a reapply, that is one more repair. `none` makes no repair. At most `maxRepairs` repairs are made in 30 minutes, counted across replacements. Instead of one more repair, the instance fails with the reason "Health check still failing after `N` repairs in 30 minutes: `<last reason>`" and writes `instance.fail`. The next enable starts a new instance with no repair history.

**Template changes.** An instance keeps the template generation it was started with, and the `healthCheck` (with `http.service` rendered) in its `health_check` column. A new generation applies to instances started afterwards. `toolgate_server_status` shows when an instance is older than its template. The owner of an outdated instance (an admin, for a shared one) can restart it from the web UI. The gateway then replaces it with a new instance at the current generation, and sessions keep the server enabled.

**Stopping.**
- A per-session instance stops when its last session disables it or ends.
- A per-user instance stops when none of its user's live sessions has it enabled and it had no calls for `idleStop`.
- A shared instance stops when no live session has it enabled and it had no calls for `idleStop`, or never if `idleStop` is `0`.
- Any instance stops when its template is deleted or its scope changes. An invalid template stops nothing (§8).

**Limits.** Each user can have a limited number of running instances (default 20), and the gateway a limited total (default 200). Enabling a server that is already running for the user always works. Stopping deletes the namespace, or the labelled objects in a shared namespace. A gateway that restarts finds its instances from the labels and the database, and reconciles them.

## 10. Policy

**Order of evaluation**, the same for enable, list and call:
1. **Catalog access:** the identity must match `access.users`, `groups`, `serviceAccounts` or `spiffeIDs`. A SPIFFE identity matches only `spiffeIDs`, and `spiffeIDs` matches nothing else. A template with no access lists is available to no one.
2. **Environment:** the server's `environments`, or the tool's override.
   - `any` passes.
   - `attested` needs `attested`.
   - `opt-in` needs `attested`, or (phase 2) a live grant for this principal and this server or tool.
   - In phase 1 there are no grants, so `opt-in` behaves like `attested`.
3. **Blocks:** any matching `ToolgateRule`, or (phase 2) any live temporary block, refuses.
4. **Approval** (phase 2): a tool with `approval: required` waits for a person's approval (§14).

**`ToolgateRule`:**
```yaml
spec:
  match:                       # every listed field must match; omitted = any
    principals: [alice@example.com]
    groups: []
    workspaces: ["k8s/ci/*"]   # glob
    sessions: []
    attestation: user
    servers: [github]
    tools: ["github__delete_*"]  # glob, server__tool
  reason: No repository deletion from CI
  expires: 2026-12-31T00:00:00Z  # optional
```
In globs, `*` matches any run of characters, including `/`. A template that doesn't set `access.environments` gets `opt-in`. Rules only refuse. Grants never come from rules. A rule matching only claimed fields (a `claimed/…` workspace or a session) still applies, but the docs say plainly that a client can avoid it by claiming something else.

**When decisions run:** on `list_servers`, on `tools/list` (blocked tools are left out), on `enable_server` and on every call. A rule change reaches every open session within 5 seconds (`LISTEN/NOTIFY`, then `list_changed`).

## 11. Secrets

- **Storage.** User secrets are rows of (principal, name, ciphertext, key ID), envelope-encrypted with Tink (`github.com/tink-crypto/tink-go/v2`, AEAD AES256-GCM). The key-encryption keyset is in a Kubernetes Secret mounted into the gateway, separate from PostgreSQL. Key rotation is a new keyset version. Old ciphertexts keep decrypting until re-encrypted by `toolgate admin rewrap`. Each ciphertext is bound to its principal and name (associated data), so a row moved to another user doesn't decrypt.
- **Setting a secret.** Phase 1: `toolgate secret set <name>` (reads stdin), `toolgate secret list` (names and dates only), `toolgate secret delete <name>`. They call `PUT /api/v1/secrets/<name>` (the body is the value), `GET /api/v1/secrets` and `DELETE /api/v1/secrets/<name>`, which act only on the verified principal's own secrets. A name matches `^[A-Za-z0-9._-]{1,128}$`; a value is at most 4 MiB. Phase 2: the same in the web UI.
- **Missing secret.** A template referring to a secret the user hasn't set fails `enable_server` with a message naming the secret and the command to set it.
- **Resolvers** (phase 2). A resolver is an HTTP service named in the gateway's configuration. The gateway POSTs the identity and the template name with a service-account token, and gets back `{value, expiresAt}`. Example: exchanging a GitHub App installation token for the user. Values are never cached beyond `expiresAt`. An instance whose resolved secret expires is restarted with a fresh one.

## 12. Audit and telemetry

**Audit events** are JSON lines on stdout (`log/slog`) and rows in PostgreSQL:
- `session.start`, `session.end`, `auth.refuse` (rate-limited);
- `server.enable`, `server.disable`, `instance.start`, `instance.ready`, `instance.fail`, `instance.stop`;
- `tool.decision` (allow or refuse, with the deciding rule), `tool.result` (success or error, duration, sizes);
- `rule.change`, `secret.set`, `secret.delete`;
- phase 2: `grant.*`, `approval.*`, `block.*`;
- phase 3: `instance.health` and `instance.reapply`, with no payload.

Every event carries principal, workspace, session, attestation, server, tool, trace ID and request ID. Tool arguments and results are not logged to stdout. They are stored only when `audit.payloads` is on. Payloads over 64 KiB go to S3 (with object lock when the bucket supports it), referenced from the row. Rows are kept for `audit.retention` (default 90 days) and deleted in batches. Secrets and tokens are never logged.

**Gateway traces:** OpenTelemetry (`go.opentelemetry.io/otel`) spans for each request, policy decision, render, apply and forward. Incoming `traceparent` is honoured, so Claude Code's tool spans (it sends `traceparent` on HTTP MCP requests) become the parents. Metrics are Prometheus-format at `/metrics` on a separate port.

**OTLP ingest** (phase 3): `/v1/traces`, `/v1/metrics` and `/v1/logs` over OTLP/HTTP (protobuf and JSON), using the collector's `pdata` packages for decoding. The request is authenticated like any other, through `otelHeadersHelper`. It is on only when the setting `otlpIngest` is `true`; otherwise the routes answer 404. The ingest removes, at every level, each attribute whose name starts with `user`, `enduser` or `workspace` after every character outside `[A-Za-z0-9]` becomes `_` and the case is lowered. Then it sets `user.id`, `enduser.id`, `user.email` (only when verified) and `workspace.id` from the verified identity on every span, metric data point and log record. `session.id` is kept as the client sent it, because it is claimed like `X-Toolgate-Session`. Prompt and tool content (what `OTEL_LOG_USER_PROMPTS`, `OTEL_LOG_TOOL_DETAILS` and `OTEL_LOG_TOOL_CONTENT` add) is removed unless the setting `telemetry.allowContent` is `true`. The result is forwarded synchronously to the configured OTLP endpoint, with nothing buffered or retried by the gateway (`docs/design/phase-3.md` §3). This gives token usage (`claude_code.token.usage`, `claude_code.cost.usage`, and the `claude_code.llm_request` spans) per principal, workspace and session, with no proxy for model traffic.

## 13. Client: CLI and Claude Code plugin

**One binary, `toolgate`**, built for Linux, macOS and Windows (amd64, arm64). Commands:

| Command | Phase | What it does |
|---|---|---|
| `toolgate login [--gateway URL] [--token-store auto\|keychain\|file]` | 1 | Device-code sign-in, stores the refresh token. It learns the IdP from the gateway's `/.well-known/toolgate-client`. The store used is saved in `config.json` as `tokenStore` and is the only store later runs use (`TOOLGATE_TOKEN_STORE` sets the flag; phase 3 design §10) |
| `toolgate logout`, `toolgate status` | 1 | |
| `toolgate headers` | 1 | `headersHelper` output: `Authorization`, `X-Toolgate-Session` (from the file the SessionStart hook writes), `X-Toolgate-Workspace` |
| `toolgate hook session-start` | 1 | SessionStart hook: check sign-in, fetch the settings profile, merge it, print context |
| `toolgate secret set/list/delete` | 1 | §11 |
| `toolgate servers` | 1 | List the catalog as the CLI user sees it |
| `toolgate otel-headers` | 3 | `otelHeadersHelper` output |
| `toolgate admin rewrap` | 1 | §11 |
| `toolgate sessions` | 3 | List the caller's open sessions and their enabled servers (`GET /api/v1/sessions`) |
| `toolgate audit tail` | 3 | Print the audit events the caller may see as they happen (`GET /api/v1/audit/tail`); `--after KEY`, `--json` |

**Workspace ID (user attestation):** `toolgate headers` sends `TOOLGATE_WORKSPACE` if set. Otherwise it sends `<hostname>/<git remote repo name or directory name>`, normalised to the header pattern: each character outside `[A-Za-z0-9._-]` becomes `-`, and the value is cut to 128 characters. The repo name is the last path element of the remote `origin`, without `.git`.

**Session ID:** from a file the SessionStart hook writes. Claude Code doesn't give the session ID to a `headersHelper`: `CLAUDE_CODE_SESSION_ID` isn't in its environment. It gives it to a SessionStart hook, on standard input (`session_id`) and in `CLAUDE_CODE_SESSION_ID`. Evidence: [`docs/dev/claude-code-integration.md`](docs/dev/claude-code-integration.md). So `toolgate hook session-start` writes the session ID to `$XDG_RUNTIME_DIR/toolgate/<claude pid>` (or a directory under the user cache directory when that variable isn't set) before anything else, and `toolgate headers` reads it by walking its parent processes to the Claude Code process. Claude Code runs the hook and the helper at the same time, so `toolgate headers` waits up to 5 s for the file to appear. The design gives the details (`docs/design/phase-1.md` §8).

**The plugin** (directory `plugin/`, installed from this repo as a Claude Code plugin marketplace) contains:
- `.mcp.json`, with one HTTP server named `toolgate` at `${user_config.gateway_url}/mcp` and `headersHelper: toolgate headers`;
- a SessionStart hook running `toolgate hook session-start`;
- `userConfig` for `gateway_url`;
- a short skill telling the model how to use the meta tools: list first, enable what the task needs, disable when done.

**Settings sync.** A `SettingsProfile` holds a fragment of Claude Code settings (`permissions`, `env`, `hooks`, and other keys) and a selector (groups, principals, attestation). The hook fetches the merged profile for the identity from `/api/v1/settings` and merges it into `~/.claude/settings.json`:
- It changes only keys it owns, recorded in `~/.config/toolgate/owned-settings.json`.
- A key the user changed by hand since the last sync is left alone and reported.
- A key removed from the profile is removed from the file.

Claude Code reloads the settings file during the session. Hooks delivered this way run commands on the user's machine, so the docs warn admins that a profile is code execution on every client.

## 14. Web UI (phase 2)

Server-rendered Go `html/template` pages, with htmx (a pinned, vendored copy) for partial updates and no JavaScript build step. Sign-in uses OIDC authorization code with PKCE and a server-side session in PostgreSQL (cookie `__Host-toolgate`, `SameSite=Lax`, CSRF tokens on every form).

Pages:
- **Servers:** the user's catalog view, their instances and status; restart an outdated instance.
- **Secrets:** set, replace and delete. Values are never shown again.
- **Grants:** create a grant (server or tool, expiry up to 7 days, optionally narrowed to one session); revoke.
- **Approvals:** pending tool calls with server, tool, arguments (when payloads are on), session and workspace; approve or deny.
- **Sessions:** open sessions, their enabled servers, recent calls.
- **Audit:** filter by principal, workspace, session, server, tool and time.
- **Blocks** (admins): temporary blocks with an expiry (up to 30 days); the standing rules from git shown read-only.

**Step-up.** Creating a grant or approving a call needs an authentication less than 5 minutes old. toolgate redirects to the IdP with `prompt=login` and `max_age=0`, and checks `auth_time`. The IdP's flow decides how the person signs in (the owner uses passkeys).

**From inside a session.** When a call needs an approval, the gateway sends a URL-mode MCP elicitation linking to the approval page. A call that needs a grant fails at once with a link to the Grants page. Claude Code opens it in the browser, and the call continues when the person acts, up to `approvalTimeout` (default 10 min). In sessions that can't show an elicitation (`-p`, CI), the call fails at once with an error naming the grant or approval needed.

**Admins** are members of the configured admin group.

## 15. Data model

PostgreSQL, accessed with `pgx/v5`, forward-only migrations embedded with `pressly/goose`, and queries generated with `sqlc`. Tables (outline; the migrations are the reference):
- `sessions`: principal, session ID, workspace, attestation, created, last seen.
- `session_servers`: the enabled set.
- `instances`: ID, template, generation, scope key, namespace, status, reason, timestamps.
- `user_secrets`: principal, name, ciphertext, key ID, timestamps.
- `audit_events`: partitioned by month. The function `toolgate_ensure_audit_partitions()` (`SECURITY DEFINER`, owned by the migrating role) creates the current and next month's partitions; the gateway calls it at start, and the leader when it gets the Lease and every 24 h (`docs/design/phase-3.md` §7).
- Phase 2: `grants`, `approvals`, `temporary_blocks`, `ui_sessions`, `ui_logins`, and three new `instances` columns for restarts and resolver expiry. `docs/design/phase-2.md` §2 gives their rules and row limits.

Concurrency: instance creation for a template and scope key is serialized with a transaction-level advisory lock, so two sessions enabling a per-user server at once share one instance. `docs/design/phase-1.md` gives the locks and the interaction rules.

## 16. Won't do

- Any runtime other than Kubernetes (no Docker or local processes).
- Running stdio MCP servers directly; templates wrap them.
- MCP prompts, resources, sampling and roots through the gateway. Tools only.
- Proxying model API traffic, or anything touching Claude subscription sign-in.
- Multi-cluster, and more than one IdP at a time.
- A public MCP registry or marketplace integration.
- toolgate issuing its own access tokens (phase 1–3).
- Hand-written cryptography or protocol implementations (MCP, OAuth, OIDC, OTLP, JWT, YAML).
- Formal verification, or simulation that defines done.
- High availability beyond stateless replicas on one PostgreSQL.
- Online restore. Backups are the admin's PostgreSQL and S3 backups.
- Billing, quotas, or rate limits beyond fixed request-size and concurrency bounds.
- Windows or macOS gateway builds (the CLI supports them).

## 17. Phases

Each phase ends with a release an admin can deploy: image `ghcr.io/soliddowant/toolgate:<version>`, chart `oci://ghcr.io/soliddowant/charts/toolgate`, and CLI binaries on the GitHub release. The owner deploys it.

1. **Core gateway** (`docs/acceptance/phase-1.md`). Sign-in from Claude Code and pods, the meta tools, templates of plain Kubernetes objects, all three scopes, per-user secrets through the CLI, standing block rules, settings sync, audit log, gateway traces, chart and CLI release.
2. **Security model and web UI** (`docs/acceptance/phase-2.md`). Web UI, grants with step-up, approvals through elicitation, temporary blocks, per-tool rules in full, HelmRelease templates, resolvers, S3 payloads, instance restart.
3. **Telemetry, attestation and operations** (`docs/acceptance/phase-3.md`). OTLP ingest with identity stamping, SPIFFE JWT-SVID attestation, CLI audit and session commands, custom health checks for running instances, and an optional migration Job with a separate database credential. Remote MCP servers with per-user OAuth are deferred to the backlog.

A phase's design (`docs/design/phase-N.md`, with its interaction table) is reviewed before its issues are filed.

## 18. Testing

- **Unit:** Go `testing` + `testify`, table-driven.
- **Integration** (`//go:build integration`): PostgreSQL in Docker, and an S3-compatible server in Docker (the first infra issue picks a maintained one and records why).
- **Kubernetes e2e:** kind with Flux installed. The gateway runs in-cluster from a locally built image, and a Go MCP client (the official SDK) drives it, with a fake OIDC provider (the first infra issue picks a maintained library, for example `github.com/oauth2-proxy/mockoidc`). The test MCP server is a small Go binary in `test/mcpserver/` that echoes its environment and secrets so tests can check rendering.
- **Acceptance:** each checklist scenario has a test tagged `[AC-<phase>-<nn>]`. `make acceptance` prints passing scenarios out of the total. Scenarios that need a real Claude Code session or a real browser are marked `manual` in the checklist and checked by the owner.
- Tests never depend on wall-clock speed. Timing-sensitive tests pass `-count=50 -race`.

## 19. Repository layout

```
cmd/toolgate/            main: serve + client commands
internal/                gateway and client packages
api/v1alpha1/            CRD Go types (controller-gen)
config/crd/bases/        generated CRD manifests (the chart renders them)
deploy/charts/toolgate/  Helm chart (CRDs, RBAC, admission policy)
plugin/                  Claude Code plugin + marketplace manifest
test/                    e2e harness, test MCP server, fixtures
docs/                    user/ admin/ dev/ agents/ design/ acceptance/
scripts/                 dev and agent scripts
```

## 20. Documentation plan

| Audience | Knows | Needs | Pages |
|---|---|---|---|
| Users | Claude Code | Install the plugin, sign in, use servers, set secrets, what a refusal means | `docs/user/` |
| Admins | Kubernetes, Helm, Flux, OIDC, PostgreSQL | Deploy, configure, write templates and rules, the security model's limits, operations | `docs/admin/` (configuration and chart references generated) |
| Developers | Go, MCP | Architecture, packages, testing, releasing | `docs/dev/` |
| Agents | This repo | Playbooks | `docs/agents/`, `.claude/`, `CLAUDE.md` |

## 21. Open questions

1. Does `headersHelper` receive `CLAUDE_CODE_SESSION_ID`? Answered: no. `toolgate headers` uses the SessionStart hook's file instead (§13).
2. Can Coder vouch for a workspace the way TokenReview does for pods? Phase 3 investigates before designing it.
