# Phase 4 design

This document decides the behaviour `SPEC.md` leaves open for phase 4. It adds to `docs/design/phase-1.md`, `docs/design/phase-2.md` and `docs/design/phase-3.md`, which still hold unless a section here says otherwise. Where they disagree, stop and ask (`HANDOFF: question`). Workers implement it and never change its rules. The operator and owner change it.

The target is `docs/acceptance/phase-4.md`: AC-4-12 and AC-4-13 (tool catalog and lazy start), AC-4-01 to AC-4-03 (tool search), AC-4-04 and AC-4-05 (curation), AC-4-14 to AC-4-17 (delegated access), AC-4-11 (unverified allow list), AC-4-07 and AC-4-08 (MCP revisions), AC-4-09 and AC-4-10 (connectors) and AC-4-06 (release).

Words used in this document:
- **Tool catalog:** the tool list toolgate keeps for each server template, so it can list and search a server's tools while nothing runs. It isn't the template catalog of phase 1, which is the set of `MCPServerTemplate` objects.
- **Upstream name:** the name a tool has on its own MCP server.
- **Exposed name:** the name clients see after curation (§6). Without an override it equals the upstream name.
- **Revision:** an MCP protocol revision, such as 2025-11-25.

## 1. Who does what

Phase 1's split stays (phase 1 §1). Request handling runs on every replica and never touches Kubernetes objects. The runtime reconciler runs only on the Lease holder, and it is still the only code that touches Kubernetes. Phase 4 adds no Kubernetes objects and no special handling for any kind.

**New request-handling work**, on every replica:
- serving catalog tools of servers that aren't running, and starting them on the first call that needs them (§3);
- calls of tools whose server isn't enabled yet: the call enables it (§4);
- `toolgate_find_tools`, which reads the search rows in PostgreSQL (§5);
- applying curation to every tool list and call (§6);
- the built-in `toolgate` server and its three tools, which call the same Go code as the CLI's API and the web UI (§7);
- routing each MCP request to the 2025-11-25 handler or the 2026-07-28 handler (§8);
- the OAuth protected resource metadata and the `WWW-Authenticate` header, and sessions tied to `Mcp-Session-Id` (§9).

**New reconciler work**, on the leader only:
- storing the tool catalog when an instance becomes ready or its tools change (§3);
- keeping the search rows current when a template or the tool catalog changes, and rebuilding them all when it gets the Lease (§5);
- choosing each instance's revision when it starts, and monitoring it on that revision (§8).

**Notifications.** One new kind: `toolsearch:<template uid>`, sent when the leader rebuilds a template's search rows. A replica sends `list_changed` to its streams in search mode whose found tools include that template (§5). The other changes reuse existing kinds: `instance:<id>`, `session:<id>` and `scope:<id>`. Phase 1's rules for missed notifications hold.

**PostgreSQL stays the only coordination channel.** Nothing in this phase shares memory between replicas.

## 2. State

One additive migration, `00007_phase_4.sql` (phase 1 §7, "Database migrations"). New IDs follow phase 2 §2.

**Instances.** `instances.mcp_revision text NOT NULL DEFAULT ''`. The reconciler writes it in the `starting` → `ready` write (§8). An empty value means the row was made ready by a 0.3 leader.

**Tool catalog.** `tool_catalog(template_uid, generation, template_ns, template_name, tools, instance_id, learned_at)`, primary key `(template_uid, generation)`.
- `tools` is the upstream tool list as the instance served it, after phase 1's tool list checks, at most 1 MiB. It's stored before curation, so a curation change needs no new learning.
- Only the reconciler writes it (§3).

**Search rows.** `tool_search(template_uid, template_ns, template_name, source_generation, upstream_name, exposed_name, document, rebuilt_at)`, primary key `(template_uid, upstream_name)`, with a GIN index on `document` (`tsvector`). Only the reconciler writes them (§5).

**Found tools.** `session_found_tools(session_pk, template_ns, template_name, upstream_name, found_at)`, primary key `(session_pk, template_ns, template_name, upstream_name)`. At most 50 per session. Search mode lists them (§5).

**Scope entries.** `scope_entries(id bigserial, scope_id, workspace, session_pk, server, tool, effect, created_at, request_id)` (§7).
- `scope_id` refers to `user_scopes.id`.
- Exactly one of `workspace` and `session_pk` is set.
- `tool` is an upstream name, or empty for the whole server.
- `effect` is `allow` or `deny`.
- Unique on `(scope_id, coalesce(workspace, ''), coalesce(session_pk, 0), server, tool, effect)`.
- At most 500 rows per principal.

**Unverified allow list.** `unverified_servers(scope_id, server, added_at)`, primary key `(scope_id, server)`, at most 64 rows per principal. `user_scopes.unverified_default` accepts `list` as well as `none` and `all`: the migration drops the column's check and adds the wider one.

**Access requests.** `access_requests.kind` accepts `scope` as well as `server` and `change`, for the changes of §7 that phase 3's `change` JSON can't describe. The migration drops the kind check and the shape check, and adds them back with the new kind.
- Why a new kind and not more fields in `change`: a 0.3 replica that read a session-scoped or tool-level change as a phase 3 `change` would apply the wrong change. A 0.3 replica doesn't know `scope` rows. Its approval page answers 404 for them until the rollout ends (row 221).

**Ending a session** (phase 1 §2, changed by phase 3 §10) also deletes its `session_found_tools` and its `scope_entries`, and expires its pending `scope` requests that target it, writing `access.expire` for each. All of this is in the same transaction.

**Cleanup.** On its 30 s pass, the leader deletes `tool_catalog` rows of template UIDs that no longer exist, and, for each UID, every row older than the newest two generations. It deletes `tool_search` rows of template UIDs that no longer exist.

## 3. Tool catalog and lazy start

AC-4-12 and AC-4-13.

### The catalog list of a template

The **catalog list** of a template is the first of these that exists:
1. `spec.declaredTools` of the current template;
2. the `tool_catalog` row for the template's UID and current generation;
3. the `tool_catalog` row for the template's UID with the newest older generation.

If none exists, the template has no catalog list. Such a server behaves as in phase 3: an enable or a call starts it and waits, and it can't be found by search until an instance becomes ready once.

- **Why declared tools come first:** they are the admin's explicit statement, and they let an admin stop toolgate from learning. That matters for a per-user server whose tools depend on the user. The catalog is kept for each template generation, not for each user, so a list learned from Alice's instance is shown to Bob until Bob's own instance is ready. Tool names and descriptions can then cross users. `docs/admin/security-model.md` says so, and tells admins to declare tools for such servers.
- **Why an older generation counts:** most template changes don't change a server's tools. Without the fallback, every template edit would hide the server from search until an instance starts. A wrong list is corrected when an instance becomes ready (below).

**Declared tools.** A new template field:

```yaml
spec:
  declaredTools:            # optional
    - name: create_issue
      description: Create an issue in a repository.
      inputSchema: {type: object, properties: {repo: {type: string}, title: {type: string}}}
```

- At most 512 items. Each name is unique. `inputSchema` is an object with `x-kubernetes-preserve-unknown-fields`. The whole field is at most 1 MiB, checked by the catalog.
- Phase 1's tool list checks apply with the template's curation (§6). A list that fails them makes the template invalid, with the reason in `Valid`.

**Learning.** The reconciler writes the `tool_catalog` row for the instance's template UID and generation:
- in the `starting` → `ready` write (`PostgresStartStore.MarkReady`), in the same transaction;
- in each tool-list change of a `ready` instance (`PostgresStartStore.SetTools`), in the same transaction.

The write is an upsert with `learned_at = now()`. It doesn't happen when the template declares tools. So the row holds the list of the last instance of that generation that became ready or changed its list. Each write also rebuilds the template's search rows (§5) in the same transaction.

### Lists while nothing runs

`tools/list` (phase 1 §3, "List") changes for each enabled server:

| The server's state for this session's scope key | Tools listed |
|---|---|
| A `ready`, healthy live instance | The instance's `tools`, curated, as today |
| A live instance that is `pending` or `starting` | The catalog list, curated |
| No live instance, and the newest row didn't fail | The catalog list, curated |
| No live instance, and the newest row has `failed_at` set | None, as phase 1 row 12 |
| A `ready` instance that is unhealthy | None, as phase 3 §6 |

Each tool still goes through the policy decision, as today. A server without a catalog list contributes no tools until its instance is ready.

