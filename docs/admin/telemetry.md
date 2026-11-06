# Telemetry

For: admins who want the usage data of Claude Code in their own telemetry backend.

Claude Code can send OpenTelemetry data (OTLP): metrics for tokens and cost, log events, and traces. toolgate can receive this data, stamp each record with the user that signed in, and pass it on to your backend. Then you can see tokens and cost for each user, workspace and session.

## Turn on the ingest

Set these gateway settings (see the [configuration reference](configuration.md)):

- `otlpEndpoint`: the OTLP/HTTP address of your backend, such as `http://otel-collector:4318`. The gateway also sends its own traces there.
- `otlpIngest`: `true`. The gateway then accepts `POST /v1/traces`, `/v1/metrics` and `/v1/logs` from signed-in clients.

The gateway removes every attribute the client sent whose name starts with `user_`, `enduser_` or `workspace_` (after it changes each character outside `A-Z`, `a-z` and `0-9` to `_`). Then it sets `user.id`, `enduser.id`, `user.email` and `workspace.id` from the verified identity on every span, metric data point and log record. A user cannot send data for someone else. The `session.id` attribute stays as Claude Code sent it, because the session ID is a claim and not a verified fact.

## Give users the settings

A plugin cannot set the environment of Claude Code. So you hand out the settings with a `SettingsProfile` (see [Settings profiles](settings-profiles.md)). Change the selector and the endpoint for your setup:

```yaml
apiVersion: toolgate.soliddowant.dev/v1alpha1
kind: SettingsProfile
metadata: {name: telemetry, namespace: toolgate-catalog}
spec:
  selector: {groups: [dev]}
  settings:
    otelHeadersHelper: toolgate otel-headers
    env:
      CLAUDE_CODE_ENABLE_TELEMETRY: "1"
      CLAUDE_CODE_ENHANCED_TELEMETRY_BETA: "1"    # traces
      OTEL_METRICS_EXPORTER: otlp
      OTEL_LOGS_EXPORTER: otlp
      OTEL_TRACES_EXPORTER: otlp
      OTEL_EXPORTER_OTLP_PROTOCOL: http/protobuf
      OTEL_EXPORTER_OTLP_ENDPOINT: https://toolgate.example.com
      OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE: cumulative
      CLAUDE_CODE_OTEL_HEADERS_HELPER_DEBOUNCE_MS: "240000"
```

- Claude Code reads `env` when it starts. A change to the profile takes effect in the next session.
- `toolgate otel-headers` prints the `Authorization` and `X-Toolgate-Workspace` headers. Claude Code runs it again every 4 minutes (`240000` ms; the default is 29 minutes). Each run prints a new token. So your identity provider must issue access tokens that live at least 5 minutes. A shorter token can expire between two runs.
- Use `cumulative` temporality. The OTLP receiver of Prometheus needs it.

## Read usage

With the default OTLP translation of Prometheus, the metrics are `claude_code_token_usage_tokens_total` and `claude_code_cost_usage_USD_total`. The labels are `user_id`, `user_email`, `workspace_id` and `session_id`. The gateway puts these on the data points, so the queries need no join.

Tokens in the last day, by type (`input`, `output`, `cacheRead`, `cacheCreation`):

```promql
sum by (user_id, user_email, workspace_id, session_id, type) (increase(claude_code_token_usage_tokens_total[1d]))
```

Cost in US dollars in the last day:

```promql
sum by (user_id, user_email, workspace_id, session_id) (increase(claude_code_cost_usage_USD_total[1d]))
```

`increase` estimates the growth of a counter between its first and last sample in the window, and may add up to half a sample interval at each end. So it is exact only to within that estimate.

## Prompts and tool content

Claude Code can add prompts, tool inputs and tool results to its data. Users turn this on with `OTEL_LOG_USER_PROMPTS`, `OTEL_LOG_TOOL_DETAILS` and `OTEL_LOG_TOOL_CONTENT`. By default the gateway removes that content before it forwards the data: the `tool.output` span events, the `claude_code.system_prompt` log records, and the attributes `prompt`, `prompt_text`, `response`, `tool_parameters`, `tool_input`, `full_command`, `bash_command`, `output`, `file_path`, `diff` and a few more. It also removes `error` from tool results, tool spans and MCP connection events, because it can quote tool content.

Set `telemetry.allowContent` to `true` only when your backend may hold user prompts and tool content. The gateway then passes the content through. It still removes `user_prompt` and `user_system_prompt`, because their names start with `user_`.
