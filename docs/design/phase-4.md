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
- At most 500 `allow` entries per principal. `deny` entries don't count toward the limit, because a narrowing must always work.

**Unverified allow list.** `unverified_servers(scope_id, server, added_at)`, primary key `(scope_id, server)`, at most 64 rows per principal. A new column, `user_scopes.unverified_list boolean NOT NULL DEFAULT false`, stores the default `list`.
- `list` is stored as `unverified_default = 'none'` with `unverified_list = true`. The column `unverified_default` gains no new value, and its check doesn't change.
- A 0.4 replica reads `none` with `unverified_list = true` as `list`. A write of `none` or `all` sets `unverified_list = false`.
- Why no new value: a 0.3 replica decides that a defaults change widens only when it goes from `none` to `all`. A stored `list` would be neither, so 0.3 would apply a change from `list` to `all` at once, with no step-up. Stored as `none`, the change still widens on 0.3 (row 241).

**Access requests.** `access_requests.kind` accepts `scope` as well as `server` and `change`, for the changes of §7 that phase 3's `change` JSON can't describe. The migration drops the kind check and the shape check, and adds them back with the new kind.
- Why a new kind and not more fields in `change`: a 0.3 replica that read a session-scoped or tool-level change as a phase 3 `change` would apply the wrong change. A 0.3 replica doesn't know `scope` rows. Its approval page answers 404 for them until the rollout ends (row 221).

**Ending a session** (phase 1 §2, changed by phase 3 §10) also deletes its `session_found_tools` and its `allow` scope entries, and expires its pending `scope` requests that target it, writing `access.expire` for each. All of this is in the same transaction. Its `deny` entries stay until the reconciler forgets the session (`session.forget`, phase 3 §11). So a resume can't undo a deny (row 240).

**Cleanup.** On its 30 s pass, the leader:
- deletes `tool_catalog` rows of template UIDs that no longer exist, and, for each UID, every row older than the newest two generations;
- deletes `tool_search` rows of template UIDs that no longer exist;
- deletes the `deny` scope entries of each session it forgets, in the `session.forget` transaction;
- deletes the `allow` scope entries and the found tools of ended sessions, and expires, with `access.expire`, the pending `scope` requests whose target session ended after the request was created. A 0.4 end transaction already does this. The pass catches sessions that a 0.3 leader ended during the rollout (row 247).

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

The write is an upsert with `learned_at = now()`. It doesn't happen when the template declares tools. So the row holds the list of the last instance of that generation that became ready or changed its list. Each write also rebuilds the template's search rows (§5) in the same transaction. The rebuild takes its lock first (§5), so two `ready` writes of one template run their rebuilds in turn.

### Lists while nothing runs

`tools/list` (phase 1 §3, "List") changes for each enabled server:

| The server's state for this session's scope key | Tools listed |
|---|---|
| A `ready`, healthy live instance | The instance's `tools`, curated, as today |
| A live instance that is `pending` or `starting` | The catalog list, curated |
| No live instance, and the newest row didn't fail, or failed on an older generation | The catalog list, curated |
| No live instance, and the newest row has `failed_at` set and has the template's current generation | None, as phase 1 row 12 |
| A `ready` instance that is unhealthy | None, as phase 3 §6 |

Each tool still goes through the policy decision, as today. A server without a catalog list contributes no tools until its instance is ready.

`toolgate_list_servers` shows an enabled server with no live instance as `stopped`.

**When the real list differs.** The instance's `ready` write and its `instance:<id>` notification already make every replica send `list_changed` to the sessions that enable the template (phase 1 §3, "Fan-out"). Those sessions then list the instance's own tools. The catalog row is written in the same transaction, so sessions of other scope keys list the new catalog list too. This is AC-4-12's last sentence.

### Starting

A new template field: `spec.start`, `onCall` (the default) or `onEnable`.

- **`onCall` with a catalog list.** Nothing starts until a call needs the instance:
  - **Enable** (`toolgate_enable_server`) runs phase 1 enable steps 1 to 3. In step 4 it locks the `sessions` row and upserts `session_servers`, but it inserts no instance. It writes `server.enable` (allow), notifies, commits, sends `list_changed`, and returns the catalog list's exposed names. It doesn't wait.
  - **Enable after a failure.** If the newest row for (template, scope key) has `failed_at` set and no live row exists, the enable also inserts a `pending` row, with the insert part of phase 1 enable step 4: the advisory lock, the instance limits and the insert. It still doesn't wait. This is how a user retries a failed server (below, row 234).
  - **Resume** (phase 3 §11) restores the server without inserting an instance.
  - **A call** inserts the instance when there is no live one (§4, step 5).
- **`onEnable`, or no catalog list.** Enable and resume work as in phase 3: they insert or reuse the instance, and enable waits for it.

So "a session may use the server" in AC-4-13 means "the server is enabled in the session". Enabling covers an explicit enable, a call's implicit enable (§4) and a resume. Starting a server for every session that could enable it would start every server for every user.

**The call waits** with phase 3 §11's rule, unchanged: `waitForCall` in `internal/gateway/forward.go`, through `waitForInstance` in `internal/gateway/wait.go`. It is woken by notifications and the 10 s tick, sends a progress notification on each status change and at least every 10 s, and stops at `startTimeout`. When the instance is `ready`, the call is decided again and forwarded. A start failure or a timeout fails the call with the reason, and writes `tool.decision` (refuse, `not-ready`, with the reason).

**A failed server isn't retried by calls.** If the newest row for (template, scope key) failed, has the template's current generation, and no live row exists, a call is refused at once with the failure reason and "Enable `<server>` again to retry." An explicit enable inserts a new instance: with `onCall`, as "Enable after a failure" above says, and with `onEnable`, as in phase 1 step 7. So a model that retries a call can't start a broken server again and again. A server that stopped for any other reason, such as an idle stop, starts on the next call.
- **A failed row of an older generation doesn't block calls.** When the admin fixes the template, its generation changes. The next call then inserts a new row of the current generation (§4, step 5; row 235).

**Idle stops** don't change (phase 1 §2, phase 3 §11). With `onCall`, a stopped instance starts again on the next call.

## 4. Calls without an enable

AC-4-01 and part of AC-4-02. A call of a tool of an allowed server enables the server for the session. There is no separate enable step.

