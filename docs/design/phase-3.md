# Phase 3 design

This document decides the behaviour `SPEC.md` leaves open for phase 3. It adds to `docs/design/phase-1.md` and `docs/design/phase-2.md`, which still hold unless a section here says otherwise. Where they disagree, stop and ask (`HANDOFF: question`). Workers implement it and never change its rules. The operator and owner change it.

The target is `docs/acceptance/phase-3.md`: AC-3-05 to AC-3-07 (telemetry), AC-3-09 (SPIFFE), AC-3-10 and AC-3-11 (CLI), AC-3-13 and AC-3-14 (instance health), AC-3-15 (migrations), AC-3-16 to AC-3-21 (workspace and session scoping) and AC-3-12 (release).

## 1. Who does what

Phase 1's split stays (phase 1 §1): request handling runs on every replica and never touches Kubernetes objects; the runtime reconciler runs only on the Lease holder.

**New request-handling work**, on every replica:
- OTLP ingest at `/v1/traces`, `/v1/metrics` and `/v1/logs` (§3). It writes nothing to PostgreSQL.
- SPIFFE JWT-SVID verification, and fetching each trust domain's bundle (§4). Each replica keeps its own bundles.
- `GET /api/v1/sessions` and `GET /api/v1/audit/tail` (§5).
- verifying `X-Toolgate-Attestation` and binding it to the user (§10), which reads pods like phase 1's pod sign-in;
- the scope step of every decision, the Workspaces page, `/api/v1/workspaces`, and session additions (§10).

**New reconciler work**, on the leader only:
- running each instance's custom health check, next to phase 1's `ping`, and repairing unhealthy instances (§6);
- creating the audit partitions when it gets the Lease and every 24 h after that (§7).

**New out-of-process work:** the chart's migration hook Job runs `toolgate migrate` before the gateway pods of a release start, when the admin turns it on (§7).

**Notifications.** §3 to §7 add none. A health-check failure is phase 1's `ready` → `failed` write with its `instance:<id>` notification, so the fan-out of `list_changed` is unchanged. §10 adds `scope:<user_scopes.id>`, and its access requests use `approval:<id>`.

## 2. State

One additive migration (phase 1 §7, "Database migrations"):
- `instances.health_check jsonb`, null when the template has no custom check. The reconciler writes it in the `pending` → `starting` write, from the template generation it renders, with `http.service` rendered (§6). An instance keeps the check it started with, like its generation (`SPEC.md` §9, "Template changes").
- The function `toolgate_ensure_audit_partitions()` (§7), owned by the role that runs the migrations.

§10 adds its own tables in a second additive migration (§10, "State"). Otherwise there are no new tables. The audit tail and the sessions list read `audit_events`, `sessions` and `session_servers` with their existing indexes: `(principal, occurred_at)` and the primary key `(occurred_at, id)` for the tail, and the `(principal, session_id)` unique index for sessions.

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

**Removed everywhere** (resource, scope, span, span event, span link, metric data point, exemplar filtered attributes, log record): every attribute whose normalized key starts with `user_`, `enduser_` or `workspace_`. To normalize a key, replace every character outside `[A-Za-z0-9]` with `_`, then lower the case. So `user.id`, `user_id`, `User-Id` and `workspace-id` are all removed. This includes Claude Code's `user.id`, `user.email`, `user.account_uuid` and any `OTEL_RESOURCE_ATTRIBUTES` the user set.
- **Why normalize:** Prometheus' OTLP receiver turns every character outside `[A-Za-z0-9_]` into `_`, and joins the values of attributes that map to the same label with `;`. Without the normalized test, a client could send `user_id="carol"`, and Prometheus would store `user_id="bob;carol"` next to the verified `user.id`.

**Set on every span, metric data point and log record:**
- `user.id` and `enduser.id`: the principal (`sub`, `system:serviceaccount:…`, or the SPIFFE ID);
- `user.email`: the verified email, only when the identity has one;
- `workspace.id`: the identity's workspace (`claimed/…` for users, as everywhere).

They go on records, not on the resource, because Prometheus and most metrics backends turn data-point attributes into labels and keep resource attributes in `target_info`. So the AC-3-06 query needs no join.

**`session.id` is kept as Claude Code sends it.** Both it and `X-Toolgate-Session` are claimed (`SPEC.md` §6), so replacing one with the other verifies nothing. Claude Code's value changes at once on `/clear` and `--resume`, while the headers helper's output can lag by its refresh interval. In normal use it equals the audit log's session, because the SessionStart hook writes that same ID. Every other attribute passes through unchanged, except the content attributes below.

**Content attributes are removed by default.** Users can set `OTEL_LOG_USER_PROMPTS`, `OTEL_LOG_TOOL_DETAILS` or `OTEL_LOG_TOOL_CONTENT` in their own settings or shell. So the ingest removes what those variables add, unless the setting `telemetry.allowContent` is `true` (default `false`).
- **Removed span events and log records:** `tool.output` span events, and `claude_code.system_prompt` log records.
- **Removed attributes**, on every span, span event and log record: `prompt`, `prompt_text`, `user_prompt`, `response`, `response.model_output`, `new_context`, `system_reminders`, `system_prompt_preview`, `user_system_prompt`, `tool_parameters`, `tool_input`, `full_command`, `file_path`, `bash_command`, `diff`, `content`, `output`, `hook_definitions` and `hook_matcher`. Also `error`, but only on `claude_code.tool_result` log records, `claude_code.tool.execution` spans and MCP server connection log records: with `OTEL_LOG_TOOL_DETAILS` it holds the full error message there, which can quote tool content. Elsewhere, such as on `claude_code.api_error`, `error` is kept.
- **Kept:** attributes that only name things, such as `skill_name`, `subagent_type`, `workflow.name`, `server_name`, `plugin.name`, `marketplace.name`, `agent_type`, `command_name`, `classifier_category`, `vcs.ref.head.*`, and the metric labels `agent.name`, `skill.name`, `plugin.name`, `mcp_server.name` and `mcp_tool.name`. They're names, not prompts, arguments or results.
- **Where the list comes from:** Claude Code's monitoring page (`https://code.claude.com/docs/en/monitoring-usage`), which marks each attribute these variables add. The worker checks the page for the Claude Code version the fixture is recorded with, and compares a recording with all three variables on against one with them off. An added attribute that carries content goes in the removed list. The list is one Go table, with a comment that links the page.
- The normalized prefix rule above removes `user_prompt` and `user_system_prompt` even when `telemetry.allowContent` is `true`.
- `SPEC.md` §12 keeps tool arguments and results out of logs, except the audit payload store. So content goes to a telemetry backend only when an admin chooses.

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
- A request body must arrive within 10 s, or the gateway ends the request with 408 without decoding it. A request takes no slot while its body uploads.
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

**Test input.** CI can't make model calls, so a developer records Claude Code's OTLP exports once, with the profile above and a real session that calls a toolgate tool, and the recording is stored in `test/fixtures/otlp/`. `docs/dev/` records how, like the session-ID evidence. The AC-3-05 test replays it through the gateway with the headers `toolgate otel-headers` prints, including client-set `user.*`, `user_id`, `enduser.id`, `workspace.id` and `workspace-id` values. It checks that a metric, a log event and a span reach the backend with the verified principal and workspace, and none of the client's values.

**Content test.** The fixture is recorded with `OTEL_LOG_USER_PROMPTS`, `OTEL_LOG_TOOL_DETAILS` and `OTEL_LOG_TOOL_CONTENT` on. The AC-3-05 test checks that none of the removed span events, log records or attributes reach the backend. A unit test replays the same fixture with `telemetry.allowContent: true` and checks that they pass through, apart from the two the prefix rule removes.

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

- Each replica fetches every trust domain's bundle at start, and then every 5 minutes, or at the bundle's `spiffe_refresh_hint` if that is shorter, but never more often than every 30 s. Bundles go in one `jwtbundle.Set`, which is safe for concurrent use. Responses are read up to 1 MiB. Each fetch, with `federation.FetchBundle` or of a JWKS, has a 10 s timeout, the same as the resolvers'.
- **A token whose key ID the bundle lacks** triggers one fetch of that trust domain, at most once every 30 s, and is then verified once more. If the trust domain was fetched less than 30 s ago, the token gets 401 at once, and `auth.refuse` records the reason "unknown SPIFFE key ID".
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
- **Start:** without `after`, the last 20 matching events of the past 24 h, then new ones. With `after`, the events after that key, at most 31 days back, like the UI's range. An older key starts 31 days back. A key older than retention starts at the oldest row.
- **Following:** the handler polls every 2 s for rows after its cursor, ordered by `(occurred_at, id)`, at most 500 per query, querying again at once while a query returns 500.
- **Commit order:** `InsertAuditEvent` writes `occurred_at` as `clock_timestamp()`, an explicit value, not a schema change. A row can still commit after a newer one, but the window only has to cover the time from its insert to its commit. The query reads only rows with `occurred_at <= now() - interval '5 seconds'`. No S3 call or other network wait runs inside an audit-writing transaction after the insert, so 5 s covers that time. A row that commits later than that can be missed by the stream, never by the table. `docs/user/` says the tail is a view and the audit log is the record.
- **Keepalive:** a lone `\n` after 15 s without events. JSON decoders skip whitespace.
- **End:** the server ends the response cleanly when the token expires or after 1 h, whichever is first. It also ends cleanly when the server starts to shut down: `http.Server.RegisterOnShutdown` cancels a context that the handler watches. So open tails don't hold up a rolling update until `shutdownTimeout`. The handler extends its write deadline with `http.ResponseController` before each write.
- **Limits:** at most 4 open tails per principal on each replica, and 64 in total on each replica. Replicas share no memory, so the per-principal limit is counted on each replica. More get 429 with "Too many audit tails are open. Close one first."

**`toolgate audit tail [--after KEY] [--json]`** prints one line per event (time, type, principal, workspace, session, server, tool, and the decision or result), or the raw lines with `--json`.
- After a clean end it reconnects at once with a fresh token and the last key. After an error it reconnects with backoff from 1 s to 30 s. So a replica restart or the 1 h limit loses nothing beyond the window above.
- Ctrl-C stops it.

### OpenAPI

Phase 2 §9 chose `github.com/swaggest/openapi-go` with its `openapi31` reflector, but it isn't in `go.mod` yet. The first issue that adds a phase 3 endpoint adds the generator, as phase 2 §9 describes, for every `/api/v1/` route. That means `docs/dev/openapi.yaml`, `make check-generate`, and the test that the document's operations are exactly the mux's `/api/v1/` routes. The tail's 200 response is `application/x-ndjson` with the event as its item schema. The `/v1/*` OTLP routes aren't part of the API and aren't in the document.

