# Tool result cache design

This document decides how the gateway caches tool results. A template turns caching on for each tool. It adds to `docs/design/phase-1.md` to `docs/design/phase-4.md`, which still hold unless a section here says otherwise. Where they disagree, stop and ask (`HANDOFF: question`). Workers implement it and never change its rules. The operator and owner change it.

The problem: many MCP servers wrap APIs with small quotas. DigiKey and Mouser allow about 1000 calls a day each. Models repeat the same calls often. A server written for the homelab can cache results itself, but a third-party server cannot be changed. The gateway sees every call, so it can cache results for any server that a template opts in.

The target is the "Acceptance" section (§14). It is the owner's "Done when" list, unchanged.

## 1. Who does what

**Request handling**, on every replica:
- the cache lookup and the cache store of a call (§5);
- the `_meta` of a hit (§6);
- the audit fields and metrics of a hit or a miss (§10).

**Reconciler work**, on the leader only, on its 30 s full pass:
- deleting expired entries, the entries of old template generations and of deleted templates, and the per-session entries of ended sessions (§8).

**The end transaction of a session** (phase 1 §2, changed by phase 3 §10 and phase 4 §2) also deletes the session's per-session entries (§8). It runs where it runs today.

**Notifications.** None are new. A cache entry changes no tool list and no decision.

## 2. Template settings

A new field, `cache`, under `tools.<name>`, the existing per-tool overrides (phase 4 §6). The key of `tools` is the tool's upstream name, as for every other override.

```yaml
spec:
  scope: per-user
  tools:
    search_parts:
      cache:
        ttl: 1h                  # optional; 1s to 24h
        scope: per-session       # optional; can only narrow the template's scope
        ignoreArgs: [reason]     # optional; top-level argument names
        normalize:               # optional; top-level argument name to operations
          query: [trim, lowercase, collapseSpace]
```

The Go type in `api/v1alpha1/mcpservertemplate_types.go`:

```go
// ToolOverride gains:
//
// Cache turns on caching of the tool's results. The tool must also be
// marked readOnlyHint by its server.
// +optional
Cache *ToolCache `json:"cache,omitempty"`

// ToolCache says how the results of one tool are cached.
type ToolCache struct {
	// TTL is how long a result is served from the cache, 1s to 24h.
	// Without it, the server's suggestion is used, else 5m.
	// +optional
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s') && duration(self) <= duration('24h')",message="ttl must be between 1s and 24h"
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// Scope says who shares cached results: per-session, per-user or
	// shared. It must be the template's effective scope or narrower.
	// Without it, the template's effective scope is used.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self in ['per-session', 'per-user', 'shared']",message="scope must be one of per-session, per-user, shared"
	Scope string `json:"scope,omitempty"`

	// IgnoreArgs are top-level argument names left out of the cache key.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	// +listType=set
	IgnoreArgs []string `json:"ignoreArgs,omitempty"`

	// Normalize maps a top-level argument name to the operations applied
	// to its string value before it goes in the cache key: trim,
	// lowercase and collapseSpace.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(name, size(name) >= 1 && size(name) <= 128 && size(self[name]) >= 1 && size(self[name]) <= 3 && self[name].all(op, op in ['trim', 'lowercase', 'collapseSpace']))",message="each normalize entry names an argument of 1 to 128 characters and lists 1 to 3 of trim, lowercase, collapseSpace"
	Normalize map[string][]string `json:"normalize,omitempty"`
}
```

`cache: {}` is valid. It turns caching on with every default.

**Catalog checks.** The catalog checks these when it reads the template, in `validateOverrides` (`internal/catalog/validate_tools.go`). A problem makes the template invalid, and the `Valid` condition gives the reason. The CRD's CEL rules check the same bounds first. The catalog checks them again, because toolgate checks every resource twice (`SPEC.md` §8).
- `ttl` is 1s to 24h.
- `scope` is one of the three scopes, and it is the template's effective scope or narrower. The check uses `templatescope.Allows(cacheScope, effectiveScope)`, the function that the template's own `scope` uses. The reason reads: "`tools.search_parts.cache.scope` is `shared`, which is broader than the template's scope `per-user`. A cache scope can only narrow the template's scope."
- `ignoreArgs` has at most 64 names of 1 to 128 characters, with no repeats.
- `normalize` has at most 64 entries. Each name has 1 to 128 characters and lists 1 to 3 operations, each one of `trim`, `lowercase` and `collapseSpace`.
- A name in both `ignoreArgs` and `normalize` is a problem: "`tools.search_parts.cache.normalize.reason` names an argument that `ignoreArgs` leaves out."