**Call** (`<server>__<tool>`) replaces phase 1 §3's call steps 1 to 3. Phase 1 steps 4 to 6 and phase 2 §4 (approvals) follow unchanged.
1. **Find the server and the tool.** Find the template by the server name. Map the exposed tool name to its upstream name with the template's curation (§6). While the current template is invalid, use its last valid generation (§6). If the template doesn't exist, has no valid generation, or counts as deleted (§7), or the tool is hidden or renamed away, refuse with reason `not-offered`: "Server `github` has no tool `delete_repo`." This writes `tool.decision`. The call carries the upstream name from here on.
2. **Decide policy** with the request's own identity and fresh state, for the upstream name. If steps 1 to 3 refuse, refuse. If only step 4 refuses because the server is outside the list (§7, "Step 4 with scope entries", rules 2 to 4), go to step 3 below only when the server isn't enabled yet. Otherwise refuse. A deny entry (rule 1) always refuses at once, with reason `denied`, and never asks (row 236).
3. **Enable when needed.** If the session has no `session_servers` row for the server, run enable steps 1 to 4 (phase 1 §3, with §3's `onCall` rule above):
   - the step-4 refusal in a verified workspace asks the user, as phase 3 §10's session additions do: a pending `server` request, a URL elicitation, and the wait. On approval the call goes on. Without elicitation the call fails with phase 3's link message. In an unverified session the call is refused at once. Phase 3 §10's rule "Calls of a server outside the effective list never elicit" no longer holds: a call that enables a server asks, as an enable does.
   - a found tool marked `ask` is registered on the session's MCP server (§5). So its call reaches this step through the go-sdk tool path, which can return the URL elicitation (row 238).
   - the wait has one more outcome: approved, but still refused (§7, "Step 4 with scope entries").
   - a missing secret refuses the call with phase 1's message, and writes `tool.decision` (refuse, `missing-secret`).
   - a 33rd server refuses the call with phase 1's message, and writes `tool.decision` (refuse, `limit`).
   - success writes `server.enable` (allow) with `reason: first-call`, and the call sends `list_changed` after it commits.
4. **Find the live instance** for (template, scope key). If it is `ready` and healthy, go on. If it is `pending` or `starting`, wait (§3). If it is `ready` but unhealthy, refuse with `not-ready` as phase 3 §6.
5. **No live instance.** If the newest row failed and has the template's current generation, refuse (§3). Otherwise take the advisory lock, check the instance limits and insert a `pending` row, in one transaction with the `sessions` row lock (phase 1 enable step 4's insert part). Then wait (§3). At a limit, refuse with phase 1's message and reason `limit`.
6. **The tool must exist.** When the instance is `ready`, check that its curated list holds the exposed name. If not, refuse with `not-offered`: "Server `github` doesn't offer `create_issue` any more." This covers a catalog list that was out of date.
7. Go on with phase 1 call step 3 (decide again with fresh state) and the steps after it.

**The `toolgate` server's tools** (§7) skip steps 4 to 6. They run in-process.

**A tool allow reaches the whole server.** A tool-level `allow` entry makes the server-level decision pass (§7). So a call of another tool of that server, while the server isn't enabled, enables it without asking at step 3, and step 5 may start an instance. Step 7 then refuses the call at step 4. This is intended: the server's allowed tools are then ready (row 250).

**Disable** (`toolgate_disable_server`) is unchanged. It removes the server from the session at once (AC-4-17). A later call enables it again, because a call enables any allowed server. A user who wants it gone for good uses `toolgate_deny` (§7).

## 5. Tool search

AC-4-01 to AC-4-03.

### Search mode

- **Turning it on.** A new header, `X-Toolgate-Tool-Search: on` or `off`. When it is absent, the setting `toolSearch` (default `false`) decides. `toolgate headers` sends the header when `TOOLGATE_TOOL_SEARCH` is set. A connector sends no header, so the setting decides for it.
- The mode is read on each request. It's a claimed value, but it only changes what is listed. It never changes a decision, so it can't widen anything.
- **`tools/list` in search mode** is the meta tools, `toolgate_find_tools`, the tools of the session's enabled servers (§3), and the session's found tools. Each tool still goes through the policy decision. It never holds every server's tools.
- **Found tools marked `ask`.** In a verified workspace, a found tool whose only refusal is step 4's outside-the-list refusal (§7, rules 2 to 4) stays in the search-mode list. It is registered on the session's MCP server like any listed tool. So a call of it goes through the go-sdk tool path, which can return the URL elicitation (§4, step 3). A tool or server with a deny entry is never kept.
- **Outside search mode** nothing changes. `toolgate_find_tools` isn't listed, and a call of it is refused with "Tool search is off for this session."

### `toolgate_find_tools`

Input: `{"query": string, "limit": integer}`. `query` is 1 to 2000 characters. `limit` is 1 to 20, default 5.

Output: `{"tools": [{"name", "server", "description", "inputSchema", "access", "approval"}]}`.
- `name` is the proxied exposed name, such as `github__create_issue`.
- `access` is `allowed`, or `ask` for a tool whose only refusal, in a verified workspace, is step 4's outside-the-list refusal (§7, rules 2 to 4; AC-4-02). The first call of an `ask` tool asks the user (§4, step 3). A deny entry never gives `ask`.
- `approval` is `true` for a tool with `approval: required`.

**Candidates.** The search loads the request's policy state once (phase 2 §3), and walks the template catalog:
- A server is a candidate when the server-level decision allows it, or refuses it only at step 4 because it is outside the list, in a verified workspace. So blocked servers, servers outside the user's access lists, and servers needing an environment the session lacks are never candidates. A server with a server-level deny entry (§7, rule 1) is never a candidate.
- In an unverified session, a step-4 refusal isn't a candidate. Such a session can't ask (phase 3 §10).
- Its tools come from its search rows. The rows already leave out hidden tools (§6).
- **Descriptions and schemas.** A search row holds only names and the document. So the output's `description` and `inputSchema`, and those of found tools in `tools/list`, come from the template's current catalog list (§3), curated. A row or found tool whose upstream name is no longer in that list is dropped.
- The built-in `toolgate` server is a candidate like any other (§7).

**Ranking** uses PostgreSQL full-text search. That's no new dependency and no new backend (`SPEC.md` §3), and it gives every replica the same answer.
- The **document** of a tool is built by the leader. It has four weighted parts, each passed through `to_tsvector('english', …)` and `setweight`:
  - A: the exposed name, with `_`, `-`, `.` and camelCase boundaries turned into spaces, so `createIssue` also gives "create issue";
  - B: the curated description, cut at 4 KiB;
  - C: the server name and the template's `description`;
  - D: the input schema's top-level property names and their descriptions, cut at 4 KiB.
- The **query**: Go lowercases the task description and keeps words of `[a-z0-9]`, at most 64 words. It joins them with ` | ` and passes the result as one parameter to `to_tsquery('english', $1)`. So the query is the OR of the stemmed words. The words hold no `tsquery` syntax, so a query can't inject any.
- **A query with no words.** When no `[a-z0-9]` words remain, for example in a query in a non-Latin script, no SQL runs. When every word is a stop word, `to_tsquery` gives an empty query, which matches no row. Both return `{"tools": []}` with "The query has no words to search for. Describe the task in English words." (row 252).
- **The SQL** selects rows whose `template_uid` is in the candidate set and whose `document @@ query`. It orders them by `ts_rank(document, query, 1)` with the default weights, then by `exposed_name`, and returns at most 100 rows. The statement runs with a 2 s `statement_timeout`.
- Go then decides each returned tool (phase 2 §3). It keeps the allowed ones, and, in a verified workspace, those whose only refusal is step 4's outside-the-list refusal, marked `ask`. It drops the rest. It keeps the first `limit`.

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
- when it gets the Lease, for every template with a valid generation.

A rebuild first takes `pg_advisory_xact_lock(hashtextextended('toolsearch/' || template_uid, 0))`, so two rebuilds of one template run in turn. Without the lock, the second rebuild's delete can miss the first one's new rows, and its insert fails with `unique_violation` (row 189). Then the rebuild deletes the template's rows and inserts one row per curated tool of the template's catalog list. `source_generation` records which generation the list came from. It notifies `toolsearch:<template uid>`.
- A deleted template has its rows deleted.
- An invalid template's rows are rebuilt from its last valid generation (§6). A template with no valid generation has its rows deleted.

The catalog and rule reconcilers already run on every replica and write only while elected (phase 1 §1). The rebuild is a write, so it follows the same rule.

### Optional embeddings

