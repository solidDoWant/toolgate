# Phase 1 acceptance checklist: core gateway

Phase 1 is done when every scenario below passes on `master` and the `0.1.0` release is published. Only the owner adds, removes or changes scenarios.

Each automated scenario has a test tagged `[AC-1-nn]`, run by `make acceptance` on kind with Flux and a fake OIDC provider. Scenarios marked **manual** need a real Claude Code session and are checked by the owner.

## Deploy and sign in

- **AC-1-01 Install.** Given a kind cluster with Flux, PostgreSQL and S3, when the chart is installed with the values in `docs/admin/quick-start.md`, then the CRDs exist, two gateway replicas are ready, and `/healthz` answers on both.
- **AC-1-02 User sign-in.** When a user runs `toolgate login` and completes the device-code flow, then `toolgate status` shows their email. After the access token expires, `toolgate headers` still prints a working token without asking the user to sign in again.
- **AC-1-03 Bad tokens.** A request with no token, an expired token, a token for another audience, or a token from another issuer gets 401 and an audit event.
- **AC-1-04 Pod sign-in.** A pod in an allowed namespace with a projected service-account token (audience `toolgate`) is identified as its service account with attestation `attested` and workspace `k8s/<namespace>/<owner kind>/<owner name>`. The same pod in a namespace that isn't allowed gets 401.
- **AC-1-05 Plugin (manual).** After installing the plugin from this repo's marketplace and setting the gateway URL, a new Claude Code session shows the `toolgate` MCP server connected, with the four `toolgate_*` tools.

## Servers in a session

- **AC-1-06 Catalog view.** `toolgate_list_servers` returns exactly the templates the identity's user, groups or service account give access to, with scope and status.
- **AC-1-07 Enable.** When a session enables a per-session server, then an instance namespace is created with Pod Security `restricted` and default-deny networking, the session receives progress notifications, the server's tools are announced with `tools/list_changed`, and they are named `<server>__<tool>`.
- **AC-1-08 Call.** A call to an enabled server's tool returns the server's result, and the audit log has a `tool.decision` and a `tool.result` event carrying principal, workspace, session, attestation, server, tool and trace ID.
- **AC-1-09 Variables.** The test MCP server reports environment values rendered from `${principal.email}`, `${workspace.id}`, `${session.id}` and `${instance.id}`, and they match the session's identity.
- **AC-1-10 No template injection.** A workspace or session header that doesn't match the allowed pattern gets 400. A template value that would render as YAML (for example a principal name containing `: ` or a newline in the fake IdP) appears as a literal string in the object and adds no fields.
- **AC-1-11 Disable.** After `toolgate_disable_server`, the server's tools are gone from `tools/list`, and when no session uses the per-session instance any more, its namespace is deleted.
- **AC-1-12 Real session (manual).** In Claude Code, the model lists servers, enables the test server, calls a tool, and disables it. The audit log shows the same session ID that Claude Code reports.

## Scopes and lifecycle

- **AC-1-13 Per-user.** Two sessions of one user enabling a per-user server at the same moment share one instance. Another user gets a separate instance.
- **AC-1-14 Shared.** All users share one instance of a shared server. A shared template that uses `${secret:…}`, `${principal.*}`, `${workspace.*}` or `${session.*}` is reported invalid in its status and isn't offered.
- **AC-1-15 Idle.** A session with no requests for the idle timeout ends, and its per-session instances stop. A per-user instance stops after its `idleStop` without calls.
- **AC-1-16 Reconnect.** A client that reconnects with the same token and session ID finds the same servers enabled and their tools listed, without enabling them again.
- **AC-1-17 Gateway restart.** When the replica holding a session's stream is deleted, the client reconnects through the other replica, the session's servers are still enabled, and no instance is duplicated.
- **AC-1-18 Start failure.** A template whose image can't be pulled fails `toolgate_enable_server` within its `startTimeout`, with a reason that names the image problem. `toolgate_server_status` shows the same reason.
- **AC-1-19 Template change.** After a template is edited, new instances use the new version, and `toolgate_server_status` shows running older instances as outdated.
- **AC-1-20 Long tool names.** A server exposing a tool whose prefixed name is over 64 characters is marked failed with a reason saying so.

## Secrets and templates

- **AC-1-21 User secret.** After `toolgate secret set github-token`, enabling a template that uses `${secret:github-token}` gives the test server the value through a Secret object. The value is in no other object, in no log line and in no audit event. `toolgate secret list` shows the name and date only.
- **AC-1-22 Missing secret.** Enabling a template whose secret the user hasn't set fails with a message naming the secret and the command to set it.
- **AC-1-23 Secrets only in Secrets.** A template that uses `${secret:…}` outside a Secret is invalid, with the reason in its status.
- **AC-1-24 Allowed kinds only.** A template containing a cluster-scoped kind or RBAC object is invalid. The admission policy rejects such an object carrying toolgate's instance label even when it is applied with the gateway's own service account.

## Rules

- **AC-1-25 Block a tool.** A `ToolgateRule` blocking one tool for one user removes it from that user's `tools/list` and refuses calls with the rule's reason, within 5 seconds of being applied and without the client reconnecting. Other users still have it.
- **AC-1-26 Block by workspace.** A rule matching `k8s/ci/*` blocks a server for pods in namespace `ci` and nowhere else.
- **AC-1-27 Attestation.** A template with `environments: opt-in` or `attested` is offered to an attested pod and not to a signed-in user on a laptop.

## Client

- **AC-1-28 Settings sync.** When a settings profile matching the user's group is applied, the SessionStart hook writes its keys into `~/.claude/settings.json`. A key the user has edited since the last sync is left as the user set it and reported. A key removed from the profile is removed from the file. Keys the profile never set are untouched.
- **AC-1-29 Traces.** A request carrying a `traceparent` header produces gateway spans (policy, render, apply, forward) that are children of that parent, exported to a test OTLP receiver.

## Release

- **AC-1-30 Release.** Release `0.1.0` publishes the image, the chart, and CLI binaries for Linux, macOS and Windows. `docs/user/` explains installing the plugin and signing in. `docs/admin/` explains deployment, writing templates and rules, and what claimed workspaces and sessions can't protect against.

## Limits

- **AC-1-31 Instance limit.** A user who has as many running servers as the per-user limit can't start another: `toolgate_enable_server` fails with a message naming the limit. Enabling a per-user server that is already running for that user still works, and other users aren't affected.
