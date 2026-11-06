# Claude Code OTLP recording (operator, 2026-11-06)

- Claude Code 2.1.318 (Claude Code), `claude -p` with model sonnet, in the toolgate VM.
- Prompt: Use the Bash tool to run `echo toolgate-fixture`, then reply with exactly the output. One built-in Bash tool call; no toolgate MCP tool (the operator decided a built-in tool exercises the same ingest paths; AC-3-07 covers the toolgate trace link manually).
- Settings: the docs/design/phase-3.md §3 profile, with OTEL_EXPORTER_OTLP_PROTOCOL=http/json (so the recording could be scrubbed as text; replay it as JSON, or convert to protobuf with pdata), OTEL_METRIC_EXPORT_INTERVAL=2000 and OTEL_LOGS_EXPORT_INTERVAL=2000, sent to a local OTLP sink.
- content-on/: OTEL_LOG_USER_PROMPTS, OTEL_LOG_TOOL_DETAILS and OTEL_LOG_TOOL_CONTENT all 1. content-off/: none set.
- Scrubbed: the account-derived user.id hash is replaced with 40 zeros. No email, account UUID, organization ID or local path appears in the recording.
- Files are the request bodies in arrival order: NNN-metrics.json, NNN-logs.json, NNN-traces.json.
