# Upstream sessions per instance and toolgate session

This document decides how the gateway holds MCP sessions to upstream servers. It changes phase 1 §3, "Call", step 5, and adds to the phase designs, which still hold unless a section here says otherwise. Where they disagree, stop and ask (`HANDOFF: question`). Workers implement it and never change its rules. The operator and owner change it.

Words used in this document:
- **Upstream session:** an MCP session between the gateway, as a client, and an upstream server. The server makes its ID and sends it in the `Mcp-Session-Id` header of its answer to `initialize`.
- **Pair:** one instance and one toolgate session (a `sessions` row).
- **Copy:** a replica's in-memory go-sdk `mcp.ClientSession` for an upstream session. Several replicas can hold copies of one upstream session.
- **Sessionless server:** a server that answers `initialize` with no `Mcp-Session-Id`.

## 1. What changes

Today each replica keeps one upstream session per instance, made on first use. So unrelated toolgate sessions share one upstream session, and one toolgate session sees a different upstream session on each replica. A server that keeps state per session, such as the Playwright MCP server with one browser context per session, then behaves wrongly.

After this change, the calls of one pair always use one upstream session, on every replica. The gateway stores the server's session ID for the pair in PostgreSQL. The first call of a pair sends `initialize`. Every later call, on any replica, uses the stored ID.

Nothing changes for clients. Their own `Mcp-Session-Id` works as phase 1 §3 says.

## 2. Who does what

Phase 1's split stays (phase 1 §1). Nothing here touches Kubernetes objects.

**Request handling**, on every replica:
- reading the pair's row on each forward, and making a copy from it;
- sending `initialize` for a pair with no row, and storing the result;
- on a lost race, sending `DELETE` for its own new session and using the winner's;
- on HTTP 404, removing the stale row, starting a new upstream session and retrying the call once;
- ending the pair's rows in the disable transaction (§5);
- releasing copies from memory (§4, "Copies").

**The leader**, on the Lease holder only:
- ending the session's rows in the session-end transaction (§5), which the gateway also runs when a connector session ends;
- the upstream session sweep (§5): it ends rows whose pair is no longer valid, sends `DELETE` for ended rows, and removes them.

**Notifications.** No new kind. The sweep runs after `session:<id>` and `instance:<id>` notifications, after missed notifications, and every 30 s. Phase 1's rules for missed notifications hold.

## 3. State

One additive migration, `00015_upstream_sessions.sql` (phase 1 §7, "Database migrations"). The five 0.4.12 designs take fixed numbers, in the recommended build order: `00015_upstream_sessions`, `00016_workspace_keys`, `00017_instance_credentials`, `00018_tool_result_cache` and `00019_persistent_volumes`. If master already has this number when the PR rebases, take the next free number and say so in the PR.

```sql
CREATE TABLE upstream_sessions (
    id                  bigserial   PRIMARY KEY,
    instance_id         text        NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    session_pk          bigint      NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    upstream_session_id text        NOT NULL CHECK (char_length(upstream_session_id) BETWEEN 1 AND 1024),
    initialize_result   jsonb       NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    ended_at            timestamptz
);

-- At most one live upstream session per pair.
CREATE UNIQUE INDEX upstream_sessions_live_idx ON upstream_sessions (instance_id, session_pk)
    WHERE ended_at IS NULL;

-- The disable and session-end transactions end a session's live rows.
CREATE INDEX upstream_sessions_session_live_idx ON upstream_sessions (session_pk)
    WHERE ended_at IS NULL;

-- The sweep reads ended rows, oldest first.
CREATE INDEX upstream_sessions_ended_idx ON upstream_sessions (ended_at)
    WHERE ended_at IS NOT NULL;
```

- `upstream_session_id` is the server's `Mcp-Session-Id`. The key of a row is the pair. Only the server-made value is stored.
- `initialize_result` is the server's `InitializeResult` as JSON, with `instructions` removed. Forwarding never reads the instructions. A copy replays this result instead of sending `initialize` (§4).
- A row is **live** while `ended_at` is null. Forwarding uses only live rows. An ended row waits for the sweep, which sends `DELETE` and removes it.
- Queries go in a new file, `internal/store/queries/upstream_sessions.sql`, through sqlc.
- The values are never logged. A log line names the instance and the `sessions.id`, never the upstream session ID.

## 4. Forwarding