**When the real list differs.** The instance's `ready` write and its `instance:<id>` notification already make every replica send `list_changed` to the sessions that enable the template (phase 1 §3, "Fan-out"). Those sessions then list the instance's own tools. The catalog row is written in the same transaction, so sessions of other scope keys list the new catalog list too. This is AC-4-12's last sentence.

### Starting

A new template field: `spec.start`, `onCall` (the default) or `onEnable`.

- **`onCall` with a catalog list.** Nothing starts until a call needs the instance:
  - **Enable** (`toolgate_enable_server`) runs phase 1 enable steps 1 to 3. In step 4 it locks the `sessions` row and upserts `session_servers`, but it inserts no instance. It writes `server.enable` (allow), notifies, commits, sends `list_changed`, and returns the catalog list's exposed names. It doesn't wait.
  - **Resume** (phase 3 §11) restores the server without inserting an instance.
  - **A call** inserts the instance when there is no live one (§4, step 5).
- **`onEnable`, or no catalog list.** Enable and resume work as in phase 3: they insert or reuse the instance, and enable waits for it.

So "a session may use the server" in AC-4-13 means "the server is enabled in the session". Enabling covers an explicit enable, a call's implicit enable (§4) and a resume. Starting a server for every session that could enable it would start every server for every user.

**The call waits** with phase 3 §11's rule, unchanged: `waitForCall` in `internal/gateway/forward.go`, through `waitForInstance` in `internal/gateway/wait.go`. It is woken by notifications and the 10 s tick, sends a progress notification on each status change and at least every 10 s, and stops at `startTimeout`. When the instance is `ready`, the call is decided again and forwarded. A start failure or a timeout fails the call with the reason, and writes `tool.decision` (refuse, `not-ready`, with the reason).

**A failed server isn't retried by calls.** If the newest row for (template, scope key) failed and no live row exists, a call is refused at once with the failure reason and "Enable `<server>` again to retry." An explicit enable inserts a new instance, as in phase 1 step 7. So a model that retries a call can't start a broken server again and again. A server that stopped for any other reason, such as an idle stop, starts on the next call.

**Idle stops** don't change (phase 1 §2, phase 3 §11). With `onCall`, a stopped instance starts again on the next call.

## 4. Calls without an enable

AC-4-01 and part of AC-4-02. A call of a tool of an allowed server enables the server for the session. There is no separate enable step.

