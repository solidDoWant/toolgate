# toolgate specification

toolgate is a self-hosted gateway between AI coding harnesses (first of all Claude Code) and MCP servers. A harness makes one MCP connection to toolgate. Within a session, the model lists the MCP servers it may use and turns them on and off with tool calls. toolgate starts the servers for that session or user in Kubernetes, checks every tool call against the owner's rules, and records everything.

This file is the authoritative reference. Each phase is done when its checklist in `docs/acceptance/` passes (see "Phases").

## 1. Purpose and reference behaviour

**The owner's goal:** run MCP servers that are built for one user at a time, including servers that need that user's credentials when they start. Each user gets their own instance, with their own secrets. A session can turn servers on and off as it needs them. The owner can block servers or single tools for chosen users, workspaces or sessions without touching the client. Everything is deployed and configured through GitOps.

**Reference products.** Where this spec doesn't decide a behaviour, decide it in this order:
1. What obot (obot-platform/obot) does for its Kubernetes MCP runtime: per-user server instances, a catalog, credentials supplied per user.
2. What the MCP specification and Claude Code's documented behaviour support.
3. The simplest behaviour that keeps principle 1 (see "Principles").

Record the choice on the issue.

**Users of toolgate:**
- **Users** run Claude Code on a laptop, in a Coder workspace, in a pod or in CI. They sign in once and use MCP servers.
- **Admins** deploy toolgate with Helm and Flux, and write catalog entries and rules in git.
- **The owner** is both.

## 2. Principles

These are non-negotiable. Reviews and tests enforce them, and only the owner can change them.

1. **No tool call is forwarded without an authorization decision, and every decision is recorded.** There is no path from a client to an MCP server that skips the policy engine or the audit log.
2. **Trust only what is verified.** Fields a client merely claims (see "Identity and attestation") can narrow access, never widen it. Attestation decides what a request may reach.
3. **Git is the source of truth for admin configuration.** Catalog entries, standing rules and settings profiles are Kubernetes objects applied by Flux. toolgate reads them and never writes them.
4. **Secrets stay secret.** User secrets are encrypted at rest, appear in plain text only inside Kubernetes Secret objects created for an instance, and are never logged, never placed in a template's other fields, and never returned by any API after they are set.
5. **Done means the owner's checklist passes.** Nobody raises the bar beyond the checklist and principles 1–4.

## 3. Target environment