### Connecting

`runtime.ConnectUpstream` and `UpstreamOptions` (`internal/runtime/revision.go`) get two new options. The probe and the monitor don't set them, so they don't change.

- **`Adopt`** (`*runtime.AdoptedSession`, with `ID` and `InitializeResult`): the copy joins an existing upstream session and sends no `initialize`. go-sdk v1.8.0 has no public way to start a client session with a known session ID. Its `Client.Connect` always runs the handshake, and its transport takes the session ID only from a response header. So an `http.RoundTripper` wrapper (new file `internal/runtime/adopt.go`) answers two requests itself, without the network:
  - The `initialize` POST gets HTTP 200, `Content-Type: application/json`, the header `Mcp-Session-Id: <ID>`, and a body made by `jsonrpc.EncodeMessage(&jsonrpc.Response{ID: <the request's ID>, Result: <InitializeResult>})`.
  - The `notifications/initialized` POST gets HTTP 202 with no body. The replica that made the session already sent it.
  - It finds these two requests by decoding the request body with `jsonrpc.DecodeMessage`. Every other request goes to the network unchanged.

  So the go-sdk still writes and parses every message, sets the session and revision headers, and reads SSE streams. The wrapper only short-cuts two messages whose answers are already known. This isn't a protocol implementation. `Adopt` is valid only for a revision before 2026-07-28.
- **`DeleteOnClose`** (`*atomic.Bool`): when it is set and false at `Close`, the same wrapper answers the go-sdk's `DELETE` with HTTP 204 and sends nothing. So closing a copy never ends an upstream session that other replicas use. When it is true, the `DELETE` goes to the server.

Every forwarding copy uses the wrapper, with or without `Adopt`. The wrapper has one more job: **it detects a lost session.** go-sdk v1.8.0 returns `mcp.ErrSessionMissing` for a 404 only when the body is not a JSON-RPC error (`checkResponse` in `streamable.go`). The MCP TypeScript and Python SDKs answer an unknown session with 404 and a JSON-RPC error body, and the go-sdk then returns an error that wraps `jsonrpc2.ErrRejected` instead. So the forward puts a `*runtime.SessionNotFound` flag in the context of each call. The go-sdk builds the HTTP request with that context (`http.NewRequestWithContext`). The wrapper sets the flag when a response has status 404 and its request carried `Mcp-Session-Id`. It reads neither body.

Forwarding wraps this in a type `upstreamCopy` (`internal/gateway/forward.go`) with two methods:
- `Release()` sets the flag false and closes the copy. The upstream session lives on.
- `End()` sets the flag true and closes the copy. The go-sdk sends `DELETE`. After a 404 the go-sdk sends no `DELETE` anyway (`ErrSessionMissing`).

**No GET stream.** Every forwarding copy sets `DisableStandaloneSSE: true`. Replicas already ignore `list_changed` on forwarding sessions (phase 1 §3, "Upstream tool changes"). The server-to-client requests that this design forwards nowhere (§7) need no stream. One open GET stream per pair and replica would only cost connections.

**Client options are the same on every replica.** Each copy uses the same `mcp.Implementation` and the same empty `mcp.ClientOptions` as today. So a replayed `initialize` describes the same client the server saw.

### The forward, step by step

The call has passed every phase 1 step up to step 4, so `tool.decision` (allow) is committed. The instance is `ready` and live.

1. **Revision and sessionless servers.** If the instance's `mcp_revision` is 2026-07-28 or later, or this replica learned that the instance is sessionless (step 4), forward as today: one copy per instance on this replica, and no rows.
2. **Read the row.** `SELECT id, upstream_session_id, initialize_result FROM upstream_sessions WHERE instance_id = $1 AND session_pk = $2 AND ended_at IS NULL`. This is one primary-key-sized index read. Every call already writes two audit rows, so the read is small next to them. If the read fails, the call fails with the existing message "Server `<server>` can't be reached. Try again, or call `toolgate_server_status` to see its status." and outcome `failed`.
3. **A row exists.** Use this replica's copy keyed by (instance ID, `upstream_session_id`). If there is none, make one with `Adopt`, with no network round trip, and keep it. Go to step 6. If the row ends or is removed after step 2, the server answers 404 once the sweep sent its `DELETE`, and step 7 handles it.
4. **No row.** Only one forward per pair on this replica runs this step at a time. The others wait, then start again at step 2. Connect with no `Adopt`, so the go-sdk sends `initialize` and `notifications/initialized`.
   - If the session has no ID (`ClientSession.ID()` is empty), the server is sessionless. The replica remembers this for the instance ID in memory, keeps the copy as the instance's one copy, stores nothing, and goes to step 6. The fact holds until the replica releases the instance's copies on `instance:<id>` (§4, "Copies"). The sweep can send a ready instance back to `pending` with the same ID and a new generation (`Sweep.restart`), and the new generation can run a server that has sessions. So the fact is forgotten then, and the next forward learns it again.
   - If the ID is longer than 1024 bytes, or has a byte outside `0x21` to `0x7E` (the visible ASCII that the transport spec allows), or the result without `instructions` is larger than 16 KiB, the replica ends its new session (`End()`), logs a warning with the instance ID, and fails the call with the "can't be reached" message.
