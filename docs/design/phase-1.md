# Phase 1 design

This document decides the behaviour `SPEC.md` leaves open for phase 1. Where they disagree, stop and ask (`HANDOFF: question`). Workers implement it and never change its rules. The operator and owner change it.

## 1. Who does what

There are two kinds of work, done by different code:

- **Request handling** runs on every replica. It authenticates, decides policy, reads and writes PostgreSQL, forwards tool calls, and holds clients' MCP streams. It **never** creates, changes or deletes Kubernetes objects.
- **The runtime reconciler** runs on exactly one replica at a time, the holder of a Kubernetes Lease (controller-runtime leader election). It is the only code that touches instance objects in Kubernetes. It drives each instance from its desired state to its actual state, stops idle instances, ends idle sessions, removes orphans, health-checks ready instances, and keeps each instance's tool list.

Request handlers express intent by writing rows. The reconciler acts on rows and reports back by updating them.

**Notifications.** Every transaction that changes a row others wait on calls `pg_notify('toolgate_events', '<kind>:<id>')` inside the transaction, so the notification is delivered at commit or not at all. The kinds are `session:<sessions.id>` and `instance:<instances.id>`, and `mcpsession:<mcp_sessions.id>` when an MCP session ends (§3). Every replica `LISTEN`s, also on a second channel, `toolgate_mcp_replies`, which carries a client's answer to a server request to the replica that sent the request (§3).

**Missed notifications.** Notifications are a speed-up, never the only trigger:
- The reconciler does a full pass over every row when it gets the Lease and every 30 s after that. Idle stops, session ends and timeouts are decided on these passes.
- When a replica's `LISTEN` connection reconnects, it treats everything as changed. It re-reads the rows its waiting enables depend on and sends `list_changed` to all its streams.
- A waiting enable re-reads its rows at every 10 s progress tick, even without a notification.

**Catalog changes** (`MCPServerTemplate`, `ToolgateRule`) reach every replica through its own informer, not through PostgreSQL. Policy always reads the informer cache. On an informer event, a replica sends `list_changed` to its streams: for a rule, all of them; for a template, those whose session enables it. Only the leader writes the `rule.change` audit event, once per change. The 5 s bound in `SPEC.md` §10 is informer delay plus this fan-out, and a test measures it.

**One controller-runtime manager per replica**, with leader election on (`LeaderElectionID: toolgate-runtime`, `LeaderElectionReleaseOnCancel: true`).
- The catalog and rule reconcilers must run on **every** replica, because each replica's policy reads its own in-memory catalog. Register them with `controller.Options{NeedLeaderElection: ptr.To(false)}`.
- These reconcilers write `status` and `rule.change` only while `mgr.Elected()` is closed, so there is one writer. A replica that becomes leader re-queues every template and rule, so statuses are written again.
- The runtime reconciler runs only on the leader, which is controller-runtime's default for a runnable. Its triggers are notifications fed through a `source.Channel` and the 30 s resync, not watches on custom resources.

**Leader fencing.** A former leader can still run for a short time after losing the Lease. Every reconciler write to PostgreSQL is a compare-and-set on the row's current status (§2). Before each Kubernetes delete, the reconciler checks that it still holds the Lease (controller-runtime's `ReleaseOnCancel` with the renew deadline). The orphan sweep (row 17) always lists objects first and reads rows second, so an object applied after the read can't be taken for an orphan.

Why: one actor on Kubernetes means two replicas can never race to create or delete the same instance. Every interaction below reduces to "a row changed, then the reconciler or a replica reacted", in commit order.

## 2. State

### Sessions

`sessions(id, principal, session_id, workspace, attestation, email, name, groups, created_at, last_seen_at, ended_at)`. `id` is a `bigserial` used in notifications. (principal, session_id) is unique.

- **Identity snapshot.** `email`, `name` and `groups` copy the claims of the most recent verified request. `email` is stored only when the token's `email_verified` claim is `true` (§7). A request writes the row when any of these fields differ from the stored ones. Otherwise it only moves `last_seen_at` forward, at most once a minute per session. The reconciler uses the snapshot to render (§3) and to re-check access (row 11). A snapshot only ever narrows access. Requests always decide policy from their own token.
- **Every request reads `ended_at`.** Only the `last_seen_at` write is throttled.
- **Ending a session.** On its 30 s pass, the reconciler ends each idle session in one transaction: `UPDATE sessions SET ended_at = now() WHERE id = $1 AND ended_at IS NULL AND last_seen_at < now() - sessionIdleTimeout`, then delete its `session_servers` rows, end its live `mcp_sessions` rows (§3), write `session.end`, and notify `session:<id>`. If the update matched no row, it does nothing else.
- **A request for an ended session restarts it.** In one transaction it locks the row, clears `ended_at`, sets `workspace` and `attestation` from this request, and writes `session.start`. The enabled set is already empty.

`session_servers(session_pk, template_ns, template_name, enabled_at)` is the enabled set. `session_pk` refers to `sessions.id`.

### Instances

`instances(id, template_ns, template_name, template_uid, generation, scope, scope_key, owner_principal, render_session, desired, status, reason, namespace, tools, secrets_used, attempt_started_at, ready_at, failed_at, last_used_at, updated_at, trace_context)`.

