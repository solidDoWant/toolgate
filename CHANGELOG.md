# Changelog

## Unreleased

### Features

- Instance namespaces and objects now carry labels and annotations for their owner, workspace, session and scope. See "Labels and annotations on instance objects" in `docs/admin/instances.md`.

- The gateway now tells the model which servers it can use: a short list in the MCP instructions, and the same names in the descriptions of `toolgate_find_tools` and `toolgate_list_servers`. Write each template's `description` for the model.

- The Sessions page of the web UI now shows who runs each session, where (with a verified or unverified badge), which client and version, and whether the session is active, idle, ended or resumable. You can filter by state, workspace and, as an admin, user.

### Upgrade notes for admins

- Entries in `access.users` and `access.groups` of a template are now glob patterns. `users: ["*"]` makes a server available to every signed-in user. Entries without `*`, `?`, `[`, `]`, `{`, `}` or `\` match exactly, as before. An entry that is not a valid pattern makes the template invalid. See "Who can use a server" in `docs/admin/templates-and-rules.md`.

- Sessions in tool search mode no longer list `toolgate_enable_server` and `toolgate_disable_server`. A call to a tool of an allowed server already turns the server on. A call to a tool of a server that the workspace blocks asks the user for access. To list the two tools in every mode, as before, set `config.metaTools.enableDisable` to `always`. See `docs/admin/tool-search.md`.
- The web UI and `toolgate status` now show people by name and email instead of their long OIDC user ID. The full ID stays on hover.

### Fixes

- The Approvals page of the web UI now lists your pending access requests, for example changes to workspace lists and defaults, in an "Access changes" section. Before, it listed only tool call approvals.

## 0.4.6

toolgate is a gateway that you run yourself, between Claude Code and MCP servers. An MCP server gives Claude tools, for example to read GitHub issues. toolgate starts each server on Kubernetes when a session needs it. It checks every tool call against the rules of the admin, and it records every decision in an audit log. This patch release fixes the database connection when the PostgreSQL client certificate changes. It also gives the web UI a new look.

To install toolgate:

- Admins install the Helm chart `oci://ghcr.io/soliddowant/charts/toolgate`, version `0.4.6`. The chart runs the image `ghcr.io/soliddowant/toolgate:0.4.6`. Follow `docs/admin/quick-start.md`.
- Users download the `toolgate` command for their system from the files of this release, and put it on their `PATH`. Then they install the Claude Code plugin with `claude plugin marketplace add solidDoWant/toolgate` and `claude plugin install toolgate@toolgate`. Follow `docs/user/README.md`.

### Upgrade notes for admins

- If the gateway connects to PostgreSQL with a client certificate that expires after a short time, for example a cert-manager certificate that is valid for one hour, upgrade to this version. Versions 0.4.0 to 0.4.5 stop working when that certificate expires.

### New

- The Servers page of the web UI now shows the scope, the environments, the secrets that are needed and the workspace availability of each server you can use, and a detail page with its tools.

### Fixes