5. **Claim the pair.** In one transaction:
   1. `SELECT 1 FROM sessions WHERE id = $session_pk AND ended_at IS NULL FOR KEY SHARE`.
   2. Check that the session has a `session_servers` row for the instance's template.
   3. If both hold, `INSERT INTO upstream_sessions (instance_id, session_pk, upstream_session_id, initialize_result) VALUES (…) ON CONFLICT (instance_id, session_pk) WHERE ended_at IS NULL DO NOTHING RETURNING id`.

   Then:
   - **A row came back:** this replica won. Keep the copy under its ID.
   - **No row came back:** another replica won the race. Read the live row in the same transaction. After the commit, end the new session (`End()`, best effort), and use the winner's row as in step 3.
   - **No row came back, and the read finds no live row:** the winner's row was ended or removed between the two statements, by the sweep or by a 404 on another replica. Disable and session end can't do it, because the claim holds the `sessions` row lock. Insert nothing. Forward this one call on the new copy, then end it with `End()`, as in the next item. The next forward starts again at step 2.
   - **Step 1 or 2 failed:** the pair ended while this call was in flight (§6, rows 296 and 300). Insert nothing. Forward this one call on the new copy, then end it with `End()`.

   The lock on the `sessions` row orders the claim against disable and session end. Both of those take `FOR UPDATE` on that row first (phase 1 §3, "Locks"). `FOR KEY SHARE` doesn't block the `last_seen_at` touches.
6. **Call.** `tools/call` on the copy, as today.
7. **HTTP 404.** If the error matches `errors.Is(err, mcp.ErrSessionMissing)`, or the call's `SessionNotFound` flag is set (§4, "Connecting"), the server forgot the session, for example after a restart. A 404 means the server didn't run the call, so one retry is safe.
   1. Drop the copy.
   2. `DELETE FROM upstream_sessions WHERE id = $row AND upstream_session_id = $old`. If another replica already removed or replaced it, this matches nothing, and that is fine.
   3. Start again at step 2, once. A second 404 fails the call with the existing "Server `<server>` returned an error" message.

   The state on the server is lost. That is no worse than today.
8. **Other errors.** After a JSON-RPC error response, the copy still works. After any other error, the replica releases the copy (`Release()`) and keeps the row. The next call makes a new copy from the row. A refused remote credential also releases the copy, as today, so the next copy reads the secret again.

The remote owner check (`errRemoteNotOwner`) stays before step 1. A remote server's copy sends the owner's headers, filled in when the copy is made, as today.

### Copies

- A replica keeps its copies in a map keyed by (instance ID, upstream session ID). Sessionless and 2026-07-28 instances use (instance ID, empty).
- A replica releases a copy that served no call for 30 minutes, with `Release()`. A ticker checks once a minute. Releasing never ends the upstream session.
- On `instance:<id>` for an instance that isn't `ready`, a replica releases every copy of that instance and forgets that the instance is sessionless. This extends today's `dropUnlessReady`, which closed the one copy with a `DELETE`.
- Each copy of a row also records the row's `session_pk`. On `session:<id>`, a replica reads that session's live rows (`SELECT instance_id, upstream_session_id FROM upstream_sessions WHERE session_pk = $1 AND ended_at IS NULL`, through `upstream_sessions_session_live_idx`). It releases each of its copies for that session that matches no live row. So a disable or a session end frees the copy at once, and not only after 30 minutes. Sessionless and 2026-07-28 copies belong to no session, and this rule never releases them.
- On shutdown, a replica releases every copy.
- Concurrent calls on one copy are safe. The go-sdk client session allows them.