The catalog does not compare argument names with the tool's input schema. The schema comes from a running server, and the template is read before that.

**Which generation's settings apply.** The cache settings come from the same generation as curation (phase 4 §6): the current template when it is valid, else its last valid generation (`Catalog.Running` for an enabled server, `Catalog.Offered` for a call that enables it). That generation also goes in the key (§4).

## 3. Which calls use the cache

A call uses the cache only when all of these are true:
1. The server is not the built-in `toolgate` server. Its tools change scope state, and it has no template in git.
2. The template's tool has `cache` set, in the generation of §2.
3. The live instance runs that generation: `instance.Generation` equals the template's `Generation`. An outdated instance runs the old server configuration, so its results must not go under the new generation's key.
4. The live instance's tool list marks the tool read-only (see below).
5. The arguments give a key (§4).

When rule 2 fails, the call goes on as today, with no cache step and no cache fields in the audit. When rule 3, 4 or 5 fails, the call is forwarded as today, nothing is stored, and the audit records `cache: skipped` with the reason (§10).

**The read-only hint.** The gateway trusts one source: the upstream tool list of the live instance, `instances.tools`. That is the list the server served at the instance's generation. The reconciler writes it when the instance becomes ready and on each tool list change (phase 1 and phase 4 §3). The call path already reads the row for each call (`readyInstance`). The tool is read-only when its entry in that list has `annotations.readOnlyHint` equal to `true`. A missing `annotations` object, or a missing or `false` hint, means not read-only.
- The template cannot mark a tool read-only. `declaredTools` has no annotations, and the catalog list (`tool_catalog`) is not used for this. The owner's rule is that the server marks the tool.
- If the hint changes, the next call reads the new list. A tool that loses the hint is neither looked up nor stored. Its entries stay until they expire or the leader deletes them (§8), and no call reads them while the hint is missing.

**The lifetime.** The first of these is used:
1. the template's `cache.ttl`;
2. the server's suggestion: in the same tool entry of `instances.tools`, `_meta["toolgate/cache"].ttl`, a string that Go's `time.ParseDuration` reads, from 1s to 24h. Any other value, type or range is ignored;
3. 5 minutes.

The server can suggest only `ttl`. The template overrides it. A suggestion never turns caching on, because only the template opts a tool in.

**The scope.** The cache scope is `cache.scope` when it is set, else the template's effective scope (`spec.scope` after `Catalog.Set`).

## 4. The key

The key has two parts. The first part is not configurable. The second part is the arguments only.

**Fixed part:**
- the template's `metadata.uid` and `metadata.generation` (§2);
- the tool's upstream name;
- the cache scope and the scope ID:

| Cache scope | Scope ID |
|---|---|
| `shared` | empty |
| `per-user` | the verified principal (`identity.Principal`) |
| `per-session` | the session's row ID (`sessions.id`), in decimal |

The session row ID belongs to the pair (principal, session ID). So another principal who sends the same session ID gets another row and another scope ID. The claimed session ID itself never goes in the key.

**Arguments part.** The steps, in `internal/resultcache`:
1. Take the call's raw arguments. No arguments means `{}`.
2. Decode them as one JSON object with `github.com/go-json-experiment/json`, into `map[string]jsontext.Value`. Its defaults refuse duplicate member names and invalid UTF-8. A value that is not an object, or that fails to decode, gives no key (rule 5 of §3).
3. Delete each member that `ignoreArgs` names.
4. For each member that `normalize` names and whose value is a JSON string, decode the string, apply the operations, and encode it again. A member of any other type is kept as it is. The operations always run in this order, whatever the order of the list:
   - `trim`: `strings.TrimSpace`;
   - `collapseSpace`: each run of characters for which `unicode.IsSpace` is true becomes one ASCII space;
   - `lowercase`: `strings.ToLower`.
5. Encode the map with the same library, then canonicalize it with `jsontext.Value.Canonicalize(jsontext.CanonicalizeRawInts(false), jsontext.CanonicalizeRawFloats(false))`. This sorts object members at every depth and removes white space. Numbers keep their text.

**The hash.** `crypto/sha256` over the `encoding/json` encoding of this array: `["toolgate/result-cache/1", uid, generation, tool, scope, scopeID, canonicalArguments]`. The array's strings are JSON-escaped, so no two different inputs give the same bytes. The 32-byte sum is the primary key of the entry.

