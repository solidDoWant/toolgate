# Phase 4 acceptance checklist: tool search, curation, MCP revisions and connectors

Same rules as phase 1. The tag is `[AC-4-nn]`. This checklist is at feature level. The owner reviews it again, with the phase 4 design, before phase 4 starts.

## Tool search

- **AC-4-01 Search instead of listing.** With tool search on for a session, its `tools/list` offers the meta tools and a `toolgate_find_tools` tool instead of every enabled server's tools. Given a task description, `toolgate_find_tools` returns the most relevant tools from the servers this session may use, with their names and input schemas. Calling a returned tool works as a normal proxied call, with the same policy and audit.
- **AC-4-02 Search respects access.** `toolgate_find_tools` never returns a tool the session's policy refuses: blocked tools, tools outside the workspace's access list, or tools needing an environment the session lacks. It can return tools of servers the session hasn't enabled yet, marked as needing an enable or an approval.
- **AC-4-03 Search quality.** On a fixed test set of task descriptions and expected tools across at least 50 tools, the expected tool is in the top 5 results for at least 90 percent of the tasks.

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