**Call** (`<server>__<tool>`) replaces phase 1 §3's call steps 1 to 3. Phase 1 steps 4 to 6 and phase 2 §4 (approvals) follow unchanged.
1. **Find the server and the tool.** Find the template by the server name. Map the exposed tool name to its upstream name with the template's curation (§6). If the template doesn't exist, or the tool is hidden or renamed away, refuse with reason `not-offered`: "Server `github` has no tool `delete_repo`." This writes `tool.decision`. The call carries the upstream name from here on.
2. **Decide policy** with the request's own identity and fresh state, for the upstream name. If steps 1 to 3 refuse, refuse. If only step 4 refuses, go to step 3 below only when the server isn't enabled yet. Otherwise refuse.
3. **Enable when needed.** If the session has no `session_servers` row for the server, run enable steps 1 to 4 (phase 1 §3, with §3's `onCall` rule above):
   - the step-4 refusal in a verified workspace asks the user, as phase 3 §10's session additions do: a pending `server` request, a URL elicitation, and the wait. On approval the call goes on. Without elicitation the call fails with phase 3's link message. In an unverified session the call is refused at once. Phase 3 §10's rule "Calls of a server outside the effective list never elicit" no longer holds: a call that enables a server asks, as an enable does.
   - a missing secret refuses the call with phase 1's message, and writes `tool.decision` (refuse, `missing-secret`).
   - a 33rd server refuses the call with phase 1's message, and writes `tool.decision` (refuse, `limit`).
   - success writes `server.enable` (allow) with `reason: first-call`, and the call sends `list_changed` after it commits.
4. **Find the live instance** for (template, scope key). If it is `ready` and healthy, go on. If it is `pending` or `starting`, wait (§3). If it is `ready` but unhealthy, refuse with `not-ready` as phase 3 §6.
5. **No live instance.** If the newest row failed, refuse (§3). Otherwise take the advisory lock, check the instance limits and insert a `pending` row, in one transaction with the `sessions` row lock (phase 1 enable step 4's insert part). Then wait (§3). At a limit, refuse with phase 1's message and reason `limit`.
6. **The tool must exist.** When the instance is `ready`, check that its curated list holds the exposed name. If not, refuse with `not-offered`: "Server `github` doesn't offer `create_issue` any more." This covers a catalog list that was out of date.
7. Go on with phase 1 call step 3 (decide again with fresh state) and the steps after it.

**The `toolgate` server's tools** (§7) skip steps 4 to 6. They run in-process.

**Disable** (`toolgate_disable_server`) is unchanged. It removes the server from the session at once (AC-4-17). A later call enables it again, because a call enables any allowed server. A user who wants it gone for good uses `toolgate_deny` (§7).

## 5. Tool search

AC-4-01 to AC-4-03.

### Search mode

- **Turning it on.** A new header, `X-Toolgate-Tool-Search: on` or `off`. When it is absent, the setting `toolSearch` (default `false`) decides. `toolgate headers` sends the header when `TOOLGATE_TOOL_SEARCH` is set. A connector sends no header, so the setting decides for it.
- The mode is read on each request. It's a claimed value, but it only changes what is listed. It never changes a decision, so it can't widen anything.
- **`tools/list` in search mode** is the meta tools, `toolgate_find_tools`, the tools of the session's enabled servers (§3), and the session's found tools. Each tool still goes through the policy decision. It never holds every server's tools.
- **Outside search mode** nothing changes. `toolgate_find_tools` isn't listed, and a call of it is refused with "Tool search is off for this session."

### `toolgate_find_tools`

Input: `{"query": string, "limit": integer}`. `query` is 1 to 2000 characters. `limit` is 1 to 20, default 5.

Output: `{"tools": [{"name", "server", "description", "inputSchema", "access", "approval"}]}`.
- `name` is the proxied exposed name, such as `github__create_issue`.
- `access` is `allowed`, or `ask` for a server that only step 4 refuses in a verified workspace (AC-4-02). The first call of an `ask` tool asks the user (§4, step 3).
- `approval` is `true` for a tool with `approval: required`.

**Candidates.** The search loads the request's policy state once (phase 2 §3), and walks the template catalog:
- A server is a candidate when the server-level decision allows it, or refuses it only at step 4 in a verified workspace. So blocked servers, servers outside the user's access lists, and servers needing an environment the session lacks are never candidates.
- In an unverified session, a step-4 refusal isn't a candidate. Such a session can't ask (phase 3 §10).
- Its tools come from its search rows. The rows already leave out hidden tools (§6).
- The built-in `toolgate` server is a candidate like any other (§7).

**Ranking** uses PostgreSQL full-text search. That's no new dependency and no new backend (`SPEC.md` §3), and it gives every replica the same answer.
- The **document** of a tool is built by the leader. It has four weighted parts, each passed through `to_tsvector('english', …)` and `setweight`:
  - A: the exposed name, with `_`, `-`, `.` and camelCase boundaries turned into spaces, so `createIssue` also gives "create issue";
  - B: the curated description, cut at 4 KiB;
  - C: the server name and the template's `description`;
  - D: the input schema's top-level property names and their descriptions, cut at 4 KiB.
- The **query**: Go lowercases the task description and keeps words of `[a-z0-9]`, at most 64 words. It joins them with ` | ` and passes the result as one parameter to `to_tsquery('english', $1)`. So the query is the OR of the stemmed words. The words hold no `tsquery` syntax, so a query can't inject any.
- **The SQL** selects rows whose `template_uid` is in the candidate set and whose `document @@ query`. It orders them by `ts_rank(document, query, 1)` with the default weights, then by `exposed_name`, and returns at most 100 rows. The statement runs with a 2 s `statement_timeout`.
- Go then decides each returned tool (phase 2 §3) and drops the refused ones. It keeps the first `limit`.

**Found tools.** Claude Code can only call tools that are in its tool list. So the search also writes its results to `session_found_tools`, in one transaction that locks the `sessions` row first.
- New rows push out the oldest, so at most 50 remain.
- If the set changed, the replica sends `list_changed` to the session's streams after the commit.
- A call of a found tool is a normal call (§4). Its first call enables the server and writes `server.enable` with `reason: first-call`.
- The found tools are rows, so a session that moves to another replica keeps them.

**Search isn't an audit event.** It is a list, like `tools/list` (phase 1 §3). Every call of a found tool is decided and recorded as usual.

### Keeping the search rows current

The leader rebuilds a template's search rows in one transaction:
- when it stores a learned list (§3);
- when a template event changes the template's generation, its curation or its declared tools;
- when it gets the Lease, for every valid template.

A rebuild deletes the template's rows and inserts one row per curated tool of the template's catalog list. `source_generation` records which generation the list came from. It notifies `toolsearch:<template uid>`. A template that is deleted or invalid has its rows deleted.

The catalog and rule reconcilers already run on every replica and write only while elected (phase 1 §1). The rebuild is a write, so it follows the same rule.

### Search quality

AC-4-03. The fixture is `test/fixtures/toolsearch/`:
- `servers.json`: at least 50 tools across at least 8 servers. The tool names, descriptions and schemas are copied from real public MCP servers, such as GitHub, Kubernetes, filesystem, mail, calendar and database servers. The file records each source.
- `tasks.json`: at least 50 task descriptions. Each names its expected tool.

One agent writes the fixture and opens it in its own PR before any ranking code merges. The search worker may not change it. So the fixture isn't fitted to the ranking.

The AC-4-03 test is an integration test. It loads the fixture through the real rebuild code into PostgreSQL and runs every task through the real search. It passes when the expected tool is in the top 5 for at least 90 percent of the tasks. If full-text search misses the bar, the worker hands back `HANDOFF: question` with the scores. The worker doesn't add a new search backend.

## 6. Tool curation

AC-4-04 and AC-4-05. Curation is in the template, so admins set it in git.

```yaml
spec:
  toolFilter:                # optional; exactly one of allow and deny
    allow: [create_issue, list_issues]
    # deny: [delete_repository]
  tools:                     # existing per-tool overrides, keyed by upstream name
    create_issue:
      name: open_issue       # new: the exposed name
      description: Open an issue in one of your repositories.   # new
      inputDescriptions:     # new: top-level input properties
        repo: The repository, as owner/name.
```

**Filter.** `allow` lists the upstream names exposed, and every other tool is hidden. `deny` lists the upstream names hidden. Names are exact, at most 512 per list, each at most 128 characters. A hidden tool:
- is absent from every tool list, the catalog list and the search rows;
- is refused at call step 1 (§4) with `not-offered`, by its upstream name or by any exposed name. The refusal writes `tool.decision`, so a call by name is recorded and never forwarded.

**Overrides.**
- `name` must match `^[A-Za-z0-9_.-]{1,128}$`. With the `<server>__` prefix it must be at most 64 characters (phase 1 §3, "Tool list checks"). The catalog checks this when it reads the template, and a bad name makes the template invalid.
- `description` is at most 4096 characters. It replaces the tool's description.
- `inputDescriptions` maps a top-level property name of the input schema to a description, at most 64 entries of 4096 characters each. A property the schema lacks is ignored. Nested schemas aren't changed (§14).
- Overrides apply to a deep copy of the tool. The stored upstream list is never changed.

**One function curates.** `Curate(template, upstream)` in a new package, `internal/toolcatalog`, returns the curated tools with both names. Every place that lists or searches tools calls it: `tools/list`, the catalog list, the search rebuild, the tool list checks and `toolgate_find_tools`. The reverse map, from an exposed name to an upstream name, is built from the overrides alone. So a call can be mapped while no instance runs:
- An exposed name that is some tool's override `name` maps to that tool.
- Any other exposed name maps to itself, unless that tool is hidden or has an override `name`.

**Collisions.** After curation, two tools of one server must not share an exposed name. The tool list checks (phase 1 §3) run on the curated list. So a collision fails the instance, as an over-long name does, with "Tool name `open_issue` of `create_issue` collides with another tool." For declared tools, the collision makes the template invalid.

**Curation is read from the current template**, like `spec.tools.<name>.blocked` is today. A running instance of an older generation gets the new curation at once. So a newly hidden tool is hidden at once, without waiting for a new instance.

**Policy uses the upstream name.**
- `spec.tools` overrides, grants, approvals, scope entries (§7) and the `tool` field of audit events all use the upstream name.
- Audit events also carry `exposedTool` when it differs.
- A `ToolgateRule`'s `tools` glob refuses when it matches `<server>__<upstream name>` **or** `<server>__<exposed name>`. Rules only refuse, so matching both names can refuse more, never allow more. An admin can write either name.

## 7. The `toolgate` server and delegated access

AC-4-14 to AC-4-17, and AC-4-11.

### The server

`toolgate` is a built-in server. Policy treats it like a template, so the rules don't change.
- **It is in the template catalog** of every replica as an in-memory template named `toolgate`, with `scope: shared`. It has no resources, no instance and no catalog row. It is listed only on a gateway with the web UI, because widening needs a step-up (phase 2 §9).
- A real `MCPServerTemplate` named `toolgate` is invalid, with the reason "The server name `toolgate` is reserved for the built-in server." The upgrade notes say so.
- **Access:** every identity of kind `user`. Service accounts and SPIFFE workloads have no scopes (phase 3 §10), so step 1 refuses them.
- **Environment:** `any`. A signed-in user already has the CLI and the UI for these operations, from any machine.
- **Blocks and rules** apply by the name `toolgate`, so an admin can turn it off for chosen users, workspaces or sessions.
- **Workspace scope** applies as for any server. The user allows `toolgate` in a workspace's list, in a default, in the unverified allow list, or for one session. This is how "usable in any session where the user allows the `toolgate` server, and nowhere else" (AC-4-14) holds.
- **It is enabled like any server:** by `toolgate_enable_server`, or by its first call (§4). It counts toward the 32 servers. `toolgate_list_servers` shows it as `ready`.
- **Tools.** Upstream names `list_scopes`, `allow` and `deny`. Exposed names `toolgate_list_scopes`, `toolgate_allow` and `toolgate_deny`, as the checklist names them. They are the only tools whose proxied name isn't `<server>__<tool>`. Rules may name them as `toolgate__allow` or `toolgate_allow` (§6).
- **Calls** go through the full call path: `tool.decision`, phase 2 approvals if an admin's rule set ever needs one, `tool.result`. The operations also write their own events (below).
- **Search rows:** the leader writes them for the built-in server when it gets the Lease, so `toolgate_find_tools` can find these tools.

**Same operations, same checks.** Each tool calls the same methods as `POST /api/v1/workspaces/changes`, `GET /api/v1/workspaces` and `GET /api/v1/sessions`: `gateway.Workspaces` (`internal/gateway/workspaces.go`) and the sessions query. The tool passes the caller's own identity. There are no management sessions and no extra rights. A tool call can do exactly what the same user's CLI can do. The tool doesn't call the HTTP API. It calls the Go methods behind it, so a check can't differ between the two paths.

### `toolgate_list_scopes`

Input: none. Output:

```json
{"current": {"session": "…", "workspace": "coder/alice/dev", "verified": true},
 "defaults": {"verified": "all", "unverified": "list", "unverifiedServers": ["github"]},
 "workspaces": [{"workspace": "coder/alice/dev", "servers": ["github"], "tools": ["k8s__get_pods"], "denied": ["github__delete_repository"]}],
 "sessions": [{"session": "…", "workspace": "…", "verified": true, "lastSeenAt": "…", "allowed": ["immich"], "denied": []}],
 "truncated": false}
```

At most 200 workspaces and 200 sessions. Only the caller's own. Tool entries are shown by exposed name.

### `toolgate_allow`

AC-4-15. Input: `{"workspace": string, "session": string, "servers": [string]}`.
- Exactly one of `workspace` and `session`.
- `servers` holds 1 to 32 items. Each is a server name, or `<server>__<tool>` with the tool's exposed name. The tool maps it to the upstream name (§6).
- A tool that is hidden, or a server the user has no access to, is refused with phase 3's "You don't have access to server `x`." or "Server `x` has no tool `y`." Nothing is written.

The target:
- **A workspace:** any workspace ID that is valid for a list (phase 3 §10: at most 256 characters, not `claimed/…`).
- **A session:** one of the caller's own open sessions, by its session ID, whose workspace is verified. Another user's session, or an ended one, gets "You have no open session `x`.". An unverified session gets "Session `x` has no verified workspace, so it uses only your default for unverified sessions." A claimed session ID never adds servers (phase 3 §10).

Then:
1. Insert a pending access request of kind `scope`, with the change, the target and its workspace, and write `access.request`. Notify and commit. The limits of phase 3 apply (100 pending per principal).
2. With URL elicitation, return it with `RequestState` set to the request ID, as phase 3 §10 step 3. On `accept`, wait as phase 3 §10 step 4 waits: on `approval:<id>`, missed events and the 10 s tick, up to `approvalTimeout`. Return `{"status": "approved"|"denied"|"expired", "request", "url"}`. The wait counts toward phase 2's 32 waits per principal per replica.
3. Without elicitation, or on `decline` or `cancel`, return `{"status": "pending", "request", "url"}` and "Approve this change at `<url>`." This isn't a tool error: the request exists.

**Nothing changes until approval.** The page `/ui/approvals/<id>` shows a `scope` request as phase 3 shows a `change` request: the target, a before-and-after list, and which session asked. Approve needs the phase 2 step-up. Approving applies the change in the same transaction as the compare-and-set:
- **Workspace, server:** add it to the workspace's list, creating the list if there was none (phase 3), and delete the workspace's `deny` entries for that server.
- **Workspace, tool:** delete a `deny` entry for that tool. If the server isn't reachable in that workspace by its list or default, insert an `allow` entry for the tool.
- **Session, server:** insert `session_additions` (phase 3), and delete the session's `deny` entries for the server.
- **Session, tool:** as for a workspace, with the session's entries.
- A **session** target is checked as phase 3 §10 step 6 checks it: the session must still be open, in the same verified workspace. Otherwise the transaction rolls back and the page says why.
- Notify `scope:<id>` for a workspace and `session:<id>` for a session. Write `workspace.change` or `session.addition` with `via` set to the request ID, and `access.approve`.

**A session-scoped allow ends with the session.** Ending a session deletes its `session_additions` and its `scope_entries`, and expires its pending `scope` requests (§2).

### `toolgate_deny`

AC-4-16. Input as `toolgate_allow`. A deny narrows, so it applies at once, with no request and no step-up. One transaction locks the target, then the `user_scopes` row, in phase 3's lock order:
- **Workspace, server:** remove it from the list if it is listed, insert a `deny` entry for the server, and delete the server's `allow` entries.
- **Workspace, tool:** insert a `deny` entry for the tool, and delete its `allow` entry.
- **Session, server:** delete its `session_additions` row, insert a session `deny` entry, and delete its `allow` entries.
- **Session, tool:** insert a session `deny` entry for the tool.

Then write `workspace.change` or `session.addition` (with `denied`), notify `scope:<id>` or `session:<id>`, and commit. A deny entry works with any default, including `all` with no list. So phase 3's 409 for "deny on a workspace with no list" doesn't apply to these tools, or to the CLI and API after this phase (§12).

A session target for a deny may be unverified, because a deny only narrows. A claimed ID can narrow, never widen (principle 2). The session must still be the caller's own.

**The 5 s bound.** The notification makes every replica send `list_changed` to the target's streams. The next decision loads fresh state. So the target's tools are withdrawn, and its calls refused, within 5 s (phase 3 §10's bound). The AC-4-16 test measures it across two replicas.

### Step 4 with scope entries

`policy.ScopeRows` gains the request's entries: the verified workspace's entries, the session's entries, and the unverified allow list. `policy.Scope` gains tool sets. For a request of kind `user`, step 4 decides (server, tool):
1. **Denied** if a `deny` entry of the verified workspace or of the session names the server with an empty tool, or names this tool. The refusal reads "`github__delete_repository` is denied in workspace `coder/alice/dev`." or "… in this session."
2. **Allowed for the whole server** if phase 3's effective list allows the server.
3. **Allowed for this tool** if an `allow` entry of the verified workspace or of the session names this tool.
4. Otherwise refused, as in phase 3.

A server-level decision (`list_servers`, enable, search candidates) passes when rule 2 holds, or when rule 3 holds for any tool of the server, and no server-level deny matches. `tools/list` then lists only the allowed tools.

In an unverified session, only the session's `deny` entries apply, and the unverified default or allow list decides rule 2. `allow` entries never apply. Workspace entries never apply, because the workspace is claimed. So a claimed field only narrows.

### Own scope

AC-4-17 needs no new tool. In any session, `toolgate_disable_server` drops a server at once (§4). In a verified workspace, an enable or a call of a server outside the list asks the user (§4, step 3). Both are meta-tool paths and don't need the `toolgate` server.

### Unverified allow list

AC-4-11. A user's default for unverified sessions is `none`, `all` or `list`.
- With `list`, an unverified session's effective list is the user's `unverified_servers`. It lists and can enable only those servers. Nothing else is added: unverified sessions still have no session additions.
- **Web UI:** the defaults form on the Workspaces page gains "Only these servers", with a checkbox for each server the user can access.
- **CLI:** `toolgate workspace defaults --unverified none|all|list`, and `toolgate workspace allow --unverified SERVER…` and `deny --unverified SERVER…`.
- Adding a server, or moving from `none` to `list` with servers, widens, so it creates an access request and needs the step-up. Removing narrows and applies at once. These are phase 3 §10's rules.
- A change notifies `scope:<id>`, so open sessions get `list_changed` within 5 s.
- The setting `list` with an empty list gives nothing, like `none`.

## 8. MCP protocol revisions

AC-4-07 and AC-4-08. toolgate prefers 2025-11-25 on both sides. It uses 2026-07-28 only with a peer that supports nothing older.

### What the go-sdk gives, and what toolgate does

`go.mod` pins `github.com/modelcontextprotocol/go-sdk` v1.8.0. That version supports 2026-07-28: `server/discover`, the per-request `_meta` fields, `subscriptions/listen`, the `cacheScope` and `ttlMs` fields through `ServerOptions.SetCacheable`, and the multi round-trip results phase 2 already uses. Its limits decide what toolgate has to do itself:
- Its `Client.Connect` tries 2026-07-28 first by default, with `server/discover`, and falls back to `initialize`. So today, toolgate reaches an upstream that speaks both revisions on 2026-07-28. Phase 4 must connect on 2025-11-25 first.
- Its `StreamableHTTPHandler` serves 2026-07-28 only with `Stateless: true`, and a stateless handler has no MCP sessions. So toolgate runs two handlers and routes each request.
- A stateful handler answers `server/discover` with only the revisions before 2026-07-28. A client that speaks both then falls back to `initialize`.
- 2026-07-28 has no `ping`.

toolgate writes no protocol code. It chooses options, routes requests and classifies errors.

### Upstream: choosing an instance's revision

One function, `runtime.ConnectUpstream(ctx, endpoint, revision, options)` in a new file `internal/runtime/revision.go`, connects every upstream session. The three callers are the readiness probe (`MCPToolLister`), the monitor (`MCPMonitorConnector`) and forwarding (`upstreams.session` in `internal/gateway/forward.go`).

**The probe**, at `starting` → `ready`:
1. Connect with `ClientSessionOptions.ProtocolVersion` set to `2025-11-25`, which makes the go-sdk send `initialize` and skip `server/discover`. Then run `tools/list`. If both succeed, the revision is the one `initialize` negotiated, usually 2025-11-25.
2. If step 1 failed with a **version answer**, connect with `ProtocolVersion` set to `2026-07-28`. The go-sdk sends `server/discover`. If it negotiates 2026-07-28 and `tools/list` succeeds, the revision is 2026-07-28.
3. Any other failure fails the probe, as in phase 1. The instance stays `starting`, and the next step probes again until `startTimeout`. The failure reason names both attempts when step 2 ran.

A **version answer** means the server is up and refused the 2025-11-25 handshake:
- a JSON-RPC error response to `initialize`, such as `-32601` or the go-sdk's `CodeUnsupportedProtocolVersion`;
- HTTP 400, 404 or 405 to `initialize`;
- an `initialize` result with a revision the go-sdk refuses;
- HTTP 400 to the first request after `initialize`, which is how a go-sdk server limited to 2026-07-28 answers.

Connection errors, timeouts, HTTP 429 and 5xx are not version answers. The worker classifies with the go-sdk's typed errors (`*jsonrpc.Error` and the transport's HTTP status error). If the pinned version doesn't expose the HTTP status as a type, the worker hands back `HANDOFF: question` and never matches error text.

**The revision is fixed for the row.** The `ready` write stores it in `instances.mcp_revision`. A row never changes it, so a later transient failure can't move the instance to the other revision (AC-4-07). A replacement row probes again.
- A row with an empty `mcp_revision` (made ready by 0.3) is connected with the probe order on each connect.

**Forwarding** connects with the row's revision. On 2026-07-28, each call is its own request with the `_meta` fields, which the go-sdk adds.

**Monitoring** (phase 1 §3, "Upstream tool changes", and phase 1 §7, "Health checks"):
- **2025-11-25:** unchanged. The monitor pings, and gets `list_changed` on the session's stream.
- **2026-07-28:** there's no `ping`. Every 30 s the monitor connects a fresh client with no handlers, which sends `server/discover`, and closes it. A success counts as a passed ping, and three failures in a row fail the instance, as in phase 1. The monitoring session is connected with a `ToolListChangedHandler`, so the go-sdk opens `subscriptions/listen` for `toolsListChanged`. On a notification, the monitor re-lists on a fresh short-lived session, as `MCPToolLister` does. So the go-sdk's TTL cache never answers a re-list. If the listen request ends, the monitor connects again on the next tick and re-lists once, so a change made in between isn't lost.
- Custom health checks (phase 3 §6) are HTTP and don't depend on the revision.

### Client side: routing each request

`mcpEndpoint` (`internal/gateway/mcp.go`) holds two go-sdk handlers:
- **The stateful handler**, as today. It serves every revision the go-sdk supports before 2026-07-28.
- **The stateless handler**: `NewStreamableHTTPHandler` with `Stateless: true` and a server built with `SupportedProtocolVersions: ["2026-07-28"]`.

`ServeHTTP` already reads the body (at most 4 MiB). It sends a request to the stateless handler when its `Mcp-Protocol-Version` header is `2026-07-28` or later, and its JSON-RPC method isn't `server/discover`. The method comes from the `Mcp-Method` header, or else from the body through the go-sdk's `jsonrpc.DecodeMessage`. Every other request goes to the stateful handler.

So:
- **A client that supports 2025-11-25** is served on it. A client that tries 2026-07-28 first sends `server/discover`. The stateful handler answers it with the older revisions only, and the client falls back to `initialize`. This is the go-sdk client's behaviour, and the 2026-07-28 rule for clients that support both.
- **A client that speaks only 2026-07-28** sends its requests with the 2026-07-28 `_meta` fields and header, and is served statelessly. It may call `server/discover` first. If it then stops because the answer lists no 2026-07-28, toolgate can't serve it without giving up the preference. That is this design's reading of the checklist (§15, reading 1).

**Stateless requests and toolgate sessions.**
- toolgate's session is (principal, `X-Toolgate-Session`), as always, so state lives in PostgreSQL and any replica can serve any request. A 2026-07-28 request without `X-Toolgate-Session` gets HTTP 400 with "This client sends no session. Use the toolgate plugin's headers helper, or a client on MCP 2025-11-25." There is no `Mcp-Session-Id` to tie a session to (§9; §15, reading 2).
- Each replica keeps one `mcp.Server` per stateless binding (principal, session, workspace, attestation), built by the existing `newServer` and released 30 minutes after its last request and listen stream end, like an MCP session (phase 1 §3). `serverFor` returns it. So `list_changed` reaches the binding's `subscriptions/listen` streams through the go-sdk. A stateless binding's `sessionTools` is found by its binding key, not by the go-sdk's per-request session ID.
- The fan-out (`internal/gateway/fanout.go`) matches stateless bindings as it matches MCP sessions.

**What a 2026-07-28 client can do** (AC-4-08):
- list, enable, disable and call, with no change to the meta tools;
- get `list_changed` through `subscriptions/listen` (above);
- complete approvals, through the multi round-trip results phase 2 §4 already returns;
- get progress notifications during waits, on the request's own response stream.

**Private tool lists.** Both servers set `ServerOptions.SetCacheable`. For `tools/list` it sets `CacheScope` to `private` and `TTLMs` to 0, so no cache shares one session's tools with another, and a list change is never hidden by a cache. On 2025-11-25 these fields are absent from the wire, so the setting changes nothing there.

## 9. Connectors

AC-4-09 and AC-4-10. toolgate becomes a standard OAuth-protected MCP server. The IdP is the authorization server. toolgate issues no tokens and runs no OAuth server.

### Protected resource metadata

- **Setting:** `mcpPublicURL`, the MCP endpoint's URL as clients on the internet use it, such as `https://mcp.example.com/mcp`. It must be `https://`, or `http://localhost` and `http://127.0.0.1` for tests. If it is empty, it is `publicURL` + `/mcp`. If both are empty, no metadata is served and the 401 header has no `resource_metadata`.
- **Document**, served without authentication at both `/.well-known/oauth-protected-resource/mcp` (the path RFC 9728 forms for a resource at `/mcp`) and `/.well-known/oauth-protected-resource`. It is built with the go-sdk's `oauthex.ProtectedResourceMetadata` and served with `auth.ProtectedResourceMetadataHandler`, which sets `Access-Control-Allow-Origin: *`:
  - `resource`: `mcpPublicURL`;
  - `authorization_servers`: [`oidcIssuerURL`];
  - `scopes_supported`: `oidcCLIScopes`;
  - `bearer_methods_supported`: [`header`].
- Both paths join phase 1 §8's list of unauthenticated routes, with the same rate limit as `/.well-known/toolgate-client`. The route test is updated.
- **The 401.** `auth.Middleware` on `/mcp` sets `WWW-Authenticate: Bearer resource_metadata="<mcpPublicURL origin>/.well-known/oauth-protected-resource/mcp"`. When a token was sent and refused, it adds `error="invalid_token"`. Other routes keep `WWW-Authenticate: Bearer`.
- **Tokens** are verified exactly as today: go-oidc against the IdP's JWKS, with `oidcAudience` (phase 3 §10). The connector's client ID must be one of the audiences. Nothing is passed to upstream servers.

### Admin setup (docs only)

`docs/admin/connectors.md` gives the steps. The worker checks the names against the pinned Authentik docs and claude.ai's connector docs.
- **Authentik:** an OAuth2/OpenID provider for the connector:
  - client type confidential;
  - redirect URI `https://claude.ai/api/mcp/auth_callback`;
  - scope mappings `openid`, `email`, `profile` and `offline_access`;
  - a signing key, so access tokens are JWTs;
  - the same subject mode as the CLI's provider (phase 2 §5);
  - the same issuer as `oidcIssuerURL` (in Authentik, the issuer mode that gives every provider one issuer), because toolgate accepts one issuer (`SPEC.md` §16);
  - an application with the same access policy as toolgate's.
- **Gateway:** add the provider's client ID to `oidcAudience`, and set `mcpPublicURL`.
- **claude.ai:** add a custom connector with the URL `mcpPublicURL`, and the client ID and secret in its advanced settings. Authentik has no dynamic client registration, so the connector needs them. Each user then signs in through Authentik when they connect.
- **Exposure:** only `/mcp` and the two metadata paths are routed from the internet. The docs give an example `HTTPRoute` that matches those paths and nothing else. The UI, `/api/v1/`, the OTLP routes and the metrics port stay internal. The chart needs no new value: the admin adds the route, as they add any ingress.
- **What users need to know:** approval and step-up links point at `publicURL`, which stays internal. A connector user opens them from a browser that can reach the internal URL. The IdP's sign-in pages and token endpoint must be reachable from the user's browser and from claude.ai's servers.

### Sessions without the helper

AC-4-10. A connector sends no `X-Toolgate-Session`.
- `auth.Middleware` gets a third mode for `/mcp`: the session header is optional on requests that aren't 2026-07-28. A missing header leaves `Identity.Session` empty, and `mcpEndpoint` fills it in.
- **Session IDs.** The stateful handler's `ServerOptions.GetSessionID` returns 32 lowercase hex characters from `crypto/rand`. For an `initialize` without `X-Toolgate-Session`, `mcpEndpoint` generates the ID first and passes it to `GetSessionID` through the request context. The toolgate session ID is then `mcp.<id>`. Later requests carry `Mcp-Session-Id`, and their session is `mcp.<Mcp-Session-Id>`.
- **Binding.** The MCP session is bound at `initialize` as today (phase 3 §10). A request whose `Mcp-Session-Id` this replica doesn't know gets 404, so a client can't pick a `mcp.` session by sending an ID.
- **Reserved prefix.** An `X-Toolgate-Session` value starting with `mcp.` gets 400. So a helper's claimed ID can never name a connector's session.
- **It's a normal toolgate session.** It has its own enabled set and works with list, enable, disable, calls, approvals and search. Its workspace is `claimed/default`, unless the client sends `X-Toolgate-Workspace`, so it is unverified. The unverified default or allow list decides what it sees (AC-4-11).
- **It ends with the MCP session.** A client `DELETE` of the MCP session, or this replica releasing the MCP session after its 30 minute idle timeout, ends the toolgate session at once, with phase 1's end transaction and `session.end`. Its ID is never used again, so it is never resumed. The reconciler forgets it after `sessionResumeWindow`, as for any ended session.
- **Replica switches.** If the replica that holds the MCP session dies, or a request reaches another replica, the client gets 404 and initializes again. That starts a new toolgate session with an empty enabled set, and the old one idles out after `sessionIdleTimeout`. The admin docs recommend session affinity on `Mcp-Session-Id` (phase 1 §3) for connectors.

## 10. Interaction table

Phase 1's rows (phase 1 §4), phase 2's rows (phase 2 §10) and phase 3's rows (phase 3 §8) still hold, except where a row below says it changes one. Rows continue the numbering. Each row is one event combined with one operation already in flight, and each row's covering test proves its rule.

| # | Event | In flight | Rule | Test |
|---|---|---|---|---|
| 174 | Call of a tool of an allowed server that isn't enabled | — | The call enables it (`server.enable`, `reason: first-call`), sends `list_changed`, and goes on as a normal call (§4). | AC-4-01 |
| 175 | Two calls at once to the same server, not yet enabled, in one session | each other | Both lock the `sessions` row. The first enables. The second finds the row and writes no second `server.enable`. | integration |
| 176 | Implicit enable of a 33rd server | — | The call is refused with phase 1's message, with `tool.decision` (refuse, `limit`). Nothing is enabled. | unit |
| 177 | Call of a server outside a verified workspace's list | — | Phase 3's session addition: a request, a URL elicitation and the wait. On approval, the call goes on (§4, step 3). Phase 3 §10's "calls never elicit" changes. | AC-4-02 |
| 178 | Same, in an unverified session | — | Refused at once with step `workspace-scope`. No request. | AC-4-02 |
| 179 | Rule added (replica B) | a found tool, before its call (replica A) | The call decides with fresh state and is refused. The next `tools/list` leaves the tool out. | unit |
| 180 | `toolgate_find_tools` | a server outside the list, in a verified workspace | Returned with `access: ask`. In an unverified session it isn't returned. | AC-4-02 |
| 181 | Call of a catalog tool | no live instance, newest row didn't fail | The call inserts a `pending` row and waits, with progress, up to `startTimeout`. Then it is decided again and forwarded. | AC-4-13 |
| 182 | Two sessions call a per-user server with no live instance | each other | The advisory lock gives one instance (phase 1 row 1). Both calls wait on it. | integration |
| 183 | Start fails | calls waiting on it | Each call fails with the reason and `tool.decision` (refuse, `not-ready`). The server stays enabled with no tools (phase 1 row 12). | AC-4-13 |
| 184 | Call | newest row failed, no live row | Refused at once with the reason and "Enable `<server>` again to retry." Calls never retry a failed start (§3). | unit |
| 185 | Instance becomes ready with a list that differs from the catalog list | sessions listing the catalog list | The catalog row is written in the `ready` transaction. `instance:<id>` sends `list_changed`, and sessions list the real tools. | AC-4-12 |
| 186 | Called tool missing from the ready instance's curated list | call that waited for the start | Refused with `not-offered`, "doesn't offer `<tool>` any more". Nothing is forwarded. | unit |
| 187 | Template's generation changes | no instance, catalog row of the old generation | The catalog list falls back to declared tools, then to the older row (§3). The first ready instance of the new generation writes its row. | unit |
| 188 | Template declares tools | instance ready with other tools | Nothing is learned. Sessions with that live instance list its real tools, and others list the declared ones. | unit |
| 189 | Instances of one generation for two users become ready | each other | The upsert's last commit wins. Either list is a list of that generation. | unit |
| 190 | Leader change | search rows being rebuilt | The new leader rebuilds every template's rows when it gets the Lease. A rebuild is one transaction, so a search sees the old rows or the new ones. | unit |
| 191 | `start: onEnable` | enable | Phase 1's enable: insert or reuse, then wait. | unit |
| 192 | Resume (phase 3 §11) | `onCall` servers with a catalog list | Restored without an instance. They start on their first call. | unit |
| 193 | Idle stop | `onCall` server still enabled | Phase 1's rule decides. The next call starts a new instance. | integration |
| 194 | Instance unhealthy | call | Refused with `not-ready` and the reason (phase 3 §6). An unhealthy instance never makes a call start another. | unit |
| 195 | Hidden tool called by its upstream or exposed name | — | Refused with `not-offered`, with `tool.decision`. Nothing is forwarded. | AC-4-04 |
| 196 | Renamed tool called by its upstream name | — | Refused with `not-offered`. Only the exposed name works. | AC-4-05 |
| 197 | Override name collides with another tool | instance becoming ready | The tool list checks fail the instance with the collision reason. | unit |
| 198 | Curation changes | instance running an older generation | The current template's curation applies at once (§6). `list_changed` follows the template event. | unit |
| 199 | Rule names a tool's exposed name | renamed tool | The rule refuses. It would also refuse by the upstream name (§6). | unit |
| 200 | `toolgate_allow` | — | A pending `scope` request, a link and a URL elicitation. Nothing changes until the user approves with a step-up. | AC-4-15 |
| 201 | Target session ends | pending `scope` request for it | The end transaction expires it with `access.expire`. The page shows it expired. | integration |
| 202 | Target session moves to another workspace, or loses its attestation | session-scoped approval | The approval rolls back, and the page says "This session is now in another workspace." | unit |
| 203 | `toolgate_deny` of a workspace (replica B) | calls in a session of that workspace (replica A) | `scope:<id>` makes A send `list_changed`. The next call is refused. Within 5 s. | AC-4-16 |
| 204 | Tool-level deny | server allowed | Only that tool is withdrawn and refused. The server's other tools stay. | AC-4-16 |
| 205 | Deny | pending allow of the same server or tool | The request stays pending. If the user approves it later, the approval deletes the deny entry. The later decision wins. | unit |
| 206 | Deny with default `all` and no list | — | A `deny` entry is inserted. Phase 3's 409 doesn't apply (§7). | unit |
| 207 | `toolgate_allow` or `toolgate_deny` names another user's session | — | "You have no open session `x`." Nothing is written. The call's `tool.result` is an error. | AC-4-14 |
| 208 | `toolgate_*` call | session where `toolgate` isn't allowed | Refused at step 4, like any server's tool. | AC-4-14 |
| 209 | Service account or SPIFFE identity calls a `toolgate_*` tool | — | Refused at step 1. | unit |
| 210 | Gateway without the web UI | — | The `toolgate` server isn't in the catalog, and its tools are refused with `not-offered`. | unit |
| 211 | Template named `toolgate` | — | Invalid, "reserved for the built-in server". | unit |
| 212 | Unverified allow list changed (replica B) | open unverified sessions (replica A) | `scope:<id>` sends `list_changed`. The next decision uses the new list. Within 5 s. | AC-4-11 |
| 213 | 2025-11-25 probe fails with a connection error or 5xx | instance `starting` | Not a version answer. No 2026-07-28 attempt, no revision stored, and the next step probes again (§8). | AC-4-07 |
| 214 | Upstream speaks only 2026-07-28 | probe | The 2025-11-25 attempt gets a version answer, 2026-07-28 succeeds, and the row is `ready` with that revision. | AC-4-07 |
| 215 | `server/discover` fails 3 times in a row | 2026-07-28 instance `ready` | Failed, as phase 1's ping rule. | unit |
| 216 | 2026-07-28 upstream's listen request ends | monitoring | The monitor connects again on the next tick and re-lists once. | unit |
| 217 | Dual-revision client sends `server/discover` | — | The stateful handler lists only the older revisions. The client initializes on 2025-11-25. | AC-4-08 |
| 218 | 2026-07-28 request without `X-Toolgate-Session` | — | HTTP 400 with the message in §8. | unit |
| 219 | 2026-07-28 listen stream on replica A, call on replica B | a change for the session | B's write notifies. A sends `list_changed` on the listen stream. | integration |
| 220 | 0.3 replica serves a session | rolling upgrade to 0.4 | It ignores `scope_entries`, `unverified_servers` and found tools, so a deny entry isn't enforced on it, and `list` counts as `none` there, until the rollout ends. Like phase 3 row 130. | unit |
| 221 | 0.3 replica gets an approval page for a `scope` request | rolling upgrade | It answers 404. The user opens the link again after the rollout. | unit |
| 222 | `/mcp` request with no token, or a refused token | — | 401 with `WWW-Authenticate` naming the metadata URL, plus `error="invalid_token"` for a refused token. | AC-4-09 |
| 223 | Connector token whose audience isn't in `oidcAudience` | — | 401 and `auth.refuse` with "audience not allowed" (phase 3 row 149). | unit |
| 224 | `initialize` without `X-Toolgate-Session` | — | A new `mcp.<id>` session, bound to the MCP session. | AC-4-10 |
| 225 | Client deletes the MCP session, or it idles out on the replica | `mcp.` session | The toolgate session ends at once and writes `session.end`. Its per-session instances stop. | AC-4-10 |
| 226 | `mcp.` session's request reaches another replica | — | 404, the client initializes, and a new toolgate session starts empty. The old one idles out. | unit |
| 227 | `X-Toolgate-Session` starting with `mcp.` | — | 400. | unit |
| 228 | Search adds a 51st found tool | — | The oldest is deleted in the same transaction. `list_changed` follows. | unit |
| 229 | Session ends | found tools, scope entries for the session | Deleted in the end transaction. | integration |
| 230 | Search mode header changes between requests | — | The next `tools/list` follows the new mode. Decisions don't change. | unit |
| 231 | Leader rebuilds search rows (replica A's searches) | search in flight | The search sees one consistent set of rows. `toolsearch:<uid>` sends `list_changed` to sessions with found tools of that template. | unit |
| 232 | Call of a found tool | the template was deleted since the search | Refused with `not-offered` at step 1. The found row is deleted on the next search or at the session's end. | unit |
| 233 | Scope entry for a tool of a renamed tool | curation changes the exposed name | Entries hold upstream names, so they keep applying. | unit |

Any combination not in this table is a design gap: hand back `HANDOFF: question` with the case.

## 11. Settled choices

- **Settings** (flat, in the generated reference):
  - `toolSearch` (default `false`): search mode when the client sends no `X-Toolgate-Tool-Search` header (§5);
  - `mcpPublicURL` (default `publicURL` + `/mcp`): the resource the protected resource metadata names (§9).

  The chart sets them from `config.toolSearch` and `config.mcpPublicURL`.
- **Request headers:** `X-Toolgate-Tool-Search` (`on` or `off`). `X-Toolgate-Session` becomes optional on `/mcp` for requests that aren't 2026-07-28, and values starting with `mcp.` are refused (§9).
- **Client environment:** `TOOLGATE_TOOL_SEARCH` (`on` or `off`) makes `toolgate headers` send the header.
- **CRD fields** on `MCPServerTemplate`: `start` (§3), `declaredTools` (§3), `toolFilter` (§6), and `name`, `description` and `inputDescriptions` under `tools.<name>` (§6). All are checked by CEL in the CRD and again by the catalog.
- **Meta tool:** `toolgate_find_tools` (§5).
- **Built-in server:** `toolgate`, with `toolgate_list_scopes`, `toolgate_allow` and `toolgate_deny` (§7).
- **Time bounds:**
  - search statement 2 s;
  - a 2026-07-28 instance's discover check every 30 s, three failures to fail;
  - a stateless binding's `mcp.Server` released 30 minutes after its last request;
  - access requests expire by `approvalTimeout` (phase 2);
  - deny withdrawal within 5 s.
- **Limits:**
  - search query 2000 characters and 64 words, `limit` 1 to 20, 100 rows ranked, 50 found tools per session;
  - declared tools 512 and 1 MiB;
  - filter lists 512 names of 128 characters;
  - override names 128 characters and 64 with the prefix, descriptions 4096 characters, 64 input descriptions;
  - `toolgate_allow` and `toolgate_deny` 32 items;
  - 500 scope entries and 64 unverified servers per principal;
  - `toolgate_list_scopes` 200 workspaces and 200 sessions.
- **Audit events:** no new types.
  - `server.enable` gains `reason: first-call`.
  - `tool.decision` and `tool.result` gain `exposedTool` when it differs from `tool`.
  - `instance.ready` gains `mcpRevision`.
  - `workspace.change` and `session.addition` gain `tools`, `denied` and `via: tool` for changes from the `toolgate` server.
  - `workspace.defaults` gains the unverified allow list.
- **Libraries:** nothing new in `go.mod`.
  - `github.com/modelcontextprotocol/go-sdk` v1.8.0: `ClientSessionOptions.ProtocolVersion`, `StreamableHTTPOptions.Stateless`, `ServerOptions.SupportedProtocolVersions`, `ServerOptions.SetCacheable`, `ServerOptions.GetSessionID`, `jsonrpc.DecodeMessage`, `oauthex.ProtectedResourceMetadata` and `auth.ProtectedResourceMetadataHandler`;
  - PostgreSQL full-text search (`to_tsvector`, `to_tsquery`, `ts_rank`, a GIN index);
  - `crypto/rand` for connector session IDs;
  - `github.com/coreos/go-oidc/v3` for connector tokens, as for every token today.

  No new cryptography and no protocol code.
- **Errors to the model** follow phase 1: short sentences naming what to do next.

## 12. API and CLI changes

**`POST /api/v1/workspaces/changes`** (phase 3 §10) gains:
- `servers` items of the form `<server>__<tool>`, with the tool's exposed name;
- `session` with `allow`, not only `deny`. A session allow widens, so it answers 202 with a `scope` request;
- `unverified: true` with `allow` and `deny`, for the unverified allow list;
- `"unverified": "list"` in `defaults`.

A `deny` on a workspace with no list inserts deny entries instead of answering 409 (§7). Every widening change of phase 3's shape still creates a `change` request. A change that phase 3's shape can't hold creates a `scope` request.

**`GET /api/v1/workspaces`** gains `defaults.unverifiedServers`, and `tools` and `denied` on each workspace. **`GET /api/v1/sessions`** gains `allowed` and `denied` on each session. The OpenAPI document is regenerated (phase 3 §5).

**CLI:**
- `toolgate workspace allow` and `deny` accept `SERVER__TOOL` items, `--session ID` (allow too) and `--unverified`.
- `toolgate workspace defaults --unverified none|all|list`.
- `toolgate workspace list` prints the unverified allow list, and the tool entries and denies of each workspace.
- `toolgate sessions` prints each session's allowed and denied entries.
- `toolgate headers` sends `X-Toolgate-Tool-Search` from `TOOLGATE_TOOL_SEARCH`.

**Web UI:**
- The Workspaces page shows tool entries and denies, and the unverified allow list.
- The approval page shows `scope` requests with the target and which session asked.

**`SPEC.md` updates** in the first implementing PRs:
- §5: the two revisions, the routing rule, and `X-Toolgate-Tool-Search`;
- §6: connector sign-in through the protected resource metadata, and `mcp.` sessions;
- §7: `toolgate_find_tools`, search mode, calls that enable, catalog tools, curated names, and the `toolgate` server with its three tools;
- §9: `start`, `declaredTools`, `toolFilter` and the new `tools` fields;
- §10: step 4 with scope entries and the unverified allow list, and rules matching both tool names;
- §13: the new CLI flags and `TOOLGATE_TOOL_SEARCH`;
- §14: the Workspaces page and `scope` requests;
- §15: the new tables and columns;
- §16: "toolgate issuing its own access tokens" stays won't-do in phase 4 too. "More than one IdP at a time" stays.

## 13. Testing plan

Every scenario's test carries its tag. The e2e harness gains:
- a test MCP server mode that speaks only 2026-07-28: the go-sdk server with `Stateless: true` and `SupportedProtocolVersions: ["2026-07-28"]`;
- a test MCP server mode with a fixed tool list for the search fixture;
- a fake IdP client for the connector audience.

| Scenario | Test | What it checks |
|---|---|---|
| AC-4-01 | e2e | With search on, `tools/list` holds the meta tools and `toolgate_find_tools` only. A search returns tools from several allowed servers with schemas. Calling one runs, writes `server.enable` with `reason: first-call`, `tool.decision` and `tool.result`. |
| AC-4-02 | integration | A blocked tool, a tool outside access, and a tool needing `attested` from a `user` session are never returned. In a verified workspace, a server outside the list is returned with `access: ask`, and its first call creates a session-addition request with an elicitation. In an unverified session it isn't returned. |
| AC-4-03 | integration | The frozen fixture (§5) through the real rebuild and search in PostgreSQL. At least 90 percent of tasks have the expected tool in the top 5. |
| AC-4-04 | e2e | An allow-list template and a deny-list template. Hidden tools are absent from `tools/list` and search, and calls by either name are refused with a recorded decision and nothing forwarded (the test server counts calls). |
| AC-4-05 | e2e | A renamed tool with a new description and input description. The client sees the override. The test server receives the call under the upstream name. A template whose override breaks the 64-character rule is invalid. |
| AC-4-07 | e2e | Three instances: 2025-11-25 only, both, and 2026-07-28 only. Each becomes `ready` with the expected `mcp_revision`, passes the monitor, serves tools, and a tool-list change on each reaches an open session. A unit test injects a 503 on the first 2025-11-25 attempt and checks that no 2026-07-28 attempt is made and no revision stored. |
| AC-4-08 | e2e | A go-sdk client at its default (tries 2026-07-28 first) ends on 2025-11-25. A 2026-07-28-only client lists, enables, disables, calls, gets `list_changed` through `subscriptions/listen`, and completes an approval. Its `tools/list` results carry `cacheScope: "private"`. The only-2026-07-28 client is the go-sdk client with a sending middleware that refuses `initialize` and treats the discover answer as listing 2026-07-28 (§15, reading 1). |
| AC-4-09 | e2e | A request with no token gets 401 and the header. The metadata document names the fake IdP. A client built with the go-sdk's `auth.AuthorizationCodeHandler` follows the flow against the fake IdP and its requests act as the user. The claude.ai connector itself is checked by the owner by hand, and that check doesn't gate. |
| AC-4-10 | e2e | A client with no `X-Toolgate-Session`: list, enable, disable, a call and an approval work. Two such clients get separate sessions. A `DELETE` ends the session with `session.end`. An idle release ends it too (with the short e2e timeouts of `internal/testtime`). |
| AC-4-11 | integration | `list` with two servers: an unverified session lists and enables only those. Adding a server needs an approved request. Removing one withdraws it within 5 s. The UI path through chromedp, as phase 3. |
| AC-4-12 | e2e | A server is listed and searched with no instance running, from its declared tools and then from a learned list. After the instance becomes ready with a different list, an open session gets `list_changed` and the real list. |
| AC-4-13 | e2e | A call to a cataloged server with nothing running starts it, gets progress notifications, and is forwarded. A start failure fails the call with its reason. A `start: onEnable` template starts on enable. |
| AC-4-14 | integration | The three tools work in a session that allows `toolgate`, and are refused at step 4 in one that doesn't, and by a rule naming `toolgate`. They refuse another user's session. The same change through the tool and through the API gives the same rows. |
| AC-4-15 | e2e with chromedp | `toolgate_allow` for another workspace and for another session returns a link and an elicitation. Nothing changes before the step-up approval, and the change applies after it. The session-scoped allow is gone after that session ends. |
| AC-4-16 | integration | `toolgate_deny` of a workspace and of a session, server-level and tool-level, withdraws the target's tools and refuses its calls within 5 s across two replicas. A tool-level deny leaves the server's other tools. |
| AC-4-17 | integration | `toolgate_disable_server` drops the server at once. In a verified workspace, an enable of a server outside the list asks. Neither needs the `toolgate` server. |
| AC-4-06 | release | Release `0.4.0` with the docs below, by the release manager. |

**Docs for AC-4-06:**
- `docs/user/`: tool search and the header, calls without enable, and the three `toolgate` tools;
- `docs/admin/templates-and-rules.md`: `start`, `declaredTools`, `toolFilter`, the override fields, and the reserved name;
- `docs/admin/connectors.md` (new): connecting a claude.ai custom connector, with the Authentik provider, the route and the limits of §9;
- `docs/admin/mcp-revisions.md` (new): the supported revisions, which one toolgate picks with each peer, and the 400 for a 2026-07-28 client without a session;
- `docs/admin/security-model.md`: learned lists cross users, and connectors on the internet;
- `docs/README.md` lists the new pages.

## 14. Won't do

- A search backend other than PostgreSQL: no embeddings, no vector database, no external service.
- An OAuth server, dynamic client registration, client metadata documents, token exchange, or passing a client's token to upstream servers.
- Exposing the UI, the API or the OTLP routes to the internet.
- A tool catalog for each user. The catalog is for each template generation, as the checklist says.
- Starting servers for sessions that haven't enabled them.
- Changing idle stops.
- A sticky disable. `toolgate_deny` is the lasting way.
- Changing defaults, or the unverified allow list, through the `toolgate` tools. They target workspaces and sessions only, as the checklist says. The CLI and UI do the rest.
- Globs in tool filters. Overrides of nested schema descriptions.
- 2026-07-28 sessions without `X-Toolgate-Session`.
- Changing an instance's revision while its row lives.
- Prompts, resources and sampling through the gateway (`SPEC.md` §16).

## 15. Readings of the checklist

These are the readings this design takes where the checklist leaves room. A worker who finds a case they don't cover hands back `HANDOFF: question`.

1. **Clients on 2026-07-28 (AC-4-08).** A client that speaks both revisions and "tries 2026-07-28 first" does so by sending `server/discover`. toolgate answers it with only the older revisions, so the client falls back to 2025-11-25. A client that speaks only 2026-07-28 is served when it sends its requests without stopping at that answer. A client that calls `server/discover` first and stops when 2026-07-28 isn't listed can't be served, because the same answer must send a dual client to 2025-11-25. The other choice is to list 2026-07-28 in the answer. Then every dual client that discovers first would use 2026-07-28, against the checklist's first sentence.
2. **2026-07-28 without a session header (AC-4-08, AC-4-10).** AC-4-10 ties a helper-less session to `Mcp-Session-Id`, which 2026-07-28 removes. So a 2026-07-28 client must send `X-Toolgate-Session`, and gets 400 without it.
3. **"A session may use the server" (AC-4-13).** It means the server is enabled in the session: by an enable, a call or a resume. Not every server the session's policy allows.
4. **Which list wins (AC-4-12).** Declared tools beat learned ones for the same template. Learned lists of the current generation beat those of older generations. An older generation's list is used rather than none.
5. **Search mode lists found tools (AC-4-01).** "Instead of every server's tools" is read as "not every server's tools". The list still holds the enabled servers' tools and the tools the session found, because Claude Code can only call listed tools.
6. **Tool-level allows (AC-4-15).** Allowing a tool undoes its deny. If its server isn't otherwise allowed, it allows that one tool.
7. **Deny on a workspace with no list (AC-4-16).** It takes effect through a deny entry. Phase 3's 409 is dropped.
8. **Disable (AC-4-17).** It works as in phase 1. A later call enables the server again.
9. **The metadata path (AC-4-09).** The header names `/.well-known/oauth-protected-resource/mcp`, the RFC 9728 path for a resource at `/mcp`. The same document is also served at `/.well-known/oauth-protected-resource`.
10. **Curation follows the current template.** A running instance gets new curation at once, as it gets new `blocked` overrides today.
11. **The `toolgate` server needs the web UI.** Without it, allows can't be approved, so the server isn't offered.

## Later

Ideas outside the checklist, for the owner to consider for a later phase. None is part of this design.
- Idle stops for `onCall` servers that are still enabled, since a call can start them again.
- Defaults and the unverified allow list through the `toolgate` tools.
- Overrides of nested input-schema descriptions, and globs in tool filters.
- A catalog for each user, for servers whose tools differ by user.
- Search over servers that have never run, by their template description.
- Session affinity in the chart for connectors.
- Metrics for search latency and for the revisions in use.