Why this canonical form:
- `github.com/go-json-experiment/json` is the Go team's JSON v2 work, which Go 1.25 and later ship as `encoding/json/v2` behind `GOEXPERIMENT=jsonv2`. Its `Canonicalize` implements RFC 8785 (the JSON Canonicalization Scheme). The module is already in `go.mod` as an indirect dependency. It becomes a direct one. Nothing is hand-written.
- Raw numbers stay as they are written, because RFC 8785 reads every number as a 64-bit float. Two integers larger than 2^53 would then give the same key. So `1` and `1.0` give different keys. That is a miss, never a wrong hit.
- Duplicate member names are refused, because the server can read the first value while the key holds the last one. In a `shared` scope, one user can then store a result under another query's key.
- Normalized values change only the key. The server always gets the caller's own arguments.

## 5. The call path

The call path in `internal/gateway/forward.go` keeps its order. The cache adds two steps inside `forwardAndRecord`, the part that runs after the allow is committed. So every refusal, block, rule, workspace scope step, grant and approval runs before the lookup, as it does before a forward today. `useApproval` also ends in `forwardAndRecord`, so a call that waited for an approval uses the cache too.

1. Identity, the tool name, curation, an implicit enable, the ready instance, health and `instanceOffers`: unchanged (`proxyCall`).
2. Policy, approval, and the committed `tool.decision` allow: unchanged (`decideAndRun`, `callWithApproval`).
3. **Lookup.** If §3 allows caching, build the key and call `Store.Get`, with a 2 s timeout.
   - On a hit, go to step 6. Nothing is forwarded, and no upstream connection is made.
   - On a miss, go on.
   - An error or a timeout counts as a miss, with a warning log that names the server and the tool and never the arguments.
4. **Forward**: unchanged (`forward`).
5. **Store.** If §3 allows caching and the outcome of `forward` is `ok`, call `Store.Put` with the result's JSON (the `json.Marshal` that `forward` already makes for the 16 MiB check) and the lifetime, with a 5 s timeout. The outcomes `error` (the result has `isError`), `failed` and `too-large` are never stored. A failed store never fails the call: the result is returned, and the store outcome is counted (§10).
6. **`tool.result`**, as today, with the cache fields of §10. With `audit.payloads` on, the payload is the result as the client gets it, so a hit's payload carries its `_meta` (§6).
7. **`last_used_at`**: a hit counts as a call, as today, so `idleStop` counts from it.

A hit still needs a ready instance (step 1), because the read-only hint comes from the live instance's row. So an `onCall` server with no live instance starts on a call that then hits. See §15.

## 6. `_meta` on a hit

A hit's result is the stored result, decoded into a fresh `mcp.CallToolResult`. The gateway sets `_meta["toolgate/cache"]` to:

```json
{"age": "12m0s"}
```

- `age` is the entry's age, `now() - stored_at`, both read from PostgreSQL. So the clocks of the replicas do not matter. It is cut to whole seconds and written with Go's `time.Duration.String`.
- Every other `_meta` key of the stored result is kept. A `toolgate/cache` key that the server itself set is replaced.
- A result from the server (a miss) gets no `toolgate/cache` key from the gateway.

**Server suggestions** are in the tool's `_meta` in `tools/list`, under the same name: `{"toolgate/cache": {"ttl": "1h"}}`. Only `ttl` is read (§3).

The admin page (§11) documents both conventions, for admins and for authors of servers.

## 7. Storage

### The store interface

A new package, `internal/resultcache`, holds the key code (§4), the settings code (§3) and a small interface. The call path and the leader use only the interface. So a store with the Redis protocol can be added later without a change to the call path (§16).

```go
// Store keeps cached tool results for every replica.
type Store interface {
	// Get returns the unexpired entry for key, with its age.
	Get(ctx context.Context, key Key) (Hit, bool, error)
	// Put stores result for key for ttl. It returns why nothing was stored,
	// or PutStored.
	Put(ctx context.Context, key Key, result []byte, ttl time.Duration) (PutResult, error)
	// DeleteTemplate deletes the entries of a template UID of every
	// generation except keep. keep 0 deletes them all.
	DeleteTemplate(ctx context.Context, templateUID string, keep int64) error
	// Cleanup deletes expired entries, per-session entries of ended
	// sessions, and, through DeleteTemplate, entries of template UIDs and
	// generations that current does not map.
	Cleanup(ctx context.Context, current map[string]int64) error
}
```

