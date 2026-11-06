# Metrics

The gateway serves Prometheus metrics at `/metrics`. This page lists each metric, what it means, and an example query.

A metric label is either a fixed set of values or the name of a template that you define. No metric has a label for a user, a session, a workspace, a tool, a request, or an error text. The audit log holds that detail.

## Series budget

A series is one metric with one set of label values. A metrics backend uses memory for each series, so toolgate keeps the number small and stable.

Each replica exports at most `300 + 10 × T` series, where `T` is the number of templates. A histogram counts as one series for each bucket, plus the `+Inf` bucket, the sum and the count. The embeddings request duration histogram has 10 buckets, so it adds 26 series (13 for each of its two operations). The metrics with a `server` label add up to 12 series for each template: 6 call outcomes, 2 start outcomes, 1 health failure series and 3 repair actions. A test in the toolgate source checks this limit, with 10 templates and every outcome and revision, and checks that every label value belongs to a fixed set. The test also counts every outcome of every kind of approval request.

The process and Go metrics add about 50 series. The Go metrics are the basic memory and garbage collector set, not the full runtime list.

## Leader-only metrics

Only the replica that holds the leader Lease sets these metrics. The other replicas export no series for them, so each series exists once. Query them without a `sum` across replicas, or use `max`.

- `toolgate_sessions`
- `toolgate_instances`
- `toolgate_instance_namespaces_stuck`
- `toolgate_approval_requests_pending`
- `toolgate_leader`

When the leader changes, the new leader sets the metrics within 30 seconds.

## Sessions and instances

`toolgate_sessions{tool_search, verified}` is a gauge of the sessions that have not ended. The label `tool_search` is `on` when the latest request of the session used [tool search](tool-search.md), and `off` otherwise. The label `verified` is `true` when the gateway attested the workspace of the session, and `false` otherwise. All four series are always present on the leader, and they are 0 when no session is open.

To scale a server with KEDA on the number of open sessions, use this query:

```promql
sum(max by (tool_search, verified) (toolgate_sessions))
```

To see how many sessions use tool search:

```promql
sum(max by (tool_search) (toolgate_sessions{tool_search="on"}))
```

`toolgate_instances{scope, status}` is a gauge of the instances that are not stopped. The label `scope` is `per-session`, `per-user` or `shared`. The label `status` is `pending`, `starting`, `ready`, `failed` or `stopping`. The label `revision` is the MCP revision the gateway uses with the instance: `2025-11-25` or `2026-07-28`. An instance that is not ready yet counts as `2025-11-25`.

```promql
max by (status) (toolgate_instances{scope="shared"})
```

`toolgate_instance_namespaces_stuck` is a gauge of the instance namespaces that stay in the `Terminating` state for more than 10 minutes. See [Instance stops and cleanup](instances.md).

```promql
max(toolgate_instance_namespaces_stuck) > 0
```

## Tool calls and policy

`toolgate_tool_calls_total{server, outcome}` counts tool calls. The label `outcome` is one of:

- `forwarded`: the call reached the server and it answered, even if the tool reported an error.
- `refused`: the gateway refused the call, for any reason other than the next ones.
- `approval`: the call needed an approval.
- `upstream_error`: the call was allowed but the server failed to answer, or its result was too large.
- `timeout`: the server did not become ready in time.
- `start_failed`: the server failed to start.

A call to a server that the catalog does not list counts with `server="unknown"`.

```promql
sum by (server) (rate(toolgate_tool_calls_total{outcome="upstream_error"}[5m]))
```

`toolgate_tool_call_duration_seconds` is a histogram of the time from the receipt of a forwarded call to the answer of the server. It includes any wait for a starting server. It has no `server` label. The buckets run from 0.05 s to 120 s.

```promql
histogram_quantile(0.95, sum by (le) (rate(toolgate_tool_call_duration_seconds_bucket[5m])))
```

`toolgate_policy_refusals_total{step, reason}` counts the calls that policy refused. The label `step` is the policy step that refused: `access`, `environment`, `blocked`, `rule`, `temporary-block` or `workspace-scope`. The label `reason` is `denied` for a deny entry and `refused` for any other refusal. The audit log has the full text.

```promql
sum by (step) (increase(toolgate_policy_refusals_total[1h]))
```

## Starts and instance health

`toolgate_instance_starts_total{server, outcome}` counts the starts that finished. The label `outcome` is `ready` or `failed`. Alert when failures rise:

