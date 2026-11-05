# toolgate for admins

For: people who deploy toolgate and write its catalog and rules.

toolgate is in early development, and these pages are not written yet. They will tell you how to:

- deploy toolgate with its Helm chart and Flux, with PostgreSQL, S3 and an OIDC identity provider;
- write `MCPServerTemplate` catalog entries, `ToolgateRule` block rules and `SettingsProfile` objects (API group `toolgate.soliddowant.dev`);
- configure the gateway and the chart, with reference pages that are generated from the code;
- understand what the security model protects against, and what a client can avoid by the workspace or session that it claims;
- operate toolgate: the audit log, metrics, traces and upgrades.

The [gateway configuration reference](configuration.md) lists the settings of `toolgate serve`. The [quick start](quick-start.md) installs the chart. [The chart page](chart.md) tells you what the chart installs and what its permissions allow, and the [chart values reference](chart-values.md) lists its values.

[Pod sign-in](pod-sign-in.md) tells how a pod signs in with a Kubernetes service-account token, and which permissions the gateway needs for it.

Until then, [`SPEC.md`](../../SPEC.md) describes the system.
