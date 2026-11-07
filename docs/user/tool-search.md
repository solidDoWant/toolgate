# Tool search

Tool search lets Claude find the tools it needs by describing a task. It helps when your admin offers many servers. Claude then does not need a long list of tools in every session.

## Turn it on

Set the `TOOLGATE_TOOL_SEARCH` environment variable to `on` before you start Claude Code. `toolgate headers` then sends the `X-Toolgate-Tool-Search: on` header. Set it to `off` to turn the mode off. If you do not set it, the gateway setting `toolSearch` decides. Your admin sets it, and it is off by default.

The header only changes which tools Claude lists. It never changes what you are allowed to do.

## What Claude sees

In search mode, the tool list holds the toolgate tools, `toolgate_find_tools`, the tools of the servers you enabled, and the tools Claude found. In search mode, `toolgate_enable_server` and `toolgate_disable_server` are not listed, because a call to a tool turns the server on. If the rules block a server in your workspace, Claude calls one of its tools to ask you for access, or uses `toolgate_allow`. Outside search mode, `toolgate_find_tools` is not listed. A call of it fails with "Tool search is off for this session."

## How a search works

Claude calls `toolgate_find_tools` with a `query` of 1 to 2000 characters and a `limit` from 1 to 20 (5 if left out). The query must use English words. Each result has the tool name, server, description, input schema, `access` and `approval`.

- `access` is `allowed` when you can call the tool now.
- `access` is `ask` when the server is outside the list of your workspace. The first call asks you to allow the server. This only applies in a verified workspace.
- `approval` is `true` when each call needs a person to approve it.

The tools Claude finds stay in the tool list of the session, up to 50 tools. A new tool replaces the oldest one. When the session ends, toolgate forgets them. A search is not an audit event, but every call of a found tool is decided and recorded as usual.