`Key` holds the fixed part and the canonical arguments of §4, and has `Hash() [32]byte`. `Hit` holds the result bytes and the age. `PutResult` is one of `PutStored`, `PutEntryTooLarge`, `PutTemplateFull` and `PutSessionEnded`.

The one implementation is `resultcache.Postgres`, built on `sqlc` queries. It gets the two size limits (below) when it is built.

### Table and migration

One additive migration, `00015_tool_result_cache.sql` (phase 1 §7, "Database migrations"):

```sql
CREATE TABLE tool_result_cache (
    key          bytea       PRIMARY KEY CHECK (octet_length(key) = 32),
    template_uid text        NOT NULL,
    generation   bigint      NOT NULL,
    tool         text        NOT NULL,
    scope        text        NOT NULL CHECK (scope IN ('shared', 'per-user', 'per-session')),
    scope_id     text        NOT NULL,
    session_pk   bigint,
    result       bytea       NOT NULL,
    size_bytes   integer     NOT NULL,
    stored_at    timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    CHECK ((scope = 'per-session') = (session_pk IS NOT NULL))
);

CREATE INDEX tool_result_cache_expires_idx ON tool_result_cache (expires_at);
CREATE INDEX tool_result_cache_template_idx ON tool_result_cache (template_uid, generation) INCLUDE (size_bytes, expires_at);
CREATE INDEX tool_result_cache_session_idx ON tool_result_cache (session_pk) WHERE session_pk IS NOT NULL;
```

- `result` is the JSON of the `CallToolResult` as the server sent it, without the gateway's `_meta`. It is `bytea`, so the bytes stay exactly as stored. PostgreSQL compresses large values on its own (TOAST).
- `session_pk` has no foreign key. Session rows are never deleted today, and the end transaction deletes the entries (§8).
- A 0.4.11 replica in a rolling upgrade never reads or writes the table, and it ignores the new CRD field. It forwards every call, as today.

### Get

```sql
SELECT result, now() - stored_at AS age FROM tool_result_cache
WHERE key = @key AND template_uid = @template_uid AND generation = @generation
  AND tool = @tool AND scope_id = @scope_id AND expires_at > now();
```

The extra column checks cost nothing, and they make a wrong hit impossible even if the hash code had a bug.

### Put

First, a result larger than `toolResultCacheMaxEntryBytes` is not stored (`PutEntryTooLarge`). This check takes no lock. Then one transaction:
1. For a `per-session` entry: `SELECT 1 FROM sessions WHERE id = @session_pk AND ended_at IS NULL FOR SHARE`. With no row, nothing is stored (`PutSessionEnded`). The share lock waits for an end transaction that holds the row (`LockSession` takes `FOR UPDATE`). So an entry is either stored before the end transaction deletes the session's entries, or not stored at all (row 9 of §12).
2. `SELECT pg_advisory_xact_lock(hashtextextended('toolresultcache/' || @template_uid, 0))`. It serializes the stores of one template, so the size check in step 3 is exact.
3. `SELECT coalesce(sum(size_bytes), 0) FROM tool_result_cache WHERE template_uid = @template_uid AND expires_at > now() AND key <> @key`. If that sum plus the new size is larger than `toolResultCacheMaxTemplateBytes`, nothing is stored (`PutTemplateFull`).
4. `INSERT … ON CONFLICT (key) DO UPDATE SET result, size_bytes, stored_at = now(), expires_at = now() + @ttl`. The last store wins.

### Size limits

Two gateway settings, flat, in the generated reference (`internal/config`, `docs/admin/configuration.md`). The chart sets them from `config.toolResultCacheMaxEntryBytes` and `config.toolResultCacheMaxTemplateBytes`.

| Setting | Default | Allowed | Meaning |
|---|---|---|---|
| `toolResultCacheMaxEntryBytes` | 1048576 (1 MiB) | 1024 to 16777216 (16 MiB, the result limit) | The largest result that is stored. |
| `toolResultCacheMaxTemplateBytes` | 67108864 (64 MiB) | 1048576 (1 MiB) to 1073741824 (1 GiB), and at least the entry limit | The most bytes of unexpired entries of one template. |

The limits are gateway settings and not template fields, because the database belongs to the gateway's admin. A template author must not be able to fill it.

