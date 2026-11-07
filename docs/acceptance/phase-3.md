# Phase 3 acceptance checklist: telemetry, attestation, scoping and operations

Same rules as phase 1. Each scenario has a `Tests:` line. This checklist is at feature level. The owner reviews it again, with the phase 3 design, before phase 3 starts.

## Telemetry

- **AC-3-05 OTLP ingest.** Claude Code's metrics, log events and traces, sent through the plugin's settings to the gateway, reach the configured OTLP backend with the verified principal and workspace. User attributes set by the client are replaced.
  Tests: `test/e2e: TestTelemetryIdentity`
- **AC-3-06 Usage per session.** `docs/admin/` shows a working query for token use and cost per user, workspace and session, checked against the test backend.
  Tests: `test/e2e: TestUsageQueries`
- **AC-3-07 One trace (manual).** In the trace backend, a Claude Code tool span and the gateway's spans for that call are in the same trace.

## Attestation

- **AC-3-09 SPIFFE.** A SPIFFE JWT-SVID from an allowed trust domain, with audience `toolgate`, is attested. Its workspace is the SPIFFE ID's path, rules can match it, and an SVID from another trust domain gets 401.
  Tests: `cmd/toolgate: TestClientCommandsSendAJWTSVID`, `test/e2e: TestSPIFFESignIn`

## CLI

- **AC-3-10 Audit tail.** `toolgate audit tail` streams the user's own events, and all events for an admin.
  Tests: `cmd/toolgate: TestAuditTail`
- **AC-3-11 Sessions.** `toolgate sessions` lists the user's open sessions and their enabled servers.
  Tests: `cmd/toolgate: TestSessionsCommand`

## Instance health

- **AC-3-13 Custom health check.** A template declares an HTTP health check (a Service, port and path) for its server. When the check starts failing after the instance is `ready`, toolgate stops sending it tool calls within the check's interval times its threshold, plus one check timeout: its tools are withdrawn with `list_changed`, and calls fail with the check's reason. toolgate then repairs it as the template's `onFailure` says (replace it, re-apply its objects, or nothing), at most `maxRepairs` times in 30 minutes, and the tools come back when the check passes again. A template without a custom check behaves as before.
  Tests: `test/e2e: TestHealthCheckRepair`
- **AC-3-14 Object health after ready.** When an object of a `ready` instance later reports failure (for example a HelmRelease going `Ready=False` for 60 s), the instance's tools are withdrawn with that object's reason, and it's repaired as for AC-3-13.
  Tests: `test/e2e: TestObjectHealthAfterReady`

## Migrations

- **AC-3-15 Separate migration credential.** With `config.database.migrateOnStart: false` and a migration secret, `helm install` and `helm upgrade` apply the migrations through a hook Job before the new gateway pods start. The gateway then runs with a database user that has only data rights. With the default values, behaviour is unchanged.
  Tests: `cmd/toolgate: TestServeWithoutMigrateOnStartRefusesAnOlderDatabase/no goose table counts as version 0 and is not created`, `test/e2e: TestSeparateMigrationCredential`

## Workspace and session scoping

- **AC-3-16 Attested user session.** A client sends the user's own access token plus a workspace attestation token (a projected service-account token, or a SPIFFE JWT-SVID) from a pod or VM whose owner labels name that user. toolgate treats the session as the user (their principal, groups, secrets, per-user instances and grants), with attestation `attested` and the workspace verified from the cluster, such as `coder/<owner>/<workspace name>`. If the attestation token belongs to a workload owned by someone else, the session falls back to `user` attestation with a claimed workspace, and `auth.refuse` records why.
  Tests: `test/e2e: TestAttestedUserSessions`
- **AC-3-17 Workspace access list.** A user sets an allow list of servers for one of their workspaces, in the web UI or with `toolgate workspace allow`. A session in that workspace lists and can enable only those servers, and its other servers' tools are withdrawn. A change reaches open sessions within 5 seconds. The list never grants more than the user's own access.
  Tests: `internal/gateway: TestWorkspaceLists`, `internal/ui: TestWorkspacesPage`, `internal/ui: TestChangeApprovalPage`
- **AC-3-18 Defaults.** A user chooses what their verified workspaces with no list get (nothing, or all their servers), and what their unverified sessions get (nothing, or all their servers). The default for both is nothing. The choices apply as AC-3-17 does.
  Tests: `internal/gateway: TestWorkspaceDefaults`
- **AC-3-19 Session addition.** When a session enables a server that its workspace's list doesn't include, the user is asked in the browser (URL elicitation, or a link for clients without elicitation) to allow it for this session only or always for this workspace. Approving needs a step-up. A session-only approval ends when the session ends, and other sessions in the workspace don't get it. "Always" adds the server to the workspace's list.
  Tests: `internal/ui: TestSessionAddition`
- **AC-3-20 Device sign-in from a workspace.** In a workspace with no browser, `toolgate login` prints a verification URL and code. After the user approves on another device, the workspace's sessions act as that user, unattended, until the refresh token expires or is revoked.
  Tests: `test/e2e: TestDeviceSignInFromAWorkspace`
- **AC-3-21 Sign-in through another service.** With `TOOLGATE_TOKEN_COMMAND` set to a command that prints the user's access token (for example Coder's `coder external-auth access-token toolgate`), a workspace's sessions act as that user with no `toolgate login` in the workspace. The gateway accepts tokens from the same issuer for any audience in its configured list, verified like the first audience.
  Tests: `cmd/toolgate: TestSignInThroughTokenCommand`
- **AC-3-22 Resume after a long pause.** A session that went idle past the session timeout, and is resumed with the same session ID from the same workspace within `sessionResumeWindow` (default 24 h), gets its enabled servers back without enabling them again: their instances start, the tools arrive with `list_changed`, and a call made while a server is still starting waits for it, instead of failing. Nothing runs during the pause. From another workspace, or after the window, the session starts with no servers. The audit log records the set at the end, the resume and the forgetting.
  Tests: `test/e2e: TestResumeAfterLongPause`

## Release

- **AC-3-12 Release.** Release `0.3.0` is published, with docs for telemetry, SPIFFE attestation, health checks, the migration Job, and workspace and session scoping.
