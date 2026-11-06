# MCP revisions

This page tells you which MCP revisions the gateway speaks with clients, and why a client can get HTTP 400. A revision is one dated version of the MCP specification.

## Supported revisions

The gateway serves clients on two revisions:

- Revision 2025-11-25 and the older revisions. A client opens a session with `initialize`. This is the revision the gateway prefers.
- Revision 2026-07-28. A client sends its revision and its details with each request, and there is no `initialize`.

## How the gateway picks the revision for a client

The gateway reads the `Mcp-Protocol-Version` header of each request:

- If the header is `2026-07-28` or later, the gateway serves the request on 2026-07-28.
- In all other cases, the gateway serves the request on the older revisions.

A `server/discover` request is the exception. A client sends it to learn the revisions of a server. The gateway answers it with the older revisions only, on every revision. So a client that speaks both revisions and asks first then uses `initialize` on 2025-11-25. The go-sdk client does this by default.

A `server/discover` request needs a valid token, as every request does. It does not need the `X-Toolgate-Session` header. It does not make a toolgate session.

## Clients on 2026-07-28 need a session header

The gateway keeps the state of a session (the enabled servers, for example) under the `X-Toolgate-Session` header. Revision 2026-07-28 has no session of its own. So a 2026-07-28 client must send `X-Toolgate-Session`. The headers helper of the toolgate plugin sends it.

If a 2026-07-28 request has no `X-Toolgate-Session` header, the gateway answers with HTTP 400 and this JSON-RPC error, code `-32600`:

> This client sends no session. Use the toolgate plugin's headers helper, or a client on MCP 2025-11-25.

The gateway does not send the "unsupported protocol version" error (`-32022`) here. A client that speaks both revisions reads the plain 400 as "this server is older". It then uses `initialize` on 2025-11-25.

A client that speaks only 2026-07-28 and sends no session header cannot use the gateway. Give it the headers helper, or use a client on 2025-11-25.

## What works on 2026-07-28

A client on 2026-07-28 with the session header can do everything a 2025-11-25 client can do:

- List, enable and disable servers, and call tools. Every call goes through the same policy checks.
- Get tool list changes through `subscriptions/listen`.
- Complete approvals. The gateway asks the client to open the approval page.
- Get progress messages while a call waits.

Tool lists are marked private with no cache time (`cacheScope: "private"`, `ttlMs: 0`). So no cache shares the tools of one session with another session.
