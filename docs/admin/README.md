# toolgate for admins

For: people who deploy toolgate and write its catalog and rules.

toolgate is in early development, and these pages are not written yet. They will tell you how to:

- deploy toolgate with its Helm chart and Flux, with PostgreSQL, S3 and an OIDC identity provider;
- write `MCPServerTemplate` catalog entries, `ToolgateRule` block rules and `SettingsProfile` objects (API group `toolgate.soliddowant.dev`);
- configure the gateway and the chart, with reference pages that are generated from the code;
- understand what the security model protects against, and what a client can avoid by the workspace or session that it claims;
- operate toolgate: the audit log, metrics, traces and upgrades.

Until then, [`SPEC.md`](../../SPEC.md) describes the system.
