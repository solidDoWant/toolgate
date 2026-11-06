# Metrics

The gateway serves Prometheus metrics at `/metrics`. This page lists each metric, what it means, and an example query.

A metric label is either a fixed set of values or the name of a template that you define. No metric has a label for a user, a session, a workspace, a tool, a request, or an error text. The audit log holds that detail.

## Series budget

A series is one metric with one set of label values. A metrics backend uses memory for each series, so toolgate keeps the number small and stable.

Each replica exports at most `300 + 10 × T` series, where `T` is the number of templates. The metrics with a `server` label add up to 12 series for each template: 6 call outcomes, 2 start outcomes, 1 health failure series and 3 repair actions. A test in the toolgate source checks this limit, with 10 templates and every outcome and revision, and checks that every label value belongs to a fixed set. The test also counts every outcome of every kind of approval request.

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

`toolgate_sessions{verified}` is a gauge of the sessions that have not ended. The label `verified` is `true` when the gateway attested the workspace of the session, and `false` otherwise. Both series are always present on the leader, and they are 0 when no session is open.

To scale a server with KEDA on the number of open sessions, use this query:

```promql
sum(max by (verified) (toolgate_sessions))
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