- `id` is a UUIDv7 (`github.com/google/uuid`) written as 32 lowercase hex characters, so `tg-<id>` is 35 characters.
- `scope` is the template's scope when the instance was created. `scope_key` is `session:<principal>:<session_id>`, `user:<principal>` or `shared`. `owner_principal` is empty for shared instances. `render_session` is the `sessions.id` whose identity snapshot the reconciler renders with: the session whose enable created the row.
- `desired` is `running` or `stopped`. `status` is one of `pending`, `starting`, `ready`, `failed`, `stopping`, `stopped`.
- `trace_context` is the W3C trace context (`traceparent` and `tracestate`) of the enable request that inserted the row, or empty. It joins the reconciler's work to that request's trace (§7, "Traces").
- `tools` holds the upstream tool list, written only by the reconciler. `secrets_used` maps each secret name the render used to that secret's `updated_at`.
- **Live instance:** `desired = 'running' AND status NOT IN ('stopping', 'stopped')`. A partial unique index enforces at most one live instance per (template_ns, template_name, scope_key).
- **One row per attempt.** A row never goes back to an earlier status. A retry after a failure, or a start after a stop, inserts a new row with a new ID and a new namespace. Leftover objects of a failed attempt are deleted with its namespace, and a late result for an old attempt can only touch the old row.

**Status changes, and who makes them.** Every reconciler write is `UPDATE … WHERE id = $1 AND status = $from`. If it matches no row, the reconciler re-reads and starts over.

