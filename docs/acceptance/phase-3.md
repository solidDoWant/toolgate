# Phase 3 acceptance checklist: SaaS servers and telemetry

Same rules as phase 1. The tag is `[AC-3-nn]`. This checklist is at feature level. The owner reviews it again, with the phase 3 design, before phase 3 starts.

## Remote MCP servers

- **AC-3-01 Connect.** For a template describing a remote MCP server with OAuth, a user's first enable sends them, through elicitation or the UI, to the provider's consent page. Afterwards their calls reach the remote server with their own token.
- **AC-3-02 Token storage.** The user's tokens are stored encrypted like other secrets, refreshed before expiry, and never returned by any API.
- **AC-3-03 Revoked token.** When the provider rejects the token, the call fails with a message telling the user to reconnect, and reconnecting works.
- **AC-3-04 Disconnect.** Disconnecting in the UI deletes the user's tokens. The next enable asks for consent again.

## Telemetry

- **AC-3-05 OTLP ingest.** Claude Code's metrics, log events and traces, sent through the plugin's settings to the gateway, reach the configured OTLP backend with the verified principal and workspace. User attributes set by the client are replaced.
- **AC-3-06 Usage per session.** `docs/admin/` shows a working query for token use and cost per user, workspace and session, checked against the test backend.
- **AC-3-07 One trace (manual).** In the trace backend, a Claude Code tool span and the gateway's spans for that call are in the same trace.

## Attestation

- **AC-3-08 GitHub Actions.** A GitHub Actions OIDC token from an allowed repository is attested, with workspace `gha/<owner>/<repo>/<workflow>`, and rules can match it.
- **AC-3-09 Coder.** Either Coder workspaces can be attested and a test shows it, or the design records why not and the docs say so.

## CLI

- **AC-3-10 Audit tail.** `toolgate audit tail` streams the user's own events, and all events for an admin.
- **AC-3-11 Sessions.** `toolgate sessions` lists the user's open sessions and their enabled servers.

## Release

- **AC-3-12 Release.** Release `0.3.0` is published, with docs for remote servers and telemetry.