```promql
sum by (server) (increase(toolgate_instance_starts_total{outcome="failed"}[15m])) > 0
```

`toolgate_instance_start_duration_seconds` is a histogram of the time from the creation of an instance to `ready`. It has no `server` label. The buckets run from 1 s to 300 s.

`toolgate_calls_waiting_for_start` is a gauge of the calls on this replica that wait for a starting instance. Sum it over the replicas.

```promql
sum(toolgate_calls_waiting_for_start)
```

`toolgate_instance_health_failures_total{server}` counts the instances that turned unhealthy.

`toolgate_instance_repairs_total{server, action}` counts the repairs. The label `action` is `replace`, `reapply`, or `gave_up` when the repairs ran out and the instance failed. Alert on `gave_up`:

```promql
sum by (server) (increase(toolgate_instance_repairs_total{action="gave_up"}[1h])) > 0
```

## Tool search embeddings

`toolgate_embeddings_request_duration_seconds{operation}` is a histogram of the time of one request to the embeddings endpoint. The label `operation` is `query` for the embedding of a search query, and `document` for the embedding of tool descriptions when the gateway builds the search index. It records requests that reached the endpoint, failed ones too. A call that the open circuit skipped records nothing. The buckets run from 0.01 s to 10 s.

```promql
histogram_quantile(0.95, sum by (le, operation) (rate(toolgate_embeddings_request_duration_seconds_bucket[5m])))
```

`toolgate_tool_search_embedding_errors_total{use, reason}` counts the calls that failed or were skipped. The label `use` is `query` or `document`. The label `reason` is `error` for a failed call, and `circuit_open` for a call skipped because the circuit is open after repeated failures. While embeddings fail, search still works with word matching only.

Suggested alert on embedding errors:

```promql
sum by (use) (increase(toolgate_tool_search_embedding_errors_total[15m])) > 0
```

## Auth and telemetry

`toolgate_spiffe_bundle_age_seconds{trust_domain}` is the time since the gateway fetched the SPIFFE bundle of the trust domain. The gateway exports it only when SPIFFE sign-in is on.

```promql
max by (trust_domain) (toolgate_spiffe_bundle_age_seconds) > 3600
```

`toolgate_otlp_ingest_requests_total{signal, code}` counts the telemetry requests that the gateway receives.

```promql
sum by (code) (rate(toolgate_otlp_ingest_requests_total[5m]))
```

`toolgate_auth_refusals_dropped_total` and `toolgate_ui_auth_refusals_dropped_total` count failed sign-ins that the gateway did not write to the audit log, because their remote address passed 10 a minute.

```promql
sum(increase(toolgate_auth_refusals_dropped_total[1h]))
```

## Audit

`toolgate_audit_partition_errors_total` counts failed attempts to create the audit partitions. `toolgate_audit_payload_errors_total` counts payloads that the gateway could not store. `toolgate_audit_retention_refused_total` counts old audit rows that the gateway kept because S3 refused to delete their objects. See [Operations](operations.md).

```promql
increase(toolgate_audit_payload_errors_total[1h]) > 0
```

## Tool search quality

