# toolgate for admins

For: people who deploy toolgate and write its catalog and rules.

These pages tell you how to deploy toolgate, write its catalog and rules, and keep it running. You need to know Kubernetes, Helm, Flux, OIDC and PostgreSQL.

## Deploy

- [Quick start](quick-start.md): install the chart in a test cluster with the smallest set of values.
- [The Helm chart](chart.md): what the chart installs, and why its permissions are safe.
- [Chart values reference](chart-values.md): every value of the chart. This page is generated from the chart.
- [Gateway configuration reference](configuration.md): every setting of `toolgate serve`. This page is generated from the code.
- [Pod sign-in](pod-sign-in.md): how pods sign in with a Kubernetes service-account token.
- [The web UI](web-ui.md): how to turn on the web UI, its OIDC client, its sessions and its admins.

## Write the catalog and rules

- [Templates and rules](templates-and-rules.md): how to write `MCPServerTemplate` and `ToolgateRule` objects.
- [Settings profiles](settings-profiles.md): how to give Claude Code settings to your users.
- [The security model](security-model.md): what toolgate protects against, and what a claimed workspace or session cannot protect against.

## Operate

- [Operations](operations.md): backups, upgrades, key rotation, instance limits, the audit log, and how to remove toolgate.
- [User secrets](user-secrets.md): how to make and rotate the keyset that encrypts user secrets.
- [Instance stops and cleanup](instances.md): when toolgate stops a server, the instance limits, and the metrics for stuck namespaces.

[`SPEC.md`](../../SPEC.md) describes the full rules of the system.