## 5. Ending upstream sessions

A pair ends when the session disables the server, when the toolgate session ends, or when the instance stops for any reason. The issue asks for a best-effort `DELETE` and the row removed in each case.

**Disable** (phase 1 §3, `removeServer` in `internal/gateway/enable.go`). In the same transaction, after it locks the `sessions` row and deletes the `session_servers` row:

```sql
UPDATE upstream_sessions SET ended_at = now()
WHERE session_pk = $1 AND ended_at IS NULL
  AND instance_id IN (SELECT id FROM instances WHERE template_ns = $2 AND template_name = $3);
```

It already notifies `session:<id>`, which wakes the sweep. Every path that removes a `session_servers` row through `removeServer` gets this, including a refused enable at phase 1 step 6.

**Session end** (`endSessionInTx` in `internal/runtime/sessionend.go`). The same transaction runs `UPDATE upstream_sessions SET ended_at = now() WHERE session_pk = $1 AND ended_at IS NULL`. This covers the idle end, a connector's `DELETE`, and a connector's idle end, because they all use this transaction. A session that resumes later (phase 3 §11) gets new upstream sessions.

**Instance stop, failure and replacement.** No status write changes. The sweep ends the rows of every instance that isn't live.

**The sweep** (new file `internal/runtime/upstreamsessions.go`). It is a leader-only runnable, added with `mgr.Add` like the monitor. It runs on its own goroutine, so a slow server never delays the reconciler's pass. It runs when it gets the Lease, every 30 s, and after `session:`, `instance:` and missed notifications. One run does this:

1. End the rows of invalid pairs, in one statement. A pair is invalid when its instance isn't live (phase 1 §2), its session has `ended_at` set, or its session has no `session_servers` row for the instance's template. This catches every path the two transactions above miss, such as a row a replica inserted during a rolling upgrade. This statement reads every live row, so it is not an indexed read like the other steps. Live rows are at most 32 for each live session, a few thousand at homelab size, and one scan of them every 30 s on the leader is a small load. The design accepts that cost.
2. Read at most 256 ended rows, oldest first.
3. For each row, send `DELETE` when the instance is live or the instance is remote. An in-cluster instance that stopped is losing its pods, so its sessions go with them, and a `DELETE` would only wait on a dead address. The `DELETE` comes from a copy made with `Adopt` and closed with `End()`, so the go-sdk sends it. A remote instance's copy gets its owner's headers through `Starter.RemoteAuth`, as the monitor does. If the headers can't be filled in, for example because a secret was deleted, it sends nothing.
4. Send at most 8 `DELETE`s at once, each with a 5 s timeout. A failure or a timeout is counted (§9) and logged at debug level. The sweep doesn't retry.
5. Delete each row after its `DELETE` finished or was skipped.

A former leader that still runs the sweep for a short time sends at most a second `DELETE`, which gets a 404, and deletes rows that are already gone. Both are harmless, so the sweep needs no fencing beyond the Lease.

**Idle expiry.** No new timer. A pair lives as long as its toolgate session keeps the server enabled. The existing `sessionIdleTimeout` ends idle toolgate sessions, and the session-end transaction above ends their rows. A server that expires its own sessions answers 404, and step 7 of §4 starts a new one.

## 6. Interaction table

Phase 1's rows (phase 1 §4) and the rows of phases 2 to 4 still hold, except where a row below says it changes one. Rows continue the numbering. Each row is one event combined with one operation already in flight, in the order PostgreSQL commits them. Each row's covering test proves its rule. The tests are in §11.