- Kubernetes 1.35 or later, with Flux v2 (helm-controller, source-controller) installed in the cluster.
- PostgreSQL 16 or later, reached with TLS. Client-certificate authentication must work (the owner's cluster uses CloudNativePG with client certificates).
- S3-compatible object storage (the owner uses Ceph RGW).
- An OIDC identity provider. The owner uses Authentik. Device-code and authorization-code flows with PKCE are both required.
- Network policies: plain Kubernetes NetworkPolicy always works. CiliumNetworkPolicy is allowed in templates (see "Catalog entries and instances") when the cluster has Cilium.
- Clients: Claude Code 2.1.300 or later. Any MCP client using Streamable HTTP can connect, but only Claude Code is tested.

The **only** backends are PostgreSQL and S3. No Redis, no message queue, no second database.

## 4. Architecture

One Go binary, `toolgate`, has two roles:
- **`toolgate serve`** is the gateway: a stateless HTTP server, run with two or more replicas.
- **The client commands** are the CLI, and the hooks and helpers of the Claude Code plugin (see "Client: CLI and Claude Code plugin").

Gateway components (packages under `internal/`):

| Component | Job |
|---|---|
| Auth | Verifies the bearer token (user OIDC token, toolgate session token, Kubernetes service-account token, or SPIFFE JWT-SVID). Produces the request's identity (see "Identity and attestation"). |
| MCP endpoint | Streamable HTTP MCP server. Serves the meta tools (see "MCP surface") and every enabled server's tools, renamed. Forwards calls. |
| Session manager | The enabled-server set per session, in PostgreSQL. Idle timeouts. |
| Policy engine | Decides every enable, list and call (see "Policy"). |
| Runtime manager | Renders templates, applies them to Kubernetes, watches readiness, tears instances down (see "Catalog entries and instances"). |
| Secret broker | Stores user secrets and resolves `${secret:…}` and `${resolver:…}` (see "Secrets"). |
| Catalog reader | Watches the custom resources (see "Custom resources") through the Kubernetes API. |
| Audit | Writes every decision and call (see "Audit and telemetry"). |
| Web UI and API | Server-rendered pages for the things that need a browser or a person (see "Web UI"). |
| OTLP ingest | Receives Claude Code's telemetry and stamps it with verified identity (see "Audit and telemetry"). |

**Replicas share nothing in memory except caches.** MCP sessions are shared through PostgreSQL. At `initialize`, the gateway stores the session's binding and protocol state in a row before it answers. A replica that gets a request for a session it doesn't hold reads the row, checks the binding against the request's verified identity, and serves the request. So a replica that dies or restarts ends no session: only its requests in flight fail. The ingress needs no session affinity. A client's open MCP stream lives on one replica. A change that affects a session (rule change, instance ready, grant) is written to PostgreSQL and announced with `LISTEN/NOTIFY`. The replica holding the stream then acts on it: it sends `notifications/tools/list_changed` and refuses calls the change no longer allows. A stream that attaches gets one `list_changed` at once, so a change made while the client had no stream is not lost. The gateway adds the replica ID to the ID of each request it sends to a client. If the client's answer reaches another replica, that replica sends it to the asking replica with `NOTIFY`.

## 5. Protocols

- **MCP.** Streamable HTTP transport, revisions 2025-11-25 (and older) and 2026-07-28. toolgate prefers 2025-11-25 and uses 2026-07-28 only with a peer that supports nothing older. 2026-07-28 removes protocol sessions (`Mcp-Session-Id`, `initialize`), `ping` and the GET stream, and replaces them with per-request `_meta`, `server/discover` and `subscriptions/listen`. The gateway runs two go-sdk handlers and routes each request: one whose `Mcp-Protocol-Version` is `2026-07-28` or later, and whose method (the `Mcp-Method` header, else the body) isn't `server/discover`, goes to the stateless handler; every other request goes to the stateful one. `server/discover` needs a token but no session header on any revision, is answered with the revisions before 2026-07-28 only, and makes no session. A 2026-07-28 request without `X-Toolgate-Session` gets HTTP 400 with a plain JSON-RPC -32600 error (never -32022), so a dual-era client falls back to `initialize`. A stateless request reads the same PostgreSQL session state, keyed by `X-Toolgate-Session`, and every call on it goes through the policy engine. `tools/list` results are marked `cacheScope: private` with `ttlMs: 0`. Details: `docs/design/phase-4.md`.
- **Upstream MCP servers** are reached over Streamable HTTP inside the cluster. toolgate speaks 2025-11-25 and 2026-07-28 with them, and prefers 2025-11-25. When an instance becomes ready, toolgate connects on 2025-11-25 and tries 2026-07-28 only after the server refuses 2025-11-25 (a JSON-RPC error or HTTP 400, 404 or 405 to `initialize`, a refused `initialize` revision, or HTTP 400 to the next request). Connection errors, timeouts, 429 and 5xx are not refusals. If `server/discover` then lists an older revision, the refusal was transient and the next step tries 2025-11-25 again. The chosen revision is stored on the instance row (`mcp_revision`) and never changes while the row lives; forwarding and monitoring use it. On 2026-07-28 the health check is a `server/discover` every 30 s instead of `ping`, and tool list changes arrive through `subscriptions/listen` (`docs/admin/mcp-revisions.md`). A stdio-only server is wrapped in its template by an adapter container that exposes it over HTTP (the template author's choice; toolgate does not run stdio processes itself).
- **Client authentication:** `Authorization: Bearer <token>` on every request.
- **Identity headers:** `X-Toolgate-Session` (session ID) and `X-Toolgate-Workspace` (claimed workspace ID). Both must match `^[A-Za-z0-9._-]{1,128}$`, or the request is refused. The session header is required, except on `server/discover`. Without a workspace header, the workspace is `default`.
- **Tool search header:** `X-Toolgate-Tool-Search: on|off` turns search mode on or off for a request. When it is absent, the setting `toolSearch` (default `false`) decides. It is read on each request. It is a claimed value that only changes what a tool list holds, never a decision.
- **Admin and user API**: JSON over HTTP under `/api/v1/`, described by an OpenAPI document generated from the Go handlers and published in `docs/dev/`.

## 6. Identity and attestation

Every authenticated request has an **identity**:

| Field | Source | Verified? |
|---|---|---|
| `principal` | User: the OIDC `sub`, with `email` and `groups`. Pod: `system:serviceaccount:<ns>:<name>`. SPIFFE workload: the SPIFFE ID. | Always |
| `workspace` | Pod: `k8s/<namespace>/<owner kind>/<owner name>`, read from the token's pod claims. SPIFFE workload: the SPIFFE ID's path, such as `/owner/alice/workspace/dev`. Attested user: the gateway's `workspaceAttestation.workspaceID`, rendered from the bound workload, such as `coder/alice/dev`. User: the claimed header, prefixed `claimed/`. | Workload and attested user |
| `session` | The `X-Toolgate-Session` header. Without it, `mcp.<Mcp-Session-Id>`, made by the gateway at `initialize` | Never |
| `attestation` | `attested` or `user` | Computed |

**Attestation levels:**
- **`attested`:** the token was issued by a platform that vouches for the environment. For a pod: a Kubernetes projected service-account token with audience `toolgate`, checked with TokenReview, from a namespace listed in the gateway's configuration. The gateway also accepts SPIFFE JWT-SVIDs from allowed trust domains (the SPIFFE ID's path becomes the workspace), which covers Coder workspaces and other workloads running a SPIRE agent.
- **Attested user session:** a user token in `Authorization` plus a workload token in `X-Toolgate-Attestation` (raw, at most 16 KiB): a projected service-account token from a namespace in `workspaceAttestation.namespaces`, bound to a pod, or a JWT-SVID matching `workspaceAttestation.spiffeIDPattern`. The gateway reads the owner from the pod's labels (or annotations) or the SPIFFE ID and compares it with the user token on every request. If they match, the identity stays the user's (principal, groups, secrets, grants), with `attested` attestation and the verified workspace. The attestation never authenticates: a bad user token gets 401. A failed attestation is served as `user` with the claimed workspace and writes `auth.refuse` with the reason and the user's principal. If TokenReview or the pod lookup is unavailable, the gateway answers 503 with `Retry-After: 1`. Workload bearer tokens ignore the header. When the setting `workspaceKeys` is on, the header can instead hold a short JWT signed by a registered workspace key, an Ed25519 key that the user approves in the web UI, and the workspace is then `registered/<username>/<name>` (`docs/design/workspace-keys.md`).
- **`user`:** a person signed in to the IdP, and the machine only describes itself.
- Anything else is refused with 401.

**Users and groups** come from the OIDC ID token or access token claims (`email`, `groups`). The claim names are configurable. `email` is used only when the `email_verified` claim is `true`, because an unverified email is a claim the user chose. Service accounts have no groups, but templates and rules can name them directly.

**Sign-in through another service:** when `TOOLGATE_TOKEN_COMMAND` is set, the CLI runs it (`/bin/sh -c`, empty stdin, 5 s limit, 16 KiB of output, one `token68` line) and uses its output as the user token instead of the stored sign-in. The token is cached in the runtime directory while its `exp` is more than 60 s away. Setting `TOOLGATE_SPIFFE_SOCKET` as well is an error. The gateway must list the token's issuer and audience: `oidcIssuerURL` with `oidcAudience`, or an item of `oidcAdditionalIssuers`.

**Client sign-in (users):** the CLI runs the OAuth 2.0 device authorization grant against the IdP with PKCE, using `golang.org/x/oauth2`. It stores the refresh token in the OS keychain (`github.com/zalando/go-keyring`), with a 0600 file fallback when no keychain exists. `toolgate headers` prints a fresh access token. The gateway accepts the IdP's access tokens directly, checked against the IdP's JWKS with `github.com/coreos/go-oidc/v3`, one of the configured audiences, and expiry. `oidcAudience` is a list of 1 to 8 audiences (a single string is a one-item list), all at the one issuer: each is a client whose access tokens the gateway accepts, so each must be a client made for toolgate, and every client must give a person the same `sub`. A token for none of them gets 401 and `auth.refuse` with "audience not allowed". `oidcAdditionalIssuers` (YAML only, at most 8) adds issuers for an IdP that gives each client its own issuer (Authentik): each item has its own `issuerURL` and 1 to 8 audiences. The token's unverified `iss` picks the issuer by exact match, then that issuer's JWKS and audiences verify it; an `iss` that isn't configured gets 401 ("invalid token"). The claim settings and the `sub` principal apply to every issuer, so every issuer must give a person the same `sub`. `oidcIssuerURL` stays the issuer the CLI and the web UI use, and must be reachable at start; an additional issuer is discovered when a token first names it, and while it is unreachable only its tokens are refused ("issuer unavailable"). toolgate issues no tokens of its own.

**Connector sign-in:** toolgate is a standard OAuth-protected MCP server, and the IdP is the authorization server. The gateway serves its protected resource metadata (RFC 9728) without authentication at `/.well-known/oauth-protected-resource/mcp` and `/.well-known/oauth-protected-resource`, built with the go-sdk's `oauthex.ProtectedResourceMetadata` and `auth.ProtectedResourceMetadataHandler`: `resource` is `mcpPublicURL` (default `publicURL` + `/mcp`), `authorization_servers` is [`oidcIssuerURL`], `scopes_supported` is `oidcCLIScopes`, and `bearer_methods_supported` is [`header`]. Both paths have the rate limit of `/.well-known/toolgate-client`. A refused `/mcp` request gets `WWW-Authenticate: Bearer resource_metadata="<mcpPublicURL origin>/.well-known/oauth-protected-resource/mcp"`, with `error="invalid_token"` when a token was sent. Other routes keep `WWW-Authenticate: Bearer`. With `mcpPublicURL` and `publicURL` both empty, no metadata is served and the header has no `resource_metadata`. A connector's token is verified as every user token, so the connector's client ID must be one of the audiences. toolgate issues no tokens and never forwards a client's token to upstream servers. The only user credential an upstream server can get is one the gateway exchanges for it, when a template asks for `${exchange:…}` (`docs/design/instance-credentials.md`).

**Sessions without the headers helper:** a client that sends no `X-Toolgate-Session`, such as a connector, gets a session tied to its MCP session. At `initialize`, the gateway makes an `Mcp-Session-Id` of 32 lowercase hex characters from `crypto/rand`, and the session ID is `mcp.<Mcp-Session-Id>`. Its workspace is `claimed/default` unless `X-Toolgate-Workspace` is sent, so it is unverified. It is a normal session with its own enabled set. A request with an `Mcp-Session-Id` whose stored binding names another identity, or that has no live session, gets 404 on every replica, so a client can't pick an `mcp.` session by sending an ID. An `X-Toolgate-Session` that starts with `mcp.` gets 400 on every route. A client `DELETE` of the MCP session on any replica, or the end of the MCP session after 30 minutes without use, ends the session at once with `session.end` (reason `mcp-session-delete` or `mcp-session-idle`), and its per-session instances stop. Its ID is never used again, so it is never resumed. On 2026-07-28, which has no MCP session, the header stays required.

## 7. MCP surface

**Meta tools**, always present:
- **Catalog summary.** The `instructions` of the `initialize` result (and of a 2026-07-28 `server/discover` result) are built per session. They have one intro line, then one line `name: description` for each server the session can use now (every policy step before the call allows it), then "More servers may be available on request; call `toolgate_list_servers`." when the user has servers that are blocked only by the workspace scope, then a short how-to. At most 20 servers are listed, then "and N more; use toolgate_find_tools"; the text has 4 KiB at most. With no usable server it points to `toolgate_list_servers` and the Workspaces page. Only the template's `spec.description` goes in, cleaned of control characters and line breaks and cut to 120 characters. Text an upstream server reports never goes in. The descriptions of `toolgate_find_tools` and `toolgate_list_servers` also name the usable servers (same cap). The gateway updates them, and sends `notifications/tools/list_changed`, when the usable set changes: approvals, revocations, defaults, or templates added or removed.
- `toolgate_list_servers()`: the catalog entries this identity may enable, with name, description, scope, whether each is enabled in this session, its status (`stopped`, `starting`, `ready`, `failed`, `blocked`) and why it is blocked if it is. An enabled server with no live instance shows as `stopped`. In a session with a verified workspace, a server outside the workspace's effective list (see "Policy") shows as `blocked`, with the reason "not allowed in this workspace; enable it to ask". In a session without one, such a server shows as `blocked` too, with a reason that names the default for unverified sessions and the commands that change it. `toolgate_server_status` returns the same status and reason, and `toolgate_find_tools` with no match says that servers are blocked in unverified sessions. Only servers outside the user's access lists are left out.
- `toolgate_enable_server(name)`: checks policy, starts or reuses the instance, waits for it to be ready while sending MCP progress notifications, then adds its tools and sends `notifications/tools/list_changed`. It returns the tools added. A timeout or failure returns an error naming the reason (image pull, readiness timeout, policy). It is idempotent.
- **Calls that enable.** A call of a tool of an allowed server that the session has not enabled enables the server first, then goes on as a normal call. It checks policy for the tool, a missing secret, and the limit of 32 servers, and each refusal is a `tool.decision`. In a verified workspace, a server outside the list asks the user as an enable does; in an unverified session the call is refused at once. A deny entry never asks. The enable writes `server.enable` (allow) with `reason: first-call`, and the session gets `list_changed`. Two calls at once write one `server.enable`. `toolgate_disable_server` still removes the server at once, and a later call enables it again.
- **Enable and disable in search mode.** Sessions list `toolgate_enable_server` and `toolgate_disable_server` in every mode. A call to a tool of an allowed server already enables it in search mode, but a server whose tools search can't find yet (a remote template without `declaredTools` and no live instance) would otherwise be a dead end. The mode is per session, and a request that changes it sends `list_changed`. In search mode the access request lives on the blocked server: a call to one of its tools asks as an enable does, `toolgate_list_servers` and `toolgate_server_status` say to call a tool or use `toolgate_allow`, and `toolgate_find_tools` only marks such tools `ask` and creates no request.
- `toolgate_disable_server(name)`: removes the server's tools from this session and sends `list_changed`. A per-session instance stops when no session uses it.
- `toolgate_server_status(name)`: status, the reason for the last failure, and the time the instance became ready.
- `toolgate_find_tools(query, limit)` (search mode only): ranks the tools of the servers this identity may use or, in a verified workspace, may ask to add, with PostgreSQL full-text search over the tool search rows (and, when an embeddings endpoint is configured, a ranking by meaning fused with it; see `docs/admin/tool-search.md`). It returns `{"tools": [{name, server, description, inputSchema, access, approval}]}` with `access` `allowed` or `ask`. The tools it returns are stored as the session's found tools (at most 50, the oldest pushed out) and listed in search mode. Outside search mode it is not listed, and a call gets "Tool search is off for this session." In search mode `tools/list` holds the meta tools, `toolgate_find_tools`, the tools of enabled servers and the found tools, each through the policy decision.

**The built-in `toolgate` server.** A gateway with the web UI has a built-in server named `toolgate` in its catalog. Without the web UI, it is not in the catalog, and a call of one of its tools is refused with `not-offered`. Policy treats it like a template with `scope: shared` and environment `any`. Every signed-in user has access to it. Service accounts and SPIFFE workloads are refused at the access step. Blocks, rules and workspace scope apply to it by the name `toolgate`. The `tools` pattern of a rule or temporary block matches each of its tools by the exact name, such as `toolgate_allow`, and as `toolgate__allow`. A session enables it like any other server, and it counts toward the 32 servers. It has no instance and always shows as `ready`. Its tools are named `toolgate_list_scopes`, `toolgate_allow` and `toolgate_deny`. Their upstream names are `list_scopes`, `allow` and `deny`. A call of one goes through the full call path, with `tool.decision`, approvals and `tool.result`, but skips the instance steps. Each tool runs the same code as the workspaces and sessions API, as the calling user, with no extra rights. `toolgate_list_scopes` takes no input. It returns the calling session, the user's defaults and unverified allow list, each workspace's allowed servers, allowed tools and denied entries, and each open session's allowed and denied entries. It shows at most 200 workspaces and 200 sessions, and only the caller's own. Tools are shown by their exposed names. `toolgate_deny` takes exactly one of `workspace` and `session`, and `servers` with 1 to 32 items. An item is a server name, or `<server>__<tool>` with the exposed tool name. A tool of the built-in server is `toolgate__<upstream>`, for example `toolgate__deny`. The tool runs the same deny as `POST /api/v1/workspaces/changes` and `toolgate workspace deny`, with `via: tool`. It applies at once, with no request and no step-up. A session target can be unverified, because a deny only narrows, but it must be one of the caller's own open sessions. `toolgate_allow` takes the same input. A workspace target must be valid for a list. A session target must be one of the caller's own open sessions with a verified workspace. A tool of a server that has a server-level `deny` entry on the target is refused with "Allow the server first." The tool never changes scope itself. It finds the `scope` access request named by `RequestState` (every field must match), else a pending one with the same target and change, and only then inserts one, as `POST /api/v1/workspaces/changes` does. With URL elicitation, it asks the client to open the approval page and waits for the decision, up to `approvalTimeout`, counted toward the 32 waits per principal. It returns `{"status": "approved"|"denied"|"expired"|"pending", "request", "url"}`. Without elicitation, or on `decline` or `cancel`, the status is `pending`, which isn't a tool error. The change applies only when the user approves it on the page with the step-up. The leader writes the search rows of the built-in server when it gets the Lease.

**Catalog tools.** The catalog list of a template is the first of: its `spec.declaredTools`, the `tool_catalog` row of its current generation, and the row of the newest older generation. The reconciler writes the row, in the same transaction as the `ready` write and each tool list change of a ready instance, and not when the template declares tools. A remote template never has a row, and its catalog list is its `declaredTools` only: its live list comes from one user's credential, so it stays on that user's instance row. Without `declaredTools`, `toolgate_find_tools` ranks the tools of the caller's own ready instance of the remote server with the search rows, so the tools are searchable for the user who enabled it and never for another user. While an enabled server's instance is `pending` or `starting`, or no instance is live, `tools/list` shows the catalog list (curated, each tool still decided by policy). It shows none when the newest instance failed on the current generation, and none for an unhealthy instance. When an instance becomes ready with a different list, sessions get `list_changed` and the real list.

**Proxied tools** are named `<server>__<tool>`. A server name matches `^[a-z][a-z0-9-]{0,30}$` and a tool keeps its own name. If the result is over 64 characters, the call is refused and the server is marked `failed`, because Claude Code's own `mcp__toolgate__` prefix makes long names unusable. Descriptions and input schemas pass through unchanged. Prompts and resources are out of scope (see "Won't do").

**Curated names.** A template's `toolFilter` hides tools and `tools.<name>.name` renames them. A session sees the exposed name in `<server>__<tool>`; policy, grants, approvals and the audit `tool` field use the upstream name, and audit events add `exposedTool` when it differs. A hidden tool, or a renamed tool called by its upstream name, is refused with reason `not-offered` (the refusal is recorded) and never forwarded. Curation is read from the current template, so a change applies at once to running instances; while the template is invalid, its last valid generation is used.

**A call** is: identity → policy (see "Policy") → audit `decision` event → forward to the instance → audit `result` event. An upstream error is returned to the client as a tool error with the upstream message. A policy refusal is a tool error that names the rule that refused it, so the model and the user can see why.

**Sessions.** A session is the pair (principal, session ID). It's created by the first request carrying them. Its enabled set survives reconnects and gateway restarts. It ends after `sessionIdleTimeout` (default 2 h) without requests, and then its per-session instances stop. An ended session keeps its enabled set for `sessionResumeWindow` (default 24 h, 1 h to 7 days); after that the reconciler deletes the set and writes `session.forget`. A request for an ended session within the window, from the same workspace and attestation, resumes it with its set and writes `session.resume`: each server's policy is decided again and its instance is started without waiting (an `onCall` server with a catalog list is restored without an instance and starts on its first call), and a server refused now or over an instance limit is dropped with `server.disable`. A call to a server whose live instance is still `pending` or `starting` (after a resume, a restart or a repair) waits for it, as an enable does, up to `startTimeout`, and is then decided and forwarded. From another workspace or attestation the set is cleared and `session.start` is written. Another principal using the same session ID gets a different session.

## 8. Custom resources

All are in API group `toolgate.soliddowant.dev`, version `v1alpha1`. The chart installs the CRDs. toolgate only reads them and writes their `status`.

- **`MCPServerTemplate`** (namespaced, in the namespaces the gateway is configured to watch): one catalog entry (see "Catalog entries and instances").
- **`ToolgateRule`**: a standing block rule (see "Policy").
- **`SettingsProfile`**: Claude Code settings handed to clients (see "Client: CLI and Claude Code plugin").
- **`TokenExchange`** (namespaced, in the catalog namespaces): an OAuth token exchange that templates in the same namespace use. The gateway uses it only if its `tokenURL` is on the chart-only allow-list (by default the token endpoints of the user OIDC issuers). Its `Valid` condition gives the reason when it is not valid.

Every resource is validated by CEL rules in its CRD and again when toolgate reads it. An invalid resource is skipped, with the reason in its `status.conditions` and in the log. For an `MCPServerTemplate`, the condition is `Valid`. Its status is `False` and its message gives the reason. A previously valid version keeps working until a valid replacement arrives.

## 9. Catalog entries and instances

```yaml
apiVersion: toolgate.soliddowant.dev/v1alpha1
kind: MCPServerTemplate
metadata: {name: github, namespace: toolgate-catalog}
spec:
  description: GitHub issues, PRs and repositories
  scope: per-session            # optional: per-session | per-user | shared
  namespace: per-instance       # per-instance | shared
  idleStop: 30m
  updatePolicy:                 # optional: when outdated instances are replaced
    mode: manual                # manual (default) | immediate | whenIdle (needs idleFor, at least 1m)
  startTimeout: 5m
  access:
    users: []                   # globs on OIDC subjects or verified emails; "*" = every user
    groups: [dev]               # globs on group names
    serviceAccounts: []         # system:serviceaccount:ns:name
    spiffeIDs: []               # globs, such as spiffe://td/owner/*/workspace/*
    environments: opt-in        # any | opt-in (default) | attested
  tools:                        # optional per-tool overrides
    delete_repository: {environments: attested, approval: required}
    list_secrets: {blocked: true}
    create_issue: {name: open_issue, description: Open an issue., inputDescriptions: {repo: owner/name}}
  start: onCall                 # onCall (default) | onEnable
  toolFilter: {deny: [delete_repository]}   # exactly one of allow and deny
  declaredTools:                # optional; the admin's own tool list
    - {name: create_issue, description: Create an issue., inputSchema: {type: object}}
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

**Remote servers.** A remote template points at an MCP server that already runs elsewhere. toolgate starts nothing for it:

```yaml
spec:
  remote:                  # scope derived: per-user, because a header uses a secret
    url: http://mail-archiver.email.svc.cluster.local:5000/mcp
    headers:               # at most 32; names 256 characters, values 4096
      Authorization: "Bearer ${secret:mail-archiver-api-key}"
    timeout: 30s           # optional; per request, 1s-5m
```

`remote` cannot be set together with `endpoint`, `resources` or `healthCheck`. Its scope follows the rule under "Scopes": a remote server whose headers use `${secret:…}` or `${principal.*}` is `per-user` or narrower, and one with no headers or fixed header values can be `shared`. `endpoint` is required only without `remote`. The CRD checks these rules. The catalog checks that `remote.url` is `https://`, or `http://` only to a host ending in `.svc` or `.svc.cluster.local`, with no user name or password. It also checks that header values use only `${secret:<name>}` and `${principal.*}`. Each problem is reported in the `Valid` condition. Header values never appear in status, events, logs, audit events or metrics. Only header names may. Remote templates cover static credentials only. OAuth to remote servers stays in the backlog. Enabling a remote server first checks that the user set each secret its headers name, with the same message as for a launched server. A remote server has an instance row of its scope with no namespace and no Kubernetes objects, and it counts toward no instance limit. The replica that serves a call connects to the URL with headers filled in from the instance owner's secrets at connect time, and keeps that connection only for that instance. A per-user or per-session instance serves only its owner's calls, so one user's headers never serve another user's call. A shared instance has no owner and its headers hold nothing per-user, so one connection serves every user. A 401 or 403 from the server gives the user "the remote server refused your credential" with the `toolgate secret set` command. The connection closes when the instance stops (no live session has the server enabled and `idleStop` passed) or the template changes. Tool naming, `tools:` overrides, rules, grants, audit and access lists work as for launched servers. The gateway's pods make the connection, so the cluster owner must allow their egress to the remote host.

`persistentVolumes` (at most 8; `name`, `size`, `storageClassName`, optional `scope`) are volumes that outlive the instance. Each volume scope must match the template's effective scope, and the field needs `namespace: per-instance`, no `remote` and the gateway setting `persistentVolumes.enabled` (`docs/admin/persistent-volumes.md`, `docs/design/persistent-volumes.md`).

**Tools and curation** (see `docs/design/phase-4.md`): `start` says when an instance starts. `declaredTools` (at most 512 tools, 1 MiB, unique names) is the admin's own tool list. `toolFilter.allow` or `toolFilter.deny` (exactly one; at most 512 names of 128 characters) hides tools. `tools.<name>` also sets the exposed `name` (`^[A-Za-z0-9_.-]{1,128}$`, at most 64 characters with the `<server>__` prefix), a `description` (4096 characters) and `inputDescriptions` (64 entries). A name that collides with another tool, or equals the name of a hidden tool, makes the template invalid. `tools.<name>.cache` caches the results of a tool that its server marks `readOnlyHint`, in PostgreSQL, after policy runs (see `docs/design/tool-result-cache.md`). The server name `toolgate` is reserved for the built-in server: a template with it is invalid and counts as deleted.

**Scopes:**
- `per-session`: one instance per session.
- `per-user`: one instance per principal, shared by that principal's sessions.
- `shared`: one instance for everyone.

A template's scope must be at least as narrow as the variables it uses in `resources`, `endpoint.service`, `healthCheck.http.service` and `remote.headers`:

| The template uses | Broadest scope allowed |
|---|---|
| nothing per-identity (only `${instance.*}`, `${template.name}`, or no variables) | `shared` |
| `${principal.*}`, `${secret:…}`, `${resolver:…}` or `${exchange:…}` | `per-user` |
| `${session.id}` or `${workspace.id}` | `per-session` |

`scope` is optional. Without it, the effective scope is the broadest one the table allows. An admin may set a narrower scope, for example `per-user` for per-user state on disk. A broader one makes the template invalid, with a reason that names the field and the variable but never a header value. The `Valid` condition of a valid template gives the effective scope and why, and every part of the gateway (the catalog, the renderer, instances, the server list and the web UI) uses the effective scope. One function decides it, so they can't disagree.

**Instance namespaces.** With `per-instance`, toolgate creates the namespace `tg-<instance id>` and prepares it before applying the template:
- Pod Security `restricted` enforced;
- a default-deny NetworkPolicy for ingress and egress, plus rules allowing DNS and ingress from the gateway's pods;
- the default service account with `automountServiceAccountToken: false`;
- labels naming the template, instance and owner principal (hashed).

With `shared`, all instances go in one configured namespace and are told apart by labels and by `${instance.id}` in their names and selectors. toolgate never renames objects: the catalog refuses a template that doesn't tell its instances apart, and every `shared` template when no shared namespace is configured. Only templates whose effective scope is `shared` may use it; for other scopes it is a validation error, so an instance holding one user's secrets always has its own namespace. Deleting a per-instance namespace is the teardown.

**Resources** is a list of Kubernetes objects. The chart grants these kinds by default: `Pod`, `Deployment`, `StatefulSet`, `Service`, `ConfigMap`, `Secret`, `ServiceAccount`, `NetworkPolicy`, `PersistentVolumeClaim`, and `CiliumNetworkPolicy` when that CRD exists. A template may contain any namespaced kind, such as Flux's `HelmRelease` and `OCIRepository`, and toolgate has no special handling for them. What the gateway can create is decided by the RBAC the cluster admin grants its service account. The chart's default ClusterRole covers the kinds above, the chart value `rbac.extraRules` adds more, and an apply that RBAC refuses fails the instance with the API server's message. Cluster-scoped kinds are a validation error, because teardown deletes the instance namespace and such an object would leak. The gateway needs a ClusterRole, because instance namespaces are created on the fly, and it reads Secrets only in the template secrets namespace, when the admin configures one for `${templateSecret:…}` (`docs/design/instance-credentials.md`). The chart also installs a ValidatingAdmissionPolicy that applies to every write by the gateway's service account: it denies any write outside namespaces labelled as toolgate's, so a compromised gateway is confined to those namespaces whatever RBAC it holds there. Kinds like `HelmRelease`, whose controllers act with their own permissions rather than the gateway's, can do whatever that controller can; allowing them is the admin's trust decision. The exact rules are in `docs/design/phase-1.md` and `docs/design/phase-2.md`.

**Rendering rules:**
1. Parse the YAML first. Substitute variables only inside string scalars of the parsed tree. A value never adds keys or changes structure.
2. Variables: `${principal.id}`, `${principal.email}`, `${principal.name}`, `${workspace.id}`, `${session.id}`, `${instance.id}`, `${instance.namespace}`, `${template.name}`, `${secret:<name>}`, `${resolver:<name>}`, `${exchange:<name>}`, `${templateSecret:<name>/<key>}`. `$${` escapes a literal `${`. An unknown variable, or a variable that doesn't exist for this scope, fails the render.
3. Claimed values (`workspace.id` on `user` attestation, `session.id`) are already pattern-checked (see "Protocols"). When rendered into an object name or label, they are hashed to fit Kubernetes length rules.
4. `${secret:…}` and `${resolver:…}` are allowed only in `stringData` or `data` of a `Secret`. The one other place for `${secret:…}` is a value of a remote template's `remote.headers`, where only `${secret:…}` and `${principal.*}` are allowed. Anywhere else, the template fails validation. `${exchange:…}` (a token exchanged from the user's own token at the IdP, refreshed while the user is active) and `${templateSecret:…}` (a value from a Secret in the admin's template secrets namespace, which never narrows the scope and is kept current) follow the same rule, must be the whole value, and are decided in `docs/design/instance-credentials.md`.
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
- Any instance stops when its template is deleted or its scope changes. An invalid template stops nothing (see "Custom resources").

**Limits.** Each user can have a limited number of running instances (default 20), and the gateway a limited total (default 200). Enabling a server that is already running for the user always works. Stopping deletes the namespace, or the labelled objects in a shared namespace. A template's `persistentVolumes` outlive the namespace: toolgate keeps their PersistentVolumes and binds them to the next instance with the same template and scope (`docs/design/persistent-volumes.md`). A gateway that restarts finds its instances from the labels and the database, and reconciles them.

## 10. Policy

**Order of evaluation**, the same for enable, list and call:
1. **Catalog access:** the identity must match `access.users`, `groups`, `serviceAccounts` or `spiffeIDs`. A SPIFFE identity matches only `spiffeIDs`, and `spiffeIDs` matches nothing else. Entries of `users` and `groups` are glob patterns that match the whole value, so `users: ["*"]` means every user identity and no other kind. An entry without glob characters matches exactly. A template with no access lists is available to no one.
2. **Environment:** the server's `environments`, or the tool's override.
   - `any` passes.
   - `attested` needs `attested`.
   - `opt-in` needs `attested`, or a live grant for this principal and this server or tool.
3. **Blocks:** any matching `ToolgateRule`, or any live temporary block, refuses.
4. **Workspace scope**: for a signed-in user (identity kind `user`), the server must be in the session's effective list, else it's refused. In a verified workspace, the effective list is the user's list for that workspace if there is one, else all or none of the user's servers by the user's default for verified workspaces, plus the servers added to this session only. Without a verified workspace, it's all or none by the user's default for unverified sessions, and nothing more. A claimed workspace never selects a list, and a claimed session ID never adds servers. A user with no defaults set has `none` for both. The default for unverified sessions can also be `list`: then the effective list is the user's unverified allow list, and an empty allow list gives nothing.

   Scope entries are allow and deny entries for one server or one tool, made for a verified workspace or for one session. Step 4 decides a tool in this order. First, a deny entry of the verified workspace or of the session that names the server or this tool refuses with reason `denied`. Second, the effective list allows the whole server. Third, an allow entry of the verified workspace or of the session allows this tool. Otherwise the tool is refused. A decision for the whole server passes when the list allows it, or an allow entry allows one of its tools, and no deny entry names the server. `tools/list` then lists only the allowed tools. In a session without a verified workspace, only the session's deny entries apply. Allow entries and workspace entries never apply there, so a claimed field only narrows. A deny is never asked about: an enable refuses it at once, with no request, and `toolgate_list_servers` shows the server as `blocked`. An approved server request deletes no deny entry. A wait for such a request ends with the deny's refusal. Without the web UI, scope entries do nothing, deny entries included. This step only refuses. Service accounts and SPIFFE workloads skip it, and it's off on a gateway without the web UI. A server that leaves the list stays enabled with no tools, and its tools come back when it returns. In a verified workspace, an enable refused only by this step asks the user, who allows the server for the session or always for the workspace, after a step-up. Ending a session deletes its additions and expires its pending requests.
5. **Approval**: a tool with `approval: required` waits for a person's approval (see "Web UI"). A call waiting for approval is decided again on each wake, through step 4.

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
In globs, `*` matches any run of characters, including `/`. A template that doesn't set `access.environments` gets `opt-in`. A rule's `tools` glob matches `<server>__<upstream name>` or `<server>__<exposed name>` (a renamed tool), so an admin can write either name. Rules only refuse. Grants never come from rules. A rule matching only claimed fields (a `claimed/…` workspace or a session) still applies, but the docs say plainly that a client can avoid it by claiming something else.

**When decisions run:** on `list_servers`, on `tools/list` (blocked tools are left out), on `enable_server` and on every call. A rule change, and a change to a user's workspace lists or defaults, reaches every open session within 5 seconds (`LISTEN/NOTIFY`, then `list_changed`).

## 11. Secrets

- **Storage.** User secrets are rows of (principal, name, ciphertext, key ID), envelope-encrypted with Tink (`github.com/tink-crypto/tink-go/v2`, AEAD AES256-GCM). The key-encryption keyset is in a Kubernetes Secret mounted into the gateway, separate from PostgreSQL. Key rotation is a new keyset version. Old ciphertexts keep decrypting until re-encrypted by `toolgate admin rewrap`. Each ciphertext is bound to its principal and name (associated data), so a row moved to another user doesn't decrypt.
- **Setting a secret.** From the CLI: `toolgate secret set <name>` (reads stdin), `toolgate secret list` (names and dates only), `toolgate secret delete <name>`. They call `PUT /api/v1/secrets/<name>` (the body is the value), `GET /api/v1/secrets` and `DELETE /api/v1/secrets/<name>`, which act only on the verified principal's own secrets. A name matches `^[A-Za-z0-9._][A-Za-z0-9._-]{0,127}$`; a value is at most 4 MiB. The same is possible in the web UI.
- **Missing secret.** A template referring to a secret the user hasn't set fails `enable_server` with a message naming the secret and the command to set it.
- **Resolvers**. A resolver is an HTTP service named in the gateway's configuration. The gateway POSTs the identity and the template name with a service-account token, and gets back `{value, expiresAt}`. Example: exchanging a GitHub App installation token for the user. Values are never cached beyond `expiresAt`. An instance whose resolved secret expires is restarted with a fresh one.

## 12. Audit and telemetry

**Audit events** are JSON lines on stdout (`log/slog`) and rows in PostgreSQL:
- `session.start`, `session.end` and `session.forget` (both with the enabled set), `auth.refuse` (rate-limited);
- `server.enable`, `server.disable`, `instance.start`, `instance.ready`, `instance.fail`, `instance.stop`;
- `tool.decision` (allow or refuse, with the deciding rule), `tool.result` (success or error, duration, sizes);
- `rule.change`, `secret.set`, `secret.delete`;
- `grant.*`, `approval.*`, `block.*`;
- `instance.health` and `instance.reapply`, with no payload.
- `credential.exchange`, one for each token exchange: the instance, the exchange, `start` or `refresh`, the outcome, and the error code of a failure. It never holds a token.

Every event carries principal, workspace, session, attestation, server, tool, trace ID and request ID. Tool arguments and results are not logged to stdout. They are stored only when `audit.payloads` is on. Results are also stored in the tool-result cache, for tools whose template turns it on, until the entry expires (`docs/design/tool-result-cache.md`). Payloads over 64 KiB go to S3 (with object lock when the bucket supports it), referenced from the row. Rows are kept for `audit.retention` (default 90 days) and deleted in batches. Secrets and tokens are never logged.

**Gateway traces:** OpenTelemetry (`go.opentelemetry.io/otel`) spans for each request, policy decision, render, apply and forward. Incoming `traceparent` is honoured, so Claude Code's tool spans (it sends `traceparent` on HTTP MCP requests) become the parents. Metrics are Prometheus-format at `/metrics` on a separate port.

**OTLP ingest**: `/v1/traces`, `/v1/metrics` and `/v1/logs` over OTLP/HTTP (protobuf and JSON), using the collector's `pdata` packages for decoding. The request is authenticated like any other, through `otelHeadersHelper`. It is on only when the setting `otlpIngest` is `true`; otherwise the routes answer 404. The ingest removes, at every level, each attribute whose name starts with `user`, `enduser` or `workspace` after every character outside `[A-Za-z0-9]` becomes `_` and the case is lowered. Then it sets `user.id`, `enduser.id`, `user.email` (only when verified) and `workspace.id` from the verified identity on every span, metric data point and log record. `session.id` is kept as the client sent it, because it is claimed like `X-Toolgate-Session`. Prompt and tool content (what `OTEL_LOG_USER_PROMPTS`, `OTEL_LOG_TOOL_DETAILS` and `OTEL_LOG_TOOL_CONTENT` add) is removed unless the setting `telemetry.allowContent` is `true`. The plugin's `Stop` and `SessionEnd` hooks send each session's title, read from Claude Code's transcript, as the gauge `toolgate.session.title`. A title is written from the user's prompts, so it is content too: the ingest removes it unless `telemetry.allowContent` is `true`. The result is forwarded synchronously to the configured OTLP endpoint, with nothing buffered or retried by the gateway. This gives token usage (`claude_code.token.usage`, `claude_code.cost.usage`, and the `claude_code.llm_request` spans) per principal, workspace and session, with no proxy for model traffic.

## 13. Client: CLI and Claude Code plugin

**One binary, `toolgate`**, built for Linux, macOS and Windows (amd64, arm64). Commands:

| Command | What it does |
|---|---|
| `toolgate login [--gateway URL] [--token-store auto\|keychain\|file]` | Device-code sign-in, stores the refresh token. It learns the IdP from the gateway's `/.well-known/toolgate-client`. The store used is saved in `config.json` as `tokenStore` and is the only store later runs use (`TOOLGATE_TOKEN_STORE` sets the flag) |
| `toolgate logout`, `toolgate status` | |
| `toolgate headers` | `headersHelper` output: `Authorization`, `X-Toolgate-Session` (from the file the SessionStart hook writes), `X-Toolgate-Workspace` |
| `toolgate hook session-start` | SessionStart hook: check sign-in, fetch the settings profile, merge it, print context |
| `toolgate hook stop`, `toolgate hook session-end` | Stop and SessionEnd hooks: when telemetry is on, send the session's title to the OTLP ingest (see "OTLP ingest"). They print nothing and always exit 0 |
| `toolgate secret set/list/delete` | See "Secrets" |
| `toolgate servers` | List the catalog as the CLI user sees it |
| `TOOLGATE_TOKEN_COMMAND` (variable) | Every command that needs a user token runs it instead of reading the stored sign-in (see "Identity and attestation") |
| `toolgate configure` | Save `--gateway`, `--token-command`, `--attestation-token-file`, `--attestation-token-request` and `--attestation-key-file` in `config.json` (mode 0600), keeping the other fields; an empty value removes one. At most one of the three attestation settings; `--attestation-key-file` loads the workspace key first, saves its absolute path and prints its fingerprint. They are the file forms of `TOOLGATE_GATEWAY`, `TOOLGATE_TOKEN_COMMAND`, `TOOLGATE_ATTESTATION_TOKEN_FILE`, `TOOLGATE_ATTESTATION_TOKEN_REQUEST` and `TOOLGATE_ATTESTATION_SIGNER_FILE`, for the plugin's `headersHelper`, whose environment Claude Code strips of credential-like names. A set variable wins over the file; `toolgate login` keeps these fields |
| `toolgate otel-headers` | `otelHeadersHelper` output |
| `toolgate admin rewrap` | See "Secrets" |
| `toolgate sessions` | List the caller's open sessions and their enabled servers (`GET /api/v1/sessions`). It also prints each session's `allowed` and `denied` entries |
| `toolgate workspace list/allow/deny/clear/defaults` | Show and change the caller's workspace lists and defaults (`GET /api/v1/workspaces`, `POST /api/v1/workspaces/changes`). `--workspace ID` defaults to the verified workspace; `deny --session ID` denies in one of the caller's open sessions, verified or not. `deny` takes `SERVER` and `SERVER__TOOL` items (the tool's exposed name) and inserts deny entries for every target, also with no list (see `docs/design/phase-4.md`). `allow` takes `SERVER__TOOL` items and `--session ID` too; such an allow creates a `scope` access request (see "Web UI"). `clear` keeps the deny entries. `list` prints the tool entries and denies of each workspace, and the unverified allow list. `defaults --unverified none|all|list` sets the default for unverified sessions; `allow --unverified SERVER...` and `deny --unverified SERVER...` change the unverified allow list, at most 64 servers. Adding a server, and moving from `none` to `list` while the list has servers, create a `scope` access request; moving from `list` to `all` creates a `change` request; removing a server applies at once. A widening change creates an access request: the CLI prints its URL and polls `GET /api/v1/access-requests/{id}` every 2 s, exiting 0 only when approved |
| `toolgate workspace register NAME [--key-file PATH]`, `workspace keys list`, `workspace keys revoke NAME` | Register a workspace key and manage the caller's keys (`POST`, `GET` and `DELETE /api/v1/workspace-keys`). `register` uses the key file `<configuration directory>/workspace-keys/NAME.pem` by default, loads it if it exists and otherwise makes an Ed25519 key there (mode 0600, parent mode 0700). It sends only the public key, prints the approval URL and fingerprint, polls like `workspace allow`, and warns on standard error when the access token lives longer than 330 s. It does not change `config.json`; the user then runs `configure --attestation-key-file`. `keys list` prints `NAME`, `WORKSPACE`, `FINGERPRINT`, `STATUS`, `REGISTERED` and `LAST USED`; `keys revoke` revokes the key and denies its pending requests (see `docs/design/workspace-keys.md`) |
| `toolgate admin volumes list`, `admin volumes delete ID...` | List persistent volumes and mark them for deletion (`GET /api/v1/admin/volumes`, `DELETE /api/v1/admin/volumes/{id}`); admins only. `--owner PRINCIPAL`, `--json` |
| `toolgate audit tail` | Print the audit events the caller may see as they happen (`GET /api/v1/audit/tail`); `--after KEY`, `--json` |

**Tool search:** `toolgate headers` sends `X-Toolgate-Tool-Search` from `TOOLGATE_TOOL_SEARCH` (`on` or `off`) when it is set.

**Workspace ID (user attestation):** `toolgate headers` sends `TOOLGATE_WORKSPACE` if set. Otherwise it sends `<hostname>/<git remote repo name or directory name>`, normalised to the header pattern: each character outside `[A-Za-z0-9._-]` becomes `-`, and the value is cut to 128 characters. The repo name is the last path element of the remote `origin`, without `.git`.

**Session ID:** from a file the SessionStart hook writes. Claude Code doesn't give the session ID to a `headersHelper`: `CLAUDE_CODE_SESSION_ID` isn't in its environment. It gives it to a SessionStart hook, on standard input (`session_id`) and in `CLAUDE_CODE_SESSION_ID`. Evidence: [`docs/dev/claude-code-integration.md`](docs/dev/claude-code-integration.md). So `toolgate hook session-start` writes the session ID to `$XDG_RUNTIME_DIR/toolgate/<claude pid>` (or a directory under the user cache directory when that variable isn't set) before anything else, and `toolgate headers` reads it by walking its parent processes to the Claude Code process. Claude Code runs the hook and the helper at the same time, so `toolgate headers` waits up to 5 s for the file to appear. The design gives the details.

**The plugin** (directory `plugin/`, installed from this repo as a Claude Code plugin marketplace) contains:
- `.mcp.json`, with one HTTP server named `toolgate` at `${user_config.gateway_url}/mcp` and `headersHelper: toolgate headers`;
- a SessionStart hook running `toolgate hook session-start`;
- Stop and SessionEnd hooks running `toolgate hook stop` and `toolgate hook session-end`;
- `userConfig` for `gateway_url`;
- a short skill telling the model how to use the meta tools: list first, enable what the task needs, disable when done.

**Settings sync.** A `SettingsProfile` holds a fragment of Claude Code settings (`permissions`, `env`, `hooks`, and other keys) and a selector (groups, principals, attestation). The hook fetches the merged profile for the identity from `/api/v1/settings` and merges it into `~/.claude/settings.json`:
- It changes only keys it owns, recorded in `~/.config/toolgate/owned-settings.json`.
- A key the user changed by hand since the last sync is left alone and reported.
- A key removed from the profile is removed from the file.

Claude Code reloads the settings file during the session. Hooks delivered this way run commands on the user's machine, so the docs warn admins that a profile is code execution on every client.

## 14. Web UI

Server-rendered Go `html/template` pages, with htmx (a pinned, vendored copy) for partial updates and no JavaScript build step. The styles use Tailwind CSS v4: its standalone tool builds the stylesheet in `make`, and the binary embeds it, so nothing needs Node. Pages sit in one app shell (a sidebar on a desktop, a menu button on a phone) and use shared template components. Every form also works without JavaScript. Sign-in uses OIDC authorization code with PKCE and a server-side session in PostgreSQL (cookie `__Host-toolgate`, `SameSite=Lax`, CSRF tokens on every form).

Pages:
- **Servers:** the user's catalog view, their instances and status; restart an outdated instance.
- **Secrets:** set, replace and delete. Values are never shown again.
- **Grants:** create a grant (server or tool, expiry up to 7 days, optionally narrowed to one session); revoke.
- **Approvals:** pending tool calls with server, tool, arguments (when payloads are on), session and workspace; approve or deny.
- **Sessions:** open sessions, their enabled servers, recent calls.
- **Audit:** filter by principal, workspace, session, server, tool and time.
- **Blocks** (admins): temporary blocks with an expiry (up to 30 days); the standing rules from git shown read-only.
- **Workspaces**: the user's defaults and workspace lists; add and remove servers, clear a list. The defaults form has "Only these servers", the default `list` for unverified sessions, with a checkbox for each server the user has access to. Checking a server, or moving from nothing to `list` while servers are checked, widens and needs the step-up; unchecking applies at once.

**Step-up.** Creating a grant or approving a call needs an authentication less than 5 minutes old. toolgate redirects to the IdP with `prompt=login` and `max_age=60` (not 0, which some IdPs such as Authentik ignore; 60 seconds is the allowed clock skew), and checks `auth_time`. When `auth_time` is not fresh, or the callback's state does not match the browser's sign-in cookie, toolgate starts one new sign-in (with the same return path and step-up) instead of showing an error, and shows the error the second time. The IdP's flow decides how the person signs in (the owner uses passkeys).

**From inside a session.** When a call needs an approval, the gateway sends a URL-mode MCP elicitation linking to the approval page. A call that needs a grant fails at once with a link to the Grants page. Claude Code opens it in the browser, and the call continues when the person acts, up to `approvalTimeout` (default 10 min). In sessions that can't show an elicitation (`-p`, CI), the call fails at once with an error naming the grant or approval needed.

**Access requests.** A change that widens a user's workspace scope from the CLI, the API or the `toolgate` server's tools is stored as a pending access request, and nothing changes until the user approves it on `/ui/approvals/<id>` with a step-up. A `change` request holds a change of this shape: servers on a workspace list, a clear, or the defaults. A `scope` request holds an allow that this shape can't hold: an allow of a `SERVER__TOOL` tool, or an allow for one open session in a verified workspace, an addition to the unverified allow list, or a move of the default for unverified sessions from `none` to `list` while the list has servers. Its page shows the target, the allowed and denied entries before and after, and the session that asked. Approving applies the change in the same transaction as the approval, and lifts the matching deny entries. A session target is checked again: if the session ended or moved to another workspace, nothing changes and the page says why. Ending a session expires its pending `scope` requests (see `docs/design/phase-4.md`). A user has at most one pending request for the same change: a request with the same kind, workspace, verified flag and change (servers sorted, without the session that asked) returns the pending one, with no new row, `access.request` or notification. A widening that is already in effect applies with no request. Approving a request also approves the user's other pending `change` and `scope` requests that its change put fully in effect (servers on a workspace list or the unverified allow list, or the defaults), and their `access.approve` names the approved request as `satisfiedBy`.

**Admins** are members of the configured admin group.

## 15. Data model

PostgreSQL, accessed with `pgx/v5`, forward-only migrations embedded with `pressly/goose`, and queries generated with `sqlc`. Tables (outline; the migrations are the reference):
- `sessions`: principal, session ID, workspace, attestation, created, last seen.
- `session_servers`: the enabled set.
- `instances`: ID, template, generation, scope key, namespace, status, reason, timestamps.
- `user_secrets`: principal, name, ciphertext, key ID, timestamps.
- `audit_events`: partitioned by month. The function `toolgate_ensure_audit_partitions()` (`SECURITY DEFINER`, owned by the migrating role) creates the current and next month's partitions; the gateway calls it at start, and the leader when it gets the Lease and every 24 h.
- Web UI: `grants`, `approvals`, `temporary_blocks`, `ui_sessions`, `ui_logins`, and three new `instances` columns for restarts and resolver expiry. `docs/design/phase-2.md` gives their rules and row limits.
- Workspaces and access requests: `sessions.kind`, `user_scopes` (each user's defaults), `workspace_lists` and `workspace_list_servers` (a user's server list for each verified workspace), `session_additions` (servers added to one session), and `access_requests` (a user's pending requests to add a server or widen a list, with the approvals' statuses, expiry and cleanup). `docs/design/phase-3.md` gives their rules and row limits.
- Tool search and curation: `instances.mcp_revision`, `tool_catalog` (a template generation's upstream tool list), `tool_search` (searchable tool rows, with a GIN index), `session_found_tools`, `scope_entries` (tool-level and session-level allow and deny entries), `unverified_servers` and `user_scopes.unverified_list` (the unverified allow list), and the access request kind `scope`. `docs/design/phase-4.md` gives their rules and row limits.

Concurrency: instance creation for a template and scope key is serialized with a transaction-level advisory lock, so two sessions enabling a per-user server at once share one instance. `docs/design/phase-1.md` gives the locks and the interaction rules.

## 16. Won't do

- Any runtime other than Kubernetes (no Docker or local processes).
- Running stdio MCP servers directly; templates wrap them.
- MCP prompts, resources, sampling and roots through the gateway. Tools only.
- Proxying model API traffic, or anything touching Claude subscription sign-in.
- Multi-cluster, and more than one IdP at a time.
- A public MCP registry or marketplace integration.
- toolgate issuing its own access tokens. Connectors sign in at the configured IdP and toolgate only validates its tokens.
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
3. **Telemetry, attestation and operations** (`docs/acceptance/phase-3.md`). OTLP ingest with identity stamping, SPIFFE JWT-SVID attestation, CLI audit and session commands, custom health checks for running instances, and an optional migration Job with a separate database credential. Remote MCP servers with per-user OAuth are deferred to the backlog. Remote servers with per-user static credentials (see "Remote servers") narrow that backlog item to the OAuth flow.
4. **Tool search, lazy start, delegated access, curation, MCP revisions and connectors** (`docs/acceptance/phase-4.md`). A `toolgate_find_tools` meta tool so sessions search and call tools with no enable step, a tool catalog with servers started on first call, access-management tools (allow with the user's approval, deny at once) for the user's other workspaces and sessions, governed like any server, template-level tool filtering and name and description overrides, MCP 2026-07-28 with peers that speak nothing older (preferring 2025-11-25), and standard MCP OAuth so claude.ai custom connectors can use toolgate.

A phase's design (`docs/design/phase-N.md`, with its interaction table) is reviewed before its issues are filed.

## 18. Testing

- **Unit:** Go `testing` + `testify`, table-driven.
- **Integration** (`//go:build integration`): PostgreSQL in Docker, and an S3-compatible server in Docker (the first infra issue picks a maintained one and records why).
- **Kubernetes e2e:** kind with Flux installed. The gateway runs in-cluster from a locally built image, and a Go MCP client (the official SDK) drives it, with a fake OIDC provider (the first infra issue picks a maintained library, for example `github.com/oauth2-proxy/mockoidc`). The test MCP server is a small Go binary in `test/mcpserver/` that echoes its environment and secrets so tests can check rendering.
- **Acceptance:** each checklist scenario lists its Go tests on a `Tests:` line in the checklist. `make acceptance` prints passing scenarios out of the total. Scenarios that need a real Claude Code session or a real browser are marked `manual` in the checklist and checked by the owner.
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

1. Does `headersHelper` receive `CLAUDE_CODE_SESSION_ID`? Answered: no. `toolgate headers` uses the SessionStart hook's file instead (see "Client: CLI and Claude Code plugin").
2. Can Coder vouch for a workspace the way TokenReview does for pods? A later phase investigates before designing it.