When a limit is hit, the result is still returned to the caller. Nothing older is evicted. Expired entries stop counting at once, and the leader deletes them within one pass (§8). See §15 for why there is no eviction.

## 8. Clean-up

**On the leader's full pass** (every 30 s, `runtime.RowCleanup` and the passes it wraps). A new wrapper, `runtime.ResultCacheCleanup`, calls `Store.Cleanup` with `current` built from `Catalog.LastValidByUID`: each template UID with its last valid generation. The pass already waits for the catalog to load (`AfterLoaded`), so a partial catalog never deletes the entries of a template that exists. `Cleanup` does these deletes, each in batches of 1000 rows per statement and at most 10 statements per kind per pass. The next pass deletes the rest.
- Expired entries: `expires_at <= now()`, through the expiry index.
- Entries of a stale template: for each `(template_uid, generation)` pair in the table (`SELECT DISTINCT`, through the template index) whose UID is not in `current`, `DeleteTemplate(uid, 0)`. For a pair whose generation differs from `current[uid]`, `DeleteTemplate(uid, current[uid])`.
- Per-session entries of ended sessions: `session_pk IN (SELECT id FROM sessions WHERE ended_at IS NOT NULL)`. The end transaction already deletes them. This also catches sessions that a 0.4.11 leader ended during the rollout.

A failed delete does not stop the pass, as with `store.DeleteStaleRows`.

**In the end transaction** (`endSessionInTx` in `internal/runtime/sessionend.go`): `DELETE FROM tool_result_cache WHERE session_pk = @id`. The same delete runs in `clearEndedState` (`internal/gateway/sessions.go`) when a session restarts. This is a function of the PostgreSQL store that takes the transaction, and not a method of `Store`, because it must be in the same transaction.

## 9. Concurrency

**Two replicas miss at once.** Both forward, and both store. The template's advisory lock runs the stores in turn, and the upsert keeps the last one. Both results come from the same read-only tool with the same key, so either result is correct.

There is no protection against a stampede (many calls for one key while it is missing):
- A double call needs the same key on two replicas within one upstream call time, a few seconds at most. Models repeat calls across turns, one after the other. Parallel calls with the same arguments are rare.
- One saved call needs a lock held across a network call: a PostgreSQL connection or an advisory lock per in-flight key, held for up to the upstream timeout. That costs more than a rare extra call of a 1000-a-day quota.

**Store and session end.** The share lock of Put step 1 orders them (§7).

**Store and template change.** A call that started before the change stores under the old generation. No call of the new generation reads it, and the next pass deletes it.

## 10. Audit and metrics

**Audit.** No new event types. `tool.result` gains details only for a tool with `cache` set (§3, rule 2):
- `cache`: `hit`, `miss` or `skipped`;
- `cache_age_ms`: the age of a hit;
- `cache_skip`: with `skipped`, one of `outdated-instance`, `not-read-only` and `arguments`.

A hit's `tool.result` has `outcome: ok`, the instance ID and the time the lookup took as `duration_ms`. Its `tool.decision` is the normal allow, written before the lookup. So a hit is audited like a call, with a cache-hit flag.

**Metrics.** Labels have fixed values only. The series budget test (`internal/runtime/metrics_test.go`) covers the new series, and `metricsbudget.AllowedLabels` gains the new values.
- `toolgate_tool_result_cache_lookups_total{server, result}`: `result` is `hit` or `miss`. Skipped calls are not counted.
- `toolgate_tool_result_cache_stores_total{result}`: `result` is `stored`, `entry_too_large`, `template_full`, `session_ended` or `failed`. It has no `server` label, to keep the series per template low.
- `toolgate_tool_calls_total{server, outcome}` gains the outcome `cached` for a hit. Before, a hit would have counted as `forwarded`, which would hide how many calls reached the server.
- `toolgate_tool_call_duration_seconds` does not observe hits. It measures the time to the upstream answer.

`AllowedLabels["result"]` gains `hit`, `miss`, `stored`, `entry_too_large`, `template_full`, `session_ended` and `failed`. `AllowedLabels["outcome"]` gains `cached`. The budget test adds, per template, two lookup series and one `cached` call series, and the five store series once. Its expected count changes with them.

## 11. Docs

