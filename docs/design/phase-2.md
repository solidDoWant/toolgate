# Phase 2 design

This document decides the behaviour `SPEC.md` leaves open for phase 2. It adds to `docs/design/phase-1.md`, which still holds unless a section here says otherwise. Where they disagree, stop and ask (`HANDOFF: question`). Workers implement it and never change its rules. The operator and owner change it.

## 1. Who does what

Phase 1's split stays (phase 1 §1): request handling runs on every replica and never touches Kubernetes objects; the runtime reconciler runs only on the Lease holder.

**New request-handling work**, on every replica:
- the web UI (§5): sign-in, pages, and the writes behind them (grants, revocations, approval decisions, temporary blocks, restart requests, secrets);
- loading grants and temporary blocks for each decision, and sending `list_changed` when they change or expire (§3);
- waiting inside a tool call for an approval (§4).

**New reconciler work**, on the leader only:
- replacing an instance when a restart is requested or a resolved value is about to expire (§6);
- calling resolvers while rendering (§6);
- on its 30 s pass: expiring pending approvals whose `expires_at` is more than 1 minute past, and deleting old rows (§2).

**Notifications.** Three new kinds on `toolgate_events`, sent inside the writing transaction like phase 1's: `grant:<grants.id>`, `block:<temporary_blocks.id>` and `approval:<approvals.id>`. A restart request sends the existing `instance:<id>`. Phase 1's rules for missed notifications hold: a replica whose `LISTEN` connection reconnects treats everything as changed, and every wait re-reads its rows at a 10 s tick.

Why: grants, blocks and approvals are rows written on one replica that change decisions on all of them. That is exactly phase 1's model (rows plus `LISTEN/NOTIFY`), so phase 2 adds no new mechanism.

**Phase 1 code that phase 2 replaces** (§7): the catalog's fixed `allowedKinds` map in `internal/catalog/validate.go` becomes the namespaced-only check, and the admission policy's `isAllowedKind` rule is removed.

## 2. State

One additive migration (phase 1 §7, "Database migrations"). New IDs are UUIDv7 written as 32 lowercase hex characters, like `instances.id`.

### Grants

`grants(id, principal, template_ns, template_name, tool, session_id, created_at, expires_at, revoked_at, revoked_by)`.

- `tool` is empty for a whole-server grant. Otherwise it is a tool's own name, and it must be a key of the template's `tools` whose `environments` is `opt-in`. A tool without an override inherits the server's environment, so a server grant already covers it.
- `session_id` is null, or the `X-Toolgate-Session` value the grant is narrowed to. It matches by value, so a session that ends and restarts with the same ID keeps the grant. A claimed field here only narrows (principle 2).
- `CHECK (expires_at > created_at AND expires_at <= created_at + interval '7 days')`.
- **Live:** `revoked_at IS NULL AND expires_at > now()`, by PostgreSQL's clock. Index on `(principal) WHERE revoked_at IS NULL`.
- A user creates grants only for themselves and only for a server they have catalog access to (§3, step 1).
  - A **tool grant** is allowed only when the server's own environment passes without a grant (`any`) and the tool's is `opt-in`. On an `opt-in` server, enable needs a whole-server grant anyway (§3, step 2), so a tool grant there could never be used.
  - A **whole-server grant** is allowed when the server's environment or any of its tools' is `opt-in`. It covers those tools.
- At most 100 live grants per principal. The insert takes a transaction advisory lock on the principal before it counts, so two concurrent inserts can't both pass the count.
- Revoking sets `revoked_at` and `revoked_by` once (`WHERE revoked_at IS NULL`). A row is never changed otherwise.
- Events: `grant.create`, `grant.revoke`. Expiry isn't an event; the row records it.

### Temporary blocks

`temporary_blocks(id, match, reason, created_by, created_at, expires_at)`.

- `match` is JSON with exactly the fields and meaning of `ToolgateRule.spec.match` (`SPEC.md` §10). It is parsed into the same Go type and compiled by the same code (`github.com/gobwas/glob`, phase 1 §7). Each list has at most 64 items of at most 256 characters, and every glob must compile. `reason` is required, at most 1024 characters.
- `CHECK (expires_at > created_at AND expires_at <= created_at + interval '30 days')`.
- At least one `match` field must be set. A block with every field empty would block every tool for every user, so it's refused.
- **Live:** `expires_at > now()`. At most 1000 live blocks. The insert takes one global transaction advisory lock before it counts.
- Only admins create them (§5). Rows are never changed after insert, so a compiled block is cached by ID and can't go stale.
- Event: `block.create`.

### Approvals

`approvals(id, principal, session_pk, workspace, template_ns, template_name, tool, arguments_sha256, status, created_at, expires_at, decided_at, decided_by, used_at)`.

- `arguments_sha256` is the SHA-256 (`crypto/sha256`) of the call's arguments in a fixed form: decoded with `encoding/json` using `UseNumber`, then encoded again with `encoding/json`, which sorts object keys and keeps numbers as written. It ties an approval to one exact call without storing the arguments.
- The gateway forwards exactly the canonical bytes that were hashed, never the raw arguments the client sent, and stores those same bytes as the `approval.request` and `tool.decision` payload (§8). So the person approves exactly what the upstream server receives, even when the client sent duplicate keys or invalid UTF-8.
- `session_pk` refers to `sessions.id`. `expires_at` starts as `created_at + approvalTimeout`.

| From | To | By | When |
|---|---|---|---|
| (none) | `pending` | request handler | a call needs approval and no usable approval exists (§4) |
| `pending` | `approved` | UI handler | the call's principal approves, with a fresh step-up, before `expires_at`. Sets `decided_at`, `decided_by`, and `expires_at = now() + approvalTimeout` |
| `pending` | `denied` | UI handler | the call's principal denies. No step-up: denying only narrows |
| `pending` | `expired` | the waiting call, or the reconciler's pass | the waiting call: `expires_at` has passed. The pass: `expires_at` is more than 1 minute past, so a live waiter, which checks every 10 s, normally wins and writes the timeout itself |

- Every write is `UPDATE … WHERE id = $1 AND status = 'pending'`, plus `AND expires_at > now()` for approve and deny. If it matches no row, the writer re-reads and reports the real status.
- An approved approval is **usable** while `used_at IS NULL AND expires_at > now()`. Using it sets `used_at` with a compare-and-set, so one approval lets exactly one call through.
- At most 100 pending approvals per principal. The insert takes a transaction advisory lock on the principal before it counts. Another call needing approval fails with "You have 100 approvals waiting. Approve or deny some first."
- Events: `approval.request` (with the arguments as payload when payloads are on, §8), `approval.approve`, `approval.deny`, `approval.expire`. The writer whose compare-and-set wins writes the event, so each is written once.

