# toolgate documentation

For: anyone who wants to find the right page. You need to know nothing yet.

toolgate has four kinds of readers. Each page is for one of them. This page says who the readers are, which pages are theirs, and what each page covers.

## Who reads these docs

| Reader | Who they are | What they already know | What they need |
|---|---|---|---|
| User | A person who runs Claude Code on a laptop, in a Coder workspace, in a pod or in CI. | Claude Code. | How to install the plugin, sign in, use servers and set secrets, and what a refusal means. |
| Admin | A person who deploys toolgate and writes its catalog and rules. | Kubernetes, Helm, Flux, OIDC and PostgreSQL. | How to deploy and configure toolgate, how to write templates and rules, the limits of the security model, and how to operate it. |
| Developer | A person who changes the code. | Go and MCP. Not this codebase. | The architecture, the packages, how to test and how to release. |
| Agent | The AI agents that build this project. | Everything in `CLAUDE.md`. | Exact rules. Their pages in `docs/agents/` and `.claude/` do not follow the style rules for people. |

The owner reads the acceptance checklists, the release notes and the status reports. These pages are for a user and an admin together, in plain English.

## Where each reader starts

- User: [`docs/user/`](user/README.md).
- Admin: [`docs/admin/`](admin/README.md).
- Developer: [`docs/dev/`](dev/README.md). [`SPEC.md`](../SPEC.md) has the full rules of the system.
- Agent: `CLAUDE.md`, then [`docs/agents/`](agents/planner.md).

## The tree

Every page under `docs/` is listed here. `make docs-check` fails when a page is missing from this list.

```
README.md                  The front door: what toolgate is and its status.
SPEC.md                    The rules of the system. For developers and agents.

docs/README.md             This page.

docs/user/
  README.md                Install the plugin, sign in, use servers, set secrets, read a refusal.
  tool-search.md           Find tools by describing a task, and turn search mode on.

docs/admin/
  README.md                The list of admin pages.
  configuration.md         The gateway configuration reference (generated).
  pod-sign-in.md           How pods sign in with service-account tokens, and the permissions it needs.
  coder.md                 Sign in Coder workspaces with Coder's external auth and toolgate configure.
  spiffe-sign-in.md        How workloads sign in with SPIFFE JWT-SVIDs, and access.spiffeIDs.
  workspace-attestation.md How a user session proves its workspace with a workload token.
  web-ui.md                How to turn on the web UI: its OIDC client, sessions and admins.
  connectors.md            Connect users from a claude.ai custom connector: the IdP client and the route.
  quick-start.md           Install the chart with the smallest set of values.
  chart.md                 What the chart installs, and why its permissions are safe.
  chart-values.md          The chart values reference (generated).
  user-secrets.md          How user secrets are encrypted, and how to rotate the key.
  token-exchange.md        How to give an MCP server a token for the user with an OAuth token exchange.
  settings-profiles.md     How to give Claude Code settings to users with SettingsProfile objects.
  instances.md             When instances stop, how they are removed, and their metrics.
  metrics.md               Every metric the gateway exports, the series budget, example queries.
  templates-and-rules.md   How to write MCPServerTemplate and ToolgateRule objects.
  telemetry.md             How to send Claude Code telemetry through the gateway, with the usage queries.
  template-secrets.md      How to give templates shared values from Secrets in one namespace.
  security-model.md        What toolgate protects against, and what claimed workspaces and sessions cannot.
  operations.md            Backups, upgrades, key rotation, instance limits, the audit log, uninstall.
  tool-search.md           Run tool search: the embeddings endpoint, the chart model server, notes and limits.
  mcp-revisions.md         Which MCP revisions clients and upstream servers use, and how the gateway picks one.

docs/dev/
  README.md                What the developer pages will cover.
  integration-tests.md     The dependencies that integration tests start: PostgreSQL, S3, fake OIDC.
  e2e-tests.md             The e2e harness on kind, the test MCP server, the acceptance scoreboard.
  otlp-fixture.md          How the recorded Claude Code OTLP fixture was made.
  claude-code-integration.md  How toolgate gets the Claude Code session ID, with the test evidence.

docs/acceptance/
  phase-1.md               The owner's checklist for phase 1: the core gateway.
  phase-2.md               The owner's checklist for phase 2: the security model and the web UI.
  phase-3.md               The owner's checklist for phase 3: telemetry, attestation, scoping and operations.
  phase-4.md               The owner's checklist for phase 4: tool search and curation.

docs/design/
  phase-1.md               The design for phase 1: who does what, state, and the interaction table.
  phase-2.md               The design for phase 2: who does what, state, and the interaction table.
  phase-3.md               The design for phase 3: who does what, state, and the interaction table.
  phase-4.md               The design for phase 4: lazy start, tool search, curation, delegated access, MCP revisions and connectors.
  instance-credentials.md  Token exchange (`${exchange:…}`) and admin template secrets (`${templateSecret:…}`) for instances.
  persistent-volumes.md    The design for template volumes that outlive an instance: state, binding, clean-up, permissions.
  workspace-keys.md        The design for registered workspace keys, a third source of workspace attestation.
  instance-upstream-sessions.md  One upstream MCP session for each instance and toolgate session, the same on every replica.
  tool-result-cache.md     The design for opt-in caching of tool results.

docs/agents/               Instructions for the agents (not for people).
  planner.md               The planner's playbook.
```

## Rules for writing the pages

- Write one page for one reader. Put each fact on the page for the reader who acts on it, and link to it from other pages.
- Write in plain English. Agents load the `simple-english` skill before they write a page for people.
- Generate reference tables (configuration, chart values, metrics) from the code when they exist, so that they stay correct.
- `make docs-check` checks the links and this list. It does not count words or score the style.