- A new admin page, `docs/admin/tool-result-cache.md`, listed in `docs/README.md` and linked from `docs/admin/templates-and-rules.md`. It explains:
  - when caching is safe: the tool reads and never changes anything, and its result depends only on its arguments and on the identity that the cache scope separates;
  - why the scope can only narrow, and why a server whose results are the same for everyone belongs in a `shared` template;
  - an example: a parts search tool of a distributor API with a daily quota, with `ttl`, `ignoreArgs` and `normalize`;
  - that the server must mark the tool `readOnlyHint`, and that the template cannot;
  - the `_meta` conventions of §6, for admins and for authors of servers;
  - the two size limits, and that a template change, a session end and the lifetime end entries.
- `docs/admin/metrics.md`: the two new metrics and the `cached` outcome.
- `docs/admin/configuration.md`: regenerated with `make generate-docs`.
- `SPEC.md` §9, "Tools and curation": one sentence that names `tools.<name>.cache` and links this design.

## 12. Interaction table

| # | Event | In flight | Rule | Test |
|---|---|---|---|---|
| 1 | Template changes to a new valid generation | entries of the old generation | The key holds the generation, so no call reads the old entries. The leader deletes them on its next pass. | integration |
| 2 | Template changes | a call that looked up and is forwarding on the old generation | The store goes under the old generation. Nobody reads it, and the next pass deletes it. | integration |
| 3 | Template becomes invalid | entries of the last valid generation | Calls keep using the last valid generation (phase 4 §6), so its entries keep serving. Nothing is deleted. | unit |
| 4 | Template deleted, or deleted and created again | entries of its UID | The UID is not in `current`, so the next pass deletes them. A new template has a new UID. | integration |
| 5 | Instance outdated (runs an older generation than the template) | call | Forwarded, nothing looked up or stored, `cache: skipped`, `cache_skip: outdated-instance`. | integration |
| 6 | Instance replaced or repaired at the same generation | entries | Entries stay and serve. The key has no instance. | integration |
| 7 | The tool loses `readOnlyHint` in a tool list change | entries, and a call forwarding | The next call is not looked up or stored (`not-read-only`). The in-flight call can still store. Its entry is not read while the hint is missing. | integration |
| 8 | The tool gets `readOnlyHint` back | unexpired entries | They serve again until they expire. | unit |
| 9 | Session ends | a per-session store | The share lock orders them. Either the store commits first and the end transaction deletes the entry, or the store sees the ended session and stores nothing (`session_ended`). | integration, `-count=50 -race` |
| 10 | Session restarts in another workspace or attestation | per-session entries | The end transaction deleted them. `clearEndedState` deletes any that a 0.4.11 leader left. The restarted session finds none. | integration |
| 11 | Session resumes in the same workspace | per-session entries | The end transaction deleted them. The resumed session starts with an empty cache. | integration |
| 12 | Another principal sends the same session ID | per-session entries | Another session row, so another scope ID. No hit. | integration |
| 13 | Another user calls a per-user template's tool | per-user entries | Another scope ID. No hit. | integration |
| 14 | Rule, temporary block or `blocked: true` added (any replica) | cached entries | Policy runs before the lookup. The call is refused with `tool.decision` and never reads the cache. | integration |
| 15 | Tool gets `approval: required` | cached entries | Every call waits for an approval first, then can hit. | integration |
| 16 | User removed from `access`, or a grant expires | the user's entries | Policy refuses first. The entries expire on their own. | unit |
| 17 | Deny entry or workspace scope change | the session's entries | Policy refuses first. | unit |
| 18 | User sets a new secret | per-user entries | Not deleted. They are the user's own results and expire within the lifetime. The admin page says so. | unit |
| 19 | Entry expires | lookup | `expires_at > now()` fails, so it is a miss. The next pass deletes it. | integration |
| 20 | Result larger than `toolResultCacheMaxEntryBytes` | store | Not stored, `entry_too_large`. The result is returned. | integration |
| 21 | The template's unexpired entries are full | store | Not stored, `template_full`. Nothing is evicted. The result is returned. | integration |
| 22 | Two replicas miss the same key | each other | Both forward. The advisory lock runs the stores in turn. One row stays. | integration, `-count=50 -race` |
| 23 | Replica dies | call before the lookup | The client's request fails, as today. Nothing changes. | none, phase 1 behaviour |
| 24 | Replica dies | lookup | The request fails, as today. A read changes nothing. | none, phase 1 behaviour |
| 25 | Replica dies | forward, before the store | The result is not stored. The next call is a miss. | none, phase 1 behaviour |
| 26 | Replica dies | store transaction | PostgreSQL rolls it back and frees the advisory lock. | integration |
| 27 | Leader changes | cleanup pass | Each batch is its own statement. The new leader's pass deletes the rest. | unit |
| 28 | Store or lookup fails, or times out | call | Lookup: a miss, and the call is forwarded. Store: the result is returned, `failed` is counted. Both log a warning without the arguments. | unit |
| 29 | Arguments with duplicate names, invalid UTF-8, or not an object | call | No key. Forwarded, `cache_skip: arguments`. | unit |
| 30 | Template or server sets an invalid `ttl` | — | A template `ttl` outside 1s to 24h makes the template invalid. A server's is ignored, and the default applies. | unit |
| 31 | Cache scope broader than the template's | — | The template is invalid, with the reason of §2. | unit |
| 32 | Call of the built-in `toolgate` server | — | Never cached. | unit |
| 33 | `onCall` server with no live instance | call that would hit | The instance starts first (§5), then the call hits. | integration |
| 34 | 0.4.11 replica during the rollout | calls | It forwards every call and ignores the table. | none |

