# toolgate for admins

For: people who deploy toolgate and write its catalog and rules.

These pages tell you how to deploy toolgate, write its catalog and rules, and keep it running. You need to know Kubernetes, Helm, Flux, OIDC and PostgreSQL.

## Deploy

- [Quick start](quick-start.md): install the chart in a test cluster with the smallest set of values.
- [The Helm chart](chart.md): what the chart installs, why its permissions are safe, and the optional migration Job for split database roles.
- [Chart values reference](chart-values.md): every value of the chart. This page is generated from the chart.
- [Gateway configuration reference](configuration.md): every setting of `toolgate serve`. This page is generated from the code.
- [Pod sign-in](pod-sign-in.md): how pods sign in with a Kubernetes service-account token.
- [Coder workspaces](coder.md): sign in workspaces through Coder's external auth, with `TOOLGATE_TOKEN_COMMAND`.
- [SPIFFE sign-in](spiffe-sign-in.md): how workloads sign in with a SPIFFE JWT-SVID, and the `spiffeIDs` access list.
- [Workspace attestation](workspace-attestation.md): how a user session in a workspace gets a verified workspace, and why the pod labels can be trusted.
- [The web UI](web-ui.md): how to turn on the web UI, its OIDC client, its sessions and its admins.
- [MCP revisions](mcp-revisions.md): which MCP revisions the gateway speaks with clients, and the 400 for a 2026-07-28 client without a session header.
- [claude.ai connectors](connectors.md): how users connect from a claude.ai custom connector, with the Authentik provider, the gateway settings and the route from the internet.

## Write the catalog and rules

- [Templates and rules](templates-and-rules.md): how to write `MCPServerTemplate` and `ToolgateRule` objects.
- [Health checks](templates-and-rules.md#health-checks): how to make toolgate check a running server and repair it.
- [Settings profiles](settings-profiles.md): how to give Claude Code settings to your users.
- [Telemetry](telemetry.md): how to send the OpenTelemetry data of Claude Code through the gateway, and how to read tokens and cost for each user.
- [The security model](security-model.md): what toolgate protects against, and what a claimed workspace or session cannot protect against.

## Operate

- [Operations](operations.md): backups, upgrades, key rotation, instance limits, the audit log, and how to remove toolgate.
- [User secrets](user-secrets.md): how to make and rotate the keyset that encrypts user secrets.
- [Instance stops and cleanup](instances.md): when toolgate stops a server, the instance limits, and the metrics for stuck namespaces.

[`SPEC.md`](../../SPEC.md) describes the full rules of the system.