## 6. Instance health

### Custom health checks

AC-3-13. The owner's aim: toolgate detects that a running MCP server can't do its job, like a Kubernetes readiness probe, and doesn't send it tool calls while that lasts. Then it repairs the instance the way the template says. Only admins write templates, so only admins define checks.

```yaml
spec:
  healthCheck:
    http:
      service: db-proxy    # rendered and validated like endpoint.service
      port: 8080
      path: /healthz       # starts with /, at most 1024 characters, not rendered
      expectedStatus: 200  # 100 to 599, default 200
    interval: 30s          # 10s to 10m, default 30s
    timeout: 5s            # 1s to 60s, default 5s, less than interval
    failureThreshold: 3    # 1 to 10, default 3
    successThreshold: 1    # 1 to 10, default 1: passes in a row to count as healthy again
    onFailure: replace     # replace (default) | reapply | none
    maxRepairs: 3          # 1 to 10, default 3: repairs allowed within 30 minutes
```

- The CRD checks these with CEL. The catalog checks them again, and also checks the shared-namespace rule for `http.service` (phase 1 §7: it must contain `${instance.id}`). A bad check makes the template invalid, with the reason in `Valid`.
- **The check** is `GET http://<service>.<instance namespace>.svc:<port><path>` with a client that doesn't follow redirects, uses no proxy and reads at most 64 KiB of the body before discarding it. It passes when the status equals `expectedStatus`. The instance namespace already admits the gateway's pods on every port (phase 1 §6). There's no MCP tool-call check: a health check is an HTTP request to an endpoint the admin chose, never a tool call, so principle 1 needs no exception.
- **Running.** The leader's `Monitor` runs the check in its own goroutine, next to phase 1's `ping`, with its own ticker and counters, so a hanging `ping` doesn't delay it (row 103). The first check runs when the instance is `ready`, then every `interval`, each with a context of `timeout`. The check doesn't gate `starting` → `ready`.

**Detection: unhealthy.** At `failureThreshold` failures in a row, the reconciler sets `instances.unhealthy_since = now()` and `unhealthy_reason` on the `ready` row (compare-and-set on `status = 'ready'`), writes `instance.health` (unhealthy, with the reason), and notifies. The status stays `ready`, because the instance still exists. While `unhealthy_since` is set:
- `tools/list` leaves the server's tools out, and every replica sends `list_changed` to the sessions that enable it (phase 1 §3 fan-out);
- a call is refused at once with reason `not-ready` and the message "`github` is unhealthy: GET http://db-proxy:8080/healthz answered 503, expected 200. toolgate is repairing it." It writes `tool.decision` as phase 1 does, so nothing is forwarded to a server known to be broken;
- `toolgate_server_status` and the Servers page show the reason.

At `successThreshold` passes in a row, the reconciler clears `unhealthy_since` (compare-and-set), writes `instance.health` (healthy), and notifies, and the tools come back with `list_changed`.

**Repair** follows `onFailure`, right after the instance becomes unhealthy:
- **`replace`** (default): the reconciler replaces the instance with phase 2 §6's restart procedure (step 2): same advisory lock, the stop conditions checked first, a new row, and reason `health-check`. Sessions keep the server enabled, and the new instance's tools arrive when it's ready.
- **`reapply`**: the reconciler re-renders the instance and server-side applies its objects again. That repairs objects that were edited or deleted. toolgate keeps only the template's newest generation, so this works only while the instance's `generation` and `template_uid` still match the template. If they don't, the repair is a `replace` instead, and it counts as one repair. A row never runs objects of a newer generation under its old one. The row keeps its ID and stays unhealthy until the check passes. If it's still unhealthy `failureThreshold × interval` later, that counts as one more repair.
- **`none`**: no repair. The instance stays unhealthy, with its tools withdrawn, until the check passes again.

**Repairs are bounded.** `instances.repairs` lists the repair times for the template and scope key, carried over to each replacement row. A repair beyond `maxRepairs` within 30 minutes isn't made. Instead the reconciler fails the row as phase 1 does (`ready` → `failed`, torn down), with the reason "Health check still failing after 3 repairs in 30 minutes: `<last reason>`." Sessions keep the server enabled with no tools, and the next enable inserts a new instance with an empty repair history. So a broken server can't restart forever, which keeps phase 1's rule that an automatic retry never loops.

**Bound (AC-3-13).** A target that turns unhealthy is seen by a check that starts within one `interval`, so the instance is marked unhealthy, and its tools withdrawn, within `interval × failureThreshold` plus one `timeout`, plus the write and the fan-out. The AC-3-13 test uses a target that answers 503 and then 200 on command. It checks the withdrawal and the refusal, the repair for each `onFailure` value, the return of the tools, and the failure after `maxRepairs`.

**Audit.** Every change of health writes `instance.health`, with no payload. Each repair writes `instance.stop` (reason `health-check`) or `instance.reapply`, and the final give-up writes `instance.fail`.

A template without `healthCheck` behaves as in phase 2: `health_check` is null, and only the `ping` runs.

### Object health after `ready`