## 13. Tests

Names say the behaviour. No test name or comment carries an issue number or a row number.

**Unit**, `internal/resultcache`:
- `TestArgumentsKey`, table-driven: the same members in another order give the same key; nested objects too; white space changes nothing; `ignoreArgs` removes a member; each `normalize` operation and their fixed order; a `normalize` name with a non-string value keeps the value; integers above 2^53 that differ give different keys; `1` and `1.0` give different keys; duplicate names, invalid UTF-8, an array and a string give no key.
- `TestKeyHash`: each fixed part changes the hash; a per-user and a per-session key with the same text give different hashes.
- `TestLifetime`: the template's `ttl` wins; the server's suggestion applies without one; a bad suggestion falls back to 5 minutes.
- `TestCacheScope`: `cache.scope` narrows; without it, the template's effective scope applies.
- `TestEligibility`: the built-in server, an outdated instance, a missing or false `readOnlyHint` and missing annotations.
- `TestHitMeta`: `age` is set and cut to seconds; the stored `_meta` keys stay; a server's own `toolgate/cache` is replaced.

**Unit**, `internal/catalog` and `api/v1alpha1`:
- `TestCacheScopeBroaderIsInvalid`, with the exact reason; `TestCacheSettingsBounds` for `ttl`, `ignoreArgs` and `normalize`; `TestCacheNormalizeOfIgnoredArgumentIsInvalid`. The CRD tests check the CEL messages.

**Integration** (`//go:build integration`, PostgreSQL in Docker), `internal/gateway` and `internal/resultcache`. The test MCP server (`test/mcpserver`) gains a tool `count` with `readOnlyHint: true`, which returns how often it was called. A flag sets its `_meta["toolgate/cache"].ttl`. So a test sees whether a call reached the server.
- `TestRepeatedCallIsServedFromTheCache`: the second call returns the first result with `toolgate/cache.age`, the server count stays 1, and `tool.result` has `cache: hit`.
- `TestPolicyRunsBeforeTheCache`: a rule, a temporary block and `blocked: true`, each added after the first call, refuse the second call with `tool.decision`.
- `TestApprovalRunsBeforeTheCache`: the second call waits for its own approval, then hits.
- `TestErrorsAreNotCached`: an `isError` result and a failed call are not stored.
- `TestTemplateChangeInvalidatesEntries`, `TestOutdatedInstanceSkipsTheCache`, `TestInstanceReplaceKeepsEntries`.
- `TestEntriesStayInTheirScope`: another user on a per-user template, another session on a per-session cache, and the same session ID from another principal all miss.
- `TestSessionEndDeletesItsEntries`, and a restart in another workspace finds none.
- `TestToolWithoutReadOnlyHintIsNotCached`, and a tool that loses the hint is no longer served.
- `TestSizeLimits`: an entry over the entry limit and a full template are not stored, and the result is still returned.
- `TestCleanupDeletesStaleEntries`: expired entries, old generations, deleted templates and ended sessions, in batches.
- `TestConcurrentMissesKeepOneEntry` and `TestStoreRacingSessionEnd`. Both run with `-count=50 -race`. They start the two sides together on a barrier and wait on conditions with deadlines, never on sleeps or wall-clock speed.

**Metrics:** the series budget test with a cached template, and a test that a hit counts `hit` and `cached` and a miss counts `miss` and `forwarded`.

## 14. Acceptance