| From | To | By | When |
|---|---|---|---|
| (none) | `pending` | request handler | enable finds no live instance for (template, scope key) |
| `pending` | `starting` | reconciler | it has rendered and applied the objects |
| `pending` | `failed` | reconciler | rendering fails (a missing secret, a variable that doesn't exist for the scope) |
| `starting` | `ready` | reconciler | kstatus reports every object current, MCP `initialize` plus `tools/list` succeed, and the tool list passes the checks in §3 |
| `starting` | `failed` | reconciler | `startTimeout` passes since `attempt_started_at`, kstatus reports a failure, or the tool list fails the checks in §3 |
| `ready` | `failed` | reconciler | kstatus reports the objects failed, 3 health checks in a row fail, or a changed tool list fails the checks in §3 |
| `starting`, `ready` | `pending` | reconciler | its objects are missing after a leader change or gateway restart (row 17). It is re-rendered with the template's current generation, and `generation` is updated in the same write. |
| `failed`, or any live status with `desired = stopped` | `stopping` | reconciler | always. A move to `failed` also sets `desired = stopped` and `failed_at` in the same write. |
| `stopping` | `stopped` | reconciler | the namespace is gone |

A failed instance is torn down at once. Its `reason` and `failed_at` stay on the row. Rows in `stopped` are kept for 7 days for the audit trail, then deleted.

If a namespace stays `Terminating` for longer than 10 minutes, the reconciler logs a warning and sets `reason` to the namespace's conditions. The row stays `stopping`. It doesn't block anything, because a `stopping` row isn't live.

**Status shown to clients** (`SPEC.md` §7) comes from the newest row for (template, scope key): `pending` and `starting` show as `starting`, `ready` as `ready`, and `stopping` and `stopped` as `stopped`, except that a row with `failed_at` set shows as `failed` with its reason. A refusal by policy shows as `blocked`.

### Who sets `desired = stopped`

Only the reconciler sets `desired = stopped`, on its passes and after notifications. Request handlers never set it. Disable only deletes a `session_servers` row (§3).

The reconciler stops an instance when any of these holds:
- **per-session:** its session has no `session_servers` row for the template. This includes an ended session, whose rows were deleted.
- **per-user:** no live session of `owner_principal` has the template enabled, and `last_used_at` (or `ready_at` if it was never used) is older than `idleStop`.
- **shared:** no live session has the template enabled, the same `last_used_at` rule holds, and `idleStop` isn't `0`.
- **any scope:**
  - the template was deleted, or its `uid` differs from `template_uid` (deleted and re-created);
  - the template's scope differs from `scope`;
  - the template's access no longer includes `owner_principal`, judged from that principal's most recently seen session snapshot.

An **invalid** template doesn't stop anything (`SPEC.md` §8). It isn't offered, and enables of it are refused, but running instances keep running on the generation they started with.

A stop decision runs in a transaction that takes the same advisory lock as enable (§3), re-checks the condition, and then sets `desired = stopped`. An enable that commits first is therefore always seen.

## 3. Enable, disable, call and list

**MCP sessions and replicas.** An MCP session (go-sdk Streamable HTTP, stateful mode) is shared by every replica. Its state is a row in PostgreSQL, and each replica keeps a copy in memory only while it serves the session. So any replica serves any request of a live session, and a replica that dies or rolls loses no session. Claude Code doesn't initialize again after a 404, so a session held by one replica alone isn't enough. Affinity on `X-Toolgate-Session` at the ingress only makes copies rarer. The chart and the docs never require it.

- **Stored at `initialize`.** `mcp_sessions(id, session_pk, principal, session_id, workspace, attestation, protocol_version, initialize_params, initialized_params, created_at, last_seen_at, ended_at)`.
  - `id` is the `Mcp-Session-Id`, 32 lowercase hex characters from `crypto/rand`. toolgate makes it for every client, as for connectors (phase 4 §9).
  - `session_pk` refers to `sessions.id`. `principal`, `session_id`, `workspace` and `attestation` are the binding: the identity's final values, not the headers as sent.
  - `protocol_version` is the negotiated revision, from the `InitializeResult`. `initialize_params` is the client's `InitializeParams` as JSON, which holds its capabilities and client info. `initialized_params` is the `notifications/initialized` params, or null until that notification arrives.
  - A receiving middleware on `initialize` inserts the row after the go-sdk made the result, and commits it before it returns the result. So the row exists before the client sees the `Mcp-Session-Id`, and the client's next request finds it on any replica. If the insert fails, `initialize` fails with a JSON-RPC error and the replica closes its copy.
  - A receiving middleware on `notifications/initialized` writes `initialized_params`.
  - toolgate serves no resources or prompts and sends no log messages, so no other session state exists. The go-sdk's `ServerSessionState` holds nothing else that toolgate uses.
- **toolgate holds the transports.** The go-sdk's `StreamableHTTPHandler` keeps its sessions in a private map, which can't be filled from a row. So the stateful path no longer goes through that handler. `mcpEndpoint` keeps a map from `Mcp-Session-Id` to its copy (the `mcp.Server`, the `StreamableServerTransport` and the `ServerSession`), and serves each request with the transport's `ServeHTTP`. It keeps the handler's checks before a session exists: the `Accept` and `Content-Type` checks and the 4 MiB body limit. The stateless handler of phase 4 §8 doesn't change.
- **Adoption on a miss.** A request whose `Mcp-Session-Id` isn't in this replica's map:
  1. is authenticated first, as every request;
  2. reads the `mcp_sessions` row by `id`. No row, or `ended_at` set, gets HTTP 404;
  3. compares the row's binding with the request's verified identity (principal, toolgate session, workspace, attestation). A difference gets the same 404, with the same body, so a client can't tell a session that exists from one that doesn't. So a client still can't pick an `mcp.` session, or any other session, by sending an ID;
  4. builds the copy: `newServer(false, id)`, then `server.Connect(ctx, &mcp.StreamableServerTransport{SessionID: id}, &mcp.ServerSessionOptions{State: state})`. `state` holds `InitializeParams`, `InitializedParams` and `NegotiatedProtocolVersion` from the row. If `initialized_params` is still null, it is an empty `InitializedParams`, because the notification can arrive at another replica after the next request. go-sdk v1.8.0 doesn't gate requests on it;
  5. serves the request on the copy.

  Concurrent misses for one ID on one replica build one copy: the second waits for the first. A hit compares the binding of the copy as before (row 33), with no database read.
- **Two copies are normal.** Each replica that served the session holds its own copy with the same ID. The copies share nothing in memory. All the toolgate state of the session (enabled set, found tools, scope entries) is in PostgreSQL already, keyed by the `sessions` row. Before a copy serves a request, its tool set is brought up to date from the rows, so a copy that missed a notification never serves a stale list.
- **Memory release is per replica.** A replica drops its copy when it has had no request in progress and no open GET stream for 30 minutes. Dropping a copy doesn't end the session. A later request on any replica adopts it again.
- **Ending a session.** The session ends when its row gets `ended_at`. Every later request on any replica gets HTTP 404, and only then.
  - **`DELETE`** on any replica: after the binding check, one transaction sets `ended_at`, notifies `mcpsession:<id>`, and, for a connector session, runs the end transaction of the toolgate session (phase 4 §9). The replica answers 204.
  - **Idle end, for connector sessions only.** A replica touches `last_seen_at` at most once a minute for each session it serves, and once a minute for each session with a GET stream open on it. On its 30 s pass, the leader ends each connector row (`session_id` starts with `mcp.`) with `last_seen_at` older than 30 minutes: `UPDATE mcp_sessions SET ended_at = now() WHERE id = $1 AND ended_at IS NULL AND session_id LIKE 'mcp.%' AND last_seen_at < now() - interval '30 minutes'`. It notifies `mcpsession:<id>` in the same transaction, and ends the connector's toolgate session as for `DELETE`. So a connector session with an open stream on a live replica never ends, and the connector sessions of a dead replica end 30 minutes after their last use. The 30 minutes are a field of the leader's pass, so tests can set a short value.
  - **Helper sessions end with their toolgate session.** An MCP session whose toolgate session came from `X-Toolgate-Session` never ends by the idle rule above. Claude Code can sit idle for longer than 30 minutes, for example while the user is away, and it doesn't initialize again after a 404. It isn't confirmed that Claude Code always keeps a GET stream open, and with this rule it doesn't matter. Such an MCP session ends only on a client `DELETE`, or when its toolgate session ends. The reconciler's end transaction (§2, after `sessionIdleTimeout`) sets `ended_at` on the session's live `mcp_sessions` rows and notifies `mcpsession:<id>` for each. A later request that restarts the toolgate session doesn't bring back the ended MCP session.
  - On `mcpsession:<id>`, each replica closes its copy, which ends its GET stream. On a `LISTEN` reconnect, a replica reads `ended_at` for every copy it holds and closes the ended ones.
  - If a touch matches no live row, the session ended. The replica closes its copy and answers the request with 404.
  - The leader deletes rows that ended more than an hour ago. A request for a deleted row gets the same 404 as for an ended one.
- **Server-to-client messages.**
  - **`list_changed`** (2025-11-25) goes on the session's GET stream, which is on one replica. The go-sdk drops a notification for the GET stream of a copy that has none, so every replica sends `list_changed` to all its copies and only the one with the stream delivers it. A request handler never sends `list_changed` only on its own copy. Every change that alters what a session lists notifies in its writing transaction (`session:<id>`, `instance:<id>`, `grant:<id>`, `scope:<id>`, `toolsearch:<uid>`), and the fan-out sends `list_changed` on each replica. When a GET stream attaches to a copy, the replica sends one `list_changed` on it at once, so a change made while the client had no stream isn't lost.
  - **Progress** goes on the response stream of the request it belongs to. That stream is on the replica that serves the request, so nothing crosses replicas.
  - **Elicitation** (2025-11-25, phase 2 §4) is a server-to-client request on the call's response stream. The client answers it with a new POST, which can reach any replica. Two copies number their requests on their own, so two replicas can use the same JSON-RPC ID on one session. So each copy's connection is wrapped (`mcp.Transport` and `mcp.Connection`, both public in the go-sdk). The wrapper changes the ID of each request the server sends to the string `<replica id>-<original id>`, and changes it back on the client's response. The replica ID is 16 lowercase hex characters from `crypto/rand`, made when the replica starts. The go-sdk still writes and parses every message. The wrapper only routes them, so this isn't a protocol implementation.
    - A response whose ID has this replica's prefix goes to its copy.
    - A response with another prefix is sent with `pg_notify('toolgate_mcp_replies', <json>)`, with the target replica ID, the session ID and the message. The POST gets 202 after the commit. The replica with that ID feeds the message to its copy. Other replicas ignore it.
    - A response with no known prefix, or one over 7 KiB, is dropped with a warning in the log. PostgreSQL limits a notification to 8000 bytes. A URL elicitation's answer has no content and is under 200 bytes.
    - A lost answer, for example after the asking replica died, leaves the call unanswered until the approval's wait ends (phase 2 §4). The call then fails with the "approve, then call again" message.
    - The wrapper passes through every optional interface the pinned go-sdk checks on a transport (for example `SupportsProtocolVersion`). The worker proves with a test that an elicitation works through the wrapper on two replicas. If go-sdk v1.8.0 can't support this through public API, the worker hands back `HANDOFF: question`.
  - **`notifications/cancelled`** for a request served on another replica is dropped. The request runs to its end.
  - 2026-07-28 has no MCP session. Its stateless bindings stay per replica (phase 4 §8), and their state is already in PostgreSQL.
- **A replica that dies.** Its POST streams break, so each request in flight there fails for the client with a transport error. A call may already be forwarded upstream, with `tool.decision` written and no `tool.result`. The session itself lives on. The client's next request reaches another replica, which adopts the session. The client reconnects its GET stream to any replica and gets one `list_changed` at once. Enables, waits and approvals continue from their rows (row 14 here, and phase 2 row 45).
- **A replica that shuts down** (a rollout) ends its readiness first, then closes the GET streams of its copies, so clients reconnect elsewhere. It lets requests in progress finish until `shutdownTimeout`.
- **Cost per request.** A hit reads nothing new from the database. The `mcp_sessions` row is read only on a miss: at most once for each replica and session, plus once after each 30 minute memory release or replica restart. The other reads of a request (the `sessions` row and policy) don't change. The writes are one insert at `initialize`, and touches at most once a minute for each session on each replica.

**Locks.** Enable and the reconciler's stop decision take `pg_advisory_xact_lock(hashtextextended(<template_ns>/<template_name>/<scope_key>, 0))`. A request that changes a session's enabled set first locks its `sessions` row. The lock order is always the `sessions` row first, then the advisory lock. Ending a session takes only the `sessions` row lock.

In tool search mode (phase 4) the gateway still lists the enable and disable tools: a call enables, but a stopped server whose tools search can't find yet would otherwise be a dead end. See `SPEC.md`.

**Enable** (`toolgate_enable_server`):
1. Decide policy with the request's own identity. If refused, write `server.enable` (refuse, with the rule) and return an error naming the rule.
2. Check that every `${secret:…}` the template names exists for the principal. If one is missing, return the AC-1-22 message, for example "Secret `github-token` isn't set. Run `toolgate secret set github-token`.", and write nothing else.
3. Refuse a 33rd enabled server: "This session already has 32 servers enabled. Disable one with `toolgate_disable_server` first."
4. In one transaction:
   - lock the `sessions` row `FOR UPDATE`, and restart the session if it has ended (§2);
   - upsert `session_servers`;
   - take the advisory lock, then select the live instance for (template, scope key);
   - if there is none, check the instance limits (§7, "Instance limits"), then insert one in `pending` with the template's current generation and `uid`, the session as `render_session`, and `attempt_started_at = now()`;
   - write `server.enable` (allow), notify, and commit.

   On a serialization failure or `unique_violation`, retry the transaction once.
5. Wait for the instance to be `ready` or `failed`, woken by notifications and the 10 s tick, re-reading the rows each time. Send a progress notification on every status change, and at least every 10 s. Stop waiting at `startTimeout`. If the `session_servers` row is gone (a disable, or the session ended), return "`<server>` was disabled while starting."
6. When the instance is `ready`, decide policy again. If it is now refused, delete the `session_servers` row and write `server.disable` with the rule, then return the refusal. A refused enable leaves nothing enabled. Otherwise send `list_changed` and return the tool names.
7. If the instance failed, return its reason. Leave the `session_servers` row in place: the session keeps the server enabled with no tools, and the next enable inserts a new instance.

**Disable** (`toolgate_disable_server`): authenticate; in one transaction lock the `sessions` row, delete the `session_servers` row, write `server.disable`, notify `session:<id>`, and commit; then send `list_changed`. Disabling a server that isn't enabled succeeds and changes nothing. Policy never refuses a disable. It never touches `desired`; the reconciler decides stops.

**Call** (`<server>__<tool>`):
1. Find the session's `session_servers` row for the server. If there is none, refuse with reason `not-enabled`.
2. Find the live instance for (template, the scope key of the template's current scope). If it isn't `ready`, refuse with reason `not-ready` and its status and reason.
3. Decide policy. Calls are never decided from data older than the replica's informer cache and the last notification it processed.
4. Write the `tool.decision` row and commit it before forwarding. If the write fails, refuse the call and forward nothing.
5. Forward on this replica's upstream session for the instance. A replica keeps one upstream MCP client session per instance for forwarding, created on first use.
6. Write `tool.result`, and update `last_used_at` (at most once a minute).

Every refusal, including `not-enabled` and `not-ready`, writes `tool.decision` with its reason.

**List** (`tools/list`): the meta tools, plus, for each enabled server with a `ready` live instance, its `tools` minus those the policy refuses for this identity. An enabled server without a ready instance contributes no tools. Filtering a list isn't an audit event; the decisions that matter are enables and calls.

**Fan-out of `list_changed`.** When a replica receives `instance:<id>` for a status change into or out of `ready`, or a tool-list change, it sends `list_changed` to its streams whose sessions enable that template. On `session:<id>` it sends `list_changed` to that session's streams.

**Tool list checks** (the reconciler, at `starting` → `ready` and on every change): each `<server>__<tool>` name must be at most 64 characters, there must be at most 512 tools, and the stored list must be at most 1 MiB. Otherwise the instance fails with a reason such as "Tool `<name>` is longer than 64 characters with its `<server>__` prefix."

**Upstream tool changes.** Only the reconciler keeps a monitoring session to each ready instance. It uses that session for health checks and subscribes on it to `list_changed`. It re-fetches the list, applies the checks, writes `instances.tools` and notifies. Replicas ignore `list_changed` on their forwarding sessions.

**Rendering** uses the identity snapshot of `render_session`. `${principal.email}` fails to render if the snapshot has no verified email. Render errors name the variable, never a value. Reasons copied from object conditions and events are cut to 1 KiB.

**Start failures** (AC-1-18). kstatus reports a pod in `ImagePullBackOff` as in progress, not failed. When `startTimeout` passes, the reason is taken from the instance pods' `containerStatuses[].state.waiting.reason` and `message`, or else from their latest Warning events.

## 4. Interaction table

Each row is one event combined with one operation already in flight, in the order PostgreSQL commits them. Each row's covering test proves its rule.

| # | Event | In flight | Rule | Test |
|---|---|---|---|---|
| 1 | Two sessions of one user enable a per-user server | each other | The advisory lock on (template, scope key) serializes them. The second finds the first's row and waits on it. One instance. | AC-1-13 |
| 2 | Same session enables the same server twice | first enable waiting | The second sees the existing row and waits on the same instance. Both return the same result. | unit |
| 3 | Disable | enable waiting | The waiting enable sees its `session_servers` row gone and returns "`<server>` was disabled while starting." The reconciler stops the instance if nothing else uses it. | integration |
| 4 | Disable | call in flight | The call finishes (it was already allowed and forwarded). Later calls are refused with `not-enabled`. The tools are withdrawn with `list_changed`. | integration |
| 5 | Rule added that blocks the server or tool | enable waiting | The enable decides policy again when the instance becomes ready. It deletes the `session_servers` row and returns the refusal. | AC-1-25 |
| 6 | Rule added | call in flight | The call finishes. The next call is refused. Open sessions get `list_changed` within 5 s. | AC-1-25 |
| 7 | Rule removed | open sessions | Open sessions get `list_changed` and the tools come back for servers that are still enabled. No re-enable needed. | unit |
| 8 | Template changed (new generation) | instance running | It keeps its generation and shows as outdated (generation or `uid` differs). New instances use the new generation. | AC-1-19 |
| 9 | Template changed | instance `pending` | The reconciler renders whatever generation is current when it applies, and records it in the same write. | unit |
| 10 | Template deleted | instance running | The reconciler stops it. Its sessions get `list_changed` and the server shows no tools. | integration |
| 11 | Access to a template removed from a user | instance running | Calls and enables are refused at once by policy. The reconciler stops the user's instances once that user's latest session snapshot no longer has access. | integration |
| 12 | Instance fails after `ready` | calls in flight | In-flight calls get the upstream error. Later calls get `not-ready` with the failure reason. The session keeps the server enabled with no tools. The next enable inserts a new instance. | integration |
| 13 | Session ends (idle) | instance `starting` | Ending the session deletes its `session_servers` rows. The reconciler then stops the instance. No orphan objects remain. | AC-1-15 |
| 14 | Replica holding the stream dies | enable waiting | The enable request fails for the client with a transport error. The MCP session lives on: the client's next request and its GET stream reach another replica, which adopts the session (§3) and sends `list_changed` when the stream attaches. The `session_servers` row exists, so `tools/list` shows the server once ready. Calling enable again waits on the same instance. | AC-1-17 |
| 15 | Leader dies | instance `pending` or `starting` | A new leader takes the Lease and continues from the rows and the labelled objects. Applying is idempotent (server-side apply, and the namespace create accepts an existing namespace with the instance label). | e2e |
| 16 | Leader dies | instance `stopping` | The new leader deletes again. Deletion is idempotent. | e2e |
| 17 | Leader change or gateway restart | everything | The new leader lists objects carrying toolgate's labels, then reads rows. Objects with no live instance row are deleted. Live rows in `starting` or `ready` with no objects go back to `pending`, with `attempt_started_at = now()`, so `startTimeout` is measured from the restart. | e2e |
| 18 | Secret deleted by the user | instance running | Running instances keep the value they started with. The next enable is refused with the missing-secret message. | unit |
| 19 | Secret changed by the user | instance running | Same as 18: running instances aren't restarted in phase 1. `toolgate_server_status` shows that the secret changed since start (`secrets_used`). | unit |
| 20 | Upstream announces `list_changed` | open sessions | §3, "Upstream tool changes". | integration |
| 21 | Session reconnects after its session ended | — | The session restarts with an empty enabled set and takes the new request's workspace (§2). The model must enable again. | unit |
| 22 | Client sends a different workspace on the same session | — | A live session keeps its first workspace. A request with a different workspace is refused with 400, so a session can't change workspace to escape a rule. | unit |
| 23 | Reconciler decides an idle stop | enable of the same instance | Both take the advisory lock. If the enable commits first, the stop's re-check sees the new `session_servers` row and doesn't stop. If the stop commits first, the instance isn't live, so the enable inserts a new one. | integration |
| 24 | Enable | instance `stopping` | A stopping row isn't live. The enable inserts a new instance in a new namespace. | unit |
| 25 | Session ends | enable waiting | Ending the session deletes the `session_servers` row, so the enable returns "`<server>` was disabled while starting." | integration |
| 26 | Session ends | call in flight | The call finishes. The next request restarts the session with an empty enabled set, so a further call is refused with `not-enabled`. | unit |
| 27 | Session ends | enable not yet committed | The enable holds the `sessions` row lock. If it commits first, the session is live with a fresh `last_seen_at`, so the end's `WHERE` doesn't match. If the end commits first, the enable restarts the session and enables the server in it. | integration |
| 28 | Template becomes invalid | instance running | It keeps running. The template isn't offered and new enables are refused with the validation reason. | unit |
| 29 | Template's scope changes | instance running | The reconciler stops instances whose `scope` differs. Sessions keep the server enabled. The next enable creates an instance under the new scope. | unit |
| 30 | Instance attempt times out | its readiness check still running | The late result's compare-and-set on the old row fails, because the row is no longer `starting`. A retry is a new row. | unit |
| 31 | Rule reaches its `expires` time | open sessions | Each replica keeps a timer for the earliest future `expires`. When it fires, the replica sends `list_changed` to all its streams. Policy treats expired rules as absent. | unit |
| 32 | `LISTEN` connection drops | enable waiting, open streams | On reconnect, the replica re-reads every waiter's rows and sends `list_changed` to all its streams. The 10 s tick covers the gap. | integration |
| 33 | Token for another principal on an existing MCP session | open stream | The request doesn't match the session's binding, so it gets 404 and never sees the other stream. The same holds on a replica without a copy: adoption compares the row's binding first (§3). | unit |
| 34 | Two enables of different servers by one user at the per-user limit | each other | The insert lock serializes them. The first takes the last slot. The second counts again, sees the limit, and is refused. | integration |

Any combination not in this table is a design gap: hand back `HANDOFF: question` with the case.

## 5. Settings sync

- The hook calls `GET /api/v1/settings`. The gateway merges every matching `SettingsProfile`, in name order. Merging is per key, at the same depth as below: profile A setting `env.FOO` and profile B setting `env.BAR` gives both. For the same key, the later profile wins.
- The hook then compares three things for each key: the file's current value, the value it wrote last time (from `~/.config/toolgate/owned-settings.json`), and the new value. Values are compared as parsed JSON (deep equality), not as text.
- It writes only keys whose file value still equals what it last wrote, or that it has never written and that are absent from the file. It reports every other key as user-changed.
- **Removal** (AC-1-28): a key in `owned-settings.json` that is no longer in the merged profile is deleted from the file only if the file still holds the value toolgate wrote. In both cases it leaves `owned-settings.json`.
- Writing is atomic (a temporary file, then rename).
- Keys are top-level settings keys, plus one level inside `permissions` and `env`.

## 6. Kubernetes permissions

Instance namespaces are created on the fly, so the gateway's service account needs a ClusterRole. The admission policy, not RBAC, confines its writes.

**ClusterRole** (the chart installs exactly this):
- `namespaces`: `create`, `delete`, `get`, `list`, `watch`. There is no `patch`, so the gateway creates an instance namespace and does not apply it with server-side apply. If the namespace already exists, the gateway uses it only when it carries the label `toolgate.soliddowant.dev/instance` with the instance ID. Otherwise the instance fails.
- Each allowed kind (`SPEC.md` §9) except `Secret`: `create`, `patch`, `delete`, `get`, `list`, `watch`.
- `secrets`: `create`, `patch`, `delete` only. The gateway never reads Secrets. Server-side apply needs only `patch`, and Secrets need no readiness check.
- `events`: `list`, `watch` (start-failure reasons).
- `tokenreviews`: `create`.
- The chart's own namespace has a Role for `leases` and the gateway's own configuration.

The informers for instance objects use the label selector `app.kubernetes.io/managed-by=toolgate`.

**ValidatingAdmissionPolicy** (installed by the chart; it applies when `request.userInfo.username` is the gateway's service account, for `CREATE`, `UPDATE` and `DELETE` on all resources). It denies:
- any kind outside the allowed list;
- any object in a namespace without the label `toolgate.soliddowant.dev/writable: "true"`. The gateway sets it on the instance namespaces it creates, and the chart sets it on the shared namespace. This isn't `app.kubernetes.io/managed-by`, because Helm 4 overwrites that label with `Helm` on every object it manages;
- any object in a namespace without the label `pod-security.kubernetes.io/enforce: restricted`;
- a Namespace create without the `toolgate.soliddowant.dev/writable: "true"` label, without `pod-security.kubernetes.io/enforce: restricted`, or without the `tg-` prefix;
- any Namespace update (the gateway only creates and deletes namespaces), so it can't remove or weaken a Pod Security label;
- a Namespace delete of a namespace without the `toolgate.soliddowant.dev/instance` label, so the gateway can never delete the shared namespace.

So a compromised gateway can't write outside toolgate's namespaces, whether or not it labels the object. Every namespace it can write to enforces Pod Security `restricted`, and Kubernetes' own Pod Security admission checks every pod there, including pods created by Deployment and StatefulSet controllers. So it can't run a privileged, host-namespace or `hostPath` pod either. The policy never re-implements pod checks itself. The chart labels the shared namespace `restricted` when it creates it. When `config.sharedNamespace.create` is `false`, the admin must add that label, and the docs say so. AC-1-24's case (an RBAC object carrying the instance label, applied with the gateway's service account) is still rejected by the first rule.

**Pod sign-in** (AC-1-04). The projected token carries the pod's name and UID, not its owner. The gateway gets the pod, checks that its UID equals the token's, and takes its first controller `ownerReference` for the workspace. A pod with no controller uses `Pod/<name>`.

## 7. Settled choices

- **Health checks:** the reconciler sends MCP `ping` to each `ready` instance every 30 s. Three failures in a row mark it `failed`.
- **Limits:**
  - request body 4 MiB;
  - tool result forwarded up to 16 MiB (larger ones are refused with an error);
  - at most 32 enabled servers per session;
  - `X-Toolgate-*` header values 128 characters;
  - `Authorization` 16 KiB;
  - all request headers 64 KiB (`http.Server.MaxHeaderBytes`).
- **Instance limits.** Each user may have at most `limits.instancesPerUser` live instances (default 20), and the gateway at most `limits.instances` (default 200). Both are chart values, and both must be at least 1.
  - Per-session and per-user instances count against their `owner_principal`. Shared instances count only against the gateway limit.
  - Only an insert is limited. Enabling a server whose live instance already exists always works, so a second session can still use a running per-user server at the limit.
  - Before counting, the insert path takes one more advisory lock, `pg_advisory_xact_lock(hashtextextended('toolgate-instance-insert', 0))`, after the (template, scope key) lock. So two enables can't both pass at the limit. Inserts are rare, so serializing them costs little. Reuse and the reconciler don't take this lock.
  - At the per-user limit, enable fails with: "You already have 20 servers running, the limit for each user. Disable servers you no longer need, or wait for idle ones to stop." At the gateway limit: "The gateway is already running its limit of 200 servers. Try again later, or ask an admin to raise `limits.instances`." Both are audited as `server.enable` refusals with reason `limit`.
- **Metrics for cleanup:** `toolgate_instances{scope,status}` counts instance rows that aren't `stopped`. `toolgate_instance_namespaces_stuck` counts instance namespaces that have been `Terminating` for more than 10 minutes. `docs/admin/` gives alert examples for both.
- **Uninstall:** uninstalling the chart leaves running instance namespaces behind. `docs/admin/` gives the cleanup command `kubectl delete namespace -l toolgate.soliddowant.dev/instance`. Instance namespaces always carry that label, and the shared namespace never does.
- **Database migrations** run when `toolgate serve` starts, on every replica, before it opens its listeners or joins leader election. They use goose's provider with its PostgreSQL session locker (`lock.NewPostgresSessionLocker`), so replicas starting together take turns and the first applies them. Readiness stays false until migrations finish. If they fail, `serve` exits non-zero and Kubernetes restarts it.
  - There's no chart hook and no separate command: Flux and `helm --wait` need nothing extra, and an image always carries the migrations its code needs.
  - The database user in `database.existingSecret` must be allowed to create tables and indexes in its schema. `docs/admin/` says so.
  - During a rolling update, old replicas run against the new schema. So a migration only adds tables, columns with defaults, and indexes. A drop or rename waits for a later release, after the code stopped using the object.
  - A database whose version is newer than the binary's (a rollback) makes `serve` log a warning and start anyway. It never migrates down.
- **Traces.** Spans follow `SPEC.md` §12. A request's spans are children of its incoming `traceparent`.
  - Enable: `toolgate.policy` for each policy decision, and `toolgate.enable` around the whole wait.
  - Call: `toolgate.policy` and `toolgate.forward`.
  - Render and apply run later, in the leader's reconciler, possibly on another replica. So when enable inserts an instance row, it writes the request's trace context to `instances.trace_context` with the OpenTelemetry W3C propagator. When the reconciler starts that row, it extracts the context and starts `toolgate.render` and `toolgate.apply` as children of it. The reconciler's other work (health checks, stops, sweeps) uses its own root spans.
  - An enable that reuses a live instance renders and applies nothing, so its trace has no render or apply spans. The AC-1-29 test enables a fresh instance.
  - The column is added by an additive migration, as the "Database migrations" entry requires.
- **Errors to the model** are short sentences naming what to do next, for example "Secret `github-token` isn't set. Run `toolgate secret set github-token`."
- **Email claims:** `email` is used for matching and rendering only when the `email_verified` claim (name configurable) is `true`. Otherwise it is treated as absent: `access.users` and rules never match by it, and `${principal.email}` fails to render.
- **Globs** in `ToolgateRule` (`workspaces`, `tools`, and the other list fields) are compiled with `github.com/gobwas/glob` with no separators. So `*` matches any run of characters, including `/`, and `k8s/ci/*` matches `k8s/ci/ReplicaSet/runner-abc`. A rule with a pattern that doesn't compile is invalid, with the reason in its status.
- **Default environment:** a template that doesn't set `access.environments` gets `opt-in`. The CRD sets this as the field's default (`+kubebuilder:default=opt-in`), so the stored object always shows it, and the policy engine also treats an empty value as `opt-in`. In phase 1, `opt-in` behaves like `attested`, so an admin must write `environments: any` for a server that people may use from their laptops. A per-tool `environments` that isn't set inherits the server's value, so that field has no default.
- **Server names:** a template whose name doesn't match `^[a-z][a-z0-9-]{0,30}$` is invalid. If valid templates in different watched namespaces share a name, all of them are invalid with the reason "duplicate server name", until one is renamed.
- **`namespace: shared`** is valid only with `scope: shared`. A per-session or per-user template with `namespace: shared` is invalid, so instances that hold one user's secrets always get their own namespace and its default-deny policy.
  - The gateway setting `sharedNamespace` names the namespace, and the chart passes it from `config.sharedNamespace.name`. If it is empty, every `namespace: shared` template is invalid, with the reason "The gateway has no shared namespace configured (`config.sharedNamespace.name`)." So the template isn't offered.
  - **toolgate never renames objects.** The template tells instances apart itself, and the catalog validates it. With `namespace: shared`, a template is invalid unless all of these hold:
    - every object's `metadata.name` contains `${instance.id}`, and so does `endpoint.service`, which is rendered with the same variables;
    - every selector in a known kind contains `toolgate.soliddowant.dev/instance: ${instance.id}`, and so does the matching pod template's labels. The known kinds are Service `spec.selector`, Deployment and StatefulSet `spec.selector.matchLabels` with `spec.template.metadata.labels`, and NetworkPolicy `spec.podSelector.matchLabels`;
    - it contains no `CiliumNetworkPolicy` or other kind whose selectors toolgate can't check.
  - The reason names the first object and field that fails, for example "`Deployment/server`: `metadata.name` must contain `${instance.id}` in a shared namespace."
  - Two shared instances can then never collide, whether they come from two templates or from an old instance still stopping while a new one starts.
  - **Cleanup without reading Secrets.** Before applying a shared instance's objects, the reconciler creates an anchor ConfigMap named `tg-<instance id>` in the shared namespace, with the instance labels. Every object it applies for that instance gets an `ownerReference` to the anchor (`blockOwnerDeletion: false`). Stopping deletes the anchor with background propagation, and Kubernetes' garbage collector deletes the rest, Secrets included. The row moves to `stopped` once no listable labelled object of the instance is left. The orphan sweep deletes anchors that have no live row. So the gateway never needs to list or read Secrets (§6).
- **Failed sign-ins** write an `auth.refuse` audit event with the reason, issuer, audience and remote address, and no principal. At most 10 per minute per remote address are written; the rest are counted in a metric.

## 8. Client

**How `toolgate login` finds the IdP.** The gateway serves `GET /.well-known/toolgate-client` without authentication. It returns `{"issuer", "clientID", "scopes", "emailClaim"}` from the gateway settings `oidcIssuerURL`, `oidcCLIClientID` (a public client with the device grant), `oidcCLIScopes` (default `openid email profile offline_access`) and `oidcEmailClaim`.
- This is the one exception to "every HTTP handler authenticates first". It returns only public client metadata, the same kind of data as the IdP's own discovery document, and never secrets. It is rate-limited like `auth.refuse` (§7), and a test asserts that it is the only unauthenticated route besides health and metrics.
- `toolgate login --gateway URL` fetches it, runs the device grant with PKCE (`golang.org/x/oauth2`), and saves the gateway URL and the fetched values to `os.UserConfigDir()/toolgate/config.json`. `--issuer`, `--client-id` and `--scopes` override the fetched values, for testing.

**`toolgate status`** shows the gateway URL, the principal (`sub`), and the email from the ID token's `emailClaim`, followed by "(unverified)" unless `email_verified` is `true`. It is for display only. Access is always decided by the gateway.

**`toolgate headers`:**
- The session ID comes from `TOOLGATE_SESSION` if it is set (manual runs, CI, other harnesses). The gateway treats session IDs as claimed anyway (`SPEC.md` §6).
- Otherwise it walks its parent processes with `github.com/shirou/gopsutil/v4/process`, on Linux, macOS and Windows alike. The Claude Code process is the first ancestor that has a session file in the runtime directory. Names of executables are never matched.
- The runtime directory is `$XDG_RUNTIME_DIR/toolgate` when that variable is set, and otherwise `os.UserCacheDir()/toolgate/run`. The SessionStart hook writes to the same directory. The directory is created with mode 0700 and the files with 0600.
- If no session file appears within 5 s (a constant), it writes a line to standard error and prints the credential headers (`Authorization`, workspace, attestation) without `X-Toolgate-Session`, and exits 0. This is what `claude mcp list` runs into: it checks the gateway without starting a session, so no hook runs. The gateway ties such a session to the MCP session (`mcp.` prefix), so its session ID shows that no Claude Code session is behind it. The wait comes first, so a slow hook does not leave a real session untied. Not signed in still exits non-zero.

