# Telemetry

For: admins who want the usage data of Claude Code in their own telemetry backend.

Claude Code can send OpenTelemetry data (OTLP): metrics for tokens and cost, log events, and traces. toolgate can receive this data, stamp each record with the user that signed in, and pass it on to your backend. Then you can see tokens and cost for each user, workspace and session.

## Send the data straight to your backends

toolgate sends each kind of data to its own backend. You do not need an OpenTelemetry collector. Set these gateway settings (see the [configuration reference](configuration.md)):

- `otlpIngest`: `true`. The gateway then accepts `POST /v1/traces`, `/v1/metrics` and `/v1/logs` from signed-in clients.
- `otlp.metricsEndpoint`: the base URL of the metrics backend. The gateway sends metrics to `<url>/v1/metrics`.
- `otlp.logsEndpoint`: the base URL of the logs backend. The gateway sends logs to `<url>/v1/logs`.
- `otlp.tracesEndpoint`: the base URL of the traces backend. The gateway sends traces to `<url>/v1/traces`. The gateway's own traces go there too.

If a kind of data has no endpoint, the gateway accepts it and discards it. It answers success, so clients do not log export errors. The counter `toolgate_otlp_ingest_discarded_total{signal}` counts the discarded requests.

The older setting `otlpEndpoint` still works. It fills in each of the three endpoints that you leave empty. So a deployment that sets only `otlpEndpoint` sends everything to one place, as before.

### Example: VictoriaMetrics and VictoriaLogs

VictoriaMetrics takes OTLP metrics at `/opentelemetry/v1/metrics` (cluster: `/insert/0/opentelemetry/v1/metrics`). VictoriaLogs takes OTLP logs at `/insert/opentelemetry/v1/logs`. This chart configuration sends metrics and logs there and drops traces:

```yaml
config:
  otlpIngest: true
  otlpMetricsEndpoint: http://victoria-metrics:8428/opentelemetry
  otlpLogsEndpoint: http://victoria-logs:9428/insert/opentelemetry
  # No traces backend: the gateway discards traces.
```

### Write logs to stdout

Set `otlp.logsEndpoint` to `stdout` (chart: `config.otlpLogsEndpoint: stdout`) to send no logs to a backend. The gateway then writes each log record it receives to its own standard output. It writes one JSON line for each record, after it sets the identity and removes the content. Your normal cluster log pipeline (node agent, log aggregator, log store) then collects the lines with the other logs of the pod.

Each line has these fields:

- `source`: always `claude-code`. Use it to tell these lines from the gateway's own logs.
- `event`: the event name, such as `api_request`, `tool_decision`, `tool_result`, `user_prompt` or `api_error`.
- `time_unix_nano` and, when set, `observed_time_unix_nano`: the times as strings, in nanoseconds since 1970.
- `severity` and `body`, when the record has them (`body` only when it is text).
- `attributes`: the attributes of the record, with the OTLP names (`user.email`, `model`, `input_tokens`). Numbers stay numbers.
- `resource`: only `service.name`, `service.version`, `os.type` and `host.arch`.

The gateway writes each line in one piece. A line can be longer than 16 KiB. The container runtime then splits it, and the node agent joins it again. The counter `toolgate_otlp_ingest_log_records_total{result}` counts the records that the gateway wrote (`written`) or could not write (`failed`).

Example query for VictoriaLogs, when the node agent puts the JSON fields in `log_fields`:

```
log_fields.source:"claude-code" log_fields.event:"api_request" | stats by (log_fields.attributes.user.email) sum(log_fields.attributes.input_tokens)
```

The field names depend on your node agent. Adjust the prefix to match.

### Remove the session label from metrics

Every session adds a new `session.id` value, so a metrics backend stores a new series for each session. Set `telemetry.dropSessionIdFromMetrics` to `true` (chart: `config.dropSessionIdFromMetrics`) to remove `session.id` from metric data points. Logs and traces keep it. If two data points in one request become identical, the gateway combines them into one. Sums and histogram counts add up, and a gauge keeps its last value. Then the backend gets no double counts and no rejected data.

### Alternative: a collector

If you already run a collector, set only `otlpEndpoint` to its OTLP/HTTP address, such as `http://otel-collector:4318`. The collector then routes the data.

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
- Use `cumulative` temporality. The OTLP receiver of Prometheus and VictoriaMetrics needs it.

## Read usage

VictoriaMetrics stores OTLP metrics with their dotted names. The metrics are `claude_code.token.usage` and `claude_code.cost.usage`. The labels are `user.email`, `workspace.id`, `model` and `type` (`input`, `output`, `cacheRead` or `cacheCreation`). Without `dropSessionIdFromMetrics`, the label `session.id` is also there. The gateway puts these on the data points, so the queries need no join.

Tokens in the last day, by type:

```metricsql
sum by (type) (increase({__name__="claude_code.token.usage"}[1d]))
```

Estimated cost in US dollars in the last day, by user:

```metricsql
sum by ("user.email") (increase({__name__="claude_code.cost.usage"}[1d]))
```

Claude Code computes the cost at API list prices. It is an estimate and not a bill.

`increase` estimates the growth of a counter between its first and last sample in the window, and may add up to half a sample interval at each end. So it is exact only to within that estimate.

A Prometheus backend that uses the default OTLP translation of Prometheus stores other names: `claude_code_token_usage_tokens_total` and `claude_code_cost_usage_USD_total`, with the labels `user_id`, `user_email`, `workspace_id` and `session_id` (and `model` and `type`). Use these queries there.

Tokens in the last day, by type (`input`, `output`, `cacheRead`, `cacheCreation`):

```promql
sum by (user_id, user_email, workspace_id, session_id, type) (increase(claude_code_token_usage_tokens_total[1d]))
```

Cost in US dollars in the last day:

```promql
sum by (user_id, user_email, workspace_id, session_id) (increase(claude_code_cost_usage_USD_total[1d]))
```

### Dashboard

The chart can install the dashboard "toolgate / Claude Code usage". Set `dashboards.usage.enabled` to `true` together with `dashboards.enabled` or `dashboards.grafanaOperator.enabled`. It has variables for user, workspace and model, token and cost charts, top users and workspaces, and a sessions table. Its queries use the VictoriaMetrics names. With `dropSessionIdFromMetrics`, the sessions table is empty. Read sessions from the logs then. The platform dashboard has an "OTLP ingest" row for the gateway's own ingest metrics.

## Prompts and tool content

Claude Code can add prompts, tool inputs and tool results to its data. Users turn this on with `OTEL_LOG_USER_PROMPTS`, `OTEL_LOG_TOOL_DETAILS` and `OTEL_LOG_TOOL_CONTENT`. By default the gateway removes that content before it forwards the data: the `tool.output` span events, the `claude_code.system_prompt` log records, and the attributes `prompt`, `prompt_text`, `response`, `tool_parameters`, `tool_input`, `full_command`, `bash_command`, `output`, `file_path`, `diff` and a few more. It also removes `error` from tool results, tool spans and MCP connection events, because it can quote tool content.

Set `telemetry.allowContent` to `true` only when your backend may hold user prompts and tool content. The gateway then passes the content through. It still removes `user_prompt` and `user_system_prompt`, because their names start with `user_`.
