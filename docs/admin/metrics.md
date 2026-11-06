# Metrics

The gateway serves Prometheus metrics at `/metrics`. This page lists each metric, what it means, and an example query.

A metric label is either a fixed set of values or the name of a template that you define. No metric has a label for a user, a session, a workspace, a tool, a request, or an error text. The audit log holds that detail.

## Series budget

A series is one metric with one set of label values. A metrics backend uses memory for each series, so toolgate keeps the number small and stable.

Each replica exports at most `300 + 10 × T` series, where `T` is the number of templates. A test in the toolgate source checks this limit and checks that every label value belongs to a fixed set.

## Leader-only metrics

Only the replica that holds the leader Lease sets these metrics. The other replicas export no series for them, so each series exists once. Query them without a `sum` across replicas, or use `max`.

- `toolgate_sessions`
- `toolgate_instances`
- `toolgate_instance_namespaces_stuck`

When the leader changes, the new leader sets the metrics within 30 seconds.

## Sessions and instances

`toolgate_sessions{verified}` is a gauge of the sessions that have not ended. The label `verified` is `true` when the gateway attested the workspace of the session, and `false` otherwise. Both series are always present on the leader, and they are 0 when no session is open.

To scale a server with KEDA on the number of open sessions, use this query:

```promql
sum(max by (verified) (toolgate_sessions))
```

`toolgate_instances{scope, status}` is a gauge of the instances that are not stopped. The label `scope` is `per-session`, `per-user` or `shared`. The label `status` is `pending`, `starting`, `ready`, `failed` or `stopping`.

```promql
max by (status) (toolgate_instances{scope="shared"})
```

`toolgate_instance_namespaces_stuck` is a gauge of the instance namespaces that stay in the `Terminating` state for more than 10 minutes. See [Instance stops and cleanup](instances.md).

```promql
max(toolgate_instance_namespaces_stuck) > 0
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