| # | Event | In flight | Rule | Test |
|---|---|---|---|---|
| 287 | Forward of a pair on replica B | the pair's row, written by replica A | B reads the row and adopts the session with no `initialize`. The server sees the same `Mcp-Session-Id` from both replicas. | e2e and integration: one upstream session across replicas |
| 288 | Forward of a second toolgate session | a shared or per-user instance that already serves one session | No row for this pair, so it gets its own upstream session. The two sessions never share server state. | integration: each toolgate session gets its own upstream session |
| 289 | First forward of a pair on replica A | first forward of the same pair on replica B | Both send `initialize`. One insert wins. The loser reads the winner's row, ends its own session with `DELETE`, and forwards on the winner's. One live row. | integration: first forward race keeps one upstream session |
| 290 | First forward of a pair | another first forward of the same pair on the same replica | The second waits for the first and then reads its row. One `initialize`. | integration: concurrent first forwards on one replica initialize once |
| 291 | Server restarts, or expires the session | forward on the stored ID | The server answers 404. The replica removes the row by its old value, starts a new session, stores it, and retries the call once. A second 404 fails the call. | integration: forward starts a new upstream session after a restart; forward retries only once after 404 |
| 292 | 404 on replica A | 404 for the same row on replica B | Each deletes by the old value, so only one delete matches. Both then claim the pair as in row 289. One live row. Each call retries once. | integration: concurrent 404s on two replicas keep one row |
| 293 | Container of an instance restarts, the instance row doesn't change | forward | Same as row 291. | integration: forward starts a new upstream session after a restart |
| 294 | Disable | forward of the pair already sent upstream | The call goes on. The disable transaction ends the row. The sweep then sends `DELETE`, which can end the call early with the server's error. Phase 1 row 4 changes: the call ends with whatever the server answers. Later calls are refused with `not-enabled`. | integration: disable ends the upstream session |
| 295 | Disable, then enable again | sweep not yet run | The disable ended the row, so the next forward finds no live row and starts a new session. The old one is ended by the sweep. | integration: enable after disable starts a new upstream session |
| 296 | Disable | first forward of the pair, before its claim | The claim locks the `sessions` row. If the disable commits first, the claim sees no `session_servers` row, inserts nothing, forwards this call on its new session and then ends it. If the claim commits first, the disable's update ends the new row. Either way no live row is left. | integration: disable during a first forward leaves no live row |
| 297 | Session ends (idle, connector `DELETE` or connector idle) | open pairs | The end transaction ends every live row of the session. The sweep sends `DELETE` and removes them. | integration: session end ends its upstream sessions |
| 298 | Session ends | forward already sent upstream | As row 294. | integration: session end ends its upstream sessions |
| 299 | Session resumes (phase 3 §11) | ended rows | The rows ended at the end, so the first forward starts a new upstream session. The server state of the old session is gone. | integration: a resumed session gets a new upstream session |
| 300 | Session ends | first forward of the pair, before its claim | As row 296, with `ended_at` in place of the missing `session_servers` row. | integration: disable during a first forward leaves no live row |
| 301 | Instance stops (idle stop, template changed or deleted, access removed, scope changed) | forwards in flight | In-flight calls get the server's answer, as phase 1 row 12. Each replica releases its copies on `instance:<id>`. The sweep ends the rows and removes them with no `DELETE`, because the pods are going away. | integration: instance stop removes rows without a delete |
| 302 | Instance fails after `ready` | forwards in flight | As row 301. | integration: instance stop removes rows without a delete |
| 303 | Instance restarted or replaced (phase 2 §6, phase 3 health repair) | open pairs | The new instance has a new ID, so every pair starts a new upstream session on its first forward. The old instance's rows go as in row 301. | integration: instance stop removes rows without a delete |
| 304 | Remote instance stops | open pairs | The sweep sends `DELETE` with the owner's headers, because a remote server keeps its state after toolgate stops using it. If the headers can't be filled in, it sends nothing. | integration: sweep ends a remote server's upstream session after a stop |
| 305 | Replica holding copies dies | forwards in flight there | Those calls fail for the client, as phase 1 §3 says. Rows and server sessions live on. The next forward on another replica adopts the row. | e2e: one upstream session across replicas |
| 306 | Replica dies after `initialize`, before its claim commits | — | No row, so the next forward starts another session. The orphan lives until the server expires it or the instance stops. This is the best-effort limit. | integration: a failed claim leaves no row |
| 307 | Replica releases a copy (30 minutes idle, shutdown, or a transport error) | other replicas' copies | Release sends no `DELETE`. The row stays, and the next forward adopts it. | unit: forwarding copies release without delete |
| 308 | Leader changes | sweep run | The new leader runs the sweep when it gets the Lease. A `DELETE` sent twice gets a 404. A row deleted twice matches nothing. | integration: sweep after a leader change is safe to repeat |
| 309 | `LISTEN` connection drops | ended rows | The sweep's 30 s tick, and the missed-notification run after the reconnect, cover the gap. | integration: sweep after a leader change is safe to repeat |
| 310 | Forward to a sessionless server | — | The replica remembers the instance as sessionless and keeps one copy for it. No row. A second pair on that replica sends no `initialize`. | integration: sessionless upstream stores nothing |
| 311 | Forward to a 2026-07-28 instance | — | No rows and no `initialize`, as today. | integration: 2026-07-28 upstream stores nothing |
| 312 | Forward to a per-session instance | — | One row, one upstream session, as for any pair. The instance serves one toolgate session, so there is no extra session on the server. | integration: per-session instance has one upstream session |
| 313 | Forward to a remote server, on two replicas | — | As row 287. Each copy sends the owner's headers. | integration: remote server uses one upstream session across replicas |
| 314 | Remote server refuses the credential | adopted copy | The replica releases the copy and returns the refusal message, as today. The row stays. The next forward makes a new copy with the headers read again. | integration: remote server uses one upstream session across replicas |
| 315 | Server sends an over-long or invalid session ID, or a result over 16 KiB | first forward | The replica ends the new session, stores nothing, logs a warning with the instance ID, and fails the call with "can't be reached". | integration: invalid upstream session ID fails the call |
| 316 | Rolling upgrade: replicas of the old and the new version | forwards | Old replicas keep one session per instance and don't read the table. An old leader doesn't sweep. The guarantee of this design holds once every replica runs the new version. The new leader's first sweep ends rows of pairs that ended meanwhile. | `test/e2e: TestChartUpgradeAndUninstall`, the existing chart upgrade test, which keeps calls working during the rollout |
| 317 | Two calls of one pair at once | one copy | Both run on the same copy. The go-sdk allows concurrent calls on a session. | integration: one upstream session across replicas |
| 349 | Sweep sends a ready instance back to `pending` (`Sweep.restart`) | a replica that learned the instance is sessionless | The `instance:<id>` notification releases the copies and the sessionless fact is forgotten. The first forward after the new start sends `initialize` again and learns the fact for the new generation. | integration: sessionless fact is forgotten when the instance leaves ready |
| 350 | Server answers 404 with a JSON-RPC error body (TypeScript or Python SDK) | forward on the stored ID | The wrapper sets the call's `SessionNotFound` flag, so step 7 runs as in row 291. | unit: not-found with a JSON-RPC body is detected. Integration: forward starts a new upstream session after a restart, with a subtest for a JSON-RPC body |
| 351 | First forward loses the insert race | the winner's row ended or removed before the read | Insert nothing. Forward this call on the new copy, then end it. The next forward claims again. | integration: lost race with no live row forwards once and ends its session |
| 352 | Disable or session end | this replica's copy of the pair | The `session:<id>` notification releases the copy, because its row is no longer live. The sweep sends the `DELETE`. | integration: disable releases the copy on every replica |

