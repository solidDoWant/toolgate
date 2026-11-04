# Phase 1 design

This document decides the behaviour `SPEC.md` leaves open for phase 1. Where they disagree, stop and ask (`HANDOFF: question`). Workers implement it and never change its rules. The operator and owner change it.

## 1. Who does what

There are two kinds of work, done by different code:

- **Request handling** runs on every replica. It authenticates, decides policy, reads and writes PostgreSQL, forwards tool calls, and holds clients' MCP streams. It **never** creates, changes or deletes Kubernetes objects.
- **The runtime reconciler** runs on exactly one replica at a time, the holder of a Kubernetes Lease (controller-runtime leader election). It is the only code that touches instance objects in Kubernetes. It drives each instance from its desired state to its actual state, stops idle instances, ends idle sessions, and removes orphans.

Request handlers express intent by writing rows. The reconciler acts on rows and reports back by updating them. Every row change is followed by `NOTIFY toolgate_events, '<kind>:<id>'`, and every replica `LISTEN`s.

Why: one actor on Kubernetes means two replicas can never race to create or delete the same instance. Every interaction below reduces to "a row changed, then the reconciler or a replica reacted", in commit order.

## 2. State

### Sessions

`sessions(principal, session_id, workspace, attestation, created_at, last_seen_at, ended_at)`. The primary key is (principal, session_id).
- Any authenticated request carrying the pair upserts the row and moves `last_seen_at` forward, at most once a minute per session.
- The reconciler ends sessions idle for longer than `sessionIdleTimeout` by setting `ended_at`.
- A request for an ended session starts a new one: it clears `ended_at` and leaves the enabled set empty.

`session_servers(principal, session_id, template_ns, template_name, enabled_at)` is the enabled set.

### Instances

`instances(id, template_ns, template_name, generation, scope_key, desired, status, reason, namespace, tools, ready_at, last_used_at, updated_at)`.
- `scope_key` is `session:<principal>:<session_id>`, `user:<principal>`, or `shared`. It is unique together with the template among instances whose `status` isn't `stopped`.
- `desired` is `running` or `stopped`.
- `status` is one of `pending`, `starting`, `ready`, `failed`, `stopping`, `stopped`.
- `tools` holds the tool list fetched when the instance became ready.

Status changes, and who makes them:

| From | To | By | When |
|---|---|---|---|
| (none) | `pending` | request handler | enable finds no live instance for the scope key |
| `pending` | `starting` | reconciler | it has rendered and applied the objects |
| `starting` | `ready` | reconciler | kstatus reports every object current, and MCP `initialize` plus `tools/list` succeed |
| `starting` | `failed` | reconciler | `startTimeout` passes, kstatus reports a failure, or rendering fails |
| `ready` | `failed` | reconciler | kstatus reports the objects failed, or 3 consecutive upstream health checks fail |
| `failed` | `pending` | request handler | a new enable, for the same scope key, after a failure |
| any but `stopped` | `stopping` | reconciler | `desired` is `stopped` |
| `stopping` | `stopped` | reconciler | the namespace or labelled objects are gone |

A request handler sets `desired = stopped`; the reconciler does the rest. Rows in `stopped` are kept for 7 days for the audit trail, then deleted.

### When an instance should stop (computed by the reconciler)

- **per-session:** no `session_servers` row refers to it, or its session ended.
- **per-user:** no live session of the principal has it enabled, and `last_used_at` is older than `idleStop`.
- **shared:** `last_used_at` is older than `idleStop`, and `idleStop` isn't `0`.
- **any scope:** its template was deleted or its access no longer includes the owner principal (shared: no one).

## 3. Enable, call and list

**Enable** (`toolgate_enable_server`):
1. Decide policy. If refused, return an error naming the rule.
2. In one transaction:
   - upsert `session_servers`;
   - `SELECT … FOR UPDATE` the live instance for (template, scope key);
   - if there is none, or it's `failed`, insert or reset it to `pending` with the template's current generation;
   - commit, then `NOTIFY`.
3. Wait for the instance to be `ready` or `failed`, woken by `NOTIFY` and re-reading the row each time. Send a progress notification on every status change, and at least every 10 seconds. Stop waiting at `startTimeout`.
4. `ready`: send `list_changed` and return the tool names. `failed`: return the reason, and leave the `session_servers` row in place so a later enable retries.

**Call** (`<server>__<tool>`):
1. Find the session's enabled server and its instance.
2. Decide policy again. Calls are never decided from cached results older than the last `NOTIFY` this replica processed.
3. Write the `tool.decision` audit row, then forward on this replica's upstream session for the instance. A replica keeps one upstream MCP client session per instance, created on first use.
4. Write `tool.result`, and update `last_used_at` (at most once a minute).

If the instance isn't `ready`, the call fails at once with its status and reason.

**List** (`tools/list`): the meta tools, plus, for each enabled server with a `ready` instance, its `tools` minus those the policy refuses for this identity.

