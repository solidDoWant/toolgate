# Phase 4 acceptance checklist: tool search and curation

Same rules as phase 1. The tag is `[AC-4-nn]`. This checklist is at feature level. The owner reviews it again, with the phase 4 design, before phase 4 starts.

## Tool search

- **AC-4-01 Search instead of listing.** With tool search on for a session, its `tools/list` offers the meta tools and a `toolgate_find_tools` tool instead of every enabled server's tools. Given a task description, `toolgate_find_tools` returns the most relevant tools from the servers this session may use, with their names and input schemas. Calling a returned tool works as a normal proxied call, with the same policy and audit.
- **AC-4-02 Search respects access.** `toolgate_find_tools` never returns a tool the session's policy refuses: blocked tools, tools outside the workspace's access list, or tools needing an environment the session lacks. It can return tools of servers the session hasn't enabled yet, marked as needing an enable or an approval.
- **AC-4-03 Search quality.** On a fixed test set of task descriptions and expected tools across at least 50 tools, the expected tool is in the top 5 results for at least 90 percent of the tasks.

## Tool curation

- **AC-4-04 Tool filtering.** A template can list the upstream tools it exposes (an allow list) or hides (a deny list). Hidden tools are absent from `tools/list` and search, and calls to them are refused, even when called by name.
- **AC-4-05 Name and description overrides.** A template can give a tool a different exposed name, description or input-schema description text. Clients see the override, while calls reach the upstream tool by its own name. Overridden names follow the phase 1 naming and length rules.

## Release

- **AC-4-06 Release.** Release `0.4.0` is published, with docs for tool search and tool curation.