Any combination not in this table is a design gap: hand back `HANDOFF: question` with the case.

## 7. Server-to-client requests

Today the forwarding client is `mcp.NewClient` with no options. So it advertises only the go-sdk's default `roots` capability, and no sampling or elicitation. The go-sdk answers a server's `roots/list` with an empty list, and answers `elicitation/create` and `sampling/createMessage` with an error. `SPEC.md` §16 keeps sampling and roots out of the gateway ("Tools only").

This design keeps that. It changes no capability and routes no server request to a client. The issue gives routing as a second reason for the change, but asks for no routing behaviour, so routing is not in scope.

What this design makes possible later: each upstream session now belongs to one toolgate session. A server request that belongs to a call comes on the response stream of the forwarded `tools/call`. That stream is on the replica that serves the client's call. So a later design could pass such a request to the client's call stream with no routing between replicas. A request that belongs to no call would need the GET stream, which this design turns off (§4). Such a design must also decide policy for each request (principle 1).

## 8. Limits

- `upstream_session_id`: 1 to 1024 bytes, `0x21` to `0x7E` only.
- `initialize_result`: at most 16 KiB, without `instructions`.
- Live rows: at most one per pair. A session has at most 32 enabled servers, so it has at most 32 live rows.
- Sweep: at most 256 ended rows per run, 8 `DELETE`s at once, 5 s each.
- Copies: released after 30 minutes with no call. There is no other cap, because live rows bound them.
- Load on servers: a shared or per-user instance now holds one upstream session for each toolgate session that uses it. A server that keeps heavy state per session, such as a browser context, uses more memory. `docs/admin/templates-and-rules.md` says so.
- A pair's first call costs one `initialize` round trip and one insert. Every call costs one indexed read.

