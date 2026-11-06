# Phase 4 acceptance checklist: tool search, lazy start, delegated access, curation, MCP revisions and connectors

Same rules as phase 1. The tag is `[AC-4-nn]`. This checklist is at feature level. The owner reviews it again, with the phase 4 design, before phase 4 starts.

## Tool search

- **AC-4-01 Search and call, no enable needed.** With tool search on for a session, its `tools/list` offers the meta tools and a `toolgate_find_tools` tool instead of every server's tools. Given a task description, `toolgate_find_tools` returns the most relevant tools from every server the session may use (by policy, workspace list and defaults), with their names and input schemas. Calling a returned tool works as a normal proxied call, with the same policy and audit, without a separate enable: the first call records `server.enable` (reason `first-call`).
- **AC-4-02 Search respects access.** `toolgate_find_tools` never returns a tool the session's policy refuses: blocked tools, tools outside the workspace's access list, or tools needing an environment the session lacks. In a verified workspace it may return tools of servers outside the list, marked as needing approval. The first call to one then asks the user, as phase 3's session additions do.
- **AC-4-03 Search quality.** On a fixed test set of task descriptions and expected tools across at least 50 tools, the expected tool is in the top 5 results for at least 59 of the 66 tasks in `test/fixtures/toolsearch/` (89 percent), with the optional embeddings endpoint configured. (Changed by the owner on 2026-11-06 from 90 percent. Hybrid search reached 59 of 66 with both `bge-small-en-v1.5` and `bge-base-en-v1.5`. A cross-encoder reranker scored lower, 52 of 66, so it was dropped.)
- **AC-4-18 Search quality in use.** (Added by the owner on 2026-11-06.) For each `toolgate_find_tools` call, toolgate records which tools it returned and their ranks, without the query text. When the session then calls one of those tools, toolgate records a hit at that rank. When the session searches again before calling any returned tool, toolgate records a re-search. Prometheus metrics give the number of searches, hits by rank, re-searches and searches with no follow-up call, labelled by whether search was degraded and by the embedding and reranker models in use, so an admin can compare models in real use. The audit log carries the search and the hit, so an admin can see per-server hit rates.

## Lazy start

- **AC-4-12 Tool catalog.** toolgate remembers each template generation's tool list from the last instance that became ready, or from tools the template declares. Tool search and the session's tool list use it while no instance runs. When a running instance's tools differ, sessions get the real list with `list_changed`.
- **AC-4-13 Start on first call.** A server whose tools are in the catalog doesn't start until a call needs it. That call waits while the instance starts, with progress notifications, up to `startTimeout`, then is forwarded. A start failure fails the call with its reason. A template can choose `start: onEnable` to start as soon as a session may use the server.

## Delegated access

- **AC-4-14 Access tools are ordinary tools.** `toolgate_list_scopes`, `toolgate_allow` and `toolgate_deny` belong to a built-in server named `toolgate`. They're the same operations as the CLI and the web UI, as tool calls. Policy, workspace lists, defaults, rules and audit govern them like any other server's tools, so they're usable in any session where the user allows the `toolgate` server, and nowhere else. They act only on the calling user's own workspaces and sessions.
- **AC-4-15 Allow needs the user.** `toolgate_allow`, for a workspace or a session of the same user and a set of servers or tools, creates a request and returns a link (and a URL elicitation where supported). Nothing changes until the user approves it in the browser with a step-up. A session-scoped allow ends with that session.
- **AC-4-16 Deny is immediate.** `toolgate_deny`, for a workspace or session of the same user and a set of servers or tools, takes effect at once. The target's affected tools are withdrawn within 5 seconds. Tool-level denies work inside an allowed server.
- **AC-4-17 Own scope.** In any session, a user's agent can drop a server from its own session at once (`toolgate_disable_server`) and, in a verified workspace, ask for a server outside the list, as in AC-4-02.

## Tool curation

- **AC-4-04 Tool filtering.** A template can list the upstream tools it exposes (an allow list) or hides (a deny list). Hidden tools are absent from `tools/list` and search, and calls to them are refused, even when called by name.
- **AC-4-05 Name and description overrides.** A template can give a tool a different exposed name, description or input-schema description text. Clients see the override, while calls reach the upstream tool by its own name. Overridden names follow the phase 1 naming and length rules.

## MCP protocol revisions

toolgate prefers revision 2025-11-25 (stateful sessions) on both sides, and uses 2026-07-28 (stateless) only with a peer that supports nothing older.

- **AC-4-07 Upstream on either revision.** An instance whose MCP server speaks 2025-11-25, or both revisions, is reached on 2025-11-25 as before. An instance whose server speaks only 2026-07-28 becomes `ready`, passes health checks, serves its tools, and its tool-list changes reach open sessions. A probe that fails for a transient reason doesn't fix the instance on the wrong revision.
- **AC-4-08 Client on either revision.** A client that supports 2025-11-25 is served on 2025-11-25, including a client that tries 2026-07-28 first. A client that speaks only 2026-07-28 can list, enable and disable servers, get `toolsListChanged` through `subscriptions/listen`, call tools and complete approvals. Its tool lists are marked `cacheScope: "private"`, so one session never sees another's tools.

## Connectors

toolgate works as a standard OAuth-protected remote MCP server, so clients without the toolgate CLI or plugin, such as a claude.ai custom connector, can use it.

- **AC-4-09 OAuth sign-in.** An MCP request with no token gets HTTP 401 with a `WWW-Authenticate` header pointing at `/.well-known/oauth-protected-resource`. That document names the IdP as the authorization server. A client that follows the MCP authorization spec (for example a claude.ai custom connector) signs the user in through the IdP, and its requests then act as that user.
- **AC-4-10 Sessions without the helper.** A client that sends no `X-Toolgate-Session` gets a toolgate session tied to its MCP session (`Mcp-Session-Id`). It has its own enabled set, works with list, enable, disable, calls and approvals, and ends when the MCP session ends or idles out.
- **AC-4-11 Unverified allow list.** A user can set an allow list of servers for their unverified sessions, in the UI or CLI, as an alternative to `none` or `all`. Such a session lists and can enable only those servers.

## Release

- **AC-4-06 Release.** Release `0.4.0` is published, with docs for tool search, tool curation the supported MCP revisions, and connecting from a claude.ai custom connector.
