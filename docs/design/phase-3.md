# Phase 3 design

This document decides the behaviour `SPEC.md` leaves open for phase 3. It adds to `docs/design/phase-1.md` and `docs/design/phase-2.md`, which still hold unless a section here says otherwise. Where they disagree, stop and ask (`HANDOFF: question`). Workers implement it and never change its rules. The operator and owner change it.

The target is `docs/acceptance/phase-3.md`: AC-3-05 to AC-3-07 (telemetry), AC-3-09 (SPIFFE), AC-3-10 and AC-3-11 (CLI), AC-3-13 and AC-3-14 (instance health), AC-3-15 (migrations) and AC-3-12 (release).

## 1. Who does what

Phase 1's split stays (phase 1 §1): request handling runs on every replica and never touches Kubernetes objects; the runtime reconciler runs only on the Lease holder.

**New request-handling work**, on every replica:
- OTLP ingest at `/v1/traces`, `/v1/metrics` and `/v1/logs` (§3). It writes nothing to PostgreSQL.
- SPIFFE JWT-SVID verification, and fetching each trust domain's bundle (§4). Each replica keeps its own bundles.
- `GET /api/v1/sessions` and `GET /api/v1/audit/tail` (§5).

**New reconciler work**, on the leader only:
- running each instance's custom health check, next to phase 1's `ping` (§6);
- creating the audit partitions when it gets the Lease and every 24 h after that (§7).

**New out-of-process work:** the chart's migration hook Job runs `toolgate migrate` before the gateway pods of a release start, when the admin turns it on (§7).

**Notifications.** None are new. A health-check failure is phase 1's `ready` → `failed` write with its `instance:<id>` notification, so the fan-out of `list_changed` is unchanged.

## 2. State

One additive migration (phase 1 §7, "Database migrations"):
- `instances.health_check jsonb`, null when the template has no custom check. The reconciler writes it in the `pending` → `starting` write, from the template generation it renders, with `http.service` rendered (§6). An instance keeps the check it started with, like its generation (`SPEC.md` §9, "Template changes").
- The function `toolgate_ensure_audit_partitions()` (§7), owned by the role that runs the migrations.

No new tables. The audit tail and the sessions list read `audit_events`, `sessions` and `session_servers` with their existing indexes: `(principal, occurred_at)` and the primary key `(occurred_at, id)` for the tail, and the `(principal, session_id)` unique index for sessions.

## 3. OTLP ingest

AC-3-05, AC-3-06 and AC-3-07.

### Routes

`POST /v1/traces`, `POST /v1/metrics` and `POST /v1/logs` on the gateway's main listener, next to `/mcp`. They exist only when the setting `otlpIngest` is `true`, which needs `otlpEndpoint`. Otherwise they answer 404.

- **Authentication.** `auth.Middleware` with `requireSession` false, like `/api/v1/`. Any verified identity may send telemetry about itself. A refusal is a 401 and an `auth.refuse` event, rate-limited as in phase 1. Ingest isn't an MCP operation, so it has no policy decision and writes no audit event.
- **No tracing of ingest.** The `/v1/*` routes are left out of the gateway's `otelhttp` handler. Otherwise each export would make gateway spans, which would be exported, and so on.
- **Encodings.** `Content-Type: application/x-protobuf` or `application/json`; anything else gets 415. `Content-Encoding: gzip` or none; anything else gets 415. The response uses the request's encoding.
- **Decoding** uses the collector's `pdata` packages: `go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp`, `pmetric/pmetricotlp` and `plog/plogotlp`. Each `ExportRequest` is read with `UnmarshalProto` or `UnmarshalJSON`. A body that doesn't decode gets 400.

### Identity stamping

The ingest walks every resource, scope and record of the request.

**Removed everywhere** (resource, scope, span, span event, span link, metric data point, exemplar filtered attributes, log record): every attribute whose key is `enduser.*`, `user.*` or `workspace.*`. This includes Claude Code's `user.id`, `user.email`, `user.account_uuid` and any `OTEL_RESOURCE_ATTRIBUTES` the user set.

**Set on every span, metric data point and log record:**
- `user.id` and `enduser.id`: the principal (`sub`, `system:serviceaccount:…`, or the SPIFFE ID);
- `user.email`: the verified email, only when the identity has one;
- `workspace.id`: the identity's workspace (`claimed/…` for users, as everywhere).

They go on records, not on the resource, because Prometheus and most metrics backends turn data-point attributes into labels and keep resource attributes in `target_info`. So the AC-3-06 query needs no join.

**`session.id` is kept as Claude Code sends it.** Both it and `X-Toolgate-Session` are claimed (`SPEC.md` §6), so replacing one with the other verifies nothing. Claude Code's value changes at once on `/clear` and `--resume`, while the headers helper's output can lag by its refresh interval. In normal use it equals the audit log's session, because the SessionStart hook writes that same ID. Every other attribute passes through unchanged.

### Forwarding