### Instances

Three new columns on `instances`:
- `restart_requested_at` and `restart_requested_by`, set by a restart from the UI (§6);
- `resolved_expires_at`, the earliest `expiresAt` of the resolver values its render used, or null.

### Web UI sessions

`ui_sessions(id_hash, principal, email, name, groups, csrf_token, auth_time, created_at, last_seen_at, expires_at)`.

- The cookie value is 32 bytes from `crypto/rand`, base64url-encoded. `id_hash` is its SHA-256, so a leaked row isn't a working cookie.
- `email`, `name` and `groups` come from the ID token at sign-in (§5).
- `csrf_token` is 32 bytes from `crypto/rand`.
- `auth_time` is the ID token's `auth_time` from the latest sign-in, or null when the IdP left it out.
- `expires_at = created_at + 8 h`. A session is valid while `now() < expires_at` and `last_seen_at > now() - 1 h`. `last_seen_at` moves forward at most once a minute.

`ui_logins(state_hash, nonce, pkce_verifier, return_to, step_up, ui_session_hash, created_at)` holds a sign-in in progress for 10 minutes. It is in PostgreSQL because the IdP's redirect back can land on any replica. It is taken with `DELETE … RETURNING`, so each state works once.

### Row cleanup

On its 30 s pass, the leader deletes `ui_sessions` rows that are no longer valid, `ui_logins` rows older than 10 minutes, and grants, temporary blocks and approvals 7 days after they stopped being live (expired, revoked, decided or used). The audit log keeps their history.

## 3. Policy

**Order of evaluation** (`SPEC.md` §10), the same for `list_servers`, `tools/list`, enable and call. The first step that refuses decides, and its refusal names it.

1. **Catalog access**, as in phase 1. A grant never adds access.
2. **Environment.** The tool's `environments` when the decision is for a tool and its override sets one, else the server's.
   - `any` passes. `attested` needs `attested`; grants don't count.
   - `opt-in` passes for `attested`. For `user`, it passes when a live grant has this principal and this server, an empty `tool` or this tool, and no `session_id` or this request's session. Decisions for the whole server (`list_servers`, enable) count only grants with an empty `tool`.
3. **Blocks**, in this order: the tool's `blocked: true`; the valid `ToolgateRule` objects (phase 1); then live temporary blocks, oldest first. A temporary block matches exactly as a rule with the same `match` would. Its refusal reads "Blocked by an admin until `<expires_at>`: `<reason>` (block `<id>`).", with `<expires_at>` in RFC 3339 in UTC, for example `2026-11-05T14:00:00Z`.
4. **Approval.** A tool with `approval: required` is allowed subject to approval: `tools/list` includes it, and a call follows §4. This applies to every attestation.

`Decision` gains `Grant` (the grant that passed step 2), `Block` (the temporary block that refused) and `NeedsApproval`. `tool.decision` records the grant, block or approval in its details.

**Per-tool environments** (AC-2-10) need no new path. `tools/list` already decides each tool on its own (phase 1 §3, "List"), so a tool marked `environments: attested` is left out for a `user` identity and present for an attested pod. The AC-2-10 test proves both.

**Loading grants and blocks.** `policy.Engine.Decide` stays a pure function. Each request that decides (one `tools/list`, call, enable or `list_servers`) first loads its **policy state** in one round trip, a `pgx.Batch` of two queries: the principal's live grants, and all live temporary blocks. Both filter with PostgreSQL's `now()`. Every `Decide` of that request uses that state.
- Why no cache: a revoke on replica B must refuse the next call on replica A at once, not when a notification arrives. Both queries use an index and return little (at most 100 grants and 1000 blocks), and a call already makes several round trips (phase 1 §3, "Call").
- `attested` identities skip the grant query, because grants never change their decisions.

**Fan-out of `list_changed`.** Decisions read fresh state, so notifications only tell clients to list again:
- On `grant:<id>`, a replica reads the row and sends `list_changed` to the streams of its principal.
- On `block:<id>`, it sends `list_changed` to all its streams.
- **Expiry timer.** Each replica keeps one timer for the earliest future `expires_at` of live grants and temporary blocks, like phase 1 row 31. The query returns `expires_at - now()` computed by PostgreSQL, so replica clock skew doesn't matter. When the timer fires, the replica sends `list_changed` to all its streams and sets the timer again. It also resets it after every `grant:` or `block:` notification and after a `LISTEN` reconnect.
- The 5 s bound (AC-2-07, AC-2-14) is the notification or timer delay plus this fan-out. A test measures it.

A grant's expiry stops nothing. The server stays enabled, its tools are left out of `tools/list`, and calls are refused at step 2. Phase 1's idle stops apply as usual.

## 4. Approvals and grants inside a session

**Elicitation through the go-sdk.** The tool handlers use the go-sdk's multi round-trip style. A handler that needs a person returns a `CallToolResult` whose `InputRequests` holds one URL-mode `ElicitParams`, with a `RequestState`.
- On revision 2026-07-28, the client opens the URL and calls the tool again with `InputResponses` and the same `RequestState`.
- On revision 2025-11-25, the go-sdk's server middleware sends `elicitation/create` itself and runs the handler again with the response.

So one code path serves both revisions, and toolgate never builds elicitation messages itself.

**Elicitation support** is read from the capabilities the client declared (`ElicitationCapabilities.URL` set), as the pinned go-sdk exposes them for the negotiated revision. The worker proves both revisions with a test. A client that declares support but answers `decline` or `cancel` is treated as one without it.

**Links.** An approval links to `publicURL` + `/ui/approvals/<id>`, and a grant to `publicURL` + `/ui/grants/new?server=<server>&tool=<tool>&session=<session>`. The elicitation ID is the approval ID. Without the UI (`oidcUIClientID` empty), a tool needing approval fails with "Tool `<server>__<tool>` needs approval, but this gateway has no web UI. Ask an admin." and a missing grant returns phase 1's environment message.