- The gateway now reads the PostgreSQL client certificate files again for each new database connection. Before, it read them one time when it started. When the first certificate expired, the database refused every new connection, and the gateway pods stopped being ready. The error was `remote error: tls: expired certificate`. See "Client certificates that change" in `docs/admin/chart.md`. ([#525](https://github.com/solidDoWant/toolgate/pull/525))
- Sessions without a verified workspace now show the servers that the workspace scope blocks as `blocked`, with the fix, in `toolgate_list_servers`, `toolgate_server_status` and `toolgate_find_tools`. Before, the list was empty and the status said "isn't in your catalog".

### Web UI

- The web UI has a new look, with a light theme and a dark theme. It uses the theme of your system. The pages also work on small screens. See `docs/admin/web-ui.md`. ([#527](https://github.com/solidDoWant/toolgate/pull/527))

### Known issues

- Sessions that enabled a template named `toolgate` before the upgrade to 0.4 cannot call the built-in tools until the session restarts.
- The `/ui/approvals` page of the web UI does not list pending access requests for changes to workspaces and defaults. ([#533](https://github.com/solidDoWant/toolgate/issues/533))
- `govulncheck ./...` reports [GO-2026-6865](https://pkg.go.dev/vuln/GO-2026-6865) in `github.com/klauspost/compress` v1.20.1 and [GO-2026-6253](https://pkg.go.dev/vuln/GO-2026-6253) in `github.com/moby/go-archive` v0.2.0. Only the test helpers in `internal/testutil` use these modules, through testcontainers and envtest. The `toolgate` command and the gateway image do not contain them, and `govulncheck ./cmd/...` finds no vulnerabilities.

## 0.4.5

toolgate is a gateway that you run yourself, between Claude Code and MCP servers. An MCP server gives Claude tools, for example to read GitHub issues. toolgate starts each server on Kubernetes when a session needs it. It checks every tool call against the rules of the admin, and it records every decision in an audit log. In this patch release, all gateway replicas share MCP sessions through PostgreSQL, and the gateway sends tool calls to remote MCP servers. The release also fixes the plugin in Coder workspaces and the settings profiles of Claude Code.

To install toolgate:

- Admins install the Helm chart `oci://ghcr.io/soliddowant/charts/toolgate`, version `0.4.5`. The chart runs the image `ghcr.io/soliddowant/toolgate:0.4.5`. Follow `docs/admin/quick-start.md`.
- Users download the `toolgate` command for their system from the files of this release, and put it on their `PATH`. Then they install the Claude Code plugin with `claude plugin marketplace add solidDoWant/toolgate` and `claude plugin install toolgate@toolgate`. Follow `docs/user/README.md`.

### Upgrade notes for admins

- Tell your users to start a new Claude Code session after the upgrade. The upgrade ends all open Claude Code sessions one time. During the rollout, old and new replicas do not know the sessions of each other, so the open sessions get HTTP 404.
- You can remove session affinity from your gateway Service or ingress. The replicas now share sessions, so a request can reach any replica. The chart value `config.service.sessionAffinity` stays `None` by default. See `docs/admin/connectors.md`.
- If a Coder template uses `TOOLGATE_TOKEN_COMMAND`, `TOOLGATE_ATTESTATION_TOKEN_FILE` or `TOOLGATE_ATTESTATION_TOKEN_REQUEST`, change it to run `toolgate configure`. Give the token command `--agent-token-file`. Claude Code removes these variables from the `headersHelper` of the plugin, so the plugin did not connect. See `docs/admin/coder.md`.
- The gateway keeps shared MCP sessions in the database table `mcp_sessions`. Version 0.4.4 added this table. If you upgrade from a version before 0.4.4, the gateway adds the table when it starts. You do not have to do anything for this.

### Servers and sessions

- MCP sessions are now shared between gateway replicas through PostgreSQL. A request can reach any replica, and a session continues when a replica stops or restarts. Claude Code does not start a session again after HTTP 404, so before this change it lost its session in these cases. Messages from a server, such as a changed tool list or a question to the user, also reach the session on any replica. Only the requests that were in progress on a stopped replica fail. See `docs/admin/connectors.md`. ([#510](https://github.com/solidDoWant/toolgate/pull/510), [#514](https://github.com/solidDoWant/toolgate/pull/514), [#513](https://github.com/solidDoWant/toolgate/pull/513), [#522](https://github.com/solidDoWant/toolgate/pull/522))
- The gateway connects to remote MCP servers. A remote server runs outside toolgate. The gateway sends the own key of each user in a header, and the template fills in the header from the secrets of the user. Each user sees the tool list that the server gives to that user, and tool search uses it. If the secret is missing, or the server refuses it, the user gets a message with the `toolgate secret set` command. See "Remote servers" in `docs/admin/templates-and-rules.md`. ([#501](https://github.com/solidDoWant/toolgate/pull/501), [#511](https://github.com/solidDoWant/toolgate/pull/511), [#512](https://github.com/solidDoWant/toolgate/pull/512), [#515](https://github.com/solidDoWant/toolgate/pull/515), [#520](https://github.com/solidDoWant/toolgate/pull/520))

### Claude Code plugin and CLI

- The new command `toolgate configure` saves the sign-in and attestation settings in the toolgate configuration file `config.json`. The `toolgate` command reads them from there when the environment variables `TOOLGATE_TOKEN_COMMAND`, `TOOLGATE_ATTESTATION_TOKEN_FILE` and `TOOLGATE_ATTESTATION_TOKEN_REQUEST` are not set. Before, the plugin did not connect in workspaces that used these variables, because Claude Code removes them from the `headersHelper` of the plugin. ([#521](https://github.com/solidDoWant/toolgate/pull/521))
- Settings profiles now reach Claude Code. The gateway serves `GET /api/v1/settings`. Before, each session start showed "the settings profile could not be fetched: the gateway answered 404 Not Found". The `toolgate` command now reads a 404 from an older gateway as "no profile". ([#518](https://github.com/solidDoWant/toolgate/pull/518))

Internal work: end-to-end tests for two gateway replicas with no session affinity and for a remote server that needs a bearer token.

### Known issues

- Sessions that enabled a template named `toolgate` before the upgrade to 0.4 cannot call the built-in tools until the session restarts.
- `govulncheck ./...` reports [GO-2026-6865](https://pkg.go.dev/vuln/GO-2026-6865) in `github.com/klauspost/compress` v1.20.1 and [GO-2026-6253](https://pkg.go.dev/vuln/GO-2026-6253) in `github.com/moby/go-archive` v0.2.0. Only the test helpers in `internal/testutil` use these modules, through testcontainers and envtest. The `toolgate` command and the gateway image do not contain them, and `govulncheck ./cmd/...` finds no vulnerabilities.

## 0.4.4

toolgate is a gateway that you run yourself, between Claude Code and MCP servers. An MCP server gives Claude tools, for example to read GitHub issues. toolgate starts each server on Kubernetes when a session needs it. It checks every tool call against the rules of the admin, and it records every decision in an audit log. This patch release fixes sign-in failures when several Claude Code sessions start at the same time, and two problems in the web UI. The gateway can now accept access tokens from more than one OIDC issuer. The Helm chart also gets values for the client ID and scopes of the `toolgate` command.

To install toolgate:

- Admins install the Helm chart `oci://ghcr.io/soliddowant/charts/toolgate`, version `0.4.4`. The chart runs the image `ghcr.io/soliddowant/toolgate:0.4.4`. Follow `docs/admin/quick-start.md`.
- Users download the `toolgate` command for their system from the files of this release, and put it on their `PATH`. Then they install the Claude Code plugin with `claude plugin marketplace add solidDoWant/toolgate` and `claude plugin install toolgate@toolgate`. Follow `docs/user/README.md`.

### Upgrade notes for admins

This release has no breaking changes. An install of 0.4.3 upgrades with no changes to its values. The gateway adds one table to its database when it starts. You do not have to do anything for this.

### Signing in

- The gateway can accept access tokens from more than one OIDC issuer. An issuer is the identity provider address in the `iss` claim of a token. Use the new chart value `config.oidc.additionalIssuers` (`oidcAdditionalIssuers` in the gateway configuration) when your identity provider gives each client its own issuer, as Authentik does. Each issuer must give one person the same `sub` claim. The `toolgate` command and the web UI still sign in with `config.oidc.issuerURL`. See `docs/admin/chart-values.md`. ([#506](https://github.com/solidDoWant/toolgate/pull/506))
- When several `toolgate` commands renewed the sign-in at the same time, for example at the start of two Claude Code sessions, the identity provider could refuse one of them with `invalid_grant`. The user then had to sign in again. This happened with identity providers that give a new refresh token at each renewal and refuse the old one, for example Authentik. Now only one `toolgate` command at a time renews the sign-in. It holds a lock on the file `token.lock` in the toolgate configuration directory. The other commands wait, then use the new token. ([#492](https://github.com/solidDoWant/toolgate/pull/492))

### Tool search

- With `embeddings.server.scaleToZero.enabled`, the gateway now stops the embeddings server when nobody searched or started a session with tool search on for `idleTimeout`. Before, an open but idle Claude Code session kept the server running for about two hours more. ([#491](https://github.com/solidDoWant/toolgate/pull/491))

### Deployment

- The Helm chart has the new values `config.oidc.cliClientID` and `config.oidc.cliScopes`. If you set `cliClientID`, users can run `toolgate login` without `--client-id`. See `docs/admin/chart.md`. ([#489](https://github.com/solidDoWant/toolgate/pull/489))

### Fixes

- The web UI no longer logs a Content Security Policy error in the browser, and its loading indicators now hide as they should. ([#484](https://github.com/solidDoWant/toolgate/pull/484))
- If the gateway has no secrets keyset, the web UI no longer shows the Secrets link in its navigation. Before, the link opened a page that returned 404. ([#485](https://github.com/solidDoWant/toolgate/pull/485))

### In progress: not usable yet

- Remote MCP servers. An MCPServerTemplate accepts the field `spec.remote`, and the gateway starts remote instances. But the gateway does not send tool calls to remote servers. The next release fixes this. ([#501](https://github.com/solidDoWant/toolgate/pull/501), [#511](https://github.com/solidDoWant/toolgate/pull/511))
- MCP sessions shared by all gateway replicas. The database has the new table `mcp_sessions`. But the gateway replicas do not share MCP sessions through it. The next release fixes this. ([#510](https://github.com/solidDoWant/toolgate/pull/510))

Internal work: design documents for MCP sessions that all gateway replicas share ([#499](https://github.com/solidDoWant/toolgate/pull/499)).

### Known issues

- Sessions that enabled a template named `toolgate` before the upgrade to 0.4 cannot call the built-in tools until the session restarts.
- With more than one gateway replica, a request can fail with `404 session not found` when it reaches another replica right after the session starts. Claude Code then marks the `toolgate` server as failed. The next release fixes this. Until then, set `config.replicas` to `1`. ([#487](https://github.com/solidDoWant/toolgate/issues/487))
- `govulncheck ./...` reports [GO-2026-6865](https://pkg.go.dev/vuln/GO-2026-6865) in `github.com/klauspost/compress` v1.20.1 and [GO-2026-6253](https://pkg.go.dev/vuln/GO-2026-6253) in `github.com/moby/go-archive` v0.2.0. Only the test helpers in `internal/testutil` use these modules, through testcontainers and envtest. The `toolgate` command and the gateway image do not contain them, and `govulncheck ./cmd/...` finds no vulnerabilities.

## 0.4.3

toolgate is a gateway that you run yourself, between Claude Code and MCP servers. An MCP server gives Claude tools, for example to read GitHub issues. toolgate starts each server on Kubernetes when a session needs it. It checks every tool call against the rules of the admin, and it records every decision in an audit log. In this patch release, the Helm chart creates no network policies. The cluster owner writes them.

To install toolgate:

- Admins install the Helm chart `oci://ghcr.io/soliddowant/charts/toolgate`, version `0.4.3`. The chart runs the image `ghcr.io/soliddowant/toolgate:0.4.3`. Follow `docs/admin/quick-start.md`.
- Users download the `toolgate` command for their system from the files of this release, and put it on their `PATH`. Then they install the Claude Code plugin with `claude plugin marketplace add solidDoWant/toolgate` and `claude plugin install toolgate@toolgate`. Follow `docs/user/README.md`.

### Upgrade notes for admins

- Breaking: the chart value `config.database.migrationEgress` is removed, and the migration Job no longer gets a NetworkPolicy. ([#480](https://github.com/solidDoWant/toolgate/pull/480))

### Deployment

- The Helm chart creates no NetworkPolicy objects. Network policy is the job of the cluster owner. The section "Network policies" in `docs/admin/chart.md` lists the traffic that each pod of the chart needs. The NetworkPolicy objects that the gateway creates in the `tg-*` instance namespaces did not change. They still isolate the MCP servers. ([#480](https://github.com/solidDoWant/toolgate/pull/480))

Internal work: a chart test that makes sure that the chart renders no network policies.

### Known issues

- Sessions that enabled a template named `toolgate` before the upgrade to 0.4 cannot call the built-in tools until the session restarts.
- `govulncheck ./...` reports [GO-2026-6865](https://pkg.go.dev/vuln/GO-2026-6865) in `github.com/klauspost/compress` v1.20.1 and [GO-2026-6253](https://pkg.go.dev/vuln/GO-2026-6253) in `github.com/moby/go-archive` v0.2.0. Only the test helpers in `internal/testutil` use these modules, through testcontainers and envtest. The `toolgate` command and the gateway image do not contain them, and `govulncheck ./cmd/...` finds no vulnerabilities.

## 0.4.2

toolgate is a gateway that you run yourself, between Claude Code and MCP servers. An MCP server gives Claude tools, for example to read GitHub issues. toolgate starts each server on Kubernetes when a session needs it. It checks every tool call against the rules of the admin, and it records every decision in an audit log. This patch release lets the Helm chart take the database connection strings inline, without a Secret.

To install toolgate:

- Admins install the Helm chart `oci://ghcr.io/soliddowant/charts/toolgate`, version `0.4.2`. The chart runs the image `ghcr.io/soliddowant/toolgate:0.4.2`. Follow `docs/admin/quick-start.md`.
- Users download the `toolgate` command for their system from the files of this release, and put it on their `PATH`. Then they install the Claude Code plugin with `claude plugin marketplace add solidDoWant/toolgate` and `claude plugin install toolgate@toolgate`. Follow `docs/user/README.md`.

### Upgrade notes for admins

This release has no breaking changes. An install of 0.4.1 upgrades with no changes to its values.

### Deployment

- You can give the chart the PostgreSQL connection string of the gateway in the value `config.database.url`, and the string of the migration Job in `config.database.migrationUrl`. Before, each string had to be in a Secret. Use the inline values only for a string with no password, for example with client-certificate authentication or with token authentication. An inline string is a plain value in the Deployment and Job specs and in the Helm release, and anyone who can read those objects can read it. Keep a string with a password in a Secret, with `config.database.existingSecret` and `config.database.migrationSecret` as before. Set exactly one source for each string. If you set both, or no source for the gateway string, the chart fails when it renders. See `docs/admin/chart.md`. ([#477](https://github.com/solidDoWant/toolgate/pull/477))

Internal work: chart tests for the sources of the connection strings.

### Known issues

- Sessions that enabled a template named `toolgate` before the upgrade to 0.4 cannot call the built-in tools until the session restarts.
- `govulncheck ./...` reports [GO-2026-6865](https://pkg.go.dev/vuln/GO-2026-6865) in `github.com/klauspost/compress` v1.20.1 and [GO-2026-6253](https://pkg.go.dev/vuln/GO-2026-6253) in `github.com/moby/go-archive` v0.2.0. Only the test helpers in `internal/testutil` use these modules, through testcontainers and envtest. The `toolgate` command and the gateway image do not contain them, and `govulncheck ./cmd/...` finds no vulnerabilities.

## 0.4.1

toolgate is a gateway that you run yourself, between Claude Code and MCP servers. An MCP server gives Claude tools, for example to read GitHub issues. toolgate starts each server on Kubernetes when a session needs it. It checks every tool call against the rules of the admin, and it records every decision in an audit log. This patch release lets the gateway stop the bundled embeddings server when nobody uses tool search. It also fixes the database setup SQL for split roles, and a chart bug that made `helm upgrade` fail.

To install toolgate:

- Admins install the Helm chart `oci://ghcr.io/soliddowant/charts/toolgate`, version `0.4.1`. The chart runs the image `ghcr.io/soliddowant/toolgate:0.4.1`. Follow `docs/admin/quick-start.md`.
- Users download the `toolgate` command for their system from the files of this release, and put it on their `PATH`. Then they install the Claude Code plugin with `claude plugin marketplace add solidDoWant/toolgate` and `claude plugin install toolgate@toolgate`. Follow `docs/user/README.md`.

### Upgrade notes for admins

This release has no breaking changes. An install of 0.4.0 upgrades with no changes to its values.

### Tool search

- The gateway can stop the embeddings server that the chart runs when tool search is idle, and start it again when a search needs it. This saves the memory that the server holds. Set `embeddings.server.scaleToZero.enabled` to `true`. The server stops after `embeddings.server.scaleToZero.idleTimeout` with no use. The default is `15m`. The chart then gives the gateway the right to change only the number of replicas of that one Deployment, and adds one exception for it to the admission policy. The setting is off by default. See `docs/admin/chart.md` and `docs/admin/tool-search.md`. ([#470](https://github.com/solidDoWant/toolgate/pull/470), [#471](https://github.com/solidDoWant/toolgate/pull/471), [#474](https://github.com/solidDoWant/toolgate/pull/474))
- While the embeddings server is stopped or starts, a search uses words only. The result tells the model that the search used keywords only. The metric `toolgate_tool_search_embedding_errors_total` counts these searches with the reason `scaled_down`. ([#474](https://github.com/solidDoWant/toolgate/pull/474))

### Fixes

- With the embeddings server on and no `embeddings.server.cache.existingClaim`, `helm upgrade` from 0.4.0 deleted the volume claim for the models while the server still used it, and the upgrade failed. The chart now creates the claim only when it does not exist, and an upgrade never deletes it. The image digests of the embeddings server and of the model download Job are now optional. ([#474](https://github.com/solidDoWant/toolgate/pull/474))
- The database setup SQL for split roles in `docs/admin/chart.md` now installs the `vector` and `pg_trgm` extensions in the `toolgate` schema. Before, the migrations failed with `type "vector" does not exist` when a superuser with another name than `toolgate` ran the SQL, for example `postgres` on CloudNativePG. The page also shows how to do this with a CloudNativePG `Database`. If the `vector` type is not visible, `toolgate migrate` now stops with an error that names the `search_path` and the schema of the extension. ([#473](https://github.com/solidDoWant/toolgate/pull/473))

Internal work: the acceptance tests have names that tell their behaviour, the checklist maps each scenario to its tests, and the code and docs have no tracking IDs, which a lint check now enforces.

### Known issues

- Sessions that enabled a template named `toolgate` before the upgrade to 0.4 cannot call the built-in tools until the session restarts.
- `govulncheck ./...` reports [GO-2026-6865](https://pkg.go.dev/vuln/GO-2026-6865) in `github.com/klauspost/compress` v1.20.1 and [GO-2026-6253](https://pkg.go.dev/vuln/GO-2026-6253) in `github.com/moby/go-archive` v0.2.0. Only the test helpers in `internal/testutil` use these modules, through testcontainers and envtest. The `toolgate` command and the gateway image do not contain them, and `govulncheck ./cmd/...` finds no vulnerabilities.

## 0.4.0

> [!WARNING]
> Read this before you upgrade. The fields `toolFilter`, `name`, `description` and `inputDescriptions` of a template take effect only after every gateway pod runs 0.4. During a rolling upgrade, an older pod ignores them. It lists hidden tools, forwards calls to them and does not rename tools. Each call still gets a recorded decision. If you hide a tool as a security control, use `tools.<name>.blocked`. It works on both versions. See `docs/admin/templates-and-rules.md`.

toolgate is a gateway that you run yourself, between Claude Code and MCP servers. An MCP server gives Claude tools, for example to read GitHub issues. toolgate starts each server on Kubernetes when a session needs it. It checks every tool call against the rules of the admin, and it records every decision in an audit log. This release adds tool search, and it starts a server only when a call needs it. Users can allow and deny servers and tools from inside a session. Admins can hide, rename and describe the tools of a server. The gateway also speaks the new MCP revision 2026-07-28, and users can connect from a claude.ai custom connector.

To install toolgate:

- Admins install the Helm chart `oci://ghcr.io/soliddowant/charts/toolgate`, version `0.4.0`. The chart runs the image `ghcr.io/soliddowant/toolgate:0.4.0`. Follow `docs/admin/quick-start.md`.
- Users download the `toolgate` command for their system from the files of this release, and put it on their `PATH`. Then they install the Claude Code plugin with `claude plugin marketplace add solidDoWant/toolgate` and `claude plugin install toolgate@toolgate`. Follow `docs/user/README.md`.

### Upgrade notes for admins

- Make sure that your PostgreSQL has the pgvector extension before you upgrade. The new migration runs `CREATE EXTENSION vector`, also when tool search is off, and the gateway does not start without it. The standard CloudNativePG images include pgvector. If the migration role is not a superuser, a superuser must run `CREATE EXTENSION IF NOT EXISTS vector;` and `CREATE EXTENSION IF NOT EXISTS pg_trgm;` in the toolgate database first. See `docs/admin/tool-search.md` and `docs/admin/chart.md`. ([#364](https://github.com/solidDoWant/toolgate/pull/364), [#385](https://github.com/solidDoWant/toolgate/pull/385))
- Back up your PostgreSQL database before you upgrade. The gateway applies new database migrations when it starts, and you cannot undo them. ([#364](https://github.com/solidDoWant/toolgate/pull/364))
- Rename any template with the name `toolgate` before you upgrade. The name is now reserved for the built-in server. A template with that name is not valid, and its running instances stop. ([#365](https://github.com/solidDoWant/toolgate/pull/365), [#390](https://github.com/solidDoWant/toolgate/pull/390))
- Wait until every gateway pod runs 0.4 before you rely on the new template fields for tool curation, as the warning above says. ([#367](https://github.com/solidDoWant/toolgate/pull/367), [#391](https://github.com/solidDoWant/toolgate/pull/391))

### Tool calls and lazy start

- Claude can call a tool of an allowed server without `toolgate_enable_server` first. The call turns on the server in the session, and the audit log records `server.enable` with the reason `first-call`. ([#380](https://github.com/solidDoWant/toolgate/pull/380), [#391](https://github.com/solidDoWant/toolgate/pull/391), [#438](https://github.com/solidDoWant/toolgate/pull/438))
- toolgate remembers the tool list of each template version. It learns the list from the last instance that became ready, or takes it from the new template field `declaredTools`. Sessions and tool search use this list while no instance runs. If a running instance has other tools, sessions get the real list. ([#365](https://github.com/solidDoWant/toolgate/pull/365), [#368](https://github.com/solidDoWant/toolgate/pull/368), [#442](https://github.com/solidDoWant/toolgate/pull/442))
- A server starts only when the first call needs it. That call waits while the server starts, and Claude shows the progress. If the server cannot start, the call fails with the reason. A template can set `start: onEnable` to start the server as soon as a session turns it on. The gateway decides about the call before it starts anything. ([#365](https://github.com/solidDoWant/toolgate/pull/365), [#377](https://github.com/solidDoWant/toolgate/pull/377), [#420](https://github.com/solidDoWant/toolgate/pull/420), [#439](https://github.com/solidDoWant/toolgate/pull/439), [#443](https://github.com/solidDoWant/toolgate/pull/443))
- For a `per-user` server whose tools depend on the user, declare the tools in `spec.declaredTools`. Otherwise, a user can see the tool names and descriptions that toolgate learned from the instance of another user until their own instance is ready. See `docs/admin/security-model.md`. ([#368](https://github.com/solidDoWant/toolgate/pull/368))

### Tool search

- In tool search mode, a session lists the toolgate tools and `toolgate_find_tools` in place of the tools of every server. Claude describes a task, and `toolgate_find_tools` returns the best tools of all the servers that the session can use, with their inputs. Claude then calls a tool, and the call turns on its server. Search never returns a tool that the rules refuse. In a verified workspace, it can return a tool of a server that is not on the workspace list, marked as needing approval. The first call then asks the user. Admins turn on the mode with `config.toolSearch`. A client can turn it on or off with the `X-Toolgate-Tool-Search` header. See `docs/user/tool-search.md` and `docs/admin/tool-search.md`. ([#376](https://github.com/solidDoWant/toolgate/pull/376), [#385](https://github.com/solidDoWant/toolgate/pull/385))
- Search ranks tools by their words. If you set `config.toolSearchEmbeddings`, it also ranks them by meaning, with an embeddings endpoint that speaks the OpenAI embeddings API. The gateway then sends the search text and the tool descriptions to that endpoint. The chart can run the endpoint for you with `embeddings.server.enabled`. If the endpoint fails or is slow, search uses words only. ([#385](https://github.com/solidDoWant/toolgate/pull/385), [#441](https://github.com/solidDoWant/toolgate/pull/441), [#444](https://github.com/solidDoWant/toolgate/pull/444))
- toolgate records how well search works in real use: which tools a search returned and at which rank, which of them Claude called, and searches that Claude repeated. The audit log gets `tool.search` and `tool.search_hit` events. The search text is not recorded. ([#412](https://github.com/solidDoWant/toolgate/pull/412))

### Workspaces and access

- The catalog has a built-in server named `toolgate` when the web UI is on. Its tools are `toolgate_list_scopes`, `toolgate_allow` and `toolgate_deny`. They do the same things as `toolgate workspace` and the web UI, from inside a session. The rules, the workspace lists and the audit log apply to them as to the tools of any other server. A rule matches them by their exact names. ([#390](https://github.com/solidDoWant/toolgate/pull/390), [#406](https://github.com/solidDoWant/toolgate/pull/406), [#394](https://github.com/solidDoWant/toolgate/pull/394), [#437](https://github.com/solidDoWant/toolgate/pull/437))
- A user can deny a server, or one tool of a server, in a workspace or in one open session. The deny applies at once, with `toolgate_deny`, `toolgate workspace deny` or `toolgate workspace deny --session`. A denied server stays refused, also when the default is all. ([#372](https://github.com/solidDoWant/toolgate/pull/372), [#387](https://github.com/solidDoWant/toolgate/pull/387), [#394](https://github.com/solidDoWant/toolgate/pull/394))
- A user can allow one tool in a workspace, or servers and tools in one open session with a verified workspace. The change waits until the user approves it in the browser after a fresh sign-in. `toolgate_allow` gives a link to the approval page. An allow for a session ends with that session. When a session starts again, its allows and its open requests go away. ([#393](https://github.com/solidDoWant/toolgate/pull/393), [#406](https://github.com/solidDoWant/toolgate/pull/406), [#436](https://github.com/solidDoWant/toolgate/pull/436), [#440](https://github.com/solidDoWant/toolgate/pull/440))
- The default for unverified sessions can be a list of servers that the user picks, as well as all or none. Use `toolgate workspace defaults --unverified list` and `toolgate workspace allow --unverified SERVER...`, or "Only these servers" on the Workspaces page of the web UI. ([#364](https://github.com/solidDoWant/toolgate/pull/364), [#400](https://github.com/solidDoWant/toolgate/pull/400))

### Templates and tool curation

- A template can hide tools of its server with `toolFilter`, as an allow list or a deny list. A hidden tool is not listed and not found by search, and a call to it is refused. ([#365](https://github.com/solidDoWant/toolgate/pull/365), [#367](https://github.com/solidDoWant/toolgate/pull/367))
- A template can give a tool another name, description or description of its inputs, with `tools.<name>.name`, `description` and `inputDescriptions`. Claude sees the new values, and calls reach the tool by its own name. Two tools cannot get the same new name. See `docs/admin/templates-and-rules.md`. ([#365](https://github.com/solidDoWant/toolgate/pull/365), [#367](https://github.com/solidDoWant/toolgate/pull/367), [#434](https://github.com/solidDoWant/toolgate/pull/434))

### MCP revisions and claude.ai connectors

- The gateway speaks MCP 2026-07-28 as well as 2025-11-25, with clients and with MCP servers. It prefers 2025-11-25. A server that speaks only 2026-07-28 works as any other server. See `docs/admin/mcp-revisions.md`. ([#371](https://github.com/solidDoWant/toolgate/pull/371), [#373](https://github.com/solidDoWant/toolgate/pull/373))
- Users can connect from a claude.ai custom connector, or from another MCP client without the toolgate plugin. The gateway publishes OAuth protected resource metadata, so the client signs the user in through your IdP. A client without the session header gets a session for its MCP session. Set `config.mcpPublicURL`, and route only `/mcp` and the metadata paths from the internet. See `docs/admin/connectors.md`. ([#369](https://github.com/solidDoWant/toolgate/pull/369), [#375](https://github.com/solidDoWant/toolgate/pull/375), [#435](https://github.com/solidDoWant/toolgate/pull/435))

### Metrics and dashboards

- The gateway exports new Prometheus metrics: open sessions, tool calls and their duration, policy refusals, server starts and their duration, calls that wait for a start, instance health, approvals, sign-ins, MCP requests, database connections, embeddings requests and tool search. `docs/admin/metrics.md` lists each metric with example queries. ([#407](https://github.com/solidDoWant/toolgate/pull/407), [#408](https://github.com/solidDoWant/toolgate/pull/408), [#409](https://github.com/solidDoWant/toolgate/pull/409), [#411](https://github.com/solidDoWant/toolgate/pull/411), [#412](https://github.com/solidDoWant/toolgate/pull/412))
- The chart includes four Grafana dashboards. Turn them on with `dashboards.enabled` for the Grafana sidecar, or with `dashboards.grafanaOperator.enabled` for grafana-operator. ([#417](https://github.com/solidDoWant/toolgate/pull/417))

Internal work: a new migration and queries, a fixed test set and `make search-eval` to compare search models, faster integration tests, CI that runs the full suite on every push to master, and updates to the design, the acceptance checklist and the agent instructions.

### Known issues

- Sessions that enabled a template named `toolgate` before the upgrade can't call the built-in tools until the session restarts.
- `govulncheck ./...` reports [GO-2026-6865](https://pkg.go.dev/vuln/GO-2026-6865) in `github.com/klauspost/compress` v1.20.1 and [GO-2026-6253](https://pkg.go.dev/vuln/GO-2026-6253) in `github.com/moby/go-archive` v0.2.0. Only the test helpers in `internal/testutil` use these modules, through testcontainers and envtest. The `toolgate` command and the gateway image do not contain them, and `govulncheck ./cmd/...` finds no vulnerabilities.

## 0.3.0

> [!WARNING]
> Read this before you upgrade. If the web UI is on, each user has two workspace defaults after the upgrade: one for verified workspaces and one for unverified sessions. Both defaults are `none`. Then the sessions of a user see no MCP servers until that user sets a default or adds a list for a workspace. Each user sets their own defaults. Tell your users to run this command after the upgrade:
>
> ```
> toolgate workspace defaults --unverified all
> ```
>
> If the sessions of a user also run in verified workspaces, the user can also run `toolgate workspace defaults --verified all`. The command prints a link to an approval page, because the change gives more access. The user approves it there, after a fresh sign-in. Users can make the same changes on the Workspaces page of the web UI, `/ui/workspaces`. If the web UI is off, workspace scoping is off, and nothing changes for your users.

toolgate is a gateway that you run yourself, between Claude Code and MCP servers. An MCP server gives Claude tools, for example to read GitHub issues. toolgate starts each server on Kubernetes when a session needs it. It checks every tool call against the rules of the admin, and it records every decision in an audit log. This release adds telemetry, attested sign-in for workloads and workspaces, and health checks for servers. It also limits each session to the servers that its user allows in its workspace, and gives a session its servers back after a long pause.

To install toolgate:

- Admins install the Helm chart `oci://ghcr.io/soliddowant/charts/toolgate`, version `0.3.0`. The chart runs the image `ghcr.io/soliddowant/toolgate:0.3.0`. Follow `docs/admin/quick-start.md`. To turn on the web UI, follow `docs/admin/web-ui.md`.
- Users download the `toolgate` command for their system from the files of this release, and put it on their `PATH`. Then they install the Claude Code plugin with `claude plugin marketplace add solidDoWant/toolgate` and `claude plugin install toolgate@toolgate`. Follow `docs/user/README.md`.

### Upgrade notes for admins

- Tell your users about the workspace defaults before you upgrade, as the warning above says. ([#310](https://github.com/solidDoWant/toolgate/pull/310), [#322](https://github.com/solidDoWant/toolgate/pull/322), [#323](https://github.com/solidDoWant/toolgate/pull/323))
- Back up your PostgreSQL database before you upgrade. The gateway applies new database migrations when it starts, and you cannot undo them. ([#246](https://github.com/solidDoWant/toolgate/pull/246), [#248](https://github.com/solidDoWant/toolgate/pull/248), [#297](https://github.com/solidDoWant/toolgate/pull/297))
- To mount a file for the gateway, for example a `caFile`, use `advancedMounts.gateway.gateway` in the chart values. The files of the chart moved there from `globalMounts`, so that the migration Job does not get them. ([#259](https://github.com/solidDoWant/toolgate/pull/259))

### Workspaces and sessions

- If the web UI is on, each session of a signed-in user can use only the servers on its effective list. The effective list is the list of the user for a verified workspace. If there is no list, or the session is unverified, the defaults of the user apply. The list only refuses. It never gives a server that the templates do not give. Service accounts and SPIFFE workloads do not have a list. See `docs/user/README.md` and `docs/admin/web-ui.md`. ([#297](https://github.com/solidDoWant/toolgate/pull/297), [#310](https://github.com/solidDoWant/toolgate/pull/310))
- The new command `toolgate workspace` shows and changes the lists and the defaults, with `list`, `allow`, `deny`, `clear` and `defaults`. A change that removes access applies at once. A change that gives more access prints a link to an approval page, and the user approves it there after a fresh sign-in. ([#320](https://github.com/solidDoWant/toolgate/pull/320), [#322](https://github.com/solidDoWant/toolgate/pull/322))
- The Workspaces page of the web UI, `/ui/workspaces`, shows and changes the same lists and defaults. ([#323](https://github.com/solidDoWant/toolgate/pull/323))
- In a verified workspace, Claude can ask to turn on a server that is not on the effective list. Then toolgate asks the user in the browser. The user selects "Allow for this session" or "Always allow in this workspace". When the session ends, toolgate removes the servers that the user allowed for that session only. ([#325](https://github.com/solidDoWant/toolgate/pull/325))
- A session that ended after a long pause gets its servers back. If you resume it in Claude Code from the same workspace within 24 hours, its servers start again, and you do not turn them on again. toolgate checks each server again against the rules and the workspace list. Admins change the 24 hours with `config.sessionResumeWindow`. ([#302](https://github.com/solidDoWant/toolgate/pull/302), [#321](https://github.com/solidDoWant/toolgate/pull/321), [#338](https://github.com/solidDoWant/toolgate/pull/338))
- If Claude calls a tool of a server that is still starting, the call waits until the server is ready, and then it runs. Before, the call failed at once. ([#337](https://github.com/solidDoWant/toolgate/pull/337))
- The new command `toolgate sessions` lists your open sessions and the servers that are turned on in each. ([#249](https://github.com/solidDoWant/toolgate/pull/249))
- The new command `toolgate audit tail` prints audit events as they happen. A user sees only their own events. A member of the admin group of the web UI sees the events of all users. ([#261](https://github.com/solidDoWant/toolgate/pull/261))

### Telemetry

- The gateway can receive the OpenTelemetry data of Claude Code: metrics for tokens and cost, log events and traces. Set `otlpIngest` to `true`, and set `otlpEndpoint` to your backend. The gateway removes the user and workspace attributes that the client sent, and sets them from the verified identity. Then it sends the data to your backend. See `docs/admin/telemetry.md`. ([#251](https://github.com/solidDoWant/toolgate/pull/251))
- By default, the gateway removes prompts and tool content from this data. Set `telemetry.allowContent` to `true` only if your backend can hold them. ([#251](https://github.com/solidDoWant/toolgate/pull/251))
- `docs/admin/telemetry.md` gives a query for the tokens and cost of each user, workspace and session. ([#265](https://github.com/solidDoWant/toolgate/pull/265))
- The new command `toolgate otel-headers` prints the headers that Claude Code sends with its telemetry. Admins give it to Claude Code with a `SettingsProfile`. ([#247](https://github.com/solidDoWant/toolgate/pull/247))

### Signing in and attestation

- A workload can sign in with a SPIFFE JWT-SVID. A JWT-SVID is a signed token from SPIRE that names the workload. List the trust domains that you allow in `config.spiffe.trustDomains`. The gateway treats these workloads as attested, and their workspace is the path of the SPIFFE ID. A JWT-SVID from another trust domain gets 401. See `docs/admin/spiffe-sign-in.md`. ([#253](https://github.com/solidDoWant/toolgate/pull/253))
- The `toolgate` command can use a JWT-SVID as its credential. Set `TOOLGATE_SPIFFE_SOCKET` to the address of the SPIFFE Workload API. ([#264](https://github.com/solidDoWant/toolgate/pull/264))
- A user session can be attested. The client sends the user token and also a workload token from the workspace, for example from a Coder workspace pod. If the workload belongs to the user, the session stays the session of the user, and its workspace is verified, for example `coder/alice/dev`. Set `config.workspaceAttestation` to turn it on. See `docs/admin/workspace-attestation.md`. ([#299](https://github.com/solidDoWant/toolgate/pull/299), [#313](https://github.com/solidDoWant/toolgate/pull/313))
- The `toolgate` command can get its access token from another program. Set `TOOLGATE_TOKEN_COMMAND` to a command that prints the token, for example `coder external-auth access-token toolgate`. See `docs/admin/coder.md`. ([#301](https://github.com/solidDoWant/toolgate/pull/301))
- `config.oidc.audience` can be a list of 1 to 8 audiences of the same issuer. The gateway accepts access tokens for each of them. The audit log now gives the reason "audience not allowed" when it refuses a token for another audience. Before, the reason was "invalid token". ([#296](https://github.com/solidDoWant/toolgate/pull/296))
- `toolgate login --token-store auto|keychain|file` selects where toolgate keeps the token. Use `file` in a workspace that has no keychain for the processes of the agent. `toolgate login` also prints how long the code is valid, and who signed in. ([#291](https://github.com/solidDoWant/toolgate/pull/291))

### Server health

- A template can have a `healthCheck`: an HTTP check of a Service, a port and a path. If the check fails `failureThreshold` times in a row, toolgate withdraws the tools of the instance, and refuses calls with the reason. Then it repairs the instance as `onFailure` says: `replace`, `reapply` or `none`. If the check still fails after `maxRepairs` repairs in 30 minutes, the instance fails. See `docs/admin/templates-and-rules.md`. ([#248](https://github.com/solidDoWant/toolgate/pull/248), [#260](https://github.com/solidDoWant/toolgate/pull/260), [#263](https://github.com/solidDoWant/toolgate/pull/263))
- If an object of a ready instance fails later, toolgate withdraws the tools of the instance and repairs it in the same way. An example is a `HelmRelease` that stays `Ready=False` for 60 seconds. Before, the instance failed. ([#267](https://github.com/solidDoWant/toolgate/pull/267), [#335](https://github.com/solidDoWant/toolgate/pull/335))

### Deployment and operations

- The new command `toolgate migrate` applies the database migrations and exits. ([#250](https://github.com/solidDoWant/toolgate/pull/250))
- The chart can apply the migrations with a separate database user. Set `config.database.migrateOnStart` to `false`, and set `config.database.migrationSecret`. Then a Helm hook Job applies the migrations before the new gateway pods start. The gateway runs with a database user that can only read and write data. With the default values, nothing changes. See `docs/admin/chart.md`. ([#259](https://github.com/solidDoWant/toolgate/pull/259))

### Fixes

- The gateway now makes the monthly partitions of the audit log every day, on the leader. Before, it made them only when it started. A gateway that ran past the end of the next month without a restart then refused every tool call. The new metric `toolgate_audit_partition_errors_total` counts failures. ([#246](https://github.com/solidDoWant/toolgate/pull/246))
- `toolgate audit tail` and `toolgate workspace list` escape control characters in their text output. A name with control characters can no longer change your terminal. ([#334](https://github.com/solidDoWant/toolgate/pull/334))
- A slow upload to the telemetry ingest no longer blocks the other uploads. The gateway reads each upload in at most 10 seconds, and answers 408 to a slower one. ([#336](https://github.com/solidDoWant/toolgate/pull/336))
- If the resume of a session stops before it ends, toolgate undoes the resume, and the next request does the full resume again. Before, some servers stayed turned on with no running instance. ([#338](https://github.com/solidDoWant/toolgate/pull/338))
- A custom health check no longer hides a failure that an object of the instance reports. ([#335](https://github.com/solidDoWant/toolgate/pull/335))

### Claude Code plugin

- The plugin did not change in this release, except for the version number. Update the `toolgate` command to get the new commands.

### Known issues

- Nobody tested the manual scenario AC-3-07 before this release. AC-3-07 checks that a tool span of Claude Code and the spans of the gateway for that call are in the same trace. The owner tests it separately.
- `govulncheck ./...` reports [GO-2026-6253](https://pkg.go.dev/vuln/GO-2026-6253) in `github.com/moby/go-archive` v0.2.0 and [GO-2026-6865](https://pkg.go.dev/vuln/GO-2026-6865) in `github.com/klauspost/compress` v1.20.1. Only the test helpers in `internal/testutil` use these modules, through testcontainers and envtest. The `toolgate` command and the gateway image do not contain them, and `govulncheck ./cmd/...` finds no vulnerabilities.

Internal work: the phase 3 and phase 4 design and acceptance documents, the identity kind in the policy code, e2e tests with a time scale and shards balanced by duration, acceptance scoring from all test results, container images from ghcr.io and mirror.gcr.io in place of Docker Hub, CI cache and disk fixes, and agent instructions. ([#224](https://github.com/solidDoWant/toolgate/pull/224), [#225](https://github.com/solidDoWant/toolgate/pull/225), [#226](https://github.com/solidDoWant/toolgate/pull/226), [#229](https://github.com/solidDoWant/toolgate/pull/229), [#231](https://github.com/solidDoWant/toolgate/pull/231), [#241](https://github.com/solidDoWant/toolgate/pull/241), [#252](https://github.com/solidDoWant/toolgate/pull/252), [#254](https://github.com/solidDoWant/toolgate/pull/254), [#256](https://github.com/solidDoWant/toolgate/pull/256), [#257](https://github.com/solidDoWant/toolgate/pull/257), [#262](https://github.com/solidDoWant/toolgate/pull/262), [#266](https://github.com/solidDoWant/toolgate/pull/266), [#268](https://github.com/solidDoWant/toolgate/pull/268), [#269](https://github.com/solidDoWant/toolgate/pull/269), [#270](https://github.com/solidDoWant/toolgate/pull/270), [#271](https://github.com/solidDoWant/toolgate/pull/271), [#274](https://github.com/solidDoWant/toolgate/pull/274), [#275](https://github.com/solidDoWant/toolgate/pull/275), [#276](https://github.com/solidDoWant/toolgate/pull/276), [#290](https://github.com/solidDoWant/toolgate/pull/290), [#292](https://github.com/solidDoWant/toolgate/pull/292), [#298](https://github.com/solidDoWant/toolgate/pull/298), [#300](https://github.com/solidDoWant/toolgate/pull/300), [#303](https://github.com/solidDoWant/toolgate/pull/303), [#311](https://github.com/solidDoWant/toolgate/pull/311), [#312](https://github.com/solidDoWant/toolgate/pull/312), [#314](https://github.com/solidDoWant/toolgate/pull/314), [#315](https://github.com/solidDoWant/toolgate/pull/315), [#317](https://github.com/solidDoWant/toolgate/pull/317), [#318](https://github.com/solidDoWant/toolgate/pull/318), [#319](https://github.com/solidDoWant/toolgate/pull/319), [#324](https://github.com/solidDoWant/toolgate/pull/324), [#328](https://github.com/solidDoWant/toolgate/pull/328))

## 0.2.0

toolgate is a gateway that you run yourself, between Claude Code and MCP servers. An MCP server gives Claude tools, for example to read GitHub issues. toolgate starts each server on Kubernetes when a session needs it. It checks every tool call against the rules of the admin, and it records every decision in an audit log. This release adds a web UI and more control over tool calls. Users sign in to the web UI to see their servers, sessions, secrets and audit events. Users can get a grant for an `opt-in` server, and approve a tool call before it runs. Admins can add temporary blocks, get values for templates from resolvers, store tool arguments and results in S3, and use any namespaced kind in a template.

To install toolgate:

- Admins install the Helm chart `oci://ghcr.io/soliddowant/charts/toolgate`, version `0.2.0`. The chart runs the image `ghcr.io/soliddowant/toolgate:0.2.0`. Follow `docs/admin/quick-start.md`. To turn on the web UI, follow `docs/admin/web-ui.md`.
- Users download the `toolgate` command for their system from the files of this release, and put it on their `PATH`. Then they install the Claude Code plugin with `claude plugin marketplace add solidDoWant/toolgate` and `claude plugin install toolgate@toolgate`. Follow `docs/user/README.md`.

### Upgrade notes for admins

- Back up your PostgreSQL database before you upgrade. The gateway applies new database migrations when it starts, and you cannot undo them. ([#190](https://github.com/solidDoWant/toolgate/pull/190))
- If you must keep audit events for more than 90 days, copy them out of PostgreSQL before you upgrade. The gateway now deletes audit events that are older than 90 days, once an hour. ([#190](https://github.com/solidDoWant/toolgate/pull/190))
- Look at all RBAC roles that you bound to the service account of the gateway. The admission policy no longer limits the kinds that the gateway can write. RBAC now decides the kinds. The admission policy still keeps every write in the namespaces of toolgate, and still refuses cluster-scoped objects. ([#191](https://github.com/solidDoWant/toolgate/pull/191))

### Web UI

- The gateway has a web UI under `/ui/`. Users sign in with your identity provider, through a separate OIDC client with PKCE. Set `config.ui.publicURL` and `config.ui.clientID` to turn it on. A sign-in lasts 8 hours, or ends after 1 hour without use. ([#193](https://github.com/solidDoWant/toolgate/pull/193))
- Members of the group in `config.ui.adminGroup` are admins in the UI. Admins see the events, sessions and shared instances of all users. ([#193](https://github.com/solidDoWant/toolgate/pull/193))
- Two actions need a fresh sign-in, less than 5 minutes old: creating a grant and approving a tool call. The UI sends the user back to the identity provider for it. ([#199](https://github.com/solidDoWant/toolgate/pull/199))
- The Servers page shows the servers that the user can use, and the running instances of the user. An outdated instance has a Restart button, which starts a new instance from the current template. An instance is outdated when its template changed, or when a secret that it uses changed. ([#200](https://github.com/solidDoWant/toolgate/pull/200), [#206](https://github.com/solidDoWant/toolgate/pull/206), [#221](https://github.com/solidDoWant/toolgate/pull/221))
- The Sessions pages show the open sessions of the user, with their servers and newest tool decisions. ([#210](https://github.com/solidDoWant/toolgate/pull/210))
- The Secrets page sets and deletes the secrets of the user, from a text box or a file. It never shows a value. ([#208](https://github.com/solidDoWant/toolgate/pull/208))
- The Audit page lists audit events, with filters and a time range of up to 31 days. A user sees only their own events. ([#205](https://github.com/solidDoWant/toolgate/pull/205))
- Each change in the UI must come from a UI page. The gateway refuses a request from another site, and a form without the token of the session. Each page has a strict Content Security Policy and is not stored in a cache. The UI uses no scripts from other sites. ([#200](https://github.com/solidDoWant/toolgate/pull/200))

### Grants and blocks

- A user can create a grant in the UI to use an `opt-in` server or tool from their own machine, for 1 minute to 7 days. A grant can be for all sessions of the user or for one session. When a call needs a grant, the refusal gives a link to the form. A user can revoke a grant at any time. ([#194](https://github.com/solidDoWant/toolgate/pull/194), [#212](https://github.com/solidDoWant/toolgate/pull/212))
- Admins can add a temporary block on the Blocks page. A block refuses the matching tools for all users until it expires, from 1 minute to 30 days. Open sessions get the change within 5 seconds. The page also lists the `ToolgateRule` objects from Git, read-only. ([#194](https://github.com/solidDoWant/toolgate/pull/194), [#211](https://github.com/solidDoWant/toolgate/pull/211))

### Approvals

- A template can mark a tool with `approval: required`. Each call of that tool then needs the approval of the user who made the call, on a page of the UI. One approval lets one call through. ([#202](https://github.com/solidDoWant/toolgate/pull/202), [#207](https://github.com/solidDoWant/toolgate/pull/207))
- If Claude Code supports URL elicitation (a request from the gateway to open a web page), the call waits while the user approves or denies it. Otherwise the call fails with a link, and the user calls the tool again after the approval. Set the time that a request waits with `config.ui.approvalTimeout`. The default is 10 minutes. ([#209](https://github.com/solidDoWant/toolgate/pull/209))

### Templates and secrets

- A template can contain any namespaced kind that the cluster has, for example a Flux `HelmRelease`. To let the gateway create more kinds, add rules to `rbac.extraRules`. ([#191](https://github.com/solidDoWant/toolgate/pull/191))
- An instance with a `HelmRelease`, or any object with a `Ready` condition, is ready when that condition is `True`. If it stays `False` for 60 seconds, the instance fails with the message of the condition. ([#195](https://github.com/solidDoWant/toolgate/pull/195))
- A template can use `${resolver:NAME}` in a Secret. A resolver is an HTTPS service that you list in `config.resolvers`. It gives a short-lived value for the user, for example a GitHub App token. The gateway replaces the instance before the value expires. ([#213](https://github.com/solidDoWant/toolgate/pull/213))

### Audit log

- The gateway can store audit payloads: the arguments of each tool call and approval request, and each tool result. Set `config.audit.payloads` to `true` and give an S3 bucket in `config.audit.s3`. Payloads under 64 KiB go in PostgreSQL, larger ones go to S3. If the bucket has object lock, the gateway locks each object for 90 days. ([#192](https://github.com/solidDoWant/toolgate/pull/192))
- The audit event page in the UI shows the payload of the event, and a link downloads it. The browser does not need access to S3. ([#216](https://github.com/solidDoWant/toolgate/pull/216))

### Claude Code plugin and CLI

- The plugin and the `toolgate` command did not change in this release, except for the version number. Users of 0.1.0 can keep their installation.

### Known issues

- Nobody tested the manual scenario AC-2-11 before this release. The owner tests it when they deploy it.
- `govulncheck ./...` reports [GO-2026-6253](https://pkg.go.dev/vuln/GO-2026-6253) in `github.com/moby/go-archive` v0.2.0 and [GO-2026-6865](https://pkg.go.dev/vuln/GO-2026-6865) in `github.com/klauspost/compress` v1.20.1. The security audit of this phase also found GO-2026-6253. Only the test helpers in `internal/testutil` use these modules, through testcontainers and envtest. The `toolgate` command and the gateway image do not contain them, and `govulncheck ./cmd/...` finds no vulnerabilities.

Internal work: a browser test harness, the e2e job split across parallel kind clusters, the acceptance scoreboard for all phases, the phase 2 and phase 3 design documents, and the agent scheduler. ([#161](https://github.com/solidDoWant/toolgate/pull/161), [#163](https://github.com/solidDoWant/toolgate/pull/163), [#166](https://github.com/solidDoWant/toolgate/pull/166), [#170](https://github.com/solidDoWant/toolgate/pull/170), [#189](https://github.com/solidDoWant/toolgate/pull/189), [#197](https://github.com/solidDoWant/toolgate/pull/197), [#198](https://github.com/solidDoWant/toolgate/pull/198), [#201](https://github.com/solidDoWant/toolgate/pull/201), [#204](https://github.com/solidDoWant/toolgate/pull/204), [#215](https://github.com/solidDoWant/toolgate/pull/215), [#217](https://github.com/solidDoWant/toolgate/pull/217), [#218](https://github.com/solidDoWant/toolgate/pull/218), [#219](https://github.com/solidDoWant/toolgate/pull/219))

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
- When a rule reaches its `expires` time, open sessions get the changed tool list within 5 seconds. ([#160](https://github.com/solidDoWant/toolgate/pull/160))
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
- The gateway records each refused sign-in as an `auth.refuse` event in the audit log, with the reason. ([#159](https://github.com/solidDoWant/toolgate/pull/159))
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

- Nobody tested the two manual scenarios AC-1-05 and AC-1-12 before this release, because the owner tests them when they deploy it.
- `govulncheck ./...` reports [GO-2026-6253](https://pkg.go.dev/vuln/GO-2026-6253) in `github.com/moby/go-archive` v0.2.0 and [GO-2026-6686](https://pkg.go.dev/vuln/GO-2026-6686) in `github.com/moby/sys/user` v0.4.0. Only the test helpers in `internal/testutil` use these modules, through testcontainers and envtest. The `toolgate` command and the gateway image do not contain them, and `govulncheck ./cmd/...` finds no vulnerabilities.

Internal work: CI, unit, integration, envtest and kind end-to-end tests, the acceptance scoreboard, the release pipeline, the design documents and the agent scheduler. ([#50](https://github.com/solidDoWant/toolgate/pull/50), [#53](https://github.com/solidDoWant/toolgate/pull/53), [#61](https://github.com/solidDoWant/toolgate/pull/61), [#69](https://github.com/solidDoWant/toolgate/pull/69), [#76](https://github.com/solidDoWant/toolgate/pull/76), [#77](https://github.com/solidDoWant/toolgate/pull/77), [#86](https://github.com/solidDoWant/toolgate/pull/86), [#88](https://github.com/solidDoWant/toolgate/pull/88), [#94](https://github.com/solidDoWant/toolgate/pull/94), [#95](https://github.com/solidDoWant/toolgate/pull/95), [#103](https://github.com/solidDoWant/toolgate/pull/103), [#112](https://github.com/solidDoWant/toolgate/pull/112), [#115](https://github.com/solidDoWant/toolgate/pull/115), [#125](https://github.com/solidDoWant/toolgate/pull/125), [#128](https://github.com/solidDoWant/toolgate/pull/128), [#129](https://github.com/solidDoWant/toolgate/pull/129), [#132](https://github.com/solidDoWant/toolgate/pull/132), [#134](https://github.com/solidDoWant/toolgate/pull/134), [#147](https://github.com/solidDoWant/toolgate/pull/147), [#151](https://github.com/solidDoWant/toolgate/pull/151), [#154](https://github.com/solidDoWant/toolgate/pull/154))