- The stamped request is encoded with `MarshalProto` and sent with `POST` to `otlpEndpoint` + `/v1/<signal>`, `Content-Type: application/x-protobuf`, from one shared `net/http` client: 10 s timeout, no redirects, pooled connections. The same endpoint gets the gateway's own traces (phase 1), so AC-3-07's spans meet in one backend.
- **Why not a collector exporter:** the collector's `otlphttp` exporter needs the collector's component runtime, and its queue would hold data in memory after the request returns. The OTel Go SDK exporters take SDK spans, not `pdata`, so they'd need a hand-written conversion. With `pdata` doing the encoding, the gateway writes no protocol code, and a synchronous forward gives clean backpressure.
- **Nothing is buffered or retried by the gateway.** The client's exporter retries, as OTLP/HTTP intends.

| Backend answer | Gateway answers |
|---|---|
| 2xx | 200, with the backend's `ExportResponse` (its partial success included) decoded with `pdata` and encoded in the client's encoding |
| 400 | 400, "The OTLP backend refused the data." Not retryable |
| 429, 502, 503, 504, a timeout or a connection error | 503, with the backend's `Retry-After` if it is 1 to 60 s, else `Retry-After: 5` |
| anything else | 502. Not retryable by OTLP |

### Limits

- Request body at most 4 MiB as sent (phase 1's limit), and at most 4 MiB after gzip, read through `http.MaxBytesReader` and `io.LimitReader`. Over that gets 413.
- At most 4 ingest requests decode or forward at once per replica. Another gets 503 with `Retry-After: 2` at once. This bounds memory: decoded `pdata` is a few times the wire size, and the gateway's default memory limit is 256 MiB.
- Metrics: `toolgate_otlp_ingest_requests_total{signal,code}`.

### Claude Code settings

A Claude Code plugin can't set `env` or `otelHeadersHelper`: Claude Code 2.1.318 reads only `agent` and `subagentStatusLine` from a plugin's `settings.json`. So the settings go in a `SettingsProfile`, the existing way to hand settings to clients (`SPEC.md` §13). Admins choose who gets it. `docs/admin/telemetry.md` gives this profile:

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

- Claude Code reads `env` when it starts, so a profile change takes effect in the next session. The docs say so.
- The helper runs every 4 minutes (Claude Code's default is 29). Each run prints a fresh token, so an IdP access token that lives at least 5 minutes never expires between runs. The docs state that requirement.
- `cumulative` temporality, because Prometheus' OTLP receiver needs it.

**`toolgate otel-headers`** prints `{"Authorization": "Bearer …", "X-Toolgate-Workspace": "…"}`, with the token from the same credential source as `toolgate headers` (§4, "Client side") and the workspace from phase 1's rules. It doesn't wait for a session file, because ingest doesn't use the session header. It exits non-zero without printing headers when it has no credential.

### Test backend and the usage query

The e2e cluster runs `mirror.gcr.io/grafana/otel-lgtm` (Google's Docker Hub mirror, so CI isn't subject to Docker Hub's pull limits), pinned by digest like the other e2e images: one pod with an OpenTelemetry Collector, Prometheus (OTLP receiver on), Tempo and Loki. The gateway's `otlpEndpoint` points at its collector. Tests read the backends through their own query APIs: PromQL, Tempo's trace API and LogQL. One image gives all three signals real query languages, and the documented query runs on real Prometheus.

**Test input.** CI can't make model calls, so a developer records Claude Code's OTLP exports once, with the profile above and a real session that calls a toolgate tool, and the recording is stored in `test/fixtures/otlp/`. `docs/dev/` records how, like the session-ID evidence. The AC-3-05 test replays it through the gateway with the headers `toolgate otel-headers` prints, including client-set `user.*`, `enduser.id` and `workspace.id` values. It checks that a metric, a log event and a span reach the backend with the verified principal and workspace, and none of the client's values.

**AC-3-06 query.** Prometheus' default OTLP translation names the metrics `claude_code_token_usage_tokens_total` and `claude_code_cost_usage_USD_total` and the labels `user_id`, `user_email`, `workspace_id` and `session_id`. `docs/admin/telemetry.md` shows:

```promql
sum by (user_id, user_email, workspace_id, session_id, type) (increase(claude_code_token_usage_tokens_total[1d]))
sum by (user_id, user_email, workspace_id, session_id) (increase(claude_code_cost_usage_USD_total[1d]))
```

The AC-3-06 test runs exactly the queries in the page against the test backend and checks the sums against the fixture. If Prometheus names them differently, the test and the page change together.

**AC-3-07** is manual: the owner opens one tool call's trace in their backend. Claude Code sends `traceparent` on MCP requests and the gateway's spans are children of it (phase 1 §7, "Traces"), so the trace holds Claude Code's tool span and the gateway's `toolgate.policy` and `toolgate.forward` spans.

## 4. SPIFFE attestation

AC-3-09. It answers `SPEC.md` §21 question 2: a Coder workspace with a SPIRE agent signs in with a JWT-SVID.

### Configuration

```yaml
spiffeTrustDomains:
  - name: coder.example.com
    bundleEndpointURL: https://spire.example.com:8443   # or jwksURL
    caFile: /etc/toolgate/spire-ca.pem                  # optional
```

- YAML only, like `resolvers`. The chart sets it from `config.spiffe.trustDomains`.
- Each item has a `name` (a trust domain, parsed with `spiffeid.TrustDomainFromString`) and exactly one of:
  - `bundleEndpointURL`: a SPIFFE bundle endpoint with the `https_web` profile, fetched with `federation.FetchBundle` and `federation.WithWebPKIRoots`;
  - `jwksURL`: a JWKS document, such as SPIRE's OIDC discovery provider's, fetched over HTTPS and parsed with `jwtbundle.Parse`.
- Both must be `https://`. `caFile` adds a private CA to the system roots.
- **The list of trust domains is the only gateway-level allow-list.** It's SPIFFE's counterpart of `serviceAccountNamespaces`. There are no ID patterns at gateway level: templates' access lists and rules already decide what each SPIFFE ID can reach. An SVID that matches no template can only send telemetry and read its own audit events and sessions.

### Bundles

- Each replica fetches every trust domain's bundle at start, and then every 5 minutes, or at the bundle's `spiffe_refresh_hint` if that is shorter, but never more often than every 30 s. Bundles go in one `jwtbundle.Set`, which is safe for concurrent use. Responses are read up to 1 MiB.
- **A token whose key ID the bundle lacks** triggers one fetch of that trust domain, at most once every 30 s, and is then verified once more.
- **Fetch failures** are retried with backoff from 1 s to 30 s and logged. The last good bundle keeps working for 24 h after its last successful fetch, then it's dropped, so a stale key can't be trusted for long. Until a trust domain's first fetch succeeds, its SVIDs get 401. Readiness doesn't wait for bundles, so one broken SPIRE server can't take the gateway down.
- Metric: `toolgate_spiffe_bundle_age_seconds{trust_domain}`.

### Verification

- **Routing.** The authenticator reads the token's claims without verifying them, only to pick a verifier, as phase 1 does with the issuer. A `sub` starting with `spiffe://` goes to the SPIFFE verifier when trust domains are configured. Otherwise the phase 1 order holds: the IdP's issuer goes to go-oidc, anything else to TokenReview. Each verifier verifies fully, so a wrong pick fails closed with 401.
- **Check:** `jwtsvid.ParseAndValidate(token, bundles, []string{"toolgate"})`, from `github.com/spiffe/go-spiffe/v2`. It checks the algorithm, the signature against the trust domain's bundle, the expiry and the audience. An SVID from a trust domain that isn't configured has no bundle and gets 401 (AC-3-09).
- Also refused with 401: a SPIFFE ID longer than 2048 bytes (the SPIFFE limit) and an ID with an empty path.
- `auth.refuse` records the reason, such as "SPIFFE trust domain not allowed", and the trust domain, never the token.

### Identity

| Field | Value |
|---|---|
| `principal` | The SPIFFE ID, such as `spiffe://coder.example.com/owner/alice/workspace/dev` |
| `workspace` | The ID's path, with its leading `/`: `/owner/alice/workspace/dev` |
| `attestation` | `attested` |
| `email`, `groups` | none |

- The `X-Toolgate-Workspace` header doesn't change the workspace, as for pods.
- The leading `/` keeps SPIFFE workspaces apart from `k8s/…` and `claimed/…`. Two trust domains can produce the same path. That only matters to rules, and rules only refuse, so a clash can refuse more, never allow more. The principal still differs.
- `${principal.email}` fails to render, as for service accounts.

**Catalog access.** `access` gets one new list, `spiffeIDs`: globs compiled like rule globs (`github.com/gobwas/glob`, no separators, phase 1 §7), at most 64 items of at most 2048 characters each. A pattern that doesn't compile makes the template invalid. A SPIFFE identity matches only `spiffeIDs`. `users`, `groups` and `serviceAccounts` never match it, and `spiffeIDs` never matches a user or a service account, because a user's `sub` is chosen by the IdP and could start with `spiffe://`. Globs, because SPIRE gives each workload its own ID, and listing every Coder workspace by hand wouldn't work.

**Rules** match as for any identity: `principals` against the SPIFFE ID, `workspaces` against the path, `attestation: attested`. Example: `workspaces: ["/owner/*/workspace/scratch-*"]`.

**Environments.** An SVID is `attested`, so `attested` and `opt-in` pass without a grant, exactly as for pods (`SPEC.md` §6).

### Client side

- With `TOOLGATE_SPIFFE_SOCKET` set, for example to `unix:///run/spire/agent.sock`, every client command uses a JWT-SVID as its bearer token instead of the stored sign-in: `headers`, `otel-headers`, `hook session-start`, `servers`, `secret`, `sessions` and `audit tail`.
- The SVID comes from `workloadapi.FetchJWTSVID(ctx, jwtsvid.Params{Audience: "toolgate"}, workloadapi.WithAddr(socket))`, with a 5 s timeout, fresh for each run.
- If it fails, the command prints the error and exits non-zero. It never falls back to the user's sign-in, because that would change the attestation without anyone noticing.
- The gateway URL comes from `TOOLGATE_GATEWAY` when no sign-in is saved. `toolgate headers` doesn't need it, because the plugin's `.mcp.json` holds the URL.
- An explicit variable rather than go-spiffe's `SPIFFE_ENDPOINT_SOCKET`, because that one may be set for other tools, and silently switching a person's sign-in to a workload identity would surprise them.

### Tests

The AC-3-09 e2e test serves a SPIFFE bundle endpoint from the test harness for `allowed.test` (with a test CA from `internal/testutil`), and keys for `other.test` that aren't configured. It signs JWT-SVIDs with `github.com/go-jose/go-jose/v4`, which is already in `go.mod`; signing is test-only. It checks attestation, the workspace, a rule matching that workspace, and a 401 for `other.test`. The client side is tested against a fake Workload API server built on go-spiffe's exported `proto/spiffe/workload` package.

## 5. CLI: sessions and audit tail

AC-3-10 and AC-3-11. Phase 2 added no `/api/v1/` endpoints (phase 2 §9). Phase 3 adds two. Both authenticate with the bearer token and take identity from it alone.

**Admins** in the API are identities whose token `groups` claim holds `adminGroup`, with attestation `user`, checked on every request. Service accounts and SPIFFE IDs are never admins.

### `GET /api/v1/sessions`

- The caller's open sessions: `ended_at IS NULL AND last_seen_at > now() - sessionIdleTimeout`, newest `last_seen_at` first. Only the caller's own, for admins too, as AC-3-11 asks. The web UI already shows admins every session.
- Body: `{"sessions": [{"session", "workspace", "attestation", "createdAt", "lastSeenAt", "servers": [{"name", "enabledAt"}]}], "truncated": false}`. At most 200 sessions. `truncated` says there were more. Each has at most 32 servers (phase 1 §3).
- One query joining `sessions` and `session_servers`.
- `toolgate sessions` prints a table: `SESSION`, `WORKSPACE`, `LAST SEEN`, `SERVERS`.

### `GET /api/v1/audit/tail?after=<key>`

- **Format:** `Content-Type: application/x-ndjson`, one JSON object per line, flushed after each batch. The CLI reads it with `encoding/json`'s `Decoder`. Not SSE: Go has no SSE parser in the standard library or in the tech stack, and NDJSON needs none.
- **Event shape:** `{"key", "occurredAt", "type", "principal", "workspace", "session", "attestation", "server", "tool", "traceId", "requestId", "details"}`. `key` is phase 2's audit key, `<occurred_at in Unix microseconds>-<id>`. Payloads are never streamed.
- **Scope:** a non-admin gets only rows with their own principal. An admin gets all rows. There are no other filters.
- **Start:** without `after`, the last 20 matching events of the past 24 h, then new ones. With `after`, the events after that key, at most 31 days back, like the UI's range. A key older than retention starts at the oldest row.
- **Following:** the handler polls every 2 s for rows after its cursor, ordered by `(occurred_at, id)`, at most 500 per query, querying again at once while a query returns 500.
- **Commit order:** `occurred_at` is the writing transaction's start time, so a row can commit after a newer one. The query reads only rows with `occurred_at <= now() - interval '5 seconds'`. Audit-writing transactions are short, so the 5 s window covers them. A row from a transaction longer than that can be missed by the stream, never by the table. `docs/user/` says the tail is a view and the audit log is the record.
- **Keepalive:** a lone `\n` after 15 s without events. JSON decoders skip whitespace.
- **End:** the server ends the response cleanly when the token expires or after 1 h, whichever is first. The handler extends its write deadline with `http.ResponseController` before each write.
- **Limits:** at most 4 open tails per principal and 64 in total on each replica. More get 429 with "Too many audit tails are open. Close one first."

**`toolgate audit tail [--after KEY] [--json]`** prints one line per event (time, type, principal, workspace, session, server, tool, and the decision or result), or the raw lines with `--json`.
- After a clean end it reconnects at once with a fresh token and the last key. After an error it reconnects with backoff from 1 s to 30 s. So a replica restart or the 1 h limit loses nothing beyond the window above.
- Ctrl-C stops it.

### OpenAPI

Phase 2 §9 chose `github.com/swaggest/openapi-go` with its `openapi31` reflector, but it isn't in `go.mod` yet. The first issue that adds a phase 3 endpoint adds the generator, as phase 2 §9 describes, for every `/api/v1/` route. That means `docs/dev/openapi.yaml`, `make check-generate`, and the test that the document's operations are exactly the mux's `/api/v1/` routes. The tail's 200 response is `application/x-ndjson` with the event as its item schema. The `/v1/*` OTLP routes aren't part of the API and aren't in the document.

## 6. Instance health

### Custom health checks

AC-3-13. Only admins write templates, so only admins define checks.

```yaml
spec:
  healthCheck:
    interval: 30s          # 10s to 10m, default 30s
    timeout: 5s            # 1s to 60s, default 5s, less than interval
    failureThreshold: 3    # 1 to 10, default 3
    http:                  # exactly one of http and tool
      service: db-proxy    # rendered and validated like endpoint.service
      port: 8080
      path: /healthz       # starts with /, at most 1024 characters, not rendered
      expectedStatus: 200  # 100 to 599, default 200
    tool:
      name: ping_database      # the tool's own name
      arguments: {}            # a JSON object, at most 4 KiB, not rendered
```

- The CRD checks these with CEL. The catalog checks them again, and also checks the shared-namespace rule for `http.service` (phase 1 §7: it must contain `${instance.id}`). A bad check makes the template invalid, with the reason in `Valid`.
- **HTTP check:** `GET http://<service>.<instance namespace>.svc:<port><path>` with a client that doesn't follow redirects, uses no proxy and reads at most 64 KiB of the body before discarding it. It passes when the status equals `expectedStatus`. The instance namespace already admits the gateway's pods on every port (phase 1 §6).
- **Tool check:** a `tools/call` on the reconciler's monitoring session (phase 1 §3, "Upstream tool changes"), with the response bounded to 1 MiB through `upstreamlimit`. It passes when the call returns a result with `isError` false. The tool must be in the instance's tool list. This is checked with the tool list checks at `starting` → `ready` and on every list change (phase 1 §3), so a missing tool fails the instance with "Health check tool `ping_database` isn't in the server's tool list."
- **Not a client call.** A tool check comes from the admin's template, not from a client. It runs no policy decision and writes no audit event per call. Its result is discarded and never logged. So principle 1, which covers paths from clients, isn't touched. Only the failure it causes is audited (`instance.fail`).

**Running.** The leader's `Monitor` runs the check in the same goroutine that runs phase 1's `ping`, each on its own ticker and with its own failure counter.
- The first check runs when monitoring starts, then every `interval`. Each check gets a context with `timeout`.
- A pass resets the counter. At `failureThreshold` failures in a row, the monitor marks the row `ready` → `failed` (phase 1's compare-and-set), with the reason, and stops.
- The check doesn't gate `starting` → `ready`. AC-3-13 is about failures after `ready`, and phase 1's readiness rules stay as they are.

**Bound.** A target that turns unhealthy is seen by a check that starts within one `interval`. So the `failureThreshold`-th failing check starts within `interval × failureThreshold` and ends at most `timeout` later. The AC-3-13 test uses a target that fails fast (HTTP 503), and allows a few seconds for the write and the fan-out.

**Reasons** name the check and never a tool result:
- "Health check failed 3 times in a row: GET http://db-proxy:8080/healthz answered 503, expected 200."
- "Health check failed 3 times in a row: tool `ping_database` returned an error."
- "Health check failed 3 times in a row: tool `ping_database` didn't answer within 5s."

They reach the model through `toolgate_server_status` and phase 1's `not-ready` refusals, and the audit log through `instance.fail`.

**After failure,** exactly as phase 1 row 12: the instance is torn down, sessions keep the server enabled with no tools and get `list_changed`, and the next enable inserts a new instance. There's no automatic restart. A check that would pass again doesn't bring a failed row back, because a row never goes back to an earlier status (phase 1 §2).

A template without `healthCheck` behaves as in phase 2: `health_check` is null and only the `ping` runs.

### Object health after `ready`

AC-3-14. This is phase 2 §7's readiness rule applied to `ready` instances, which `Starter.checkReady` already does on every step.
- On each 30 s pass, and on each notification for the row, the reconciler judges every object of a `ready` instance. It uses kstatus, with phase 2's rule first: a `Ready` condition that has been `False` for at least 60 s at its object's current generation (read from the condition's own `observedGeneration`) counts as failed.
- A failed object moves the row `ready` → `failed` with "`<Kind>` `<name>` failed: `<Ready message>`".
- **Bound:** within 60 s of the condition turning `False`, plus up to one 30 s pass.
- Objects whose `Ready` condition has no `observedGeneration`, such as Pods, are judged by kstatus alone, as in phase 2. A pod that only restarts is left to the health checks.
- The AC-3-14 e2e test points a `ready` HelmRelease instance's `OCIRepository` at a tag that doesn't exist. Its `Ready` turns `False` at the new generation, and the instance must fail with that message. The work for this scenario is the test, plus any defect it finds.

## 7. Migrations

AC-3-15.

### `toolgate migrate` and `migrateOnStart`

- **`toolgate migrate [--config FILE]`** reads only `databaseURL`, runs `store.Migrate` (goose's provider with the PostgreSQL session locker, phase 1 §7), calls `toolgate_ensure_audit_partitions()`, and exits 0. It exits 1 on failure. It ignores the other settings, so a Job needs no OIDC or Kubernetes configuration.
- **`migrateOnStart`** (`TOOLGATE_MIGRATE_ON_START`, default `true`):
  - With `true`, `serve` works as in phase 1.
  - With `false`, `serve` doesn't migrate. Before it opens its listeners, it reads `store.SchemaVersions`. If the database is older than the binary, it logs "The database schema is at version 3. This gateway needs version 4. Run `toolgate migrate`, or let the chart's migration Job run." and exits 1.
  - Kubernetes then restarts it with backoff, so the pod stays unready, and the message shows in its logs and in `kubectl describe` (the chart sets `terminationMessagePolicy: FallbackToLogsOnError`).
  - Exiting matches phase 1's failed-migration behaviour, and nothing waits in a half-started state.
  - A newer database (a rollback) is only logged, as in phase 1.

### Audit partitions

**A defect today.** `store.EnsureAuditPartitions` runs only when `serve` starts, and creates this month's and next month's partitions. A gateway that runs past the end of next month without a restart has no partition, so audit inserts fail and every tool call is refused. And a role with data rights only can't create partitions at all.

**The fix,** in this phase's migration: `toolgate_ensure_audit_partitions()`.
- It's a PL/pgSQL function, `SECURITY DEFINER` and owned by the migrating role, which owns `audit_events`. It takes no arguments and creates the partitions for the month of `now()` and the next one, under the existing `partitionLockKey` advisory lock.
- Its `search_path` is pinned to the migration's schema and `pg_temp`, set with `ALTER FUNCTION … SET search_path` in a `DO` block, because the schema is known only at run time. Partition names are built with `format('%I', …)` from dates alone.
- `EXECUTE` is revoked from `PUBLIC`.
- `serve` calls it at start, in both modes, and the leader calls it when it gets the Lease and every 24 h. `store.EnsureAuditPartitions` becomes that call.
- This is PostgreSQL's standard way to give a role one narrow privilege. It isn't new cryptography or protocol code.

### Database rights

`docs/admin/` gives the SQL for both modes. For the split mode, it is:

```sql
CREATE ROLE toolgate_migrator LOGIN;
CREATE ROLE toolgate_app LOGIN;
CREATE SCHEMA toolgate AUTHORIZATION toolgate_migrator;
ALTER ROLE toolgate_migrator SET search_path = toolgate;
ALTER ROLE toolgate_app SET search_path = toolgate;
GRANT USAGE ON SCHEMA toolgate TO toolgate_app;
ALTER DEFAULT PRIVILEGES FOR ROLE toolgate_migrator IN SCHEMA toolgate
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO toolgate_app;
ALTER DEFAULT PRIVILEGES FOR ROLE toolgate_migrator IN SCHEMA toolgate
  GRANT USAGE, SELECT ON SEQUENCES TO toolgate_app;
ALTER DEFAULT PRIVILEGES FOR ROLE toolgate_migrator IN SCHEMA toolgate
  GRANT EXECUTE ON FUNCTIONS TO toolgate_app;
```

- For an existing database, it adds the matching `GRANT … ON ALL TABLES` / `ALL SEQUENCES` / `FUNCTION` statements, plus `REASSIGN OWNED` from the old single role to the migrator.
- The app role reads `goose_db_version` for the version check, which the table grant covers.
- `LISTEN`, `NOTIFY` and advisory locks need no grant.

### Chart

Values, under `config.database`:

| Value | Default | Meaning |
|---|---|---|
| `migrateOnStart` | `true` | Passed to the gateway |
| `migrationSecret` | `""` | Existing Secret with the migrating role's connection string. The Job renders only when this is set and `migrateOnStart` is `false` |
| `migrationSecretKey` | `url` | Its key |
| `migrationEgress` | `[]` | NetworkPolicy egress rules that reach PostgreSQL. Required when the Job renders: the chart fails with a message naming the value |

With `migrateOnStart: false` and no `migrationSecret`, nothing runs migrations. The admin runs `toolgate migrate` some other way, and the docs say so.

**Hook objects**, each annotated `helm.sh/hook: pre-install,pre-upgrade` and `helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded`:

| Object | Weight | Notes |
|---|---|---|
| ServiceAccount `<fullname>-migrate` | `-20` | No RBAC. The release's ServiceAccount doesn't exist yet at pre-install |
| NetworkPolicy `<fullname>-migrate` | `-10` | Selects only the Job's pods. No ingress. Egress to port 53 (UDP and TCP), as instance namespaces allow DNS (phase 1), plus `migrationEgress` |
| Job `<fullname>-migrate` | `0` | `toolgate migrate`. `restartPolicy: Never`, `backoffLimit: 2`, `activeDeadlineSeconds: 240`, `automountServiceAccountToken: false`, the gateway's pod and container security context, `TOOLGATE_DATABASE_URL` from `migrationSecret` |

- The Job is a bjw-s `common` controller named `migrate` (`type: job`), enabled by the condition above, so admins override it under `resources.controllers.migrate` and mount client certificates (CloudNativePG) with `resources.persistence.<name>.advancedMounts.migrate`.
- The ServiceAccount and NetworkPolicy use `common`'s own types where they take annotations, and `rawResources` otherwise. The worker checks the pinned `common` 5.3.0.
- Helm and Flux's helm-controller run pre-install and pre-upgrade hooks before they apply the release's other objects, and wait for the Job. So the new gateway pods start only after the migrations. A failed Job fails the install or upgrade, and the old pods keep serving.
- The 240 s deadline fits inside helm-controller's default 5 minute timeout, and the docs say a longer `spec.timeout` needs a longer deadline.
- Helm hooks don't run with `helm template | kubectl apply`. The docs say the split mode needs Helm or Flux.

### Tests

**AC-3-15 e2e.** The harness creates the two roles, the schema and the default privileges above as the PostgreSQL superuser through `kubectl exec` with `psql`, and stores both connection strings in the `dependencies` Secret. The test then:
1. installs the chart with `migrateOnStart: false`, `existingSecret` as the app role and `migrationSecret` as the migrator;
2. checks that the Job completed before the first gateway pod was created, and that the schema version equals the binary's;
3. checks that `CREATE TABLE` as the app role fails with "permission denied", and that an enable and a call work, which writes audit rows;
4. runs `helm upgrade` and checks that the hook ran again before the new pods started.

The default values stay covered by the existing e2e tests, and a chart test checks that they render no hook objects. An integration test covers the version check against an older database.

## 8. Interaction table

Phase 1's rows (phase 1 §4) and phase 2's rows (phase 2 §10) still hold. Rows continue the numbering. Each row is one event combined with one operation already in flight, and each row's covering test proves its rule.

| # | Event | In flight | Rule | Test |
|---|---|---|---|---|
| 74 | Access token expires between `otelHeadersHelper` runs | exports | Exports get 401 and Claude Code drops them until the helper runs again. A 4 min refresh and a token of at least 5 min prevent it (§3). | unit |
| 75 | Backend slow or down | ingest requests | 503 with `Retry-After` after at most 10 s. Nothing is buffered; the client retries. | integration |
| 76 | Fifth concurrent ingest request on a replica | 4 decoding | 503 with `Retry-After: 2` at once. | unit |
| 77 | Client sends `user.*`, `enduser.*` or `workspace.*` at any level | ingest | Removed everywhere; the verified values are set on each record. `session.id` passes through. | AC-3-05 |
| 78 | Replica dies after forwarding, before answering | ingest request | The client retries, so the backend may get the batch twice. OTLP delivery is at least once. | unit |
| 79 | Bundle fetch fails | SVID sign-ins | The last good bundle verifies for 24 h after its last successful fetch, then that trust domain's SVIDs get 401. | unit |
| 80 | SPIRE rotates its key | SVID signed with the new key | Unknown key ID: one fetch (at most every 30 s per trust domain), then verify again. | unit |
| 81 | SVID from a trust domain that isn't configured | — | No bundle, so 401 and `auth.refuse`. | AC-3-09 |
| 82 | Token with a `spiffe://` subject issued by the IdP | — | Routed to the SPIFFE verifier and refused with 401. A wrong route never lets a token through. | unit |
| 83 | A different SVID on an existing MCP session | open stream | The session's binding doesn't match, so 404 (phase 1 row 33). | unit |
| 84 | Trust domain removed from the configuration | open sessions | Settings are read at start. After the restart, its SVIDs get 401. | unit |
| 85 | Audit row commits after a newer row | tail streaming | The 5 s window lets it in, in key order. A transaction longer than that can be missed by the stream only. | integration |
| 86 | Replica restart, or the 1 h or token end | tail streaming | The CLI reconnects with the last key and gets the next rows, with no duplicates. | integration |
| 87 | Admin leaves the admin group | admin's tail open | The stream ends when the token expires, at most 1 h. The new token's groups decide the next stream. | unit |
| 88 | Retention deletes rows | tail with an old `after` | Starts at the oldest remaining row. | unit |
| 89 | Session ends | `toolgate sessions` | One query is one snapshot. A session idle past `sessionIdleTimeout` is left out even before the 30 s pass ends it. | unit |
| 90 | Custom check reaches `failureThreshold` | calls in flight | As phase 1 row 12: in-flight calls get the upstream result or error, later calls `not-ready` with the check's reason, `list_changed`, and the next enable inserts a new instance. | AC-3-13 |
| 91 | Check passes after failures below the threshold | monitoring | The counter resets. | unit |
| 92 | Leader change | failures being counted | The new leader's monitor starts at 0, so the bound restarts from the change. | unit |
| 93 | Template's `healthCheck` changes | instance running | The instance keeps the check in its `health_check` column. New instances get the new one. | unit |
| 94 | Check and `ping` both reach their thresholds | each other | The first compare-and-set from `ready` wins. The other matches no row and stops. | unit |
| 95 | Restart, replacement or idle stop | check in flight | The late failure's compare-and-set on the old row matches nothing. | unit |
| 96 | Upstream drops the check's tool | `list_changed` | The tool list checks fail the instance with the missing-tool reason. | unit |
| 97 | Object's `Ready` turns `False` after `ready` | sessions using it | After 60 s at its generation, the next pass fails the instance with that object's message. | AC-3-14 |
| 98 | `Ready` is `False` for less than 60 s, then `True` | instance `ready` | Stays `ready`. | unit |
| 99 | Hook Job fails | `helm upgrade` or Flux upgrade | The upgrade fails before the new objects apply, and the old pods keep serving. | e2e |
| 100 | Gateway with `migrateOnStart: false` starts on an older schema | — | Exits 1 naming both versions. It stays unready until the migrations run. | integration |
| 101 | Migration Job, and running gateways creating partitions | each other | The function takes `partitionLockKey`. `CREATE TABLE IF NOT EXISTS` makes the loser a no-op. | integration |
| 102 | Month boundary | long-running gateway | The leader's 24 h call has already created next month's partition. | integration |

Any combination not in this table is a design gap: hand back `HANDOFF: question` with the case.

## 9. Settled choices

- **Settings** (flat, in the generated reference):
  - `otlpIngest` (default `false`; needs `otlpEndpoint`);
  - `spiffeTrustDomains` (YAML only);
  - `migrateOnStart` (default `true`).

  The chart sets them from `config.otlpIngest`, `config.spiffe.trustDomains` and `config.database.migrateOnStart`, and mounts each `caFile` through `resources.persistence`.
- **Client environment:** `TOOLGATE_SPIFFE_SOCKET` and `TOOLGATE_GATEWAY` (§4).
- **CRD fields:** `access.spiffeIDs` (§4) and `healthCheck` (§6) on `MCPServerTemplate`.
- **Time bounds:**
  - OTLP forward 10 s;
  - bundle refresh 5 min (from 30 s with a refresh hint); a stale bundle trusted for 24 h; a Workload API fetch 5 s;
  - tail poll 2 s, commit window 5 s, keepalive 15 s, stream at most 1 h or the token's expiry; CLI reconnect backoff 1 to 30 s;
  - health check interval 10 s to 10 min (default 30 s), timeout 1 s to 60 s (default 5 s), threshold 1 to 10 (default 3);
  - partitions created at start, at Lease acquisition and every 24 h;
  - migration Job deadline 240 s.
- **Limits:**
  - OTLP body 4 MiB, sent and decompressed; 4 ingest requests at once per replica;
  - bundle and JWKS responses 1 MiB;
  - SPIFFE ID 2048 bytes; `spiffeIDs` 64 items;
  - sessions list 200;
  - tail backlog 20 events from the past 24 h, `after` at most 31 days back, 500 rows a query; 4 tails per principal and 64 per replica;
  - health check: HTTP body 64 KiB read, tool response 1 MiB, arguments 4 KiB, path 1024 characters.
- **Audit events:** none are new. Health failures are `instance.fail` with the check's reason.
- **Libraries:**
  - `go.opentelemetry.io/collector/pdata` (the `ptraceotlp`, `pmetricotlp` and `plogotlp` packages);
  - `github.com/spiffe/go-spiffe/v2` (`jwtsvid`, `jwtbundle`, `federation`, `spiffeid`, `workloadapi`, and `proto/spiffe/workload` in tests);
  - `github.com/swaggest/openapi-go` (phase 2 §9);
  - `encoding/json` for NDJSON; `net/http` for forwarding and HTTP checks;
  - the go-sdk's `CallTool` on the monitoring session.

  No new cryptography: signatures are checked by go-spiffe, and test SVIDs are signed by go-jose.
- **Test backend:** `mirror.gcr.io/grafana/otel-lgtm`, pinned by digest (§3).
- **Token refresh is serialized.** `toolgate headers` and `toolgate otel-headers` both refresh the IdP token. The token store takes an exclusive file lock (`github.com/gofrs/flock`) on a lock file next to the token file before it reads, refreshes and writes. A process that waits for the lock re-reads the token afterwards and uses it if it's still valid. So two helpers never send the same refresh token, and an IdP that rotates refresh tokens and detects reuse doesn't sign the user out. A unit test runs two refreshes at once and expects one IdP call.
- **`SPEC.md` updates** in the first implementing PRs:
  - §9: `access.spiffeIDs` and `healthCheck`;
  - §12: ingest stamping, and that `session.id` is kept;
  - §13: `toolgate migrate`, `toolgate otel-headers`' output and `TOOLGATE_SPIFFE_SOCKET`;
  - §15: the `instances` column and the partition function;
  - §21 question 2: answered by SPIFFE.
- **Errors to the model** follow phase 1: short sentences naming what to do next.

## Decisions on the draft's open questions

The operator decided these on the owner's behalf. The owner is away for phase 3, and the decisions are logged on the decision issue.

1. **Prompt and tool content in telemetry:** the ingest passes through what the client sends. Whether Claude Code sends prompts and tool content is the admin's choice, through the `SettingsProfile` that sets `OTEL_LOG_*`. The example profile leaves them off, and `docs/admin/` warns that turning them on sends user content to the backend.
2. **SPIFFE identity is the workload:** accepted. An SVID sign-in is attested as the workload. Whether a workspace uses it is the admin's choice, through `TOOLGATE_SPIFFE_SOCKET`. Rules and templates name SPIFFE IDs (`access.spiffeIDs`). Usage per person is read from the ID's path.