The owner's "Done when" list:
- A template can turn caching on per tool, with a lifetime, a narrower scope, `ignoreArgs` and `normalize`. A broader cache scope is invalid, with a clear `Valid` reason. Tests: `TestCacheScopeBroaderIsInvalid`, `TestCacheSettingsBounds`, `TestArgumentsKey`.
- A repeated call within the lifetime returns the cached result without reaching the server, after rules and approvals ran, and the audit log records the hit. Tests: `TestRepeatedCallIsServedFromTheCache`, `TestPolicyRunsBeforeTheCache`, `TestApprovalRunsBeforeTheCache`.
- Errors are not cached. A template change invalidates entries. Per-user and per-session entries are never served to another user or session. Tests: `TestErrorsAreNotCached`, `TestTemplateChangeInvalidatesEntries`, `TestEntriesStayInTheirScope`, `TestSessionEndDeletesItsEntries`.
- Docs: the admin guide (when caching is safe, with an example), and the `_meta` conventions. The page `docs/admin/tool-result-cache.md` (§11).

## 15. Choices made

- **PostgreSQL, not Redis.** The issue names PostgreSQL or a Redis-protocol server. `SPEC.md` §3 allows only PostgreSQL and S3 as backends. The owner has no strong view, and the operator chose PostgreSQL, the smallest choice that meets the acceptance list.
  - Load: at homelab rates (tens of calls a minute, a few thousand a day), one primary-key read and one short write per cached call are a small load for PostgreSQL.
  - Latency: a lookup takes about a millisecond in the cluster, and Redis takes less. Both are small next to an upstream call of 100 ms to several seconds.
  - Eviction: Redis expires keys by itself and can evict under a memory limit, so it needs no clean-up pass. PostgreSQL needs the expiry index and the leader's pass. A per-template limit needs its own counting in either store, because a Redis memory limit is global.
  - Operations: Redis is one more service to run, to secure (a password or ACL, and TLS), to watch, and to give a network policy from the gateway pods. PostgreSQL is already there. Its transactions also let a session end delete its entries atomically.
  - The `Store` interface (§7) keeps the door open. A Redis-protocol store with standard commands only can be added later without a change to the call path.
- **The generation in the key is the template's, not the instance's.** It is the generation whose cache settings apply. So any template change, the cache settings included, starts a fresh cache. An outdated instance skips the cache, so its results never go under the new generation's key. The cost: a long-running per-user instance does not cache after a template change until it is restarted (the Servers page) or stops at `idleStop`.
- **A hit still needs a ready instance.** The trusted read-only hint is on the live instance's row, and the lookup goes in the existing post-decision step. Looking up before the instance steps would need a second hint source, such as the catalog list, which remote templates do not have. The quota saving is the same, because a hit never calls the tool. A hit counts as a call for `idleStop`, so a server that only serves hits stays running.
- **Per-session scope uses the session row ID**, and a session end deletes its entries, also on a resume. A resumed session starts with an empty cache. That is simpler than a rule about when a restarted row is "the same session".
- **No eviction when a template is full.** The new result is not stored. Eviction would let one user of a `shared` template push out the entries of other users. Expired entries stop counting at once. The admin sees `template_full` in the metric and raises the limit.
- **Size limits are gateway settings**, not template fields (§7).
- **Only `ttl` can come from the server.** A server-suggested `ignoreArgs` or `normalize` would let a server's claim merge different calls into one key. Only the admin decides that.
- **Default lifetime 5 minutes, maximum 24 hours.** The maximum bounds how long a result can outlive a change at the source, such as prices.
- **The `_meta` age format** is Go's duration text (`12m0s`), not the issue's example `12m`. It is one standard function, and models read both.
- **`_meta` and the model.** The issue puts the age in `_meta` "so the model can judge freshness". The design does that. This design does not check whether Claude Code shows a result's `_meta` to the model. If it does not, a text note in the result is a later change, because it changes what the server returned.

## 16. Not in scope

- A store with the Redis protocol. The `Store` interface allows one later. The session-end delete would then need its own design.
- An expression language for keys, such as CEL.
- Arguments below the top level in `ignoreArgs` and `normalize`.
- Eviction of entries to make room.
- Protection against a stampede (§9).
- A gateway-wide switch to turn caching off. An admin removes `cache` from the template.
- A gauge of the bytes in the cache, and pages or API routes to read or clear it.
- Server-suggested settings other than `ttl`.
- Caching the built-in `toolgate` server's tools, or `tools/list`.
- Deleting a user's per-user entries when the user's secrets change.