## 9. Metrics, audit and traces

One counter, registered with the gateway's other operational metrics and listed in `docs/admin/metrics.md`:
- `toolgate_upstream_session_events_total{event}`. `event` is one of `initialize`, `adopt`, `race_lost`, `not_found`, `delete_sent`, `delete_failed`. Replicas count the first four. The leader counts the last two.

It has no server or instance label. No histogram changes.

No new audit events. `tool.decision` and `tool.result` don't change. The upstream session ID is a value the server made, not a policy fact.

No new spans. The `initialize` and the claim run inside `toolgate.forward`.

## 10. Acceptance

These are the user-visible scenarios from the issue. The design is done when each passes.

1. **One upstream session per pair on every replica.** A toolgate session calls a stateful server through two replicas, with no ingress affinity. Every call reaches the same upstream session, so state that the server keeps for the session is visible from both replicas.
2. **No state shared between toolgate sessions.** Two toolgate sessions call one shared or per-user instance. Each has its own upstream session, and neither sees the other's state.
3. **Server restart.** After the server forgets its sessions and answers 404, the next call succeeds. The gateway starts a new upstream session and retries once. The old state is lost.
4. **Clean-up.** When the toolgate session ends, or disables the server, or the instance stops, the gateway sends `DELETE` for the upstream session, best effort, and removes its row.
5. **Sessionless servers.** A server that returns no `Mcp-Session-Id` works as today, and nothing is stored.
6. **2026-07-28 servers.** They work as today, and nothing is stored.
7. **Remote servers.** Scenarios 1 to 4 hold for a remote server too.
8. **Per-session instances.** They get exactly one upstream session.

## 11. Tests

Names describe the behaviour. Tests marked with -count=50 must pass `go test -count=50 -race`.

The test MCP server (`test/mcpserver`) gains two tools:
- `session_id` returns the `Mcp-Session-Id` of the request's session (`request.Session.ID()`).
- `live_sessions` returns how many sessions the server holds (`Server.Sessions()`).

**Unit** (`internal/runtime`):
- `TestAdoptedSessionSendsStoredIDWithoutInitialize`: a server behind a recording handler sees no `initialize` and no `notifications/initialized`, and sees the stored ID on `tools/call`.
- `TestForwardingCopiesReleaseWithoutDelete`: `Release()` sends no `DELETE`, and `End()` sends one.
- `TestForwardingCopiesOpenNoGetStream`: the server sees no GET.
- `TestCallToolAfterNotFoundMatchesErrSessionMissing`: go-sdk v1.8.0 returns an error matching `mcp.ErrSessionMissing` for an HTTP 404 with a plain-text body to `tools/call`. If it doesn't, the worker hands back `HANDOFF: question`.
- `TestNotFoundWithJSONRPCBodyIsDetected`: a 404 with the body `{"jsonrpc":"2.0","error":{"code":-32001,"message":"Session not found"},"id":null}` sets the call's `SessionNotFound` flag. If the go-sdk doesn't pass the call's context to the HTTP request, the worker hands back `HANDOFF: question`.

**Integration** (`internal/gateway`, two replicas in one process, as the cross-replica tests do, with an in-process go-sdk upstream that records session IDs and `DELETE`s):
- `TestForwardUsesOneUpstreamSessionAcrossReplicas`
- `TestEachToolgateSessionGetsItsOwnUpstreamSession`
- `TestFirstForwardRaceKeepsOneUpstreamSession` (-count=50)
- `TestConcurrentFirstForwardsOnOneReplicaInitializeOnce` (-count=50)
- `TestForwardStartsNewUpstreamSessionAfterRestart`, with a subtest for a 404 with a JSON-RPC error body
- `TestForwardRetriesOnlyOnceAfterNotFound`
- `TestConcurrentNotFoundOnTwoReplicasKeepsOneRow` (-count=50)
- `TestDisableEndsUpstreamSession`
- `TestEnableAfterDisableStartsNewUpstreamSession`
- `TestDisableDuringFirstForwardLeavesNoLiveRow` (-count=50), with a subtest for a session end in place of the disable
- `TestSessionEndEndsItsUpstreamSessions`
- `TestResumedSessionGetsNewUpstreamSession`
- `TestPerSessionInstanceHasOneUpstreamSession`
- `TestSessionlessUpstreamStoresNothing`
- `TestStatelessRevisionUpstreamStoresNothing`
- `TestRemoteServerUsesOneUpstreamSessionAcrossReplicas`, with a subtest for a refused credential
- `TestInvalidUpstreamSessionIDFailsTheCall`
- `TestFailedClaimLeavesNoRow`: the claim transaction fails after `initialize`. No row is stored, and the next forward starts a new session.
- `TestLostRaceWithNoLiveRowForwardsOnceAndEnds` (-count=50)
- `TestSessionlessFactIsForgottenWhenInstanceLeavesReady`
- `TestDisableReleasesCopyOnEveryReplica`