The counters `toolgate_tool_search_total`, `toolgate_tool_search_hits_total` (extra label `rank`), `toolgate_tool_search_researches_total` and `toolgate_tool_search_no_call_total` show how well tool search works in real use. They have the labels `degraded` and `embedding_model`. See [Tool search](tool-search.md#search-quality-in-use).

## Approvals

`toolgate_approval_requests_total{kind, outcome}` counts approval and access requests that reached an outcome. The label `kind` is `tool` (a tool call that needs approval), `server` (a request to enable a server), `scope` or `change`. The label `outcome` is `approved`, `denied`, `expired` or `cancelled`. A request is `cancelled` when its session ended before anyone decided.

```promql
sum by (kind) (increase(toolgate_approval_requests_total{outcome="expired"}[1h]))
```

`toolgate_approval_requests_pending{kind}` is a gauge of the requests that wait for a decision and have not expired. All four kinds are always present on the leader.

```promql
max(sum by (kind) (toolgate_approval_requests_pending)) > 20
```

Suggested alert: pending approvals that grow for 30 minutes mean that nobody decides.

```promql
deriv(max(sum(toolgate_approval_requests_pending))[30m:]) > 0
```

## Clients

`toolgate_mcp_requests_total{revision, method}` counts MCP requests. The label `revision` is the MCP protocol revision the client sent: `2024-11-05`, `2025-03-26`, `2025-06-18`, `2025-11-25` or `2026-07-28`. The label `method` is `initialize`, `tools/list`, `tools/call` or `subscriptions/listen`. Any other value, or a missing one, is `other`. The values come from the client, so the gateway never uses them as they are.

```promql
sum by (revision) (rate(toolgate_mcp_requests_total{method="tools/call"}[5m]))
```

`toolgate_auth_results_total{kind, result}` counts sign-ins. The label `kind` is `user`, `service_account`, `spiffe` or `connector`. A `connector` is a user on the MCP endpoint without the plugin's session header. The label `result` is `ok`, `refused` or `attested`. A user whose workspace the gateway attested counts once as `ok` and once more as `attested`.

```promql
sum by (kind) (rate(toolgate_auth_results_total{result="refused"}[5m]))
```

## Database and process

`toolgate_db_pool_connections{state}` is a gauge of the database connections of the replica, where `state` is `idle`, `in_use` or `max`. `toolgate_db_pool_wait_seconds_total` is the time that requests waited for a free connection. `toolgate_db_pool_acquire_total` counts the connections taken from the pool.

Suggested alert: a pool at its maximum.

```promql
max by (pod) (toolgate_db_pool_connections{state="in_use"}) >= max by (pod) (toolgate_db_pool_connections{state="max"})
```

```promql
rate(toolgate_db_pool_wait_seconds_total[5m])
```

`toolgate_notify_reconnects_total` counts reconnects of the PostgreSQL `LISTEN` connection. After a reconnect the replica reads its state again.

```promql
increase(toolgate_notify_reconnects_total[1h])
```

`toolgate_leader` is 1 on the replica that holds the Lease. The other replicas export no series. Suggested alert: no leader.

```promql
absent(toolgate_leader == 1)
```

`toolgate_build_info{version}` is always 1. Its label is the version of the build.

```promql
count by (version) (toolgate_build_info)
```

The gateway also exports the standard process metrics (`process_*`) and a basic set of Go memory and garbage collector metrics (`go_*`).

## Dashboards

The chart ships four Grafana dashboards. They read the metrics on this page, work with any Prometheus-compatible data source (such as VictoriaMetrics), and are off by default. Each has a `datasource` variable, a `namespace` variable and a `job` variable, the tag `toolgate`, and a fixed `uid`.

- **toolgate / Overview** (`toolgate-overview`): open sessions, tool calls per second by outcome, the top servers by calls and by errors, call latency (p50, p95, p99), policy refusals by step, pending approvals and approval outcomes.
- **toolgate / Servers and instances** (`toolgate-servers`): instances by status, scope and revision, starts and start failures, start latency, calls that wait for a start, health failures and repairs. Repairs with the action `gave_up` show in red.
- **toolgate / Tool search** (`toolgate-tool-search`): searches per second, the share of degraded searches, the top-1 and top-5 hit rates, and the re-search and no-call rates for each `embedding_model` (a model change shows as a step), and embeddings latency and errors by reason.
- **toolgate / Platform** (`toolgate-platform`): database pool use and wait time, `LISTEN` reconnects, the leader replica, the version of each replica, and memory and CPU of each replica.

Panels for each server use counters and `topk`, so a deployment with many servers stays readable. The `namespace` and `job` variables need the `namespace` and `job` labels that Prometheus adds when it scrapes the gateway (for example through the chart's `ServiceMonitor`). Panels for each replica need a `pod` label.

### Grafana sidecar

Set `dashboards.enabled=true`. The chart creates one ConfigMap for each dashboard, with the label `grafana_dashboard: "1"`. The Grafana dashboard sidecar loads ConfigMaps with that label. Change the label with `dashboards.labelKey` and `dashboards.labelValue`. Use `dashboards.annotations` for a folder annotation such as `grafana_folder`.

### grafana-operator

Set `dashboards.grafanaOperator.enabled=true` and `dashboards.grafanaOperator.instanceSelector.matchLabels` to the labels of your `Grafana` resource. The chart then creates one `GrafanaDashboard` (grafana-operator v5) for each dashboard, and no ConfigMaps. Set `dashboards.grafanaOperator.folder` to choose the Grafana folder. The grafana-operator CRDs must exist in the cluster.
