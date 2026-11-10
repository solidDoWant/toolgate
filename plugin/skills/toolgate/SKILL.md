---
name: toolgate
description: How to use the toolgate gateway's MCP servers. Use when a task needs a tool that is not loaded yet, or when the toolgate_list_servers, toolgate_enable_server, toolgate_disable_server or toolgate_server_status tools are available.
---

# Using toolgate

First check which tools you have. If you have `toolgate_find_tools`, tool search
is on: follow "Tool search mode". Otherwise follow "Enable mode".

## Tool search mode

1. **Find.** Call `toolgate_find_tools` with a short description of the task.
   It returns tools you can call now.
2. **Call.** Call the tool you need. Its server starts on the first call; wait
   for the call to return.
3. **Stopped servers.** If a server is `stopped` in `toolgate_list_servers`, or
   search finds none of its tools yet, call `toolgate_enable_server` with its
   name, wait for it to return, then search again. Call
   `toolgate_disable_server` when you no longer need a server.
4. **Blocked servers.** If a server shows `blocked` in `toolgate_list_servers`
   or `toolgate_server_status`, or a search result has access `ask`, call one
   of its tools to ask the user for access, or use `toolgate_allow`. Do not
   ask in a loop.

## Enable mode

The `toolgate` MCP server is a gateway. At the start it offers only four tools.
The other MCP servers are off until you turn them on in this session.

1. **List first.** Call `toolgate_list_servers` before you enable anything. It
   shows the servers you may use, what each one does, whether it is already
   enabled in this session, and its status.
2. **Enable only what the task needs.** Call `toolgate_enable_server` with the
   name of one server that the task needs. Do not enable servers "just in
   case": each one starts a process and adds tools to the session. Starting a
   server can take a while; wait for the call to return. The call returns the
   tools that were added. They are named `<server>__<tool>`.
3. **Disable when done.** When the task no longer needs a server, call
   `toolgate_disable_server` with its name.

If an enable fails, or a server shows `failed` or `blocked`, call
`toolgate_server_status` with its name and tell the user the reason. Do not
retry a refused call in a loop: a refusal comes from the admin's policy, and
the message says which rule refused it.