**A call of a tool that needs approval** (this replaces phase 1 §3 call steps 3 and 4 when the decision has `NeedsApproval`):
1. Compute `arguments_sha256`. Look for an approval with this principal, `session_pk`, server, tool and hash, in this order: the one named by `RequestState` (the client sends it, so it is only a lookup key and every field must match); a usable approved one; a pending one. If none is found, insert a `pending` row, write `approval.request`, notify, and commit.
   - The approval named by `RequestState` is taken only if it is pending or usable. A used or denied one is skipped, and the lookup goes on.
   - If the approval named by `RequestState` has status `expired`, write `tool.decision` (refuse, `approval-timeout`) without `approval.expire`, and return the AC-2-13 message (step 5).
   - If the request carries `InputResponses` and this step inserted a new approval, do as step 3: the user never saw this approval, so the call doesn't wait on it.
2. If the approval is usable, go to step 6.
3. If the client can't show an elicitation, write `tool.decision` (refuse, reason `approval-pending`, with the approval ID) and return "Tool `github__delete_repository` needs approval. Approve request `<id>` at `<url>`, then call the tool again with the same arguments." The approval stays pending, so the user can approve it on the page and retry (AC-2-12).
4. If the request carries no `InputResponses`, return the URL elicitation with `RequestState` set to the approval ID. If the response is `decline` or `cancel`, do as step 3. A request that carries `InputResponses` never gets `InputRequests` back, because on 2025-11-25 the go-sdk re-invokes the handler only once.
5. On `accept`, **wait**. Subscribe to notifications before reading. On every `approval:<id>`, missed event and 10 s tick, re-read the row, send a progress notification, and decide steps 1–3 of §3 again with fresh state; if they now refuse, stop with that refusal.
   - `approved`: go to step 6.
   - `denied`: write `tool.decision` (refuse, `approval-denied`) and return "Approval `<id>` was denied." (AC-2-11)
   - `expires_at` has passed: compare-and-set `pending` to `expired`. If it wins, write `approval.expire` and `tool.decision` (refuse, `approval-timeout`), and return "Nobody approved `<server>__<tool>` within `<approvalTimeout>` (approval `<id>`)." (AC-2-13), with the setting rendered in words, for example "10 minutes". If it loses, re-read and go on.
   - `expired` (the leader's pass won the race): write `tool.decision` (refuse, `approval-timeout`) without `approval.expire`, because the pass wrote it, and return the AC-2-13 message.
   - The client cancels or disconnects: stop and write nothing. The approval stays pending until someone decides or it expires.
6. **Use.** In one transaction:
   - re-run phase 1 call steps 1–2: find the session's `session_servers` row for the server, then re-select the live instance for (template, scope key), which must be `ready`;
   - compare-and-set `used_at`;
   - load fresh policy state and decide again;
   - write `tool.decision` (allow, with the approval ID), and commit.

   Then forward the canonical bytes (§2, Approvals) to the instance selected here, as in phase 1. If any check refuses, roll back, so `used_at` stays null and a later retry can still use the approval. Then write `tool.decision` (refuse) with that reason (`not-enabled`, `not-ready` or the policy step) and return the refusal. If the compare-and-set fails because another call used the approval, start again at step 1, once.

**The click lands on another replica.** The call waits on replica A, on its POST stream (2025-11-25) or on the retry's POST (2026-07-28). The approval on replica B commits `pending` to `approved` with `pg_notify('toolgate_events', 'approval:<id>')`. Replica A gets the notification, or its 10 s tick, re-reads the row and goes on. No memory is shared. On 2026-07-28 the retry may reach another replica than the first request, which changes nothing, because the approval is a row.

**Who approves:** only the call's principal. Anyone else gets 404 from the approval's page. A service account can't sign in to the UI, so its calls of approval tools always fail at step 3 (decision 1).

**A missing grant.** When the only refusal is step 2 for a `user` identity on an `opt-in` environment, so that a grant would allow it, enable and call fail at once with "Server `<server>` needs a grant on a signed-in user's machine. Create one at `<url>`, then try again." (or "Tool `<server>__<tool>` needs …"), and audit the refusal as in phase 1. There's no grant elicitation (decision 5). The link fills in the server, tool and session.

**Waits are bounded:** at most 32 waiting calls per principal on one replica. More fail at once with the step 3 message.

## 5. Web UI

Package `internal/ui`, served under `/ui/` on the gateway's main listener. `GET /` redirects to `/ui/servers`. The UI is on when `oidcUIClientID` is set, and `publicURL` is then required.

### Sign-in

Authorization code with PKCE, using `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2`:
1. `GET /ui/login?return_to=…&step_up=1`. `return_to` must be a path starting with `/ui/`, else it becomes `/ui/servers`, so there's no open redirect. Generate a state and a nonce (32 bytes each from `crypto/rand`) and a PKCE verifier (`oauth2.GenerateVerifier`), and insert the `ui_logins` row. Set the cookie `__Host-toolgate-login-<first 8 hex of the state hash>` to the state (`Secure`, `HttpOnly`, `Path=/`, `SameSite=Lax`, `Max-Age=600`). The name is per sign-in, so two sign-ins at once in one browser don't overwrite each other. Redirect to the IdP with `oauth2.S256ChallengeOption`, the nonce, the scopes `oidcUIScopes`, and for a step-up `prompt=login` and `max_age=0`.
2. `GET /ui/auth/callback`. Read the cookie whose name matches the callback's state hash. The `state` parameter must equal that cookie, so a callback started in another browser is refused (login CSRF). Take the `ui_logins` row with `DELETE … RETURNING`. Exchange the code with `oauth2.VerifierOption`. Verify the ID token with go-oidc's `IDTokenVerifier` (issuer `oidcIssuerURL`, audience `oidcUIClientID`) and check its nonce.
3. Read `sub`, the email (only when the email-verified claim is `true`), the name and the groups, with phase 1's claim settings (`oidcEmailClaim` and the others). For a step-up, also run the checks under "Step-up".
4. A normal sign-in inserts a `ui_sessions` row, deletes the browser's old row if it had one, and sets `__Host-toolgate` (`Secure`, `HttpOnly`, `Path=/`, `SameSite=Lax`, `Max-Age` 8 h). A step-up updates the existing row's `auth_time` and identity snapshot instead. If `sub` differs from the row's principal, it refuses with "You signed in as a different user." and signs out. A step-up with no `ui_session_hash` runs the step-up checks and then creates a session, as a normal sign-in does.
   - **Failed sign-ins** (a bad state or cookie, a token or ID token failure, a failed step-up check, or "different user") write an `auth.refuse` event with the reason, using phase 1's rate rule (phase 1 §7: at most 10 per minute per remote address, the rest counted in a metric).
5. Redirect to `return_to`.

**The UI's client** is separate from the CLI's (`oidcCLIClientID`), because it uses another grant and a redirect URI (`publicURL` + `/ui/auth/callback`). With `oidcUIClientSecretFile` set, it is a confidential client: the secret is read from that file into `oauth2.Config.ClientSecret`, and the auth style is detected automatically. Without it, it is a public client. PKCE is used in both cases. `oidcUIScopes` defaults to `openid email profile`; the UI keeps no tokens, so it needs no `offline_access`.

**Both clients must issue the same public `sub`** for a user. The gateway's principal is the CLI access token's `sub`, and the UI's is the UI ID token's `sub`. Grants, approvals and UI secrets match on that principal. An IdP with pairwise subject identifiers (OIDC Core §8), or one whose subject mode is set per client (such as Authentik's "Subject mode"), gives the two clients different subjects, so grants never match, users get 404 on their own approvals, and UI secrets are invisible to sessions. The admin docs say so, and the AC-2-07 and AC-2-11 docs point to it.

The UI reads identity from the **ID token**, because its access token is issued for the UI client, not the `toolgate` audience. The admin docs say the IdP must put the email and groups claims in that ID token.

### Step-up

Creating a grant and approving a call need `ui_sessions.auth_time` less than 5 minutes old (AC-2-09).
- The pages with those forms (`/ui/grants/new`, `/ui/approvals/<id>`) check on GET. If the sign-in is older than 4 minutes, they show a "Sign in again to continue" button to `/ui/login?step_up=1&return_to=<this page>` instead of the form. The extra minute leaves time to submit.
- The POST checks again, against 5 minutes. If too old, it changes nothing and redirects to a step-up that returns to the form.
- The step-up callback requires `auth_time` in the ID token; OIDC Core requires the IdP to send it when the request has `max_age`. It must be at most 5 minutes old, at most 60 s in the future, and no earlier than 60 s before the `ui_logins` row was created. The same 60 s of clock skew is allowed both ways. So an IdP that ignored `prompt=login` and returned an old sign-in doesn't pass.
- **Fail closed:** without `auth_time` the step-up fails with "Your identity provider didn't say when you signed in, so toolgate can't confirm a fresh sign-in. Ask an admin to make it send `auth_time`." The rest of the UI keeps working, but no grant or approval can be made.
- A user without a UI session who opens an approval link goes straight to a step-up sign-in, so they sign in once.

### CSRF

AC-2-03: Go's `net/http.CrossOriginProtection` on every `/ui/` handler, **plus** a synchronizer token.
- `CrossOriginProtection` (Go 1.25 and later; the module uses 1.26) refuses cross-origin unsafe requests by `Sec-Fetch-Site` and `Origin`. It is in the standard library, maintained by the Go security team, and adds no dependency.
- The token is the session's `csrf_token`. Every form has it in the hidden field `csrf_token`, and htmx sends it as the header `X-CSRF-Token` (`hx-headers` on `<body>`). Every POST compares it with `crypto/subtle.ConstantTimeCompare` before doing anything. A mismatch gets 403 and changes nothing.
- Why not a library: `filippo.io/csrf` checks headers only, without tokens, and `SPEC.md` §14 asks for tokens. `gorilla/csrf` signs a token into a cookie, which a server-side session doesn't need, and its Origin checks have had flaws. A random token stored in the session row needs no signing key and no new code beyond a constant-time compare.
- `/api/v1/` accepts only bearer tokens and never reads the cookie, so it needs no CSRF protection.

### Authorization

Routes register through one helper that states the access level: `public`, `user` or `admin`, with step-up on the POSTs named below.
- `public` is only `/ui/login`, `/ui/auth/callback` and `/ui/static/`. They have no per-address limit, because behind an ingress every user shares one remote address. Instead, `/ui/login`, which writes a row without authentication, is refused while more than 10,000 `ui_logins` rows exist, and the 10-minute cleanup (§2) keeps the table small. The callback only deletes a row, so it needs no limit. A test asserts that these, plus phase 1's well-known, health and metrics routes, are the only unauthenticated routes.
- Every other handler first loads the UI session. Without a valid one, a GET redirects to sign-in and a POST gets 401.
- **Admins** are members of the group `adminGroup`, checked against the session's groups on every request. If `adminGroup` is empty, nobody is an admin. `admin` routes give 403 to others (AC-2-01), and the navigation leaves out the Blocks link.
- Groups are a snapshot from sign-in, so removing someone from the admin group takes effect at the latest when their 8 h session ends. The admin docs say so.
- An object of another user (an approval, grant, instance, session or audit event) gives 404 to a non-admin, so its ID leaks nothing. Approvals and grants are owner-only, for admins too.

### Pages

`html/template` files in `internal/ui/templates/`, embedded with `embed`.

| Route | Access | What it does |
|---|---|---|
| `GET /ui/servers` | user | The catalog as `list_servers` shows it to this user with `user` attestation and no workspace or session claim; the user's live instances (admins: also shared ones), with status and whether each is outdated |
| `POST /ui/instances/{id}/restart` | owner; admin for shared | §6 |
| `GET /ui/secrets`, `POST /ui/secrets`, `POST /ui/secrets/{name}/delete` | user | Names and dates; set or replace; delete. Through the same secret broker as `/api/v1/secrets` |
| `GET /ui/grants`, `GET /ui/grants/new`, `POST /ui/grants`, `POST /ui/grants/{id}/revoke` | user; step-up to create | Live and recent grants; create; revoke (no step-up, because it only narrows) |
| `GET /ui/approvals`, `GET /ui/approvals/{id}`, `POST /ui/approvals/{id}/approve`, `POST /ui/approvals/{id}/deny` | the call's principal; step-up to approve | Pending approvals with server, tool, session, workspace, and the arguments when payloads are on |
| `GET /ui/sessions`, `GET /ui/sessions/{id}` | user; admin sees all | Open sessions, their enabled servers, and the session's last 20 `tool.decision` events |
| `GET /ui/audit`, `GET /ui/audit/events/{key}`, `GET /ui/audit/events/{key}/payload` | user; admin sees all | The audit log (below) and payloads (§8) |
| `GET /ui/blocks`, `POST /ui/blocks` | admin | Live temporary blocks and the form to add one; the `ToolgateRule` objects from the informer cache, read-only, with their validity (AC-2-15) |
| `POST /ui/logout` | user | Deletes the session row and the cookie |

- **Secrets** (AC-2-02): the form offers both a `textarea` and a file input, neither ever filled in, and each POST answers with a redirect, so no page or response ever holds a value.
  - The form posts `multipart/form-data`. The 4 MiB limit applies to the decoded value. The body is limited to 4 MiB plus 64 KiB, other forms to 64 KiB (`http.MaxBytesReader`).
  - A `textarea` value has CRLF converted to LF, so a PEM key typed or pasted in the UI matches one set with the CLI. A file's bytes are kept exactly.
- **Audit** (AC-2-05): exact matches on principal, workspace, session, server and tool, and a time range (default the last 24 h, at most 31 days, so a query reads one or two monthly partitions). A non-admin always gets their own principal; a principal parameter is ignored. 100 rows a page, newest first, paged by (`occurred_at`, `id`). `{key}` is the primary key, written `<occurred_at in Unix microseconds>-<id>`.
- **Sessions** (AC-2-04): a non-admin sees only rows with their principal.
- **Grant form:** the server (those the user can access whose environment, or one of whose tools', is `opt-in`), an optional tool (the template's `opt-in` overrides), a duration (1 minute to 7 days), and an optional session (one of the user's open sessions; the elicitation link fills it in).
- **Block form:** the `match` fields (at least one must be set, §2), the reason, and a duration (1 minute to 30 days).
- htmx refreshes the approvals list every 5 s and swaps rows after a revoke. Every action also works as a plain form.

**htmx 2.0.7**, minified, vendored at `internal/ui/static/htmx-2.0.7.min.js`, embedded, and served at `/ui/static/htmx-2.0.7.min.js` with a one-year cache. `make vendor-htmx` downloads `https://registry.npmjs.org/htmx.org/-/htmx.org-2.0.7.tgz`, checks it against the registry's `integrity` value, and extracts `dist/htmx.min.js`. Its SHA-256 is in `internal/ui/static/SHA256SUMS`, which `make check-generate` checks. The version in the file name means a browser never mixes a cached old copy with new pages.

**Headers** on every UI response: `Content-Security-Policy: default-src 'self'; frame-ancestors 'none'` (the pages have no inline script, and no other site may frame the approve button), and `Cache-Control: no-store` on pages.

## 6. Instance restart and resolvers

### Restart

AC-2-06. It keeps phase 1's one row per attempt, and phase 1's rule that only the reconciler sets `desired = stopped`.
1. The UI handler checks that the user owns the instance (an admin, for a shared one), and that it is live, `ready` and outdated: its generation or `uid` differs from the template's, or a secret in `secrets_used` changed. Then, in one transaction, `UPDATE instances SET restart_requested_at = now(), restart_requested_by = $2 WHERE id = $1 AND desired = 'running' AND status = 'ready'`, notify `instance:<id>`, and commit. If no row matched, the page says the instance changed and reloads.
2. The reconciler, on the notification or its next pass, **replaces** the instance in one transaction: take the advisory lock on (template, scope key) (phase 1 §3), and re-read the row `FOR UPDATE`. If it isn't live any more, do nothing.
   - Next, evaluate phase 1's stop conditions (phase 1 §2, "Who sets `desired = stopped`"), idle included. If any holds, stop the instance as phase 1 does, with that condition's reason, instead of replacing it. So a principal who lost access never gets a freshly resolved value, and an idle instance isn't kept alive by replacements.
   - Otherwise set its `desired = stopped`, insert a new `pending` row with the same scope, scope key, `owner_principal` and `render_session`, the template's current generation and `uid`, `attempt_started_at = now()`, and `last_used_at` set to the old row's `coalesce(last_used_at, ready_at)`, so a replacement doesn't count as use. Write `instance.stop` with reason `restart` and the user, and notify both IDs.
3. The old row stops being live in the same transaction, so the insert doesn't conflict with the partial unique index. A replacement doesn't change how many instances run, so it skips the instance limits.
4. `session_servers` rows are keyed by template, not instance, so every session keeps the server enabled. Its tools leave `tools/list` while the new instance starts, and come back with `list_changed` when it is ready.
5. Enable and call already select the live instance for (template, scope key) (phase 1 §3), so a waiting enable moves to the new row.

### Resolvers

AC-2-18. Configuration, in the YAML file only:
```yaml
resolvers:
  - name: github-app                    # ^[a-z][a-z0-9-]{0,62}$
    url: https://resolver.tools.svc/resolve   # must be https://
    audience: toolgate-resolver-github-app
    timeout: 10s                        # default 10s, at most 60s
    caFile: /etc/toolgate/resolver-ca.pem   # optional
```
- `url` must be `https://`, because the request carries a bearer token and its body chooses the principal. `caFile` covers private CAs.
- A template that names a resolver that isn't configured is invalid, with the reason in its `Valid` condition.
- **Token.** The chart mounts one projected service-account token per resolver, with that resolver's `audience` and `expirationSeconds: 600`, at `/var/run/secrets/toolgate/resolvers/<name>/token`. The gateway reads the file for each request, because the kubelet rotates it. One audience per resolver means a resolver can't replay the token to another. The resolver checks it with TokenReview.
- **Request.** The reconciler calls the resolver while rendering (`pending` to `starting`): `POST`, `Authorization: Bearer <token>`, `Content-Type: application/json`, body `{"resolver", "template": {"namespace", "name"}, "instance", "principal": {"id", "email", "name", "groups"}, "workspace", "session"}` from the render session's snapshot. `workspace` and `session` are sent only for per-session scope, matching the variables that exist for each scope (`SPEC.md` §9).
- **Response.** HTTP 200 with `{"value": "…", "expiresAt": "<RFC 3339>"}`. The body is read through `io.LimitReader` up to 1 MiB; `value` must not be empty; `expiresAt` must be at least 2 minutes in the future. Redirects aren't followed. Anything else fails the render with a reason naming the resolver and the status or field, never the body or the value, for example "Resolver `github-app` answered HTTP 503."
- **No caching.** Each render calls each resolver it names once and forgets the value after applying the Secret. The value lives only in the instance's Secret, never in PostgreSQL or a log. So the gateway never holds a value past its `expiresAt`.
- The render's `pending` to `starting` write sets `resolved_expires_at` to the earliest `expiresAt`.
- **Restart on expiry.** On its 30 s pass, the reconciler replaces (step 2 above, reason `resolver-expiry`) every live instance whose `resolved_expires_at` is less than 60 s away. So the fresh value is fetched 30 to 60 s before the old one expires, and the 2-minute minimum gives each instance at least a minute.
- The test resolver is a small Go binary in `test/resolver/` that returns a counter as the value, with an expiry the test sets.

## 7. Any namespaced kind

toolgate has no HelmRelease integration and keeps no list of allowed kinds. A template may contain any namespaced kind, HelmRelease included, like a Deployment. What the gateway can create is decided by the RBAC the cluster admin grants its service account.

**RBAC decides what the gateway can create.**
- The chart's default ClusterRole keeps phase 1's rules (phase 1 §6). The chart value `rbac.extraRules`, a list of RBAC rules, adds more to it. Admins may also bind their own roles to the gateway's service account.
- An extra kind needs `create`, `patch`, `delete`, `get`, `list` and `watch`, like phase 1's kinds, because readiness reads it.
- An apply that RBAC refuses fails the instance, with the API server's message as the reason.
- For a Helm chart, an admin grants `helm.toolkit.fluxcd.io` `helmreleases` with `source.toolkit.fluxcd.io` `ocirepositories` or `helmrepositories`.

**Only namespaced kinds.** This one rule is kept for lifecycle, not for permissions: teardown deletes the instance namespace, so a cluster-scoped object would leak.
- The catalog refuses a template with a kind whose `RESTMapping` is missing or cluster-scoped, with the reason in its `Valid` condition.
- The gateway reads the `RESTMapping`s once, at startup. A CRD installed after the gateway starts needs a gateway restart, and the admin docs say so.

**The admission policy keeps phase 1 §6's *where* rules and drops its kind allow-list.** It still denies writes outside namespaces labelled `toolgate.soliddowant.dev/writable: "true"` and Pod Security `restricted`, Namespace creates without those labels and the `tg-` prefix, every Namespace update, and Namespace deletes outside instance namespaces. Its exceptions for the gateway's own Lease and catalog status are unchanged. So a compromised gateway is still confined to toolgate's namespaces, whatever RBAC it holds there. The gateway still never reads Secrets.
- **RBAC kinds** inside an instance namespace are allowed when the admin grants them. A Role or RoleBinding there can grant only what the gateway already holds, by Kubernetes' escalation checks. A ClusterRole or ClusterRoleBinding is cluster-scoped, so the namespaced-only rule already refuses it.

**Nothing kind-specific in the runtime.**
- **Readiness** is kstatus for every object, as in phase 1, with one rule first: an object whose `Ready` condition is `False` at its current `observedGeneration`, and that isn't `Reconciling=True`, counts as failed, with the `Ready` message as the reason. Otherwise kstatus decides. Without that rule, kstatus reads a HelmRelease with `Ready=False` between helm-controller's retries as current. Flux objects follow kstatus conventions (`Ready`, `Reconciling` and `Stalled` conditions, and `observedGeneration`), so a HelmRelease is current once Flux reports it ready, and failed when it stalls or its install fails. The worker proves this with an e2e test, and hands back `HANDOFF: question` if kstatus misreads a Flux object.
- **The failure reason** (AC-2-17) comes from the failed object's conditions and events, as in phase 1, so a failed install reports the HelmRelease's own message.
- **Teardown** (AC-2-16) deletes the namespace, as in phase 1. Kubernetes keeps the namespace until objects with finalizers have finished, so helm-controller uninstalls the chart first. A HelmRelease that can't finish holds the namespace in `Terminating`, and phase 1's stuck-namespace warning and metric show it.
- **Shared namespaces:** phase 1 §7 already refuses kinds whose names and selectors toolgate can't check, so a `namespace: shared` template can't contain Flux kinds.

**The admin decides what charts may do.** helm-controller installs a chart with its own service account, often `cluster-admin`, so a template with a HelmRelease, or a compromised gateway, can install anything helm-controller can. `docs/admin/` says this plainly. An admin who wants Flux's multi-tenancy lockdown (`--default-service-account` and `--no-cross-namespace-refs` on helm-controller) grants the gateway `rolebindings` `create` (and the verbs above), and templates include a RoleBinding for the helm service account in their own namespace. Kubernetes' escalation checks keep that RoleBinding within what the gateway holds. The admin docs explain this. toolgate doesn't set or check `spec.serviceAccountName`.

## 8. Audit payloads

AC-2-19. `auditPayloads`, default `false`. When it is on:
- `tool.decision` and `approval.request` store the call's arguments, and `tool.result` stores the result (the `CallToolResult` as JSON). No other event has a payload. When it is off, nothing is stored.
- A payload under 64 KiB (65,536 bytes of JSON) goes in `audit_events.payload`. At or above that, it goes to S3 and `payload_ref` holds the object key.
- S3 is reached through the AWS SDK v2 (`github.com/aws/aws-sdk-go-v2/service/s3`), with settings `auditS3Endpoint`, `auditS3Region`, `auditS3Bucket`, `auditS3Prefix` and `auditS3PathStyle`, and credentials from the SDK's default chain (the chart sets them from a Secret). Payloads on without a bucket is a configuration error.
- The key is `<prefix>payloads/<yyyy>/<mm>/<dd>/<uuidv7>.json`, written with one `PutObject` (a payload is at most 16 MiB, phase 1 §7) before the row that refers to it.
- **Object lock.** At startup the gateway calls `GetObjectLockConfiguration`. If the bucket has object lock enabled, every put sets `ObjectLockMode: GOVERNANCE`, `ObjectLockRetainUntilDate` to now plus the audit retention, and `ChecksumAlgorithm: SHA256` (S3 requires a checksum on a locked put). Governance rather than compliance, so an admin with the bypass permission can still remove data; the gateway never has that permission. A not-found answer means no lock. Any other error is retried for 30 s, then `serve` exits non-zero, like a failed migration.
- **Failures.** If the arguments can't be stored, the call is refused before forwarding, with "The call couldn't be recorded. Try again later." If a result can't be stored, the result is still returned, the row records `payload_error` in its details, and `toolgate_audit_payload_errors_total` counts it.
- **Viewing** (`/ui/audit/events/{key}`). The viewer must be the event's principal or an admin, else 404. The gateway reads the object with `GetObject` and streams it itself. It uses no presigned URLs, which would be bearer links and would need browsers to reach the S3 endpoint. The page shows the first 1 MiB, escaped by `html/template`. `/payload` downloads all of it with `Content-Type: application/json`, `Content-Disposition: attachment` and `X-Content-Type-Options: nosniff`.
- **Approval pages** show the arguments from the `approval.request` event's payload. With payloads off they say "Arguments aren't shown because audit payloads are off."
- **Retention** (`SPEC.md` §12, default 90 days). The retention job selects each batch of old rows, deletes their objects with `DeleteObjects` (up to 1000 keys a request), and then deletes those rows. A missing object counts as deleted. If S3 refuses, the job keeps those rows for its next run and logs it, so no object is left behind without a row pointing at it.
  - The job pages with an (`occurred_at`, `id`) cursor and moves past refused rows, so one refused batch can't block the rest. It counts refused rows in a metric.
  - The admin docs require a bucket lifecycle rule under the payload prefix that expires current objects and noncurrent versions after the retention time plus a margin. That removes objects whose row was never written (a failed insert or transaction) and the versions left under delete markers in a versioned, locked bucket.

## 9. JSON API

Phase 2 adds **no** `/api/v1/` endpoints:
- The UI is rendered on the server and calls the same Go packages directly. Its htmx requests get HTML from `/ui/` routes.
- The two actions that widen access, creating a grant and approving a call, need a fresh step-up, which a bearer access token can't prove.
- The phase 2 CLI has no new commands. `toolgate audit tail` and `toolgate sessions` are phase 3, and their endpoints come with them.

**OpenAPI** (`SPEC.md` §5). The document for the existing endpoints (`/api/v1/secrets`, `/api/v1/settings`) is generated with `github.com/swaggest/openapi-go`, using its `openapi31` reflector on the request and response Go types the handlers use. Each API package exports its operations (method, path, types, status codes) next to its handlers. `make generate` writes `docs/dev/openapi.yaml`, and `make check-generate` fails on drift. A unit test asserts that the document's operations are exactly the `/api/v1/` routes on the mux, so no handler is added without its description. Why this library: it builds the document from types without changing how handlers are written, so phase 1's handlers and error bodies stay as they are. A framework such as Huma would replace them.

## 10. Interaction table

Phase 1's rows (phase 1 §4) still hold. Rows continue its numbering. Each row is one event combined with one operation already in flight, in the order PostgreSQL commits them, and each row's covering test proves its rule.

| # | Event | In flight | Rule | Test |
|---|---|---|---|---|
| 35 | Grant created (replica B) | a retry after a refusal (replica A) | No wait: the call or enable failed at once. The retry loads fresh policy state and is allowed. | integration |
| 36 | Grant revoked | call in flight | The call finishes. The next decision loads fresh state and refuses. The principal's streams get `list_changed` within 5 s. | AC-2-07 |
| 37 | Grant reaches `expires_at` | open sessions | Decisions filter by PostgreSQL's `now()`, so the next call is refused. Each replica's timer sends `list_changed` within 5 s. | AC-2-07 |
| 38 | Grant revoked or expires | enable waiting for its instance | The enable decides again when the instance is ready (phase 1 §3, enable step 6). It is refused, deletes its `session_servers` row and returns the refusal. | integration |
| 39 | Grant narrowed to session S | an enable from another session of the same user | Step 2 compares `session_id`, so the other session is refused. | AC-2-08 |
| 40 | Approve (replica B) | call waiting (replica A) | B's compare-and-set commits with `approval:<id>`. A re-reads the row and uses the approval (§4, step 6). | AC-2-11 |
| 41 | Approve | the waiting call's expiry, or the leader's pass | All compare-and-set on `status = 'pending'`, and the first commit wins. If expiry wins, the approver sees "This approval expired." If approval wins, the expiring write matches nothing; the waiter re-reads and uses the approval. If the pass expired it, the waiter re-reads status `expired` and refuses with `approval-timeout` without writing `approval.expire`. | unit |
| 42 | Approve and deny at once (two tabs) | each other | The first compare-and-set wins. The other page shows the decided status. | unit |
| 43 | Rule, temporary block or grant change that refuses the call | call waiting for approval | The wait decides steps 1–3 again on every wake and stops with the refusal. Step 6 decides again before forwarding. The approval expires unused. | integration |
| 44 | Client disconnects or cancels | call waiting for approval | The wait ends and writes nothing. The approval stays pending until decided or expired. Once approved, a retry with the same arguments uses it. | integration |
| 45 | Replica holding the waiting call dies | approval pending | As row 44. If nobody acts, the leader's pass expires it once `expires_at` is more than 1 minute past. A later retry whose `RequestState` names it gets the AC-2-13 message. | e2e |
| 46 | The same call again | its approval pending | Step 1 finds the pending row, so one call never has two approvals. | unit |
| 47 | Two calls with the same arguments | one usable approval | The `used_at` compare-and-set lets one through. The other starts again and gets a new pending approval. | unit |
| 48 | A retry whose `RequestState` names another principal's approval | — | Every field must match, so the lookup finds nothing, and the caller gets a new approval of their own. | unit |
| 49 | Template drops `approval: required` | approval pending | The next call needs no approval. The pending row expires unused. | unit |
| 50 | Temporary block created (replica B) | open sessions on every replica | `block:<id>` makes every replica send `list_changed`. The next decision loads the block. Within 5 s. | AC-2-14 |
| 51 | Temporary block reaches `expires_at` | open sessions | Decisions stop loading it. The timer sends `list_changed`, and the tools come back without a new enable. | AC-2-14 |
| 52 | `LISTEN` connection drops | waits, open streams | On reconnect, every wait re-reads its rows, the expiry timer is reset, and all streams get `list_changed`. The 10 s tick and the per-request policy state cover the gap. | integration |
| 53 | Restart requested | enable waiting on the same instance | The replacement inserts a new live row. The enable re-selects by (template, scope key) and waits on it. | integration |
| 54 | Restart requested | call in flight | The call may get the upstream error when the old namespace goes. Later calls get `not-ready` until the new instance is ready, then `list_changed`. Sessions keep the server enabled. | AC-2-06 |
| 55 | Restart clicked twice, or on two replicas | each other | Both before the replacement: the column is set twice and the reconciler replaces once. After it: the old row isn't `ready` and live, so the second request matches nothing and the page reloads. | unit |
| 56 | Restart or resolver replacement | idle stop of the same instance | Both take the advisory lock. If the stop commits first, the row isn't live and the replacement does nothing. If the replacement commits first, the stop's re-check runs on the new row. | integration |
| 57 | Leader change | restart requested, not yet done | The new leader's first pass finds `restart_requested_at` on a live row and replaces it. | unit |
| 58 | Resolver value close to expiry | instance `starting` | It is replaced like a ready one. The new attempt calls the resolver again. | unit |
| 59 | Resolver fails during a replacement | sessions using the server | The new row fails with the resolver's reason. Sessions keep the server enabled with no tools, and the next enable inserts a new instance (phase 1 row 12). | integration |

Rows 60 and 61 were dropped when #163 replaced §7; the numbers are left unused.

| # | Event | In flight | Rule | Test |
|---|---|---|---|---|
| 62 | Sign-in callback reaches another replica | sign-in started | The state is a `ui_logins` row, so any replica completes it. | integration |
| 63 | Step-up older than 5 minutes | grant or approval form open | The POST changes nothing and redirects to a step-up that returns to the form. | AC-2-09 |
| 64 | UI session idle for 1 h, or older than 8 h | page open | The next GET redirects to sign-in, and a POST gets 401 and changes nothing. | unit |
| 65 | S3 put fails | call with payloads on | For arguments, the call is refused before forwarding. For a result, the result is returned and the row records `payload_error`. | integration |
| 66 | Retention run | S3 refuses to delete objects | Those rows are kept for the next run. The cursor moves past them, so later rows are still deleted, and a metric counts them. | integration |
| 67 | Disable (`toolgate_disable_server`) | call waiting for approval | Step 6 finds no `session_servers` row, rolls back so `used_at` stays null, and refuses with `not-enabled`. | integration |
| 68 | Session ends | call waiting for approval | As row 67: the session's rows are gone, so step 6 refuses with `not-enabled`. | integration |
| 69 | Restart or resolver replacement | call waiting for approval | Step 6 re-selects the live instance by (template, scope key). If the new row isn't `ready` yet, it rolls back and refuses with `not-ready`; the approval stays usable for a retry. If it is, the call goes to the new instance. | integration |
| 70 | Instance fails | call waiting for approval | Step 6 finds the live instance isn't `ready`, rolls back and refuses with `not-ready` and the failure reason. | integration |
| 71 | Template deleted | call waiting for approval | The wait's re-decision or step 6 refuses (catalog access or `not-ready`), rolls back and writes `tool.decision` (refuse) with that reason. | integration |
| 72 | Resolver replacement due | instance idle with no enabling session | Step 2 evaluates phase 1's stop conditions first under the advisory lock. The idle condition holds, so it stops the instance instead of replacing it, and the resolver isn't called. | integration |
| 73 | Resolver replacement due | template deleted, scope changed, or access no longer includes `owner_principal` | As row 72: the stop condition holds, so the instance stops and no new row is inserted. | integration |

Any combination not in this table is a design gap: hand back `HANDOFF: question` with the case.

## 11. Settled choices

- **Time bounds:**
  - step-up 5 minutes (form pages ask at 4);
  - UI session 8 h, idle timeout 1 h; sign-in state 10 minutes;
  - grant 1 minute to 7 days; temporary block 1 minute to 30 days;
  - `approvalTimeout` default 10 minutes, from 1 minute to 1 hour; the leader's pass expires approvals 1 minute after `expires_at`;
  - 60 s of clock skew allowed both ways on a step-up's `auth_time`;
  - resolver timeout default 10 s, at most 60 s; a resolved value valid for at least 2 minutes; replacement when it is under 60 s from expiry.
- **Limits:**
  - 100 live grants and 100 pending approvals per principal; 32 waiting calls per principal on one replica;
  - 1000 live temporary blocks, with at least one `match` field; block lists 64 items of 256 characters; reasons 1024 characters;
  - `/ui/login` refused while more than 10,000 `ui_logins` rows exist;
  - resolver response 1 MiB;
  - UI form bodies 64 KiB; the secrets form 4 MiB plus 64 KiB, with the 4 MiB applied to the decoded value;
  - audit page 100 rows, time range at most 31 days; payload shown inline up to 1 MiB.
- **Settings** (flat, like phase 1's, in the generated reference): `publicURL`, `oidcUIClientID`, `oidcUIClientSecretFile`, `oidcUIScopes`, `adminGroup`, `approvalTimeout`, `auditPayloads`, `auditS3Endpoint`, `auditS3Region`, `auditS3Bucket`, `auditS3Prefix`, `auditS3PathStyle`, `resolvers`. `publicURL` must be `https://`, or `http://localhost` and `http://127.0.0.1` for tests, because a `__Host-` cookie needs `Secure`. Each resolver `url` must be `https://`. The chart sets them from `config.ui`, `config.audit` and `config.resolvers`, and mounts each resolver's token.
- **Audit events:** `grant.create`, `grant.revoke`, `block.create`, `approval.request`, `approval.approve`, `approval.deny`, `approval.expire`. UI actions, `secret.set` and `secret.delete` included, carry the UI user's principal, attestation `user`, and an empty workspace and session.
- **Libraries:** `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2` (both already in `go.mod`) for sign-in and PKCE; `net/http.CrossOriginProtection`, `crypto/rand`, `crypto/sha256` and `crypto/subtle` from the standard library; `html/template` and `embed`; htmx 2.0.7; the go-sdk's multi round-trip API and URL elicitation; `github.com/aws/aws-sdk-go-v2/service/s3` (already in `go.mod`); `github.com/swaggest/openapi-go`. No new cryptography: every random value, hash and comparison is a standard-library call.
- **Browser tests** use `github.com/chromedp/chromedp` with Chromium from the dev shell, against the kind environment. The fake IdP (`internal/testutil`, built on `github.com/zitadel/oidc/v3`) gets an authorization-code client for the UI; it supports `prompt=login`, `max_age` and `auth_time`. The test for a missing `auth_time` uses a provider that leaves it out.
- **Errors to the model** follow phase 1: short sentences that name what to do next and the approval or grant involved, for example "Tool `github__delete_repository` needs approval. Approve request `0190…` at `https://toolgate.example.com/ui/approvals/0190…`, then call the tool again with the same arguments."

## Decisions on the draft's open questions

The operator decided these. None of them changes `docs/acceptance/phase-2.md`.

1. **Approvals for service accounts:** only the call's principal approves, so a pod's call to an `approval: required` tool always fails with the approval error. A service account can't sign in. Admins approving other principals' calls isn't in phase 2.
2. **Ending a temporary block early:** not in phase 2. A block ends at its expiry. A mistaken block lasts until its expiry, so admins choose short expiries. Ending one early is a backlog idea.
3. **JSON API:** phase 2 adds no `/api/v1/` endpoints (§9).
4. **What charts may do** is the admin's choice, through the RBAC granted to the gateway and helm-controller's own permissions (§7). toolgate doesn't confine HelmReleases itself and keeps no list of allowed kinds.
5. **Grant elicitation:** dropped. A call that needs a grant fails at once with an error that links to the Grants page. Only approvals use elicitation (AC-2-11). `SPEC.md` §14 is updated to match.
6. **Claude Code's URL elicitation:** if the owner's manual check shows Claude Code doesn't open the page, the AC-2-12 path (an error with a link, approve, retry) is accepted for AC-2-11 in phase 2.