AC-3-14. This is phase 2 §7's readiness rule applied to `ready` instances, which `Starter.checkReady` already does on every step.
- On each 30 s pass, and on each notification for the row, the reconciler judges every object of a `ready` instance. It uses kstatus, with phase 2's rule first: a `Ready` condition that has been `False` for at least 60 s at its object's current generation (read from the condition's own `observedGeneration`) counts as failed.
- A failed object marks the instance unhealthy with the reason "`<Kind>` `<name>` failed: `<Ready message>`", and it's repaired as its template's `healthCheck.onFailure` says, bounded by `maxRepairs` (above). A template without `healthCheck` uses `replace` with the default bound. When the object is ready again, the instance becomes healthy.
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
  - It first checks whether `goose_db_version` exists, because goose's `GetVersions` creates the table when it's missing, and the app role gets "permission denied" for that. A missing table counts as version 0, with the same message.
  - Kubernetes then restarts it with backoff, so the pod stays unready, and the message shows in its logs and in `kubectl describe` (the chart sets `terminationMessagePolicy: FallbackToLogsOnError`).
  - Exiting matches phase 1's failed-migration behaviour, and nothing waits in a half-started state.
  - A newer database (a rollback) is only logged, as in phase 1.

### Audit partitions

**A defect today.** `store.EnsureAuditPartitions` runs only when `serve` starts, and creates this month's and next month's partitions. A gateway that runs past the end of next month without a restart has no partition, so audit inserts fail and every tool call is refused. And a role with data rights only can't create partitions at all.

**The fix,** in this phase's migration: `toolgate_ensure_audit_partitions()`.
- It's a PL/pgSQL function, `SECURITY DEFINER` and owned by the migrating role, which owns `audit_events`. It takes no arguments and creates the partitions for the month of `now()` and the next one, under the existing `partitionLockKey` advisory lock.
- Its `search_path` is pinned with `SET search_path = <schema>, pg_temp`, in that order, with `pg_temp` last, as PostgreSQL's documentation for `SECURITY DEFINER` recommends. It's set with `ALTER FUNCTION … SET search_path` in a `DO` block, because the schema is known only at run time. The function also schema-qualifies `audit_events`. Partition names are built with `format('%I', …)` from dates alone.
- Its `TimeZone` is pinned to UTC with `ALTER FUNCTION … SET TimeZone = 'UTC'`, next to the `search_path` setting. Otherwise `date_trunc('month', now())` uses the session's time zone, and the month bounds would overlap the partitions the Go code created in UTC.
- `EXECUTE` is revoked from `PUBLIC`.
- `serve` calls it at start, in both modes, and the leader calls it when it gets the Lease and every 24 h. `store.EnsureAuditPartitions` becomes that call.
- When the leader's call fails, it retries with backoff until it succeeds, and logs an error each time. The metric `toolgate_audit_partition_errors_total` counts the failures, and `docs/admin/` gives an alert example for it.
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

- For an existing database, it adds the matching `GRANT … ON ALL TABLES` / `ALL SEQUENCES` statements, plus `REASSIGN OWNED` from the old single role to the migrator. It has no `GRANT EXECUTE ON FUNCTION` statement: the function doesn't exist until the Job runs, and the `ALTER DEFAULT PRIVILEGES … ON FUNCTIONS` statement, run before the Job, covers it.
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
- **The Job mounts nothing from the release.** The release's ConfigMap and Secrets don't exist yet at pre-install, and at pre-upgrade they're the old release's. In bjw-s `common`, `globalMounts` mounts a volume into every controller, so the chart moves its own mounts (the configuration ConfigMap, each resolver token and each `caFile`) from `globalMounts` to `advancedMounts.gateway.gateway`.
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

The default values stay covered by the existing e2e tests, and a chart test checks that they render no hook objects. A second chart test sets resolvers and trust domains, and checks that the `migrate` Job has no volumes except the ones the admin adds under `advancedMounts.migrate`. An integration test covers the version check against an older database. Another runs the partition function with the session `TimeZone` set to a zone other than UTC, against partitions that the Go code created.

## 8. Interaction table

Phase 1's rows (phase 1 §4) and phase 2's rows (phase 2 §10) still hold. Rows continue the numbering. Each row is one event combined with one operation already in flight, and each row's covering test proves its rule.

| # | Event | In flight | Rule | Test |
|---|---|---|---|---|
| 74 | Access token expires between `otelHeadersHelper` runs | exports | Exports get 401 and Claude Code drops them until the helper runs again. A 4 min refresh and a token of at least 5 min prevent it (§3). | unit |
| 75 | Backend slow or down | ingest requests | 503 with `Retry-After` after at most 10 s. Nothing is buffered; the client retries. | integration |
| 76 | Fifth concurrent ingest request on a replica | 4 decoding | 503 with `Retry-After: 2` at once. | unit |
| 77 | Client sends an attribute whose normalized key starts with `user_`, `enduser_` or `workspace_` (such as `user.id`, `user_id` or `workspace-id`), at any level | ingest | Removed everywhere; the verified values are set on each record. `session.id` passes through. | AC-3-05 |
| 78 | Replica dies after forwarding, before answering | ingest request | The client retries, so the backend may get the batch twice. OTLP delivery is at least once. | unit |
| 79 | Bundle fetch fails | SVID sign-ins | The last good bundle verifies for 24 h after its last successful fetch, then that trust domain's SVIDs get 401. | unit |
| 80 | SPIRE rotates its key | SVID signed with the new key | Unknown key ID: one fetch (at most every 30 s per trust domain, 10 s timeout), then verify again. Within 30 s of the last fetch, 401 at once with "unknown SPIFFE key ID". | unit |
| 81 | SVID from a trust domain that isn't configured | — | No bundle, so 401 and `auth.refuse`. | AC-3-09 |
| 82 | Token with a `spiffe://` subject issued by the IdP | — | Routed to the SPIFFE verifier and refused with 401. A wrong route never lets a token through. | unit |
| 83 | A different SVID on an existing MCP session | open stream | The session's binding doesn't match, so 404 (phase 1 row 33). | unit |
| 84 | Trust domain removed from the configuration | open sessions | Settings are read at start. After the restart, its SVIDs get 401. | unit |
| 85 | Audit row commits after a newer row | tail streaming | `occurred_at` is `clock_timestamp()` at the insert, and no network wait follows the insert, so the 5 s window lets it in, in key order. A row that commits later than that can be missed by the stream only. | integration |
| 86 | Replica restart or shutdown, or the 1 h or token end | tail streaming | The CLI reconnects with the last key and gets the next rows, with no duplicates. | integration |
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
| 102 | Month boundary | long-running gateway | The leader's 24 h call has already created next month's partition. A failed call retries with backoff, logs an error and counts in `toolgate_audit_partition_errors_total`. | integration |
| 103 | `ping` hangs for its timeout | custom check failing | The check runs in its own goroutine, so it reaches its threshold within `interval × failureThreshold + timeout`. | unit |
| 104 | A 0.2 leader starts an instance of a template with `healthCheck` | rolling upgrade from 0.2 | `health_check` stays null, and only `ping` runs. The next instance gets the check. | unit |
| 105 | Client sends prompt or tool content attributes (`OTEL_LOG_USER_PROMPTS`, `OTEL_LOG_TOOL_DETAILS`, `OTEL_LOG_TOOL_CONTENT`) | ingest | Removed, unless `telemetry.allowContent` is `true` (§3). | AC-3-05 |
| 106 | Instance becomes unhealthy | call in flight | The call finishes or gets the upstream error. Later calls are refused with the unhealthy reason, and the tools are withdrawn with `list_changed`. | integration |
| 107 | Instance becomes unhealthy | enable waiting on it | The enable returns the unhealthy reason and leaves the server enabled. The tools arrive with `list_changed` once it's healthy again or replaced. | integration |
| 108 | Health check passes again | repair (`replace`) under way | The old row is already stopping, so its compare-and-set to healthy matches nothing. The new row starts healthy. | unit |
| 109 | Leader dies | instance unhealthy, repair not yet made | The new leader sees `unhealthy_since` on its first pass and makes the repair, counting it in `repairs`. | e2e |
| 110 | The `maxRepairs`-th repair window is full | check still failing | The row fails as phase 1 does, with the give-up reason. The next enable starts with an empty repair history. | unit |
| 111 | Template's `onFailure` changes | instance unhealthy | The instance keeps the `health_check` it started with (row 93). New instances use the new one. | unit |
| 112 | Attestation token's pod is labelled for another user | — | `user` attestation, the claimed workspace, and `auth.refuse` with "attestation owner mismatch" and the user's principal. Never a 401 for the attestation alone (§10). | AC-3-16 |
| 113 | Attestation token invalid or expired, from a namespace not listed, not bound to a pod, repeated, or over 16 KiB | — | As row 112, with that reason. | unit |
| 114 | User token invalid | valid attestation | 401, as phase 1. An attestation never authenticates. | unit |
| 115 | Service-account or SPIFFE bearer token | attestation header sent | The header is ignored. A workload is attested as itself. | unit |
| 116 | Pod relabelled or deleted | attested sessions | The cached pod facts last until the token expires, at most 1 h (10 min for TokenRequest tokens). Then TokenReview refuses a deleted pod's token, or the new labels decide. | unit |
| 117 | Attestation stops working on the client (file gone, Workload API or TokenRequest fails) | open session | The client exits non-zero with no headers, so Claude Code reports the connection as failed. A client that sends the request without it gets 404 on the MCP session, then 400 for the changed workspace (phase 1 row 22). | unit |
| 118 | A session started without an attestation later sends a valid one | — | The workspace differs, so 404, then 400. The user starts a new session. | unit |
| 119 | `match.by: claim`, and the user token lacks the claim or it isn't a string | — | As row 112, with "attestation owner claim missing". | unit |
| 120 | Two VMs share a service account, and VM A's guest binds a token to VM B's pod | — | TokenRequest allows it, but B's labels name B's owner. With A's user token that's a mismatch (row 112). The docs ask for one service account per workspace. | unit |
| 121 | TokenRequest refused (no Role, API server unreachable, mounted token expired) | `toolgate headers` | Exits non-zero with the API server's status and message, never a token. | unit |
| 122 | Workspace list or defaults changed (replica B) | open sessions on replica A | `scope:<id>` makes A send `list_changed` to the principal's streams. The next decision loads fresh state. Within 5 s. | AC-3-17 |
| 123 | Server removed from the list | call in flight | The call finishes. Later calls are refused at step `workspace-scope`, and the tools are withdrawn. The server stays enabled, and its tools come back when it's re-added. | AC-3-17 |
| 124 | Server removed from the list | enable waiting for its instance | The enable decides again at `ready` (phase 1 §3, enable step 6), is refused, and deletes its `session_servers` row. | integration |
| 125 | The list names a server the user can't reach | — | Step 1 or 2 refuses first. A list never widens. | AC-3-17 |
| 126 | Default changed, or the list cleared | sessions with no list, or that workspace's sessions | As row 122. | AC-3-18 |
| 127 | UI and CLI write the same principal's lists at once | each other | Both lock the `user_scopes` row, so they run in turn. Each is a set operation on the current rows. | unit |
| 128 | `deny` of a server | session additions of it in that workspace | Deleted in the same transaction, so the deny withdraws it from those sessions too. | unit |
| 129 | An agent runs `toolgate workspace allow` | — | 202 and a pending `change` request. Nothing widens until the user approves with a step-up. | AC-3-17 |
| 130 | A 0.2 replica serves a session | rolling upgrade to 0.3 | It doesn't apply step 4, so its sessions see more until the rollout ends. The tables are new, so 0.2 never reads them. An attested session that reaches a 0.2 replica is seen with its claimed workspace, because 0.2 ignores the header, so it gets 404 and then 400 (row 118) until the rollout ends. | unit |
| 131 | Enable of a server outside the effective list | — | In a verified workspace: a pending `server` request and a URL elicitation, or the error with the link (§10, "Session additions"). In an unverified session: refused with how to change `unverified_default`, with no request and no approval. | AC-3-19 |
| 132 | "This session only" approved (replica B) | enable waiting (replica A) | B locks the `sessions` row, then `user_scopes`, inserts `session_additions`, and commits with `approval:<id>` and `session:<id>`. A re-decides with fresh state and goes on. | AC-3-19 |
| 133 | "Always" approved | enables waiting in two sessions of that workspace | `scope:<id>` wakes both. Both pass step 4 and go on. The other session's own request stays pending and expires unused. | integration |
| 134 | Session ends | pending `server` request | The end transaction expires it and writes `access.expire`. The page shows it expired and offers nothing. | integration |
| 135 | Session ends | its session additions | Deleted in the end transaction. A restarted session has none. | AC-3-19 |
| 136 | Another session in the same workspace | a session-only addition in the first | Not applied: additions are keyed by `session_pk`. | AC-3-19 |
| 137 | Same enable again, or a retry | its request pending | The lookup finds the pending row (as phase 2 row 46). | unit |
| 138 | Approve and deny at once (two tabs) | each other | The first compare-and-set wins, as phase 2 row 42. | unit |
| 139 | Request expires | enable waiting | As phase 2 row 41: the waiter or the leader's pass expires it, and the enable returns the timeout message. | unit |
| 140 | Replica holding the wait dies | request approved later | The approver already wrote the effect, so the next enable passes step 4 at once. | e2e |
| 141 | `change` request approved | list changed since the request | The change is applied as a set operation to the current list. | unit |
| 142 | Two helpers refresh the stored sign-in at once | unattended sessions | §9's file lock: one refresh at a time, and no refresh token is reused. | unit |
| 143 | Keychain usable in the login shell but not in the agent's process | unattended run | The store recorded in `config.json` is the only one used. With `file`, every process finds the token. With `keychain`, the error says to sign in again with `--token-store file`. | unit |
| 144 | Device code expires before the user approves | `toolgate login` | Exits 1 with "The sign-in code expired. Run `toolgate login` again." Nothing is stored. | unit |
| 145 | Refresh token expires or is revoked | unattended sessions | `headers` prints "Sign in again: run `toolgate login`." and exits non-zero with no headers. | unit |
| 146 | `TOOLGATE_TOKEN_COMMAND` exits non-zero | any command | The error gives the exit status and passes the command's standard error through (at most 4 KiB). No token is printed and no headers are sent. | AC-3-21 |
| 147 | `TOOLGATE_TOKEN_COMMAND` runs past 5 s | any command | Its process group is killed, and the command fails with the timeout message. | unit |
| 148 | `TOOLGATE_TOKEN_COMMAND` prints more than 16 KiB, nothing, or not a token | any command | Fails with that cause, never showing the output. | unit |
| 149 | Token whose audiences are all unlisted | — | 401, and `auth.refuse` with "audience not allowed" and the token's audiences. | AC-3-21 |
| 150 | Cached command token revoked before its expiry | CLI command | The 401 deletes the cache, runs the command once more, and retries once. `headers` sends the cached token until 60 s before its expiry, then gets a new one. | unit |
| 151 | Two helpers with an empty command cache | each other | The `token-command.lock` lock lets one run the command. The other reads its cached token. | unit |
| 152 | `TOOLGATE_TOKEN_COMMAND` and `TOOLGATE_SPIFFE_SOCKET` both set, or two attestation variables | — | Every command exits non-zero naming the variables. | unit |
| 153 | Audience removed from `oidcAudience` | open sessions using it | Settings are read at start. After the restart, its tokens get 401 (row 149). | unit |
| 154 | Session ends and restarts, in another workspace or with no attestation | pending session-only request | The end expired the request (row 134), so the old link offers nothing. A session approval also needs the session's current workspace and attestation to equal the request's, so it never applies to the restarted session. | integration |
| 155 | TokenReview or the pod GET fails with a transport error or a 5xx | attested request | 503 with `Retry-After: 1`, and no `auth.refuse`. The session keeps its workspace. | unit |
| 156 | Client leaves out the attestation in a listed workspace | — | Served as unverified, with `unverified_default`. The docs and the Workspaces page say a list limits a process holding the user's token only while that default is nothing. | unit |
| 157 | Session ends | addition enable waiting | The wake reads `ended_at`, stops with "This session ended while waiting for `immich`.", and writes `server.enable` (refuse). It never restarts the session. | integration |
| 158 | Client sends another unverified session's ID and claimed workspace | — | It gets only `unverified_default`. Unverified sessions have no additions, so a claimed ID never widens. | unit |
| 159 | Server removed from the list, or default narrowed | call waiting for approval | Phase 2's wait re-decides steps 1 to 4, so the call is refused at step `workspace-scope` on the next wake, and the approval isn't used. | unit |
| 160 | "Always" or "session" approved at 64 servers in the list, 200 lists, or 32 additions in the session | — | The transaction rolls back, the request stays pending, and the page names the limit. | unit |
| 161 | `deny` and a "session" approval of the same server | each other | The approval locks the `sessions` row, then `user_scopes`, so they run in turn. If the deny commits last, it deletes the addition. | unit |
| 162 | UI turned off (`oidcUIClientID` removed) | lists exist | Settings are read at start. After the restart scoping is off, so access widens, as for row 153. The rows stay, and the workspace API answers 409 with "Workspace scoping is off, because this gateway has no web UI." | unit |
| 163 | "Session" approval | `session_additions` row already there | `ON CONFLICT DO NOTHING` keeps the row, and the approval succeeds. | unit |
| 164 | `toolgate workspace deny --session ID` | additions in that session | Deletes them in one transaction, which locks the `sessions` row and then `user_scopes`, and notifies `session:<id>`. Lists don't change. | unit |
| 165 | Session ends (idle) | servers enabled | The rows stay. Per-session instances stop (session not live). `session.end` lists the set. | integration |
| 166 | Request for the ended session within `sessionResumeWindow` | same workspace and attestation | The session restarts with its set. Each server's instance is inserted or reused without waiting, and the tools arrive with `list_changed`. `session.resume` lists the set. | AC-3-22 |
| 167 | Same, from another workspace or attestation | — | The set is cleared, as in phase 1. `session.start` is written. | AC-3-22 |
| 168 | A rule or workspace list added during the pause | resume | The refused servers are dropped with `server.disable`. The rest are restored. | integration |
| 169 | `sessionResumeWindow` passes | session still ended | The reconciler deletes the rows and writes `session.forget`. A later request starts empty. | unit |
| 170 | Resume | instance limit reached | That server is dropped with `server.disable`, reason `limit`. The others are restored. | unit |
| 171 | Call to a restored or replaced server | instance `pending` or `starting` | The call waits, with progress, up to `startTimeout`, then is decided and forwarded. A failure or timeout fails the call with the reason. | AC-3-22 |
| 172 | Session disabled or ended | call waiting for a start | The wait ends with "`<server>` was disabled while starting" (phase 1 row 3), and nothing is forwarded. | integration |
| 173 | Session ends | enable or call waiting for a start | Phase 1 row 25 changes: the `session_servers` row now stays (§11), so every waiter also re-reads the session's `ended_at` on each wake. When it's set, the wait ends with "This session ended while waiting for `<server>`.", as in row 157, and nothing is forwarded. The server stays in the remembered set, so a resume within `sessionResumeWindow` restores it. | integration |

Any combination not in this table is a design gap: hand back `HANDOFF: question` with the case.

## 9. Settled choices

- **Settings** (flat, in the generated reference):
  - `otlpIngest` (default `false`; needs `otlpEndpoint`);
  - `telemetry.allowContent` (default `false`; passes prompt and tool content through the ingest, §3);
  - `spiffeTrustDomains` (YAML only);
  - `migrateOnStart` (default `true`);
  - `workspaceAttestation` (YAML only; off when absent, §10);
  - `oidcAudience` is now a list of 1 to 8 audiences; a single string still works (§10).

  The chart sets them from `config.otlpIngest`, `config.spiffe.trustDomains`, `config.database.migrateOnStart`, `config.workspaceAttestation` and `config.oidc.audience`, and mounts each `caFile` through `resources.persistence`, with `advancedMounts.gateway.gateway` (§7).
- **Client environment:** `TOOLGATE_SPIFFE_SOCKET` and `TOOLGATE_GATEWAY` (§4); `TOOLGATE_ATTESTATION_TOKEN_FILE`, `TOOLGATE_ATTESTATION_SPIFFE_SOCKET`, `TOOLGATE_ATTESTATION_TOKEN_REQUEST`, `TOOLGATE_KUBE_API`, `TOOLGATE_TOKEN_COMMAND` and `TOOLGATE_TOKEN_STORE` (§10). `config.json` gains `tokenStore`.
- **Request header:** `X-Toolgate-Attestation`, at most 16 KiB (§10).
- **Identity:** `auth.Identity.Kind` (`user`, `service-account`, `spiffe`) decides catalog access lists, admin checks and who scoping applies to (§10).
- **CRD fields:** `access.spiffeIDs` (§4) and `healthCheck` (§6) on `MCPServerTemplate`.
- **Time bounds:**
  - OTLP forward 10 s;
  - bundle and JWKS fetch 10 s; bundle refresh 5 min (from 30 s with a refresh hint); a stale bundle trusted for 24 h; a Workload API fetch 5 s;
  - tail poll 2 s, commit window 5 s, keepalive 15 s, stream at most 1 h or the token's expiry; CLI reconnect backoff 1 to 30 s;
  - health check interval 10 s to 10 min (default 30 s), timeout 1 s to 60 s (default 5 s), threshold 1 to 10 (default 3);
  - partitions created at start, at Lease acquisition and every 24 h;
  - migration Job deadline 240 s;
  - token command 5 s, cached until 60 s before its expiry; TokenRequest 5 s, tokens of 600 s; attestation pod facts cached until the token's expiry, at most 1 h; CLI access-request poll 2 s; access requests expire by `approvalTimeout` (§10).
- **Limits:**
  - OTLP body 4 MiB, sent and decompressed; 4 ingest requests at once per replica;
  - bundle and JWKS responses 1 MiB;
  - SPIFFE ID 2048 bytes; `spiffeIDs` 64 items;
  - sessions list 200;
  - tail backlog 20 events from the past 24 h, `after` at most 31 days back, 500 rows a query; 4 tails per principal on each replica, and 64 on each replica;
  - health check: HTTP body 64 KiB read, path 1024 characters, at most 10 repairs in 30 minutes;
  - attestation header 16 KiB; token command output 16 KiB, its standard error passed through up to 4 KiB; workspace ID 256 characters; key values 63 characters;
  - 200 workspace lists per principal, 64 servers per list, 32 session additions per session, 100 pending access requests per principal (a count of its own); 1 to 8 audiences.
- **Audit events:** `instance.health` on every change of health, and `instance.reapply` for each reapply, both with no payload (§6). Health failures are `instance.fail` with the check's reason. §10 adds `workspace.change`, `workspace.defaults`, `session.addition`, `access.request`, `access.approve`, `access.deny` and `access.expire`. `auth.refuse` for an attestation carries the verified user's principal.
- **Libraries:**
  - `go.opentelemetry.io/collector/pdata` (the `ptraceotlp`, `pmetricotlp` and `plogotlp` packages);
  - `github.com/spiffe/go-spiffe/v2` (`jwtsvid`, `jwtbundle`, `federation`, `spiffeid`, `workloadapi`, and `proto/spiffe/workload` in tests);
  - `github.com/swaggest/openapi-go` (phase 2 §9);
  - `encoding/json` for NDJSON; `net/http` for forwarding and HTTP checks;
  - the go-sdk's `CallTool` on the monitoring session;
  - `k8s.io/client-go` for the guest's TokenRequest, `os/exec` for the token command, `regexp` for `spiffeIDPattern`, `github.com/gofrs/flock` for the command cache's lock, and go-oidc's `IDTokenVerifier` once per audience (§10).

  No new cryptography: signatures are checked by go-spiffe, and test SVIDs are signed by go-jose.
- **Test backend:** `mirror.gcr.io/grafana/otel-lgtm`, pinned by digest (§3).
- **Token refresh is serialized.** `toolgate headers` and `toolgate otel-headers` both refresh the IdP token. The token store takes an exclusive file lock (`github.com/gofrs/flock`) on `os.UserConfigDir()/toolgate/token.lock` before it reads, refreshes and writes, wherever the token is stored. There's no access-token cache. A process that waits for the lock re-reads the refresh token after it gets the lock, and refreshes with it. So two helpers never send the same refresh token, and an IdP that rotates refresh tokens and detects reuse doesn't sign the user out. A unit test runs two refreshes at once and expects two IdP calls, with two different refresh tokens, and no reuse.
- **`SPEC.md` updates** in the first implementing PRs:
  - §4: SPIFFE in the Auth row;
  - §6: SPIFFE as a principal and workspace source, in the identity table;
  - §9: `access.spiffeIDs` and `healthCheck`;
  - §10: `spiffeIDs` in step 1, next to `access.users`, `groups` and `serviceAccounts`;
  - §12: ingest stamping, that `session.id` is kept, content removal and `telemetry.allowContent`, and the `instance.health` event;
  - §13: `toolgate migrate`, `toolgate otel-headers`' output and `TOOLGATE_SPIFFE_SOCKET`;
  - §15: the `instances` column and the partition function;
  - §21 question 2: answered by SPIFFE;
  - §2: no change. Principle 2 holds without exceptions: a claimed workspace never selects a list, a claimed session ID never adds servers (only sessions with a verified workspace get session additions), and the scope step only refuses;
  - §5: the `X-Toolgate-Attestation` header;
  - §6: attested user sessions (a user token plus a bound workload token), the identity kinds, `oidcAudience` as a list, and `TOOLGATE_TOKEN_COMMAND` under client sign-in;
  - §7: `toolgate_list_servers` shows servers outside a verified workspace's effective list as `blocked`;
  - §10: the workspace-scope step as step 4, approval moving to step 5;
  - §13: `toolgate workspace list`, `allow`, `deny` (with `--session`), `clear` and `defaults`, `toolgate login --token-store`, and the new client variables;
  - §14: the Workspaces page, and the approval page's server-access and change requests;
  - §15: `user_scopes`, `workspace_lists`, `workspace_list_servers`, `session_additions`, `access_requests` and `sessions.kind`.
- **Errors to the model** follow phase 1: short sentences naming what to do next.

## 10. Workspace and session scoping

AC-3-16 to AC-3-21. The owner's need: even for one person, different environments need different servers. A Coder workspace for cluster work needs `github` and `k8s`, not the email archive. Another needs the email archive, not `k8s`. A one-off session might need the email archive and `immich`. The same person is the real user each time, and they set this out of band, in the UI or the CLI. An agent in a workspace the owner never looked at must still act as the owner.

So a session gets two credentials: the user's own token says *who*, and a workload token says *where*. The gateway checks that the workload belongs to that user. Then a per-workspace list, set by the user, narrows which of the user's servers the session sees.

### Identity kinds

`auth.Identity` gains `Kind`: `user`, `service-account` or `spiffe`, set by the verifier that accepted the bearer token. Today the code infers the kind from attestation plus a principal prefix: `policy.HasAccess` (`isSPIFFE`, `isServiceAccount`), `audittail.IsAdmin`, and the approval wait's "a service account can't sign in" check. An attested user (below) breaks that inference: a user whose IdP `sub` starts with `spiffe://` would match `access.spiffeIDs`. So:
- `HasAccess` picks the access list by `Kind`, never by attestation or prefix.
- Admins in the API are `Kind == user` with `adminGroup` in their token's groups, at any attestation. The user token is the same with or without an attestation, so this gives nobody more than the token alone already gives.
- The approval wait's step 3 test becomes `Kind != user`.
- Scoping (below) applies only to `Kind == user`. Service accounts and SPIFFE workloads keep phase 1 and §4: templates and rules decide what they reach.
- Identities the UI builds for its own decisions (phase 2's Servers page, grants, approvals) are `Kind == user`, and their decisions skip the scope step: the UI shows the user's whole catalog, not one session's view.
- Identities rebuilt from a session snapshot take `Kind` from the new `sessions.kind` column (State), never from attestation or prefix. That covers the reconciler's access re-check, `runtime.ownerHasAccess` in `internal/runtime/stop.go`, which phase 1 row 11 relies on, and the starter's render identity. An attested snapshot can now be a user, a service account or a SPIFFE ID, so inferring would judge a pod's per-user instance against `users` and `groups`, or an attested user whose `sub` looks like `system:serviceaccount:…` as a service account.

### Attested user sessions

AC-3-16.

**Configuration** (YAML only, like `spiffeTrustDomains`; the chart sets it from `config.workspaceAttestation`):

```yaml
workspaceAttestation:
  namespaces: [coder]                  # service-account tokens from these namespaces only
  keys:                                # pod label keys (annotations are read too, below)
    userID: com.coder.user.id
    email: com.coder.user.email
    username: com.coder.user.username
    workspace: com.coder.workspace.name
  match:
    by: email                          # email | claim
    # by: claim
    # claim: preferred_username        # a claim of the user token
    # key: username                    # userID | username, compared with that claim
  workspaceID: coder/{username}/{workspace}
  spiffeIDPattern: ""                  # e.g. spiffe://coder.example.com/coder/{username}/{workspace}
```

- The feature is off when `workspaceAttestation` is absent. The header is then ignored and writes no event.
- The key names are the only integration. Coder's template keys are the defaults, and nothing in code knows about Coder.
- `namespaces` is required, and each must be a namespace name. It's separate from `serviceAccountNamespaces`: that list lets a pod sign in *as itself*. The docs say not to list workspace namespaces there unless pods should also sign in as their own service accounts.
- `workspaceID` may use `{userID}`, `{username}` and `{workspace}`. It must start with a literal segment that isn't `claimed`, `k8s` or empty, so a verified workspace never looks like a claimed one, a pod's or a SPIFFE path. It's rendered at most 256 characters.
- `spiffeIDPattern` names a trust domain in `spiffeTrustDomains`. Each placeholder is one whole path segment. It's compiled at start into a `regexp` with `regexp.QuoteMeta` around the literal parts and a named group `([A-Za-z0-9._-]+)` per placeholder. SPIFFE paths can't hold `@`, so with `spiffeIDPattern` set, `match.by` must be `claim`. The pattern must also contain `{<match.key>}`, so there's an owner to compare, and every placeholder that `workspaceID` uses, so it can be rendered. Otherwise the configuration is refused at start. A captured value longer than 63 characters is refused, as for pod keys.

**The request.** The client sends the user token in `Authorization`, as today, and the workload token in `X-Toolgate-Attestation`, raw, without `Bearer`. The header is at most 16 KiB, like `Authorization`, and is exempt from the 128-character `X-Toolgate-*` limit.

**Order in `auth.Middleware`:**
1. Authenticate `Authorization` exactly as today. A bad user token gets 401. The attestation never authenticates anyone.
2. If the identity's `Kind` isn't `user`, ignore the header. A workload is already attested as itself.
3. If the header is absent, go on as today: `user` attestation, `claimed/…` workspace.
4. Otherwise verify and bind it (below). On success, set `Attestation = attested` and `Workspace` to the rendered `workspaceID`. Then the `X-Toolgate-Workspace` header doesn't change it, as for pods.
5. On a failed verdict, keep `user` attestation and the claimed workspace, write `auth.refuse` with the reason, and serve the request. **Never a 401 just for the attestation.** The event uses phase 1's rate limit. Unlike other refusals, it carries the verified user's principal, because the user token passed. Every refusal reason listed below is a verdict.
6. If the check is unavailable (a transport error or a 5xx from TokenReview or the pod GET), answer 503 with `Retry-After: 1` and write no `auth.refuse`. Falling back here would move a live attested session to another workspace, and it would then get 404 and 400 (rows 117 and 118) although its binding was valid all along.

**Verifying the attestation token.** The verifier is chosen by the token's unverified `sub`, as phase 1 routes bearer tokens:
- `spiffe://…`: `jwtsvid.ParseAndValidate` with the §4 bundles and audience `toolgate`. It's refused when `spiffeIDPattern` is empty or doesn't match the ID.
- Anything else: TokenReview with audience `toolgate`, through the existing `serviceAccountVerifier`, but with `workspaceAttestation.namespaces` as its namespace list. The token must be bound to a pod (the pod-name and pod-UID extras), and the pod's UID must match, as in phase 1 §6.

**Binding a service-account token.** The gateway gets the pod (it already gets pods for pod sign-in, so there's no new RBAC) and reads each configured key from the pod's labels, or from its annotations when the labels lack it. Coder's template puts `com.coder.user.email` in annotations, because label values can't hold `@`.
- For KubeVirt, the token is bound to the VM's `virt-launcher` pod, whose controller is the `VirtualMachineInstance`. KubeVirt copies the VM template's labels to that pod, so the same reading works and the gateway never reads VMIs. The docs say a VM template must set the keys in `spec.template.metadata`.
- **Match.** With `by: email`, the `email` key's value must equal the user token's verified email, compared with `strings.EqualFold`. An unverified email counts as absent (phase 1 §7), so it never matches. With `by: claim`, the configured claim of the user token must be a string equal to the named key's value. The docs say to choose a claim the user can't change, such as one the IdP sets from the same directory as Coder.
- Every key that `workspaceID` uses must be present and match `^[A-Za-z0-9._-]{1,63}$` (the label-value syntax), so a value can't add path segments.
- **Refusal reasons:** "attestation token invalid", "attestation namespace not allowed", "attestation not bound to a pod", "attestation pod lookup failed" (the pod isn't found, or its UID differs), "attestation owner key missing", "attestation owner mismatch", "attestation owner claim missing", "attestation workspace key invalid", "attestation SPIFFE ID doesn't match the pattern", "attestation header repeated or too large".

**Binding a JWT-SVID.** The SVID's ID must match `spiffeIDPattern`. The captured `{username}` or `{userID}` is compared with the user token's claim, as for pods, and `{workspace}` fills `workspaceID`. SPIRE's registration entries are admin-written, so the ID is as trustworthy as the labels.

**Why the labels can be trusted.** A pod's labels and annotations are written by whoever creates or patches the pod.
- The admin lists the namespaces that may bind.
- In those namespaces, cluster RBAC lets only the Coder provisioner create or change pods and VMs. Users, workspace service accounts and agents have no `create`, `patch` or `update` on `pods`, `virtualmachines` or `virtualmachineinstances` there.
- The provisioner runs only templates that admins write. Coder fills in the owner and the workspace name itself, so a workspace owner can't set another user's email.
- toolgate doesn't check RBAC. The cluster decides, and the admin docs give a `kubectl auth can-i --as=system:serviceaccount:coder:<workspace sa> patch pods -n coder` check to run.
- So a matching label means "an admin's template made this pod for this user". The user token proves the person, the attestation proves the place, and neither alone is enough.

**Caching.** The pod facts (the key values) are cached with the TokenReview result, keyed by the token's SHA-256, until the token's expiry, capped at 1 h (phase 1). The comparison with the user token runs on every request, because the user token differs per request.

**MCP session binding.** Phase 1 binds an MCP session to (principal, `X-Toolgate-Session`, `X-Toolgate-Workspace` as sent). It now binds to the identity's final workspace and attestation instead of the header as sent. A change gets 404 and the client re-initializes. Then phase 1 row 22 refuses a different workspace on the same toolgate session with 400.

**What attested users get.** They are `attested`, so `attested` and `opt-in` environments pass without a grant (`SPEC.md` §6), and rules with `attestation: attested` match them. Their principal, groups, secrets, per-user instances and grants are the user's own, because the principal is the user's `sub`. The reconciler renders with the session snapshot as today, so `${workspace.id}` is `coder/alice/dev`.

### Client side: attestation

Three new variables. At most one may be set, otherwise every command exits non-zero with "Set only one of TOOLGATE_ATTESTATION_TOKEN_FILE, TOOLGATE_ATTESTATION_SPIFFE_SOCKET and TOOLGATE_ATTESTATION_TOKEN_REQUEST." They can't be combined with `TOOLGATE_SPIFFE_SOCKET` (§4), which makes the workload the bearer and leaves no user to attest.

- **`TOOLGATE_ATTESTATION_TOKEN_FILE`**: the path of a projected service-account token with audience `toolgate`, read on each run, because the kubelet rotates it. The admin docs give the pod volume:
  ```yaml
  volumes:
    - name: toolgate-attestation
      projected:
        sources:
          - serviceAccountToken: {audience: toolgate, expirationSeconds: 3600, path: token}
  ```
- **`TOOLGATE_ATTESTATION_SPIFFE_SOCKET`**: a Workload API address. The SVID comes from `workloadapi.FetchJWTSVID` with audience `toolgate` and a 5 s timeout, as in §4.
- **`TOOLGATE_ATTESTATION_TOKEN_REQUEST`**: for KubeVirt guests. A VM can't get a custom-audience projected token, so toolgate mints one itself. The value is a directory holding `token`, `namespace` and `ca.crt`: KubeVirt's `serviceAccount` disk, mounted in the guest. Then:
  1. Read the mounted token. Read its `kubernetes.io` claims (service-account name, pod name and pod UID) without verifying them, with go-jose's `UnsafeClaimsWithoutVerification`. They only fill in the request. The API server checks everything.
  2. Call TokenRequest with `k8s.io/client-go` (already in `go.mod`): `CoreV1().ServiceAccounts(ns).CreateToken(ctx, name, …)` with audience `toolgate`, `expirationSeconds: 600` (the API server's minimum) and `boundObjectRef` to the `virt-launcher` pod by name and UID. It authenticates with the mounted token, trusts `ca.crt`, and uses `TOOLGATE_KUBE_API` (default `https://kubernetes.default.svc`) with a 5 s timeout.
  3. Send the result. It isn't cached: one request a run is cheap, and a fresh token can't go stale.
  - **Why toolgate does it.** It's one client-go call. Without it every VM template needs its own script, and a script that writes the token to disk is worse than a call that keeps it in memory.
  - **RBAC**, in the admin docs: a namespaced Role allowing `create` on `serviceaccounts/token` with `resourceNames` set to the workspace's own service account, bound to that service account. The API server only binds a token to a pod that runs as that service account. If several VMs share one service account, a guest could bind a token to another VM's pod. That pod's labels name the other VM's owner, so without that owner's user token it gets nothing. The docs still ask for one service account per workspace, which the admin's template can create.
  - The guest needs egress to the API server, and the docs say so.
- **Failure.** If a set variable can't produce a token, the command prints why (never a token) and exits non-zero, with no headers. It never sends the request without the attestation, because that would silently change the session's attestation. This matches §4.
- **Which commands send it:** every command that calls the gateway: `headers`, `otel-headers`, `hook session-start`, `servers`, `secret`, `sessions`, `audit tail` and `workspace`. `toolgate headers` adds `X-Toolgate-Attestation` to its output.

### Sign-in through another service

AC-3-21. A workspace can get the user's access token from a service that already holds the user's consent, such as Coder's external auth. Then no `toolgate login` runs in the workspace.

**Client: `TOOLGATE_TOKEN_COMMAND`.**
- When it's set, every command that needs a user token runs it instead of reading the stored sign-in. A stored sign-in is ignored, and `toolgate login` isn't needed. With `TOOLGATE_SPIFFE_SOCKET` also set, every command exits non-zero with "Set only one of TOOLGATE_SPIFFE_SOCKET and TOOLGATE_TOKEN_COMMAND." The gateway URL comes from `TOOLGATE_GATEWAY` when no sign-in is saved (§4).
- **Running it:** `os/exec` with `/bin/sh -c` (`cmd.exe /d /c` on Windows), so it is written the way Claude Code's helpers are. Standard input is empty. A 5 s timeout through `exec.CommandContext`, with `WaitDelay` of 1 s. On Unix the command runs in its own process group, and the timeout kills the group.
- **Output:** standard output is read through `io.LimitReader` up to 16 KiB plus one byte. More is an error. Surrounding whitespace is trimmed. The rest must be one non-empty line of RFC 6750 `token68` characters. The command's standard error is passed through to toolgate's, cut at 4 KiB, so its own messages ("link your account at …") reach the user.
- **Errors** name the cause, never the output: "TOOLGATE_TOKEN_COMMAND exited with status 1.", "TOOLGATE_TOKEN_COMMAND didn't finish within 5 s.", "TOOLGATE_TOKEN_COMMAND printed more than 16 KiB.", "TOOLGATE_TOKEN_COMMAND printed no token." and "TOOLGATE_TOKEN_COMMAND printed something that isn't a token." The command exits non-zero, with no headers.
- **Caching:** the token is kept in the runtime directory (phase 1 §8) as `token-command-<first 16 hex of the command's SHA-256>`, mode 0600. It's reused while its `exp` claim, read without verification, is more than 60 s away. A token without a readable `exp` isn't cached. An exclusive `github.com/gofrs/flock` lock on `token-command.lock` in the same directory, held while checking, running and writing, means concurrent helpers run the command once.
- **On 401:** commands that read the gateway's answer (`servers`, `secret`, `sessions`, `audit tail`, `workspace`, `hook session-start`) delete the cached token, run the command once more and retry once. `headers` and `otel-headers` never see the answer. They get a fresh token on their next run, because the cache never hands out a token within 60 s of its expiry.

**Gateway: `oidcAudience` becomes a list.**
- YAML takes a string or a list, through a custom `UnmarshalJSON`. `TOOLGATE_OIDC_AUDIENCE` is split on commas, like the other list settings. The chart's `config.oidc.audience` takes a string or a list. So every existing single-value configuration means a one-item list and keeps working.
- 1 to 8 items, each non-empty, unique and at most 256 characters.
- All items share `oidcIssuerURL`, its discovery document and its JWKS: one `oidc.Provider`, and one go-oidc `IDTokenVerifier` per audience built from it, so the key set is shared. A token is routed to the verifier of the first configured audience in its unverified `aud` claim, and that verifier checks it fully: signature, issuer, audience and expiry. Claims are read as for the first audience. A token with no listed audience gets 401 and `auth.refuse` with "audience not allowed" and the token's audiences.
- `/.well-known/toolgate-client` still describes only the CLI client.
- **Security note for the docs:** every audience in the list is a client whose access tokens the gateway accepts. So the Coder provider must be one made for toolgate, never the provider Coder uses for its own sign-in. The IdP must also give every client the same `sub` for a person (phase 2 §5, "Both clients must issue the same public `sub`").

**With an attestation.** The command's token is the user token, and it goes in `Authorization`. The attestation variables above work the same with either source. So a Coder workspace pod can run with no `toolgate login`, `TOOLGATE_TOKEN_COMMAND` and `TOOLGATE_ATTESTATION_TOKEN_FILE`, and its sessions are the user's, attested, in `coder/alice/dev`.

**Admin docs example** (`docs/admin/coder.md`). The worker checks the names against the pinned Coder and Authentik docs.
- **Authentik:** an OAuth2/OpenID provider for Coder: client type confidential; redirect URI `https://coder.example.com/external-auth/toolgate/callback`; scope mappings `openid`, `email`, `profile` and `offline_access`; a signing key, so access tokens are JWTs the JWKS verifies; the same subject mode as the CLI's provider; and an access-token lifetime of at least 5 minutes (§3). Bind it to an application with the same access policy as toolgate's.
- **Gateway:** `oidcAudience: [toolgate, coder-toolgate]`, where the second item is that provider's client ID.
- **Coder** (generic OAuth2 external auth):
  ```sh
  CODER_EXTERNAL_AUTH_0_ID=toolgate
  CODER_EXTERNAL_AUTH_0_TYPE=toolgate
  CODER_EXTERNAL_AUTH_0_DISPLAY_NAME=toolgate
  CODER_EXTERNAL_AUTH_0_CLIENT_ID=coder-toolgate
  CODER_EXTERNAL_AUTH_0_CLIENT_SECRET=…        # from a Secret
  CODER_EXTERNAL_AUTH_0_AUTH_URL=https://auth.example.com/application/o/authorize/
  CODER_EXTERNAL_AUTH_0_TOKEN_URL=https://auth.example.com/application/o/token/
  CODER_EXTERNAL_AUTH_0_VALIDATE_URL=https://auth.example.com/application/o/userinfo/
  CODER_EXTERNAL_AUTH_0_SCOPES="openid email profile offline_access"
  ```
- **Template:** declaring `data "coder_external_auth" "toolgate" { id = "toolgate" }` makes Coder ask the user to link the account when the workspace is created. The agent's `env` sets `TOOLGATE_TOKEN_COMMAND = "coder external-auth access-token toolgate"` and `TOOLGATE_GATEWAY`. Coder refreshes the token with the refresh token it holds.

### Device sign-in without a browser

AC-3-20. **What phase 1 already gives:** `toolgate login` runs the device grant with PKCE and never opens a browser. It prints the verification URL (the complete one when the IdP sends it) and the code, waits, verifies the ID token, and stores the refresh token. `toolgate headers` refreshes an access token on every run, and §9's file lock keeps concurrent helpers from reusing a rotated refresh token. That works over SSH in a workspace.

**What's missing, and added:**
- **The store must be the same for every process.** go-keyring uses the Secret Service over D-Bus on Linux. A workspace's interactive shell may have a session bus while the agent's processes (Coder apps, systemd units, cron) don't. Then login saves to the keychain and deletes the file, and the unattended process finds no token. So:
  - `toolgate login --token-store auto|keychain|file` (default `auto`), also `TOOLGATE_TOKEN_STORE`. `auto` tries the keychain and falls back to the file, as today.
  - The store actually used is written to `config.json` as `tokenStore`. Every later load and save uses only that store, with no fallback, so the store never changes without anyone noticing.
  - A `config.json` without `tokenStore` (from 0.2) keeps today's keychain-then-file behaviour.
  - The docs for workspaces say: use `--token-store file`, and keep the home directory on a persistent volume, or the sign-in is lost when the workspace restarts.
- **What the CLI prints**, on standard output:
  ```
  To sign in, open https://auth.example.com/device?code=WDJB-MJHT
  and check that it shows the code WDJB-MJHT
  The code expires in 10 minutes. You can open the link on any device.
  Signed in as alice@example.com (sub 7f3a…).
  The refresh token is stored in /home/coder/.config/toolgate/refresh-token.
  Sessions on this machine act as you until the IdP ends this sign-in or you run `toolgate logout`.
  ```
  The expiry comes from the device response's `expires_in`. An expired code exits 1 with "The sign-in code expired. Run `toolgate login` again." The store line says "the OS keychain" for that store.
- **When the refresh token stops working** (expired or revoked), `headers` prints "Sign in again: run `toolgate login`." and exits non-zero with no headers, as in phase 1. How long an unattended sign-in lasts is the IdP's refresh-token lifetime, and the docs name Authentik's setting.

Nothing changes on the gateway for AC-3-20.

### State

One more additive migration. New IDs follow phase 2 §2.

- `user_scopes(id bigserial, principal text unique, verified_default, unverified_default, updated_at)`. Each default is `none` or `all`, default `none`. The row is created on a principal's first scoping write and never deleted. A missing row means both `none`.
- `workspace_lists(id bigserial, scope_id, workspace, created_at, updated_at)`, unique `(scope_id, workspace)`, where `scope_id` refers to `user_scopes.id`. At most 200 lists per principal. `workspace` is at most 256 characters and never starts with `claimed/`.
- `workspace_list_servers(list_id, server, added_at)`, primary key `(list_id, server)`. At most 64 servers per list. `server` is a server name (phase 1 §7 makes names unique).
- `session_additions(session_pk, server, request_id, created_at)`, primary key `(session_pk, server)`. At most 32 per session, the enabled-server limit. Only sessions with a verified workspace get them (Session additions).
- `sessions` gains `kind` (`user`, `service-account` or `spiffe`), written on create and on restart from the identity's `Kind`. The migration backfills it from attestation and principal prefix, which is exact for 0.2 rows, because 0.2 has no attested users. `ownerHasAccess` and the render identity read it (Identity kinds).
- `access_requests(id, principal, kind, session_pk, workspace, verified, server, change, status, created_at, expires_at, decided_at, decided_by, decided_scope)`.
  - `kind` is `server` (an enable outside the list, AC-3-19) or `change` (a CLI change that widens, below).
  - `session_pk` and `server` are set for `server`. `change` is JSON for `change`. `decided_scope` is `session` or `workspace`.
  - Statuses, compare-and-set writes, `approvalTimeout` and the leader's expiry pass are exactly phase 2's for approvals (phase 2 §2). At most 100 pending per principal. That's a count of its own, separate from phase 2's approvals count, under its own advisory lock on the principal.
  - **Why not the `approvals` table:** its `session_pk`, `arguments_sha256` and `tool` are `NOT NULL` with checks, and relaxing them would hand 0.2 replicas rows they can't scan during a rolling upgrade. The code that waits, notifies and expires is shared. Only the row type differs.
- **Notifications:** `scope:<user_scopes.id>` for any change to a principal's defaults or lists. A replica reads the row's principal, which is never deleted, and sends `list_changed` to that principal's streams. Access requests send `approval:<id>`, because IDs are unique UUIDv7 values across both tables, so a waiter reads its own table. A new `session_additions` row sends the existing `session:<id>`.
- **Ending a session** (phase 1 §2) also deletes its `session_additions` rows and sets its pending `server` requests to `expired`, writing `access.expire` for each, in the same transaction. A restart reuses the same `sessions.id` with the new request's workspace and attestation, so an old request must never apply to it.
- **Cleanup:** the leader deletes access requests 7 days after they stopped being live, as for approvals.
- **Audit events:** `workspace.change` (workspace, servers added and removed, `cleared`, and `via`: `ui`, `cli` or the request ID); `workspace.defaults` (old and new values); `session.addition` (session, server, and the request ID, or `removed` for `deny --session`); and `access.request`, `access.approve`, `access.deny`, `access.expire`, written as phase 2 writes the approval events.

### Workspace access lists and defaults

AC-3-17 and AC-3-18.

**The effective list** of a request with `Kind == user`:
- **Verified workspace** (attestation `attested`, from the binding above): the workspace's list if one exists, else everything when `verified_default` is `all`, else nothing. Plus the session's `session_additions`.
- **Unverified** (attestation `user`): everything when `unverified_default` is `all`, else nothing. Nothing more: an unverified session never has session additions.

Lists and session additions exist only for verified workspaces. A claimed workspace never selects a list, so a client can't pick a list by claiming a name. A claimed session ID never adds servers either: an unverified session's ID is only claimed, and any client with the user's token could send it (`GET /api/v1/sessions` lists it). So a claimed field never widens access, and principle 2 holds without exceptions. Everything the effective list allows still has to pass the other steps.

**Dropping the attestation.** The attestation is optional for the client. A process holding the user's token can leave it out, even inside a listed workspace, and is then served as unverified, with `unverified_default`. The user docs and the defaults form on the Workspaces page say so: "A workspace's list limits a process holding your token only while the default for unverified sessions is nothing." Nothing in the gateway changes for this.

**Policy order** (`SPEC.md` §10, phase 2 §3) gains a step, after the blocks and before approval:
1. catalog access;
2. environment, with grants;
3. blocks;
4. **workspace scope** (new): for `Kind == user`, the server must be in the effective list, else refuse with step `workspace-scope`;
5. approval.

The step only refuses, so it never widens anything: a server on a list that the user can't reach is refused at step 1 or 2 first (AC-3-17). `Decide` stays pure. The policy state batch (phase 2 §3) gains three queries in the same round trip, for `Kind == user` only: the `user_scopes` row, the list for (principal, verified workspace) with its servers, and the additions joined through `sessions` on (principal, session ID). `policy.State` gains `Scope{Applies, All bool; Servers set}`.

**Calls waiting for approval.** Phase 2 §4 step 5 re-decides steps 1 to 3 on each wake. It now re-decides steps 1 to 4. So a server removed from the list, or a narrowed default, refuses a call that's waiting for approval on the next wake, with the step-4 message, and the approval isn't used.

**Effects**, on the paths phase 1 §3 already decides:
- `toolgate_list_servers`, in a verified workspace, shows servers the step refuses with status `blocked` and the reason "not allowed in this workspace; enable it to ask", so the model can find a name to enable (AC-3-19). In an unverified session it leaves them out, because enabling them can't ask.
- `tools/list` leaves out their tools.
- A call is refused with "Server `k8s` isn't in the list for workspace `coder/alice/mail`. Allow it in the Workspaces page or with `toolgate workspace allow k8s`."
- An enable starts a session addition in a verified workspace, and is refused in an unverified session (below).
- A server that leaves the list stays enabled with no tools, like a grant that expired (phase 2 §3), and its tools come back without a new enable when it returns.

**The 5 s bound.** Every write notifies `scope:<id>` in its transaction. Every replica sends `list_changed` to the principal's streams, and the next decision loads fresh state, so a call after the commit is refused at once. A `LISTEN` reconnect sends `list_changed` to all streams, as before. A test measures the bound.

**Without the web UI** (`oidcUIClientID` empty), the scope step is off and nothing above applies. Widening needs a step-up, and only the UI can do one. So a gateway without the UI behaves as in 0.2, and the docs say scoping needs the UI. If the UI is turned off while lists exist, the rows stay but do nothing, so access widens after the restart, as for row 153. Then `GET /api/v1/workspaces` and `POST /api/v1/workspaces/changes` answer 409 with "Workspace scoping is off, because this gateway has no web UI.", and `GET /ui/workspaces` doesn't exist.

**Upgrading with the UI on.** Both defaults are `none`, and a missing `user_scopes` row means `none` (AC-3-18). There's no "only once a row exists" rule. So after the upgrade, every user session sees no servers until the user chooses defaults or lists. The CHANGELOG and the admin docs carry a prominent upgrade note: "After upgrading with the web UI on, every user sees no servers until they choose their defaults on the Workspaces page." The e2e harness (`scripts/e2e/run.sh` sets `config.ui.clientID`) writes `user_scopes(verified_default = 'all', unverified_default = 'all')` for the e2e principals through the store package during setup, so no phase 1 or phase 2 acceptance test changes. Tests of AC-3-17 to AC-3-19 set their own rows.

**Writes** lock the principal's `user_scopes` row (`INSERT … ON CONFLICT DO NOTHING`, then `SELECT … FOR UPDATE`), check the limits, change the rows, write the event, notify, and commit. A write *widens* when it adds a server to a list, clears a list while `verified_default` is `all`, or changes a default from `none` to `all`. Everything else narrows. `allow` refuses a server the user has no catalog access to, with "You don't have access to server `x`.", to catch typos. Access lost later leaves the entry in place, but it does nothing. `deny` also deletes that server's `session_additions` for the user's open sessions in that workspace, so a deny really removes it. Lists are allow lists, so `deny` on a workspace with no list changes nothing and answers 409 with "Workspace `x` has no list. Add one with `toolgate workspace allow` first." Denying a list's last server keeps an empty list, which gives nothing. Only `clear` deletes a list, so the default applies again.

**Removing a session's additions.** `deny --session ID SERVER…` deletes those servers' `session_additions` for that one session, and leaves every list as it is. It narrows, so it applies at once. The session must be the caller's own, else 404. It locks the `sessions` row and then the `user_scopes` row, the same order as a "session" approval, writes `session.addition` with `removed`, and notifies `session:<id>`.

**Web UI: `GET /ui/workspaces`.** Access `user`.
- It shows the defaults, each list with its servers, and the verified workspaces of the user's open sessions, each with an "Add a list" button.
- Its forms are `POST /ui/workspaces/defaults`, `POST /ui/workspaces/allow`, `POST /ui/workspaces/deny` and `POST /ui/workspaces/clear`, with phase 2's CSRF rules.
- Widening POSTs need the phase 2 step-up (5 minutes, the form asking at 4), checked as the grant form does. Narrowing POSTs don't.

**API and CLI.** Bearer tokens can't prove a step-up (phase 2 §9). An agent in a workspace holds the user's token, so a widening call from the CLI never applies directly. It creates a `change` access request that the user approves in the browser.
- `GET /api/v1/workspaces` returns `{"current", "defaults": {"verified", "unverified"}, "workspaces": [{"workspace", "servers", "updatedAt"}]}`, where `current` is the request's verified workspace or `null`.
- `POST /api/v1/workspaces/changes` takes `{"action": "allow"|"deny"|"clear"|"defaults", "workspace", "session", "servers", "verified", "unverified"}`. `session` is only for `deny`, in place of `workspace`. A narrowing change applies and answers 200 `{"applied": true}`. A widening one inserts a pending `change` request and answers 202 `{"request", "url"}`. Only `Kind == user` may call it; others get 403.
- `GET /api/v1/access-requests/{id}` returns `{"id", "status", "expiresAt"}`, to the requester only. Others get 404.
- `toolgate workspace list` prints the defaults and a table: `WORKSPACE`, `SERVERS`, `UPDATED`.
- `toolgate workspace allow SERVER…`, `deny SERVER…` and `clear` take `--workspace ID`. It defaults to `current`, and without one they exit with "This machine has no verified workspace. Pass --workspace."
- `toolgate workspace deny --session ID SERVER…` removes that session's additions (above). It can't be combined with `--workspace`.
- `toolgate workspace defaults [--verified none|all] [--unverified none|all]`.
- For a 202, the CLI prints "Approve this change at `<url>`. Waiting… (Ctrl-C stops waiting; the request stays open until it expires.)". It then polls the request every 2 s until it's decided or expired, and exits 0 only when approved.
- The approval page for a `change` shows the change as a before-and-after list, with Approve (step-up) and Deny. Approving applies the change in the same transaction as the compare-and-set, as a set operation on the current list, so a list that changed in between needs no merge.

### Session additions

AC-3-19. In a session with a verified workspace, an enable whose only refusal is step 4 asks the user, reusing phase 2 §4's elicitation, wait and fan-out.

**Unverified sessions never ask.** An unverified session gets the user's `unverified_default` and nothing more (Effective list). An enable outside it is refused at once with "Server `immich` isn't allowed in sessions without a verified workspace. To allow all your servers in them, set the default for unverified sessions to all on the Workspaces page, or run `toolgate workspace defaults --unverified all`." No request is inserted and no approval is offered. The enable writes `server.enable` (refuse, `workspace-scope`).

1. **Find or insert a request.** Phase 2 §4 steps 1 and 4 apply in full, with `server` requests in place of approvals. That covers the lookup order (the request named by `RequestState`, a lookup key only, so every field must match, then a pending `server` request for this session and server), its rules for a named request that's no longer pending, and the rule that a request carrying `InputResponses` never gets `InputRequests` back. Their messages:
   - A named request that was denied: "Adding `immich` was denied (request `<id>`)."
   - A named request that expired: step 4's timeout message.
   - A request carrying `InputResponses` whose lookup inserted a new row: step 2's message, with no wait.

   If none is found, insert one with the session's workspace and `verified` true, write `access.request`, notify, and commit.
2. **No elicitation**, or `decline`/`cancel`: return "Server `immich` isn't allowed in workspace `coder/alice/mail`. Allow it at `<publicURL>/ui/approvals/<id>`, then enable it again." The request stays pending. The enable writes `server.enable` (refuse, `workspace-scope`, with the request ID).
3. **Elicitation:** return the URL elicitation with `RequestState` set to the request ID, as phase 2 §4 step 4.
4. **Wait** as phase 2 §4 step 5: on `approval:<id>`, `scope:<id>`, `session:<id>`, a missed event or the 10 s tick, re-read the request and the session's `ended_at`, and decide steps 1 to 4 again with fresh state. Every outcome but cancel writes `server.enable`, so every decision is recorded:
   - If step 4 now passes (approved here, or the server was added another way), go on with phase 1 enable step 2, which writes the allow with the request ID.
   - If steps 1 to 3 refuse, stop with that refusal, and write `server.enable` (refuse, with the refusing step).
   - The session has ended: stop with "This session ended while waiting for `immich`." and write `server.enable` (refuse, `workspace-scope`, reason `session-ended`). It never goes on to phase 1 enable step 4, which would restart the ended session.
   - Denied: "Adding `immich` was denied (request `<id>`)." Write `server.enable` (refuse, `workspace-scope`, reason `addition-denied`).
   - Expired: as phase 2's timeout, with "Nobody allowed `immich` within `<approvalTimeout>` (request `<id>`).", the setting rendered in words, for example "10 minutes". Write `server.enable` (refuse, `workspace-scope`, reason `addition-timeout`).
   - Cancel or disconnect: stop and write nothing, as phase 2.
   - Waits count toward phase 2's 32 per principal per replica.
5. **The page** (`/ui/approvals/<id>`, which looks the ID up in both tables; owner only, else 404) shows the server, session and workspace. It offers "Allow for this session", "Always allow in this workspace", and Deny. A request that's no longer pending shows its status and offers nothing. Both approvals need the phase 2 step-up. Deny doesn't.
6. **Approving writes the effect.** One transaction locks the `sessions` row first (phase 1's lock order), then the `user_scopes` row, for both choices. The "session" choice takes the `user_scopes` lock too, so it runs in turn with a `deny`, and a deny that commits later never leaves the addition in place. It compare-and-sets `pending` to `approved` with `decided_scope`, then:
   - **session:** checks the session hasn't ended, and that its current workspace equals the request's and its attestation is `attested`, as the request's `verified` says. If the session has ended, the page says "This session has ended." If it's now in another workspace, or unverified, the page says "This session is now in another workspace." In both cases the transaction rolls back and nothing changes. Otherwise it inserts `session_additions` with `ON CONFLICT DO NOTHING`, so an existing row is kept and the approval still succeeds, writes `session.addition`, and notifies `session:<id>`.
   - **workspace:** adds the server to the workspace's list, creating the list if there was none (so a workspace with `none` and no list gets a list of just this server). Writes `workspace.change` with `via` set to the request ID, and notifies `scope:<id>`.
   - **Limits:** if the approval would pass 32 additions for the session, 64 servers in the list or 200 lists, the transaction rolls back and the request stays pending. The page says "This session already has 32 added servers.", "Workspace `x`'s list already has 64 servers." or "You already have 200 workspace lists.", followed by "Remove one, then approve again."
   - In both cases it writes `access.approve` and notifies `approval:<id>`.
   Because the approver writes the effect, a waiter that died loses nothing. The next enable passes step 4 at once. A request is never "used" by a later call, so it needs no `used_at`.

**Where session-only rows end.** Ending a session deletes them and expires its pending `server` requests (State). A restarted session starts with none and inherits no pending request, and other sessions in the workspace never see them, because they are keyed by `session_pk`. Before then, `toolgate workspace deny --session ID` removes them.

**Calls** of a server outside the effective list never elicit. They're refused with the message in "Effects". Only enable asks, because that's when the person is deciding what the session needs.

### Tests

- **Harness:** the e2e setup writes `user_scopes` rows with both defaults `all` for the e2e principals, through the store package ("Upgrading with the UI on"), so earlier acceptance tests keep passing.
- **AC-3-16 (e2e):** a pod in an allowed namespace, labelled for the fake IdP's user, sends a user token plus a projected token. The test checks `attested`, the `coder/…` workspace, the user's per-user instance and secrets, an `opt-in` server with no grant, and a server whose access names only one of the user's groups. A second pod, labelled for another user, falls back to `user` with an `auth.refuse` naming "attestation owner mismatch". Its session gets the claimed workspace, and the `opt-in` server is refused. The KubeVirt path is a unit test against a fake API server: a `virt-launcher`-shaped pod whose controller is a VMI, and the client's TokenRequest call with its `boundObjectRef`. The SPIFFE binding reuses §4's test bundle endpoint.
- **AC-3-17 and AC-3-18 (integration):** list, enable and call filtering, withdrawal within 5 s across two replicas, `allow` of a server outside catalog access, a listed server the user loses access to later staying refused (row 125), both defaults both ways, and a CLI widen that applies only after the approval. The UI path is covered too: a widening POST with a fresh step-up applies, and one without it is refused.
- **AC-3-19 (browser, chromedp as in phase 2):** "this session" and "always", the step-up, a second session not getting a session-only addition, and the end of the session removing it. The no-elicitation path and the unverified session's refusal are integration tests.
- **AC-3-20 (e2e):** `toolgate login --token-store file` against the fake IdP with no D-Bus session. The device code is approved by the test, and a later `toolgate headers` from a process without the login's environment gets a token. The test opens an MCP session with those headers and checks it acts as the user. Then the fake IdP revokes the refresh token, and `headers` fails with "Sign in again".
- **AC-3-21 (integration):** the CLI runs with an empty config directory (no sign-in), `TOOLGATE_GATEWAY`, and `TOOLGATE_TOKEN_COMMAND` printing a fake-IdP token for the second audience. The test opens an MCP session with `toolgate headers`' output, and checks the principal and groups through `toolgate sessions` and an enable of a group-only server. Second-audience tokens with a bad signature, an expired `exp` or a wrong issuer each get 401, and an unlisted audience is refused. Unit tests cover the timeout, the size limit, the cache, the lock and the 401 retry.

## 11. Resuming an ended session

AC-3-22. Claude Code resumes a session with the same session ID after a long pause, for example a usage limit. Phase 1 §2 cleared a session's enabled set when it ended, so the model had to enable every server again. Now the set is kept for `sessionResumeWindow` (default 24 h, 1 h to 7 days) after the session ends, and restored on resume.

- **Ending a session** (phase 1 §2) no longer deletes its `session_servers` rows. In the same transaction it sets `ended_at`, writes `session.end` with the list of enabled servers, and notifies, as before. Per-session instances stop because the session isn't live: phase 1's per-session stop condition becomes "its session has no `session_servers` row for the template, **or its session has ended**". Per-user and shared instances already count only live sessions. So nothing runs during the pause.
- **A request for an ended session within the window restarts it with its set** (phase 1 §2's restart, changed). In one transaction it locks the row and clears `ended_at`. It keeps the `session_servers` rows only if the request's workspace and attestation equal the stored ones. That's the same rule as §10's session approvals, so a resumed session never carries servers into another workspace. Otherwise it deletes them, as phase 1 did. It writes `session.resume` with the list of restored servers, or `session.start` when nothing is restored. Then, for each restored server, it runs the insert part of phase 1 §3 enable step 4: the advisory lock, a live instance or a new `pending` row, and the instance limits. It doesn't wait. A server that fails the limits or policy now is removed from the set, with a `server.disable` event naming the reason. The restart and the restore of every server run in one transaction. If the restore fails or stops part-way, for example on a database error, a closed request or a lost replica, nothing is kept: the session stays ended, and the next request restarts it and restores the whole set.
- **What the model sees:** the restored servers' tools arrive with `list_changed` as each instance becomes ready. A call that arrives while the server's live instance is `pending` or `starting` **waits** instead of being refused. It uses phase 1 §3's enable wait: woken by notifications and the 10 s tick, sending a progress notification at least every 10 s, up to the template's `startTimeout`. When the instance is `ready`, it's decided and forwarded as a normal call. If the instance fails or the wait times out, the call fails with the reason. A call to a `failed` instance with no live replacement is still refused at once. The same wait applies to a call that arrives while a phase 2 restart or a phase 3 health repair replaces the instance. A server whose start fails behaves as phase 1 row 12: still enabled, no tools.
- **After the window,** the reconciler's 30 s pass deletes the `session_servers` rows of sessions ended longer ago than `sessionResumeWindow`, writing `session.forget`. A later request then starts with an empty set, as before.
- **Policy** is decided again for every restored server (phase 1 §3 enable step 1), so a rule, a workspace list or a lost access added during the pause applies. Restoring never widens what the session could do when it ended.
- **Audit:** `session.end`, `session.resume` and `session.forget` carry the enabled set. So the audit log shows what a session had, when it paused, and what came back.

## Decisions on the draft's open questions

The operator decided these on the owner's behalf. The owner is away for phase 3, and the decisions are logged on the decision issue.

1. **Prompt and tool content in telemetry:** the ingest removes prompt and tool content attributes by default: whatever `OTEL_LOG_USER_PROMPTS`, `OTEL_LOG_TOOL_DETAILS` and `OTEL_LOG_TOOL_CONTENT` add (§3, "Content attributes are removed by default"). Users can set those variables themselves, so the client's settings can't decide this. The gateway setting `telemetry.allowContent` (default `false`) lets an admin pass them through. Why: `SPEC.md` §12 keeps tool arguments and results out of logs, except the audit payload store, so content goes to a telemetry backend only when an admin chooses. The example profile leaves the variables off, and `docs/admin/` warns that `telemetry.allowContent` sends user content to the backend.
2. **SPIFFE identity is the workload:** accepted. An SVID sign-in is attested as the workload. Whether a workspace uses it is the admin's choice, through `TOOLGATE_SPIFFE_SOCKET`. Rules and templates name SPIFFE IDs (`access.spiffeIDs`). Usage per person is read from the ID's path.