The owner decided this on 2026-11-06 (#354). On the fixture, full-text search alone reached 61 percent, and about 75 to 82 percent with a richer document, trigram name matching and synonyms. Most of the remaining misses are paraphrases that share no word with the tool ("Catch me up on #general" for a channel history tool). So search can also rank by meaning, when the admin configures an embeddings endpoint.

**Setting.** `toolSearch.embeddings` in the chart and the gateway config:
- `url`: an endpoint that speaks the OpenAI embeddings API (`POST <url>/v1/embeddings`). Examples are Hugging Face text-embeddings-inference and Ollama, run in the cluster.
- `model`: the model name sent in the request.
- `apiKeySecret` (optional): a Secret key, sent as `Authorization: Bearer`.
- `timeout`: 2 s by default.

Without `url`, search is full-text only, as before. The gateway uses a maintained HTTP client and JSON. It needs no SDK and no new Go dependency beyond the standard library.

**Vectors.** The leader embeds each search row's document text when it rebuilds the row (§ "Keeping the search rows current"). It stores the vector in `tool_search.embedding` (`real[]`), with `embedding_model` (text). A row whose model differs from the setting is embedded again on the next rebuild. No `pgvector` is needed: a deployment has at most a few thousand tools, so a replica loads the candidate rows' vectors with the full-text query and computes cosine similarity in Go. If embedding a row fails, the row keeps an empty vector and is retried on the next rebuild, with a warning log and a metric. A failure never blocks a `ready` write.

**Query.** `toolgate_find_tools` embeds the query with the same model, with the timeout. The candidates are every search row the session's policy allows (as before), not only the full-text matches. Ranking uses reciprocal rank fusion (k = 60) of the full-text rank and the cosine rank. If the embeddings call fails or times out, the search uses full-text only, writes a metric, and still answers. The query text is never logged.

**Data and egress.** The query and the tool documents are sent to the endpoint. The admin docs say to run the endpoint in the cluster, and the chart's network policy allows egress only to the configured host and port.

**Synonyms.** The synonym list keeps only general computing vocabulary chosen before the fixture's misses were known: folder/directory, file/document, message/post/chat, mail/email, remove/delete, show/list/get/read, fetch/download, search/find/look up, remember/memory/note, history/recent/past. Groups added after looking at the misses are removed.

### Search quality

AC-4-03. The fixture is `test/fixtures/toolsearch/`:
- `servers.json`: at least 50 tools across at least 8 servers. The tool names, descriptions and schemas are copied from real public MCP servers, such as GitHub, Kubernetes, filesystem, mail, calendar and database servers. The file records each source.
- `tasks.json`: at least 50 task descriptions. Each names its expected tool.

One agent writes the fixture and opens it in its own PR before any ranking code merges. The search worker may not change it. So the fixture isn't fitted to the ranking.

The AC-4-03 test is an integration test. It loads the fixture through the real rebuild code into PostgreSQL and runs every task through the real search, **with embeddings configured**. In CI, the embeddings endpoint is Hugging Face text-embeddings-inference (CPU image from `ghcr.io/huggingface/text-embeddings-inference`, pinned by digest) with `BAAI/bge-small-en-v1.5`, cached between runs. The test passes when the expected tool is in the top 5 for at least 90 percent of the tasks. It also prints the full-text-only score for information, which doesn't gate. If hybrid search misses the bar, the worker hands back with the per-task misses and doesn't add words or change the fixture.

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
- `name` must not equal the upstream name of a hidden tool. The catalog checks this against `toolFilter.deny` when it reads the template, and a match makes the template invalid. The tool list checks also check it against the whole upstream list, so with `toolFilter.allow` a match fails the instance, as a collision does. The reason reads "Tool name `list_issues` of `create_issue` is the name of a hidden tool." (row 253).

**One function curates.** `Curate(template, upstream)` in a new package, `internal/toolcatalog`, returns the curated tools with both names. Every place that lists or searches tools calls it: `tools/list`, the catalog list, the search rebuild, the tool list checks and `toolgate_find_tools`. The reverse map, from an exposed name to an upstream name, is built from the overrides alone. So a call can be mapped while no instance runs:
- An exposed name that is some tool's override `name` maps to that tool.
- Any other exposed name maps to itself, unless that tool is hidden or has an override `name`.

**Collisions.** After curation, two tools of one server must not share an exposed name. The tool list checks (phase 1 §3) run on the curated list. So a collision fails the instance, as an over-long name does, with "Tool name `open_issue` of `create_issue` collides with another tool." For declared tools, the collision makes the template invalid.

**Curation is read from the current template**, like `spec.tools.<name>.blocked` is today. A running instance of an older generation gets the new curation at once. So a newly hidden tool is hidden at once, without waiting for a new instance.

**While the current template is invalid**, curation, the reverse map and call step 1 (§4) use the template's last valid generation. That is what `Catalog.Running` (`internal/catalog`) already serves running instances from today. The search rows are rebuilt from that generation, not deleted (§5). A template with no valid generation behaves as a deleted one (row 246).

**Policy uses the upstream name.**
- `spec.tools` overrides, grants, approvals, scope entries (§7) and the `tool` field of audit events all use the upstream name.
- Audit events also carry `exposedTool` when it differs.
- A `ToolgateRule`'s `tools` glob refuses when it matches `<server>__<upstream name>` **or** `<server>__<exposed name>`. Rules only refuse, so matching both names can refuse more, never allow more. An admin can write either name.

## 7. The `toolgate` server and delegated access

AC-4-14 to AC-4-17, and AC-4-11.

### The server

`toolgate` is a built-in server. Policy treats it like a template, so the rules don't change.
- **It is in the template catalog** of every replica as an in-memory template named `toolgate`, with `scope: shared`. It has no resources, no instance and no catalog row. It is listed only on a gateway with the web UI, because widening needs a step-up (phase 2 §9).
- A real `MCPServerTemplate` named `toolgate` is invalid, with the reason "The server name `toolgate` is reserved for the built-in server." It also counts as deleted: the catalog serves no last valid generation for it, so its running instances stop, and the name always means the built-in server (row 251). The upgrade notes say so.
- **Access:** every identity of kind `user`. Service accounts and SPIFFE workloads have no scopes (phase 3 §10), so step 1 refuses them.
- **Environment:** `any`. A signed-in user already has the CLI and the UI for these operations, from any machine.
- **Blocks and rules** apply by the name `toolgate`, so an admin can turn it off for chosen users, workspaces or sessions.
- **Workspace scope** applies as for any server. The user allows `toolgate` in a workspace's list, in a default, in the unverified allow list, or for one session. This is how "usable in any session where the user allows the `toolgate` server, and nowhere else" (AC-4-14) holds.
- **It is enabled like any server:** by `toolgate_enable_server`, or by its first call (§4). It counts toward the 32 servers. `toolgate_list_servers` shows it as `ready`, and `toolgate_server_status` returns `ready` for it.
- **Tools.** Upstream names `list_scopes`, `allow` and `deny`. Exposed names `toolgate_list_scopes`, `toolgate_allow` and `toolgate_deny`, as the checklist names them. They are the only tools whose proxied name isn't `<server>__<tool>`. Rules may name them as `toolgate__allow` or `toolgate_allow` (§6).
- **Calls** go through the full call path: `tool.decision`, phase 2 approvals if an admin's rule set ever needs one, `tool.result`. The operations also write their own events (below).
  - Today `routeProxiedCalls` (`internal/gateway/tools.go`) sends a call to `callTool` only when its name holds `__`. It also treats the three exact names `toolgate_list_scopes`, `toolgate_allow` and `toolgate_deny` as proxied names of server `toolgate`. So a call of one in a session that doesn't list it still reaches the call path, and its refusal writes `tool.decision` (row 208).
- **Search rows:** the leader writes them for the built-in server when it gets the Lease, so `toolgate_find_tools` can find these tools.

**Without the web UI**, the scope step is off (phase 3 §10), so scope entries do nothing, `deny` entries included. They stay in the database, as lists do (phase 3 row 162). So a deny stops applying when the UI is turned off (row 254).

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
- `servers` holds 1 to 32 items. Each is a server name, or `<server>__<tool>` with the tool's exposed name. The tool maps it to the upstream name (§6). A tool of the built-in server is named by its upstream name under server `toolgate`, such as `toolgate__deny`.
- A tool that is hidden, or a server the user has no access to, is refused with phase 3's "You don't have access to server `x`." or "Server `x` has no tool `y`." Nothing is written.
- A tool of a server that has a server-level `deny` entry on the target is refused with "Server `github` is denied in workspace `coder/alice/dev`. Allow the server first." or "… in this session. Allow the server first." Nothing is written (row 249).

The target:
- **A workspace:** any workspace ID that is valid for a list (phase 3 §10: at most 256 characters, not `claimed/…`).
- **A session:** one of the caller's own open sessions, by its session ID, whose workspace is verified. Another user's session, or an ended one, gets "You have no open session `x`.". An unverified session gets "Session `x` has no verified workspace, so it uses only your default for unverified sessions." A claimed session ID never adds servers (phase 3 §10).

Then:
1. **Find or insert a request.** First find the request named by `RequestState`. It is a lookup key only, so every field must match: principal, kind `scope`, target and change, as in phase 3 §10 step 1. Then look for a pending `scope` request with the same target and change. Only when none is found, insert a pending access request of kind `scope`, with the change, the target and its workspace, and write `access.request`. Notify and commit. The limits of phase 3 apply (100 pending per principal). A named request that was denied or has expired gets phase 3 §10 step 1's messages (row 242).
2. With URL elicitation, return it with `RequestState` set to the request ID, as phase 3 §10 step 3. On `accept`, wait as phase 3 §10 step 4 waits: on `approval:<id>`, missed events and the 10 s tick, up to `approvalTimeout`. Return `{"status": "approved"|"denied"|"expired", "request", "url"}`. The wait counts toward phase 2's 32 waits per principal per replica.
3. Without elicitation, or on `decline` or `cancel`, return `{"status": "pending", "request", "url"}` and "Approve this change at `<url>`." This isn't a tool error: the request exists.

**Nothing changes until approval.** The page `/ui/approvals/<id>` shows a `scope` request as phase 3 shows a `change` request: the target, a before-and-after list, and which session asked. Approve needs the phase 2 step-up. Approving applies the change in the same transaction as the compare-and-set:
- **Workspace, server:** add it to the workspace's list, creating the list if there was none (phase 3), and delete the workspace's `deny` entries for that server.
- **Workspace, tool:** delete a `deny` entry for that tool. If the server isn't reachable in that workspace by its list or default, insert an `allow` entry for the tool.
- **Session, server:** insert `session_additions` (phase 3), and delete the session's `deny` entries for the server.
- **Session, tool:** as for a workspace, with the session's entries.
- A **session** target is checked as phase 3 §10 step 6 checks it: the session must still be open, in the same verified workspace. Otherwise the transaction rolls back and the page says why.
- Notify `scope:<id>` for a workspace and `session:<id>` for a session. Write `workspace.change` or `session.addition` with `via` set to the request ID, and `access.approve`.

**A session-scoped allow ends with the session.** Ending a session deletes its `session_additions` and its `allow` entries, and expires its pending `scope` requests (§2). Its `deny` entries stay until the session is forgotten.

### `toolgate_deny`

AC-4-16. Input as `toolgate_allow`. A deny narrows, so it applies at once, with no request and no step-up. One transaction locks the target, then the `user_scopes` row, in phase 3's lock order:
- **Workspace, server:** remove it from the list if it is listed, insert a `deny` entry for the server, and delete the server's `allow` entries.
- **Workspace, tool:** insert a `deny` entry for the tool, and delete its `allow` entry.
- **Session, server:** delete its `session_additions` row, insert a session `deny` entry, and delete its `allow` entries.
- **Session, tool:** insert a session `deny` entry for the tool.

Then write `workspace.change` or `session.addition` (with `denied`), notify `scope:<id>` or `session:<id>`, and commit. A deny entry works with any default, including `all` with no list. So phase 3's 409 for "deny on a workspace with no list" doesn't apply to these tools, or to the CLI and API after this phase (§12). Deny entries don't count toward the 500-entry limit (§2, row 248).

A session's `deny` entries last until the session is forgotten (`session.forget`), not only until it ends. So a session that idles out and resumes still has its deny (row 240).

**The API and the CLI run the same steps.** `POST /api/v1/workspaces/changes` with `deny`, and `toolgate workspace deny`, run exactly these steps for every target. So phase 3's `deny` on a listed workspace and `deny --session` now also insert the deny entry. The tool and the API make the same rows (AC-4-14).

**`clear`** (phase 3) deletes the workspace's list and its `allow` entries, and keeps its `deny` entries. When `verified_default` is `all`, clearing a list widens, so it goes through phase 3's `change` request, as today (row 243).

A session target for a deny may be unverified, because a deny only narrows. A claimed ID can narrow, never widen (principle 2). The session must still be the caller's own.

**The 5 s bound.** The notification makes every replica send `list_changed` to the target's streams. The next decision loads fresh state. So the target's tools are withdrawn, and its calls refused, within 5 s (phase 3 §10's bound). The AC-4-16 test measures it across two replicas.

### Step 4 with scope entries

`policy.ScopeRows` gains the request's entries: the verified workspace's entries, the session's entries, and the unverified allow list. `policy.Scope` gains tool sets. For a request of kind `user`, step 4 decides (server, tool):
1. **Denied** if a `deny` entry of the verified workspace or of the session names the server with an empty tool, or names this tool. The refusal has step `workspace-scope` and reason `denied`, and reads "`github__delete_repository` is denied in workspace `coder/alice/dev`." or "… in this session."
2. **Allowed for the whole server** if phase 3's effective list allows the server.
3. **Allowed for this tool** if an `allow` entry of the verified workspace or of the session names this tool.
4. Otherwise refused, as in phase 3.

A server-level decision (`list_servers`, enable, search candidates) passes when rule 2 holds, or when rule 3 holds for any tool of the server, and no server-level deny matches. `tools/list` then lists only the allowed tools.

**A deny is never asked about.** Rule 1's refusal isn't the outside-the-list refusal of rules 2 to 4:
- An enable, or a call's implicit enable (§4, step 3), refuses it at once. No request is inserted.
- It isn't a search candidate (§5).
- `toolgate_list_servers` shows a denied server as `blocked` with the reason "denied in this workspace" or "denied in this session", not "enable it to ask".
- A phase 3 `server` approval ("Allow for this session" or "Always allow in this workspace") deletes no deny entry. Only an approved `scope` allow does (above).

**The wait gains an outcome.** Phase 3 §10 step 4 lists what ends the wait. Phase 4 adds one: **approved, but still refused.** The request is approved, but step 4 still refuses, for example because of a deny entry. The wait stops with that refusal, and writes `server.enable` (refuse, `workspace-scope`, with that reason). A call also writes `tool.decision`. So the waiter never runs on to `approvalTimeout` for an approved request (row 237).

In an unverified session, only the session's `deny` entries apply, and the unverified default or allow list decides rule 2. `allow` entries never apply. Workspace entries never apply, because the workspace is claimed. So a claimed field only narrows.

### Own scope

AC-4-17 needs no new tool. In any session, `toolgate_disable_server` drops a server at once (§4). In a verified workspace, an enable or a call of a server outside the list asks the user (§4, step 3). Both are meta-tool paths and don't need the `toolgate` server.

### Unverified allow list

AC-4-11. A user's default for unverified sessions is `none`, `all` or `list`. §2 says how `list` is stored.
- With `list`, an unverified session's effective list is the user's `unverified_servers`. It lists and can enable only those servers. Nothing else is added: unverified sessions still have no session additions.
- **Web UI:** the defaults form on the Workspaces page gains "Only these servers", with a checkbox for each server the user can access.
- **CLI:** `toolgate workspace defaults --unverified none|all|list`, and `toolgate workspace allow --unverified SERVER…` and `deny --unverified SERVER…`.
- Adding a server, moving from `none` to `list` with servers, or moving from `list` to `all`, widens, so it creates an access request and needs the step-up. Removing narrows and applies at once. These are phase 3 §10's rules.
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
2. If step 1 failed with a **version answer**, connect with `ProtocolVersion` set to `2026-07-28`. The go-sdk sends `server/discover`. The probe reads the result's `supportedVersions`, through a client sending middleware that sees the `*mcp.DiscoverResult`. If it lists 2025-11-25 or any revision before 2026-07-28, the server speaks an older revision, so step 1's version answer was transient, for example from a sidecar that is still starting. The probe then stores no revision and fails this attempt as transient, and the next step probes again from step 1 (row 245). Otherwise, if the go-sdk negotiates 2026-07-28 and `tools/list` succeeds, the revision is 2026-07-28.
3. Any other failure fails the probe, as in phase 1. The instance stays `starting`, and the next step probes again until `startTimeout`. The failure reason names both attempts when step 2 ran.

A **version answer** means the server is up and refused the 2025-11-25 handshake:
- a JSON-RPC error response to `initialize`, such as `-32601` or the go-sdk's `CodeUnsupportedProtocolVersion`;
- HTTP 400, 404 or 405 to `initialize`;
- an `initialize` result with a revision the go-sdk refuses;
- HTTP 400 to the first request after `initialize`, which is how a go-sdk server limited to 2026-07-28 answers.

Connection errors, timeouts, HTTP 429 and 5xx are not version answers. go-sdk v1.8.0 has no typed HTTP status error (#358), so `ConnectUpstream` records what it needs itself, and never matches error text:
- The `StreamableClientTransport`'s `HTTPClient` gets a `RoundTripper` that saves each response's status code with the JSON-RPC method of its request. The classifier reads that saved status.
- A `*jsonrpc.Error` body is typed. `jsonrpc2.ErrRejected` (-32005) is what the go-sdk returns for 429 and 5xx, so it counts as transient.
- A refused `initialize` revision is read from `InitializeResult.ProtocolVersion` in a client receiving middleware, because the go-sdk's own error for it is unexported.

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

**`server/discover` needs no session.** It is exempt from the session-header rules on both paths: the 400 below and §9's rules. It still needs a token. The stateful handler serves it without resolving or creating a toolgate session, and without binding an MCP session. For a discover, `newServer` returns one shared `mcp.Server` with no tools and no session state, so no `sessions` row is written (row 239).

So:
- **A client that supports 2025-11-25** is served on it. A client that tries 2026-07-28 first sends `server/discover`. The stateful handler answers it with the older revisions only, and the client falls back to `initialize`. This is the go-sdk client's behaviour, and the 2026-07-28 rule for clients that support both.
- **A client that speaks only 2026-07-28** sends its requests with the 2026-07-28 `_meta` fields and header, and is served statelessly. The 2026-07-28 spec lets a client skip `server/discover` (§15, reading 1). A client that calls it first and then stops, because the answer lists no 2026-07-28, can't be served without giving up the preference. That is this design's reading of the checklist (§15, reading 1).

**Stateless requests and toolgate sessions.**
- toolgate's session is (principal, `X-Toolgate-Session`), as always, so state lives in PostgreSQL and any replica can serve any request. A 2026-07-28 request other than `server/discover` without `X-Toolgate-Session` gets HTTP 400 with "This client sends no session. Use the toolgate plugin's headers helper, or a client on MCP 2025-11-25." There is no `Mcp-Session-Id` to tie a session to (§9; §15, reading 2). The body is a plain JSON-RPC error with code -32600 (invalid request), never `UnsupportedProtocolVersionError` (-32022). This matters: by the 2026-07-28 Streamable HTTP backward-compatibility rules, a dual-era client that sees a 400 without a recognized modern error treats the server as legacy and falls back to `initialize`. So claude.ai connectors and other helper-less dual-era clients reach 2025-11-25 and get an `mcp.` session (§9). A modern error would keep them on 2026-07-28 with no session.
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
- `auth.Middleware` gets a third mode for `/mcp`: the session header is optional on requests that aren't 2026-07-28, and on `server/discover` of any revision (§8). A missing header leaves `Identity.Session` empty, and `mcpEndpoint` fills it in. For `server/discover` it stays empty, and the request makes no session (row 239).
- **Session IDs.** For an `initialize` without `X-Toolgate-Session`, `mcpEndpoint` generates the ID first: 32 lowercase hex characters from `crypto/rand`. In go-sdk v1.8.0, `ServerOptions.GetSessionID` is a `func() string`, so it can't read the request context. But `newServer` runs once per `initialize`. So `newServer` builds that request's server with a `GetSessionID` closure that returns the ID `mcpEndpoint` generated. The toolgate session ID is then `mcp.<id>`. Later requests carry `Mcp-Session-Id`, and their session is `mcp.<Mcp-Session-Id>`.
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
| 180 | `toolgate_find_tools` | a server outside the list, in a verified workspace | Returned with `access: ask`. In an unverified session it isn't returned. A server or tool with a deny entry is never returned (row 236). | AC-4-02 |
| 181 | Call of a catalog tool | no live instance, newest row didn't fail | The call inserts a `pending` row and waits, with progress, up to `startTimeout`. Then it is decided again and forwarded. | AC-4-13 |
| 182 | Two sessions call a per-user server with no live instance | each other | The advisory lock gives one instance (phase 1 row 1). Both calls wait on it. | integration |
| 183 | Start fails | calls waiting on it | Each call fails with the reason and `tool.decision` (refuse, `not-ready`). The server stays enabled with no tools (phase 1 row 12). | AC-4-13 |
| 184 | Call | newest row failed with the template's current generation, no live row | Refused at once with the reason and "Enable `<server>` again to retry." Calls never retry a failed start (§3). | unit |
| 185 | Instance becomes ready with a list that differs from the catalog list | sessions listing the catalog list | The catalog row is written in the `ready` transaction. `instance:<id>` sends `list_changed`, and sessions list the real tools. | AC-4-12 |
| 186 | Called tool missing from the ready instance's curated list | call that waited for the start | Refused with `not-offered`, "doesn't offer `<tool>` any more". Nothing is forwarded. | unit |
| 187 | Template's generation changes | no instance, catalog row of the old generation | The catalog list falls back to declared tools, then to the older row (§3). The first ready instance of the new generation writes its row. | unit |
| 188 | Template declares tools | instance ready with other tools | Nothing is learned. Sessions with that live instance list its real tools, and others list the declared ones. | unit |
| 189 | Instances of one generation for two users become ready | each other | Each search rebuild takes the `toolsearch/<template uid>` advisory lock first (§5), so the rebuilds run in turn and neither `ready` write fails with `unique_violation`. The upsert's last commit wins. Either list is a list of that generation. | unit |
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
| 208 | `toolgate_*` call | session where `toolgate` isn't allowed | `routeProxiedCalls` sends the exact name to the call path (§7). Refused at step 4, like any server's tool, with `tool.decision`. | AC-4-14 |
| 209 | Service account or SPIFFE identity calls a `toolgate_*` tool | — | Refused at step 1. | unit |
| 210 | Gateway without the web UI | — | The `toolgate` server isn't in the catalog, and its tools are refused with `not-offered`. | unit |
| 211 | Template named `toolgate` | — | Invalid, "reserved for the built-in server", and counts as deleted (row 251). | unit |
| 212 | Unverified allow list changed (replica B) | open unverified sessions (replica A) | `scope:<id>` sends `list_changed`. The next decision uses the new list. Within 5 s. | AC-4-11 |
| 213 | 2025-11-25 probe fails with a connection error or 5xx | instance `starting` | Not a version answer. No 2026-07-28 attempt, no revision stored, and the next step probes again (§8). | AC-4-07 |
| 214 | Upstream speaks only 2026-07-28 | probe | The 2025-11-25 attempt gets a version answer, 2026-07-28 succeeds, and the row is `ready` with that revision. | AC-4-07 |
| 215 | `server/discover` fails 3 times in a row | 2026-07-28 instance `ready` | Failed, as phase 1's ping rule. | unit |
| 216 | 2026-07-28 upstream's listen request ends | monitoring | The monitor connects again on the next tick and re-lists once. | unit |
| 217 | Dual-revision client sends `server/discover` | — | The stateful handler lists only the older revisions. The client initializes on 2025-11-25. | AC-4-08 |
| 218 | 2026-07-28 request without `X-Toolgate-Session`, other than `server/discover` | — | HTTP 400 with the message in §8, as a plain -32600 error and never -32022. A dual-era client then falls back to `initialize` on 2025-11-25. A `server/discover` follows row 239. | unit, and e2e in AC-4-08 and AC-4-10 |
| 219 | 2026-07-28 listen stream on replica A, call on replica B | a change for the session | B's write notifies. A sends `list_changed` on the listen stream. | integration |
| 220 | 0.3 replica serves a session | rolling upgrade to 0.4 | It ignores `scope_entries`, `unverified_servers`, `unverified_list` and found tools, so a deny entry isn't enforced on it, and `list` counts as the stored `none` there, until the rollout ends. Its types drop `toolFilter` and the new `tools.<name>` fields, so hidden tools are listed and forwarded there, each call still with a recorded decision, and renames don't apply. `tools.<name>.blocked` works on both versions, so it is the setting to use when hiding a tool is a security control (§13, upgrade note). Its writes follow row 241, and its session ends row 247. Like phase 3 row 130. | unit |
| 221 | 0.3 replica gets an approval page for a `scope` request | rolling upgrade | It answers 404. The user opens the link again after the rollout. | unit |
| 222 | `/mcp` request with no token, or a refused token | — | 401 with `WWW-Authenticate` naming the metadata URL, plus `error="invalid_token"` for a refused token. | AC-4-09 |
| 223 | Connector token whose audience isn't in `oidcAudience` | — | 401 and `auth.refuse` with "audience not allowed" (phase 3 row 149). | unit |
| 224 | `initialize` without `X-Toolgate-Session` | — | A new `mcp.<id>` session, bound to the MCP session. | AC-4-10 |
| 225 | Client deletes the MCP session, or it idles out on the replica | `mcp.` session | The toolgate session ends at once and writes `session.end`. Its per-session instances stop. | AC-4-10 |
| 226 | `mcp.` session's request reaches another replica | — | 404, the client initializes, and a new toolgate session starts empty. The old one idles out. | unit |
| 227 | `X-Toolgate-Session` starting with `mcp.` | — | 400. | unit |
| 228 | Search adds a 51st found tool | — | The oldest is deleted in the same transaction. `list_changed` follows. | unit |
| 229 | Session ends | found tools, scope entries for the session | Found tools and `allow` entries are deleted in the end transaction. `deny` entries stay until `session.forget` (row 240). | integration |
| 230 | Search mode header changes between requests | — | The next `tools/list` follows the new mode. Decisions don't change. | unit |
| 231 | Leader rebuilds search rows (replica A's searches) | search in flight | The search sees one consistent set of rows. `toolsearch:<uid>` sends `list_changed` to sessions with found tools of that template. | unit |
| 232 | Call of a found tool | the template was deleted since the search | Refused with `not-offered` at step 1. The found row is deleted on the next search or at the session's end. | unit |
| 233 | Scope entry for a tool of a renamed tool | curation changes the exposed name | Entries hold upstream names, so they keep applying. | unit |
| 234 | Enable of an `onCall` server with a catalog list | newest row failed, no live row | The enable inserts a `pending` row and doesn't wait (§3). The next call waits on that row. For `scope: shared`, this is how any user retries. | unit |
| 235 | Call | newest row failed with an older generation, no live row | Not refused. The call inserts a `pending` row of the current generation and waits (§3). | unit |
| 236 | Call or enable of a server or tool with a deny entry | verified workspace | Refused at once with step `workspace-scope` and reason `denied`. No request and no elicitation. Never a search candidate. `toolgate_list_servers` shows `blocked` with "denied in this workspace". | AC-4-16 |
| 237 | A `server` request is approved ("this session" or "always") | a deny entry for that server, and a waiting enable or call | The approval deletes no deny entry. The wait ends with "approved, but still refused": the deny's refusal, and `server.enable` (refuse, `workspace-scope`, `denied`). It doesn't wait for `approvalTimeout`. | unit |
| 238 | Call of a found tool marked `ask` | verified workspace, server not enabled | The tool is registered on the session's MCP server, so the call goes through the go-sdk tool path. It gets a URL elicitation and phase 3's wait (§4, step 3). | AC-4-02 |
| 239 | `server/discover` without `X-Toolgate-Session`, on any revision | — | The stateful handler answers with the older revisions. No toolgate session, no `sessions` row and no MCP session binding. | AC-4-08, AC-4-10 |
| 240 | Resume (phase 3 §11) | `deny` entries of the session | The entries outlived the end, so the resumed session still refuses the server or tool. Its `allow` entries were deleted at the end. `session.forget` deletes the deny entries. | integration |
| 241 | 0.3 replica gets a defaults change | `list` stored as `none` with `unverified_list` | 0.3 sees `none`. A change to `all` widens on 0.3 too, so it needs a request and a step-up. A 0.3 write of `none` leaves `unverified_list` as it was, so a `list` stays a `list`. | unit |
| 242 | `toolgate_allow` round trip carrying `InputResponses` and `RequestState` | its pending `scope` request | Step 1 finds the named request. No second request is inserted, and the wait watches that request. | unit |
| 243 | `clear` of a workspace (API, CLI or UI) | its scope entries | Deletes the list and the `allow` entries, and keeps the `deny` entries. With `verified_default` `all`, it widens through phase 3's `change` request. | unit |
| 244 | API or CLI `deny` of a listed workspace or of a session | — | Runs §7's steps, so it inserts the same deny entry as `toolgate_deny`. | AC-4-14 |
| 245 | Probe step 2's `server/discover` lists 2025-11-25 or an older revision | instance `starting` | Step 1's version answer was transient. No revision is stored. The attempt fails as transient, and the next step probes again from step 1. | AC-4-07 |
| 246 | Template becomes invalid | instance running | Curation, the reverse map, call step 1 and the search rows use the last valid generation (§6). Calls go on. | unit |
| 247 | 0.3 leader ends a session | rolling upgrade, session-scoped rows | The 0.4 leader's 30 s pass deletes the ended session's `allow` entries and found tools, and expires `scope` requests whose target session ended after the request was created (§2). | unit |
| 248 | Deny | principal has 500 `allow` entries | The deny applies. Deny entries don't count toward the limit. | unit |
| 249 | `toolgate_allow` of a tool | server-level `deny` entry on the target | Refused when it's requested, with "Allow the server first." Nothing is written. | unit |
| 250 | Call of a tool with no `allow` entry | server reached only by another tool's `allow` entry, not enabled | The call enables the server without asking, and an instance may start. Step 7 refuses the call at step 4. This is intended (§4). | unit |
| 251 | Template named `toolgate` | its instances running from before the upgrade | It counts as deleted. The catalog serves no last valid generation for it, its instances stop, and the name means the built-in server. | unit |
| 252 | `toolgate_find_tools` query with no words, or only stop words | — | `{"tools": []}` with the message in §5. No SQL runs when no words remain. | unit |
| 253 | Override `name` equals a hidden tool's upstream name | — | With `toolFilter.deny`, the template is invalid. With `toolFilter.allow`, the tool list checks fail the instance. | unit |
| 254 | UI turned off (phase 3 row 162) | scope entries exist | The rows stay but do nothing, `deny` entries included, so a deny stops applying after the restart. | unit |
| 255 | Embeddings endpoint down, slow or erroring | search | The search uses full-text only, writes a metric, and answers. | unit |
| 256 | Embeddings model changed in the setting | rows embedded with the old model | The next rebuild embeds them again. Until then, those rows rank by full-text only. | integration |
| 257 | No embeddings setting | — | Full-text only, as before. No call is made. | unit |

Any combination not in this table is a design gap: hand back `HANDOFF: question` with the case.

## 11. Settled choices

- **Settings** (flat, in the generated reference):
  - `toolSearch` (default `false`): search mode when the client sends no `X-Toolgate-Tool-Search` header (§5);
  - `mcpPublicURL` (default `publicURL` + `/mcp`): the resource the protected resource metadata names (§9).

  The chart sets them from `config.toolSearch` and `config.mcpPublicURL`.
- **Request headers:** `X-Toolgate-Tool-Search` (`on` or `off`). `X-Toolgate-Session` becomes optional on `/mcp` for requests that aren't 2026-07-28 and for `server/discover` of any revision, and values starting with `mcp.` are refused (§8, §9).
- **Client environment:** `TOOLGATE_TOOL_SEARCH` (`on` or `off`) makes `toolgate headers` send the header.
- **CRD fields** on `MCPServerTemplate`: `start` (§3), `declaredTools` (§3), `toolFilter` (§6), and `name`, `description` and `inputDescriptions` under `tools.<name>` (§6). All are checked by CEL in the CRD and again by the catalog.
- **Meta tool:** `toolgate_find_tools` (§5).
- **Statuses:** `toolgate_list_servers` shows an enabled server with no live instance as `stopped` (§3). `toolgate_server_status` returns `ready` for the built-in server (§7).
- **Refusal reasons:** step `workspace-scope` gains reason `denied` for a deny entry (§7). It never asks.
- **Columns:** `user_scopes.unverified_list` stores the default `list`, with `unverified_default = 'none'` (§2).
- **Locks:** a search rebuild takes `pg_advisory_xact_lock(hashtextextended('toolsearch/' || template_uid, 0))` first (§5).
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
  - 500 `allow` scope entries and 64 unverified servers per principal. `deny` entries have no limit (§2);
  - `toolgate_list_scopes` 200 workspaces and 200 sessions.
- **Audit events:** no new types.
  - `server.enable` gains `reason: first-call`.
  - `tool.decision` and `tool.result` gain `exposedTool` when it differs from `tool`.
  - `instance.ready` gains `mcpRevision`.
  - `workspace.change` and `session.addition` gain `tools`, `denied` and `via: tool` for changes from the `toolgate` server.
  - `workspace.defaults` gains the unverified allow list.
- **Libraries:** nothing new in `go.mod`.
  - `github.com/modelcontextprotocol/go-sdk` v1.8.0: `ClientSessionOptions.ProtocolVersion`, `StreamableHTTPOptions.Stateless`, `ServerOptions.SupportedProtocolVersions`, `ServerOptions.SetCacheable`, `ServerOptions.GetSessionID` (a `func() string`, §9), `jsonrpc.DecodeMessage`, `oauthex.ProtectedResourceMetadata` and `auth.ProtectedResourceMetadataHandler`;
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

A `deny` runs exactly `toolgate_deny`'s steps (§7) for every target. So a `deny` on a workspace with no list inserts deny entries instead of answering 409, and a `deny` on a listed workspace or a session inserts them too. `clear` deletes the workspace's list and its `allow` entries, and keeps its `deny` entries (§7). Every widening change of phase 3's shape still creates a `change` request. A change that phase 3's shape can't hold creates a `scope` request.

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
- §7: `toolgate_find_tools`, search mode, calls that enable, catalog tools, curated names, and the `toolgate` server with its three tools. The resume sentence ("its instance is started without waiting") changes too: an `onCall` server with a catalog list is restored without an instance, and starts on its first call (§3);
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

**Skipping `server/discover` (AC-4-08).** The MCP 2026-07-28 spec lets a client skip discover. "Versioning and Compatibility", section "Protocol Version Negotiation", says: "Clients **MAY** call it before sending any other requests to learn the server's supported versions up front, but are not required to: a client is free to invoke any RPC inline and handle `UnsupportedProtocolVersionError` if its preferred version is not supported." Source: https://modelcontextprotocol.io/specification/2026-07-28/basic/lifecycle, read on 2026-11-06. The worker checks that the spec still says this before writing the AC-4-08 test. If it no longer allows a client to skip discover, the test falls back to reading 1's earlier form: the client goes on past the discover answer. Then the worker hands back `HANDOFF: question`, so §15 records it.

| Scenario | Test | What it checks |
|---|---|---|
| AC-4-01 | e2e | With search on, `tools/list` holds the meta tools and `toolgate_find_tools` only. A search returns tools from several allowed servers with schemas. Calling one runs, writes `server.enable` with `reason: first-call`, `tool.decision` and `tool.result`. |
| AC-4-02 | integration | A blocked tool, a tool outside access, and a tool needing `attested` from a `user` session are never returned. In a verified workspace, a server outside the list is returned with `access: ask` and stays in the search-mode `tools/list`. Its first call goes through the go-sdk tool path and creates a session-addition request with a URL elicitation. A server with a deny entry is never returned, and its call is refused at once with reason `denied` and no request. In an unverified session it isn't returned. |
| AC-4-03 | integration | The frozen fixture (§5) through the real rebuild and search in PostgreSQL. At least 90 percent of tasks have the expected tool in the top 5. |
| AC-4-04 | e2e | An allow-list template and a deny-list template. Hidden tools are absent from `tools/list` and search, and calls by either name are refused with a recorded decision and nothing forwarded (the test server counts calls). |
| AC-4-05 | e2e | A renamed tool with a new description and input description. The client sees the override. The test server receives the call under the upstream name. A template whose override breaks the 64-character rule is invalid. |
| AC-4-07 | e2e | Three instances: 2025-11-25 only, both, and 2026-07-28 only. Each becomes `ready` with the expected `mcp_revision`, passes the monitor, serves tools, and a tool-list change on each reaches an open session. A unit test injects a 503 on the first 2025-11-25 attempt and checks that no 2026-07-28 attempt is made and no revision stored. Another unit test gives the 2025-11-25 attempt a version answer while `server/discover` lists 2025-11-25, and checks that no revision is stored and the next attempt starts from step 1 (row 245). |
| AC-4-08 | e2e | A go-sdk client at its default (tries 2026-07-28 first) ends on 2025-11-25. A 2026-07-28-only client lists, enables, disables, calls, gets `list_changed` through `subscriptions/listen`, and completes an approval. Its `tools/list` results carry `cacheScope: "private"`. The only-2026-07-28 client skips `server/discover`: it is the go-sdk client with a sending middleware that refuses `initialize`, and answers `server/discover` itself with only 2026-07-28 without sending it. So toolgate sees no discover, and the first request it gets carries the 2026-07-28 `_meta` (§15, reading 1). A `server/discover` with no `X-Toolgate-Session` gets the older revisions and makes no session (row 239). |
| AC-4-09 | e2e | A request with no token gets 401 and the header. The metadata document names the fake IdP. The test client follows the MCP authorization flow with library code only: `oauthex.ParseWWWAuthenticate` for the 401, a read of the metadata document, go-oidc discovery, and `golang.org/x/oauth2` authorization code with PKCE and `resource` against the fake IdP's connector client. Its requests then act as the user. It doesn't use `auth.AuthorizationCodeHandler`: go-sdk v1.8.0 refuses an `http://` private-IP authorization server, which is what the e2e fake IdP is (#360). The claude.ai connector itself is checked by the owner by hand, and that check doesn't gate. |
| AC-4-10 | e2e | A client with no `X-Toolgate-Session`: list, enable, disable, a call and an approval work. Two such clients get separate sessions. A `server/discover` before `initialize` gets the older revisions, not a 400 or 401, and creates no `sessions` row and no MCP session (row 239). A `DELETE` ends the session with `session.end`. An idle release ends it too (with the short e2e timeouts of `internal/testtime`). |
| AC-4-11 | integration | `list` with two servers: an unverified session lists and enables only those. Adding a server needs an approved request. Removing one withdraws it within 5 s. The UI path through chromedp, as phase 3. |
| AC-4-12 | e2e | A server is listed and searched with no instance running, from its declared tools and then from a learned list. After the instance becomes ready with a different list, an open session gets `list_changed` and the real list. |
| AC-4-13 | e2e | A call to a cataloged server with nothing running starts it, gets progress notifications, and is forwarded. A start failure fails the call with its reason. A `start: onEnable` template starts on enable. |
| AC-4-14 | integration | The three tools work in a session that allows `toolgate`, and are refused at step 4 in one that doesn't, and by a rule naming `toolgate`. They refuse another user's session. A call of a `toolgate_*` tool where `toolgate` isn't allowed writes `tool.decision` (row 208). The same change through the tool and through the API gives the same rows, for every target: a deny on a workspace with no list, on a listed workspace and on a session, and a `clear` after them (rows 243 and 244). |
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
- `docs/admin/web-ui.md`: one sentence in "Workspace scoping": with the UI off, the deny entries of the `toolgate` tools and the CLI do nothing too, so a deny stops applying (§7, row 254);
- the CHANGELOG and `docs/admin/templates-and-rules.md`: an upgrade note. `toolFilter` and the override fields take effect only after every replica runs 0.4. To hide a tool as a security control, use `tools.<name>.blocked`, which works on both versions (row 220);
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

1. **Clients on 2026-07-28 (AC-4-08).** On Streamable HTTP, the 2026-07-28 spec has a dual-era client find out the server's era by sending a modern request first, not by calling `server/discover` ("Versioning and Compatibility", "Backward Compatibility with Initialization-Based Versions"). toolgate can't tell that request from a 2026-07-28-only client's, so it decides by the session header:
   - **Without `X-Toolgate-Session`** (claude.ai connectors and most clients): the request gets the plain 400 in §8. A dual-era client falls back to `initialize` and is served on 2025-11-25, as the checklist's first sentence asks. A client that calls `server/discover` first gets only the older revisions, with the same result.
   - **With `X-Toolgate-Session`** (the toolgate plugin's headers helper): the request is served on 2026-07-28. A 2026-07-28-only client is served this way, as the checklist's second sentence asks. A dual-era client that sends the header and a modern request first also stays on 2026-07-28. It loses nothing: every AC-4-08 feature works on 2026-07-28.
   - A 2026-07-28-only client without the header can't be served (reading 2).
   The AC-4-08 test covers all three cases. The operator settled this reading for the owner (#341).
2. **2026-07-28 without a session header (AC-4-08, AC-4-10).** AC-4-10 ties a helper-less session to `Mcp-Session-Id`, which 2026-07-28 removes. So a 2026-07-28 client must send `X-Toolgate-Session`, and gets 400 without it. `server/discover` is the exception on every revision, because it makes no session (§8).
3. **"A session may use the server" (AC-4-13).** It means the server is enabled in the session: by an enable, a call or a resume. Not every server the session's policy allows.
4. **Which list wins (AC-4-12).** Declared tools beat learned ones for the same template. Learned lists of the current generation beat those of older generations. An older generation's list is used rather than none.
5. **Search mode lists found tools (AC-4-01).** "Instead of every server's tools" is read as "not every server's tools". The list still holds the enabled servers' tools and the tools the session found, because Claude Code can only call listed tools.
6. **Tool-level allows (AC-4-15).** Allowing a tool undoes its deny. If its server isn't otherwise allowed, it allows that one tool. Under a server-level deny, a tool allow is refused when it's requested, and the user allows the server first (§7).
7. **Deny on a workspace with no list (AC-4-16).** It takes effect through a deny entry. Phase 3's 409 is dropped.
8. **Disable (AC-4-17).** It works as in phase 1. A later call enables the server again.
9. **The metadata path (AC-4-09).** The header names `/.well-known/oauth-protected-resource/mcp`, the RFC 9728 path for a resource at `/mcp`. The same document is also served at `/.well-known/oauth-protected-resource`.
10. **Curation follows the current template.** A running instance gets new curation at once, as it gets new `blocked` overrides today.
11. **The `toolgate` server needs the web UI.** Without it, allows can't be approved, so the server isn't offered.
12. **Servers that have never run (AC-4-01).** "From every server the session may use" covers servers with a catalog list. A server that has never run and declares no tools has no catalog list, so search can't find it until an instance becomes ready once. Admins who want it searchable declare its tools (§3).

## Later

Ideas outside the checklist, for the owner to consider for a later phase. None is part of this design.
- Idle stops for `onCall` servers that are still enabled, since a call can start them again.
- Defaults and the unverified allow list through the `toolgate` tools.
- Overrides of nested input-schema descriptions, and globs in tool filters.
- A catalog for each user, for servers whose tools differ by user.
- Search over servers that have never run, by their template description.
- Session affinity in the chart for connectors.
- Metrics for search latency and for the revisions in use.