**Integration** (`internal/runtime`, the sweep against PostgreSQL and a recording upstream):
- `TestInstanceStopRemovesRowsWithoutDelete`
- `TestSweepEndsRemoteUpstreamSessionAfterStop`
- `TestSweepAfterLeaderChangeIsSafeToRepeat`

**E2E** (`test/e2e`, two gateway replicas, requests sent to a chosen replica with the switchable transport of the shared-session test):
- `TestStatefulServerKeepsOneUpstreamSessionAcrossReplicas`: calls of `session_id` through both replicas return one ID. A second toolgate session gets another ID. After a disable, `live_sessions` drops within 60 s.

## 12. Choices made

- **The row is read on every forward, not cached.** The issue allows a cache. A cache would need invalidation on disable, session end and 404 on every replica. One indexed read per call is small next to the two audit rows each call writes. Copies are still cached, keyed by the stored ID, so a changed row makes a new copy by itself.
- **The leader sends the `DELETE`s, not the replica that disabled.** Disable and session end only set `ended_at`. One place then sends every `DELETE`, with the owner's headers for remote servers, and a slow server never delays a disable. The cost is the delay of one sweep.
- **Adopting a session short-cuts `initialize` in an HTTP wrapper.** go-sdk v1.8.0 can't start a client session on a known ID through public API. The wrapper answers `initialize` with the server's own stored result, encoded by the go-sdk's `jsonrpc` package. The other choice, a fork of the go-sdk or a hand-written Streamable HTTP client, breaks the rules on protocol code.
- **Forwarding copies open no GET stream.** Nothing reads it today, and one stream per pair and replica costs connections.
- **Server-to-client requests stay as today** (§7), because `SPEC.md` §16 keeps sampling and roots out.
- **A sessionless server is remembered in each replica's memory, not stored.** One extra `initialize` per replica and instance is cheaper than a column the reconciler writes. The fact is forgotten when the instance leaves `ready` (row 349), because the sweep can restart an instance ID at a new generation.
- **A 404 is detected in the HTTP wrapper too** (row 350). The go-sdk recognizes only a plain-text 404 as a lost session. The wrapper reads only the status and the request's `Mcp-Session-Id` header, so it implements no protocol.
- **The sweep's first statement scans the live rows** (§5). An index on changed pairs would need state about the last run. The scan is small at homelab size.
- **Phase 1 row 4 changes for disable and session end.** A call in flight when its pair ends can be cut off by the sweep's `DELETE` (rows 294 and 298). Holding the `DELETE` until in-flight calls finish needs a count of calls across replicas.
- **The claim re-checks the pair under the `sessions` row lock.** Otherwise a first forward that races a disable leaves a live row, and a quick re-enable reuses old state (row 296).
- **Per-session instances use the same table.** They get one row, and no special case.
- **A stopped in-cluster instance gets no `DELETE`.** Its pods are going away. A remote server always gets one.
- **Over-long IDs and large results fail the call** instead of storing a reduced value. No known server comes near the limits.
- **The phase 1 statement is in §3, "Call", step 5,** not §5 as the brief said. That step now points to this document.

## 13. Not in scope

- Routing sampling, elicitation or roots requests from servers to clients (§7).
- Changing clients' own `Mcp-Session-Id`s (phase 1 §3).
- The sidecar mapping proxy (the issue's option B).
- Keeping server state across a server restart.
- Retrying a `DELETE` that failed.
- A gauge of live upstream sessions.
- Per-replica caps on copies.
