# Tool result cache

Many MCP servers wrap an API with a small quota. A distributor API can allow 1000 calls a day, and models often repeat the same call. The gateway can keep the result of a tool call and serve it again. This page explains when to turn the cache on and what it stores.

You turn the cache on for one tool in a template. The field is `tools.<name>.cache`, and the key of `tools` is the upstream name of the tool. A tool without `cache` is never cached. See [Templates and rules](templates-and-rules.md).

## When caching is safe

Cache a tool only when both of these are true:

- The tool reads data and never changes anything.
- The result depends only on the arguments and on the identity that the cache scope separates.

If the result depends on state that the server keeps for the session, such as a browser page, set `scope: per-session`. Then each session has its own entries.

A tool must be marked read-only by its server, with `readOnlyHint: true` in its tool list. The template cannot mark a tool read-only. If the server does not mark the tool, the gateway forwards every call and stores nothing. The audit event of the call shows `cache: skipped` and `cache_skip: not-read-only`. If the server removes the hint later, the gateway stops reading the old entries at once.

## Results sit in the database

The gateway stores each cached result in plain text in the PostgreSQL table `tool_result_cache`, until the entry expires. This happens whatever `audit.payloads` says. Anyone who can read the database can read the results.

A read-only tool can still return secrets, such as the mail of a user. Turn the cache on only for tools whose results are allowed to sit in the database.

## A hit skips the server

A hit never reaches the server, so it skips the checks of the server. The gateway still checks its own policy before it looks in the cache:

- A user that the policy of toolgate refuses gets no hits.
- A user whom the identity provider (IdP) refuses for a `${exchange:...}` template keeps getting hits until the token expires. The refused refresh does not stop hits at once. The instance stops when its token expires, and the next call must start a new instance. That start fails. So the refusal takes effect within one token lifetime, and not after the cache lifetime.

A hit still needs a ready instance, because the read-only hint comes from the live instance. An `onCall` server that is not running starts for a call that then hits. A call also skips the cache while the instance runs an older version of the template than the current one. The audit event shows `cache_skip: outdated-instance`.

A hit counts as a call by the user. For a `${exchange:...}` template, a hit refreshes the exchanged token in the same way as a forwarded call. So an instance that gets only hits keeps a valid token and does not stop when the old token expires.

## The scope

The cache scope says who shares a result. It is `per-session`, `per-user` or `shared`. When you leave it out, the cache uses the scope of the template.

The cache scope can only narrow the scope of the template. In a `per-user` template, you can set `per-session` but not `shared`. The gateway marks a template invalid when the cache scope is broader. The reason is that a template of one scope has one instance for each user or session, and that instance can hold credentials of that user. A broader cache would show the results of one user's credentials to another user.

If the results of a server are the same for everyone, the server belongs in a `shared` template. Then the default cache scope is `shared`, and all users use the same entries.

Cache keys use only the verified identity. The session ID that a client claims never goes in a key.

## Example: a parts search with a daily quota

This template caches the search of a distributor API. The quota is 1000 calls a day, and parts data changes slowly.

```yaml
apiVersion: toolgate.soliddowant.dev/v1alpha1
kind: MCPServerTemplate
metadata:
  name: parts
spec:
  scope: shared
  tools:
    search_parts:
      cache:
        ttl: 6h
        ignoreArgs: [reason]
        normalize:
          query: [trim, lowercase, collapseSpace]
  # endpoint, resources and the rest of the template
```

- `ttl` is how long a result is served. It is from 1s to 24h. If you leave it out, the gateway uses the suggestion of the server (see below), and then 5 minutes.
- `ignoreArgs` lists top-level argument names that the gateway leaves out of the key. Here, a `reason` text that the model writes does not change the result, so two calls with different reasons share an entry.
- `normalize` maps a top-level argument name to the operations `trim`, `lowercase` and `collapseSpace`. The operations change only the key. The server always gets the argument as the caller wrote it. Here, `"  LM358  op amp"` and `"lm358 op amp"` share one entry.

A name cannot be in both `ignoreArgs` and `normalize`. Two calls share an entry only when all other arguments are equal. A number written as `1` and one written as `1.0` give different keys, so the difference causes a miss and never a wrong hit.

## The `_meta` conventions

The gateway uses one `_meta` key, `toolgate/cache`, in two places.

A hit carries the age of the entry:

```json
{"_meta": {"toolgate/cache": {"age": "12m0s"}}}
```

The `age` is a Go duration, cut to whole seconds. A result from the server, which is a miss, gets no `toolgate/cache` key. The gateway keeps the other `_meta` keys of the stored result.

Authors of servers can suggest a lifetime in the `_meta` of a tool in `tools/list`:

```json
{"toolgate/cache": {"ttl": "1h"}}
```

The value is a string that Go's `time.ParseDuration` reads, from 1s to 24h. The gateway ignores any other value. The suggestion never turns the cache on, because only a template does that. A `ttl` in the template overrides it.

## Limits and when entries end

Two gateway settings bound the cache. They are `toolResultCacheMaxEntryBytes` and `toolResultCacheMaxTemplateBytes`. The first is the largest result that the gateway stores. The second is the most bytes of unexpired entries for one template. See the [configuration reference](configuration.md) for the defaults and the allowed values, and the chart values for how to set them.

When a limit is reached, the gateway returns the result to the caller and does not store it. It does not remove older entries. Expired entries stop counting at once, and the leader deletes them within about 30 seconds.

An entry ends in one of these ways:

- Its lifetime ends.
- The template changes to a new valid generation. The old entries are never read again, and the leader deletes them.
- The template is deleted.
- The session ends, for `per-session` entries.

When a user sets a new secret, their `per-user` entries stay. They are the results of that user, and they expire when their lifetime ends.
