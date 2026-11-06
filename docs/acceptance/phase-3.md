# Phase 3 acceptance checklist: telemetry, attestation and operations

Same rules as phase 1. The tag is `[AC-3-nn]`. This checklist is at feature level. The owner reviews it again, with the phase 3 design, before phase 3 starts.

## Telemetry

- **AC-3-05 OTLP ingest.** Claude Code's metrics, log events and traces, sent through the plugin's settings to the gateway, reach the configured OTLP backend with the verified principal and workspace. User attributes set by the client are replaced.
- **AC-3-06 Usage per session.** `docs/admin/` shows a working query for token use and cost per user, workspace and session, checked against the test backend.
- **AC-3-07 One trace (manual).** In the trace backend, a Claude Code tool span and the gateway's spans for that call are in the same trace.

## Attestation

- **AC-3-09 SPIFFE.** A SPIFFE JWT-SVID from an allowed trust domain, with audience `toolgate`, is attested. Its workspace is the SPIFFE ID's path, rules can match it, and an SVID from another trust domain gets 401.

## CLI

- **AC-3-10 Audit tail.** `toolgate audit tail` streams the user's own events, and all events for an admin.
- **AC-3-11 Sessions.** `toolgate sessions` lists the user's open sessions and their enabled servers.

## Instance health

- **AC-3-13 Custom health check.** A template declares a health check (an HTTP probe or a cheap MCP tool call) for its server. When the check starts failing after the instance is `ready`, the instance leaves `ready` within the check's interval times its threshold, with the check's reason. Open sessions get `list_changed`. A template without a custom check behaves as before.
- **AC-3-14 Object health after ready.** When an object of a `ready` instance later reports failure (for example a HelmRelease going `Ready=False` for 60 s), the instance leaves `ready` with that object's reason.

## Migrations

- **AC-3-15 Separate migration credential.** With `config.database.migrateOnStart: false` and a migration secret, `helm install` and `helm upgrade` apply the migrations through a hook Job before the new gateway pods start. The gateway then runs with a database user that has only data rights. With the default values, behaviour is unchanged.

## Release

- **AC-3-12 Release.** Release `0.3.0` is published, with docs for telemetry, SPIFFE attestation, health checks and the migration Job.