**Upstream tool changes.** When an instance's upstream session announces `list_changed`, the replica re-fetches the list, writes `instances.tools` and `NOTIFY`s. Every replica then sends `list_changed` to its streams whose sessions enable that server.

## 4. Interaction table

Each row is one event combined with one operation already in flight, in the order PostgreSQL commits them. Each row's covering test proves its rule.

| # | Event | In flight | Rule | Test |
|---|---|---|---|---|
| 1 | Two sessions of one user enable a per-user server | each other | The `FOR UPDATE` on (template, scope key) serializes them. The second finds the first's row and waits on it. One instance. | AC-1-13 |
| 2 | Same session enables the same server twice | first enable waiting | The second sees the existing row and waits on the same instance. Both return the same result. | unit |
| 3 | Disable | enable waiting | The waiting enable gets a `disabled` error. The instance is stopped by the reconciler if nothing else uses it. | integration |
| 4 | Disable | call in flight | The call finishes (it was already allowed and forwarded). Later calls are refused. The tools are withdrawn with `list_changed`. | integration |
| 5 | Rule added that blocks the server or tool | enable waiting | The enable re-decides policy when the instance becomes ready, and returns the refusal. | AC-1-25 |
| 6 | Rule added | call in flight | The call finishes. The next call is refused. Open sessions get `list_changed` within 5 s. | AC-1-25 |
| 7 | Rule removed | open sessions | Open sessions get `list_changed` and the tools come back. No re-enable needed. | unit |
| 8 | Template changed (new generation) | instance running | It keeps its generation and shows as outdated. New instances use the new generation. | AC-1-19 |
| 9 | Template changed | instance `pending` | The reconciler renders whatever generation is current when it applies, and records it. | unit |
| 10 | Template deleted or becomes invalid | instance running | The reconciler stops it. Its sessions get `list_changed`. The last valid version is used only for instances that are already running and only until they stop (`SPEC.md` §8). | integration |
| 11 | Access to a template removed from a user | instance running | Calls are refused at once by policy. The reconciler stops the user's per-session and per-user instances. | integration |
| 12 | Instance fails after `ready` | calls in flight | In-flight calls get the upstream error. Later calls get the `failed` reason. The session keeps the server enabled. The next enable retries (`failed` to `pending`). | integration |
| 13 | Session ends (idle) | instance `starting` | The reconciler computes that no session needs it and stops it once it is applied. No orphan objects remain. | AC-1-15 |
| 14 | Replica holding the stream dies | enable waiting | The client reconnects to another replica. The `session_servers` row exists, so `tools/list` shows the server once ready. Calling enable again waits on the same instance. | AC-1-17 |
| 15 | Leader dies | instance `pending` or `starting` | A new leader takes the Lease and continues from the rows and the labelled objects. Applying is idempotent (server-side apply). | e2e |
| 16 | Leader dies | instance `stopping` | The new leader deletes again. Deletion is idempotent. | e2e |
| 17 | Gateway restarts entirely | everything | The reconciler lists objects carrying toolgate's labels. Objects with no live instance row are deleted. Rows with no objects and `status` `ready` or `starting` go back to `pending`. | e2e |
| 18 | Secret deleted by the user | instance running | Running instances keep the value they started with. The next start fails with the missing-secret reason. | unit |
| 19 | Secret changed by the user | instance running | Same as 18: running instances aren't restarted in phase 1. `toolgate_server_status` shows that the secret changed since start. | unit |
| 20 | Upstream announces `list_changed` | open sessions | §3, "Upstream tool changes". | integration |
| 21 | Session reconnects after its session ended | — | A new session starts with an empty enabled set (§2). The model must enable again. | unit |
| 22 | Client sends a different workspace on the same session | — | `sessions.workspace` keeps its first value. A later request with a different workspace is refused with 400, so a session can't change workspace to escape a rule. | unit |

Any combination not in this table is a design gap: hand back `HANDOFF: question` with the case.

## 5. Settings sync

- The hook calls `GET /api/v1/settings`. The gateway merges every matching `SettingsProfile`, in name order; a later profile's key wins.
- The hook then compares three things for each key: the file's current value, the value it wrote last time (from `~/.config/toolgate/owned-settings.json`), and the new value.
- It writes only keys whose file value still equals what it last wrote, or that it has never written and that are absent from the file. It reports every other key as user-changed.
- Writing is atomic (a temporary file, then rename).
- Keys are top-level settings keys, plus one level inside `permissions` and `env`.

## 6. Settled choices

- **Health checks:** the reconciler sends MCP `ping` to each `ready` instance every 30 s. Three failures in a row mark it `failed`.
- **Limits:** request body 4 MiB; tool result forwarded up to 16 MiB (larger ones are refused with an error); at most 32 enabled servers per session; header values 128 characters.
- **Errors to the model** are short sentences naming what to do next, for example "Secret `github-token` isn't set. Run `toolgate secret set github-token`."
