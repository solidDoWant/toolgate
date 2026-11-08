# The web UI

For: admins who turn on the toolgate web UI.

The web UI is a set of pages under `/ui/` on the gateway's main port. Users sign in to it with your identity provider (IdP). The UI is on only when you set `config.ui.clientID` in the chart (`oidcUIClientID` in the [gateway configuration](configuration.md)).

The pages follow the light or dark setting of the browser. They work on a phone screen. On the Blocks page, each table row shows as a card on a phone, so you do not scroll sideways. On the other pages, wide tables scroll sideways. The UI loads no outside fonts, styles or scripts.

## Workspace scoping

When the web UI is on, toolgate limits the servers of each session of a signed-in user to an effective list. Each user chooses this list for each verified workspace, and chooses two defaults: one for verified workspaces and one for sessions without a verified workspace. Each default is all servers or none. From 0.4, the default for sessions without a verified workspace can also be an allow list of at most 64 servers that the user picks. The effective list only refuses. It never gives a user a server that the template does not give them. Service accounts and SPIFFE workloads do not have an effective list.

Scoping needs the web UI, because only the UI can ask the user to sign in again before access gets wider. If you turn off the UI, scoping is off. The lists of the users stay in the database, but they do nothing, so the users can use more servers after the restart. This is also true for the deny entries that users make with the `toolgate_deny` tool or with `toolgate workspace deny`. If you turn off the UI, a deny stops applying.

Upgrade note: After upgrading with the web UI on, every user sees no servers until they choose their defaults on the Workspaces page.

Users choose their lists and defaults on `/ui/workspaces`. A change that gives more access needs a fresh sign-in, as a grant does. A change that removes access does not. The approval page `/ui/approvals/<id>` also shows the change requests that the CLI makes, with the list before and after the change. Without the web UI, `/ui/workspaces` does not exist.

A user who has no defaults gets none for both defaults. Tell your users before you upgrade.

A user can also change their lists and defaults through the gateway API, which the CLI uses. The API applies a change that removes access at once. A change that gives more access does not apply at once. The gateway makes an access request instead (a pending change that the user must approve in the browser, with a fresh sign-in). The request expires after `approvalTimeout`, as an approval does. Without the web UI, `GET /api/v1/workspaces` and `POST /api/v1/workspaces/changes` answer 409. `docs/dev/openapi.yaml` describes these routes.

These limits apply to each user:

- At most 200 workspace lists.
- At most 64 servers on one list.
- At most 100 pending access requests. This count is separate from the count of pending approvals.

The gateway deletes access requests 7 days after they are approved, denied or expired. The audit log keeps their history.

## Make the OIDC client

The web UI uses its own OIDC client. It is not the client of `toolgate login`, because it uses the authorization code grant with a redirect URI.

1. In your IdP, make a client for the authorization code grant with PKCE. PKCE is a protection for the code that the IdP sends back.
2. Set its redirect URI to the public URL of the gateway plus `/ui/auth/callback`, for example `https://toolgate.example.com/ui/auth/callback`.
3. Make the IdP put the email, the email-verified and the groups claims in the ID token. The UI reads the user from the ID token only.
4. Set `config.ui.publicURL` and `config.ui.clientID` in the chart values.
5. If the client is confidential (it has a secret), mount the secret from a Kubernetes Secret with `resources.persistence` and `advancedMounts.gateway.gateway`, and set `config.ui.clientSecretFile` to the path of the file.

The public URL must start with `https://`. The UI cookies use the `__Host-` prefix, and a browser keeps them only on a secure origin.

Both clients must give a user the same subject (`sub`). Grants, approvals and secrets set in the UI match on that subject. Some IdPs give each client its own subject. Examples are pairwise subject identifiers (OIDC Core, section 8) and the "Subject mode" setting of Authentik. With such an IdP, grants never match, users get 404 on their own approvals, and their sessions do not see the secrets that they set in the UI.

## Sessions

A sign-in to the UI lasts 8 hours. It ends earlier when the user does nothing for 1 hour, or when the user signs out.

The gateway stores each sign-in in progress in PostgreSQL for 10 minutes, so the IdP can send the browser back to any replica. While more than 10,000 sign-ins are in progress, `/ui/login` refuses new ones.

Each sign-in sets a cookie in the browser. When the IdP sends the browser back, this cookie must match the sign-in. If it does not match, the gateway does not complete that sign-in. This check stops a link from another browser from signing the user in. The cookie is missing, for example, when the user goes back to an old page, or when 10 minutes pass at the IdP. The gateway then starts a new sign-in from the same browser, to the same page. If the sign-in is not in the database any more, the new sign-in goes to the home page. If the new sign-in fails in the same way, the user sees an error page. Then the user must allow cookies for the gateway and sign in from one tab only.

A failed sign-in writes an `auth.refuse` event to the audit log. The gateway writes at most 10 of these events a minute for each remote address. The metric `toolgate_ui_auth_refusals_dropped_total` counts the events that it did not write.

## Fresh sign-in

Two actions in the UI give more access: creating a grant and approving a tool call. They need a fresh sign-in, which is a sign-in less than 5 minutes old. When the sign-in is older, the UI sends the user back to the IdP with `prompt=login` and `max_age=60`. The IdP then asks the user to sign in again.

`max_age=60` tells the IdP to ask again when its own sign-in is more than 60 seconds old. The gateway does not send `max_age=0`, because some IdPs ignore the value 0. Authentik is an example. Authentik also obeys `prompt=login` only one time in each browser session. Thus, with Authentik, only `max_age` makes the user sign in again. You do not have to change a setting in Authentik for this.

The IdP must put `auth_time` in the ID token. `auth_time` is the time when the user signed in. The gateway accepts a fresh sign-in only when `auth_time` meets all of these conditions:

- It is at most 5 minutes old.
- It is at most 60 seconds in the future.
- It is not earlier than 60 seconds before the sign-in started.

If `auth_time` is too old, the gateway starts a new fresh sign-in one time. If that sign-in is also too old, the user sees an error page. This happens when the IdP signs the user in with the existing IdP session and ignores both `prompt=login` and `max_age`. Then make sure that your IdP asks for the password or passkey again when it gets one of them.

If the IdP does not send `auth_time`, the fresh sign-in fails. The other pages of the UI keep working, but nobody can create a grant or approve a call. If the user signs in again as a different user, the gateway refuses the sign-in and signs the user out.

## Admins

Set `config.ui.adminGroup` to the group whose members are admins in the UI. If it is empty, nobody is an admin. The UI reads the groups at sign-in. If you remove a user from the group, the change takes effect at the latest when the user's 8-hour session ends. So the admin group is a snapshot from the last sign-in, and a removed admin can keep admin access for up to 8 hours.

## How the pages show people

The pages show a person by name and email, for example `Fred <fred@example.com>`. If toolgate knows only an email, a name or a username, it shows that. Your own user shows as "you". A service account shows as `serviceaccount namespace/name`. A SPIFFE workload shows its SPIFFE ID, shortened if it is long. If toolgate has never seen a name for a user, the page shows a short form such as `user 914009fe…`, and a button opens the full ID. Hover over any name to see the full ID.

toolgate takes the names from the verified claims of the ID token when a user signs in and from the access token when a session starts. It keeps the newest values for each user in its database. It shows an email only if the identity provider verified it. It never asks the identity provider for names. A workspace has a short label too: `coder/user/name` shows as "Coder workspace name", and `claimed/name` shows as "unverified: name".

## The Servers page

`/ui/servers` is the first page after a sign-in. It starts with a header and then shows two tables:

- The catalog. These are the servers that `toolgate_list_servers` shows to the user from their own machine, with no workspace and no session. A blocked server shows the reason. The scope and environments labels explain themselves on hover.
- The running instances of the user. An admin also sees the shared instances. Each instance shows its status, how long ago it became ready (hold the pointer over the time for the exact UTC time) and if it runs an old version of its template.

An instance is outdated when it runs an old version of its template. The Reason and Ready since columns show only when an instance has a value for them. A ready, outdated instance has a Restart button at the end of its row. The owner of the instance can use it. For a shared instance, only an admin can use it. The gateway then starts a new instance from the current version of the template and stops the old one. Sessions keep the server enabled. The tools of the server leave the tool list while the new instance starts, and come back when it is ready. The audit log gets an `instance.stop` event with the reason `restart` and the user who asked for it.

If the instance stopped or changed before the request arrived, the page tells the user and shows the current list. If the template was deleted, or the owner lost access, or the instance is idle with no session that uses it, the gateway stops the instance and does not start a new one.

The server detail page (`/ui/servers/NAME`) has a header with the description, a row of labelled facts (scope, access, environments, kind, status and secrets), and cards for the workspace availability, the tools and the terminal commands. On a phone, each table row shows as a card.

The sidebar shows the Blocks link only to admins.

## The Sessions pages

`/ui/sessions` opens with a header and a one-sentence summary, then the filters and the table of sessions, newest activity first. On a narrow screen each row shows as a card. Each row shows:

- **Who:** the user's name or email, or "you". An admin sees other users by name.
- **Where:** the workspace, with a "verified" or "unverified" badge. The raw workspace ID shows on hover.
- **Client:** the client name and version that the client sent in `initialize`, for example "Claude Code 2.1.319", and the kind: a helper session, or a connector session (the session ID starts with `mcp.`). The full session ID shows on hover. The session page shows the short ID with a small copy button.
- **State:** `active` when the session is open and made a request in the last 5 minutes. `idle` when it is open and made none for 5 minutes or more. The gateway ends an idle session after `sessionIdleTimeout`. `resumable until` a time when the session ended and can still be resumed (`sessionResumeWindow` after it ended). Connector sessions are never resumed. Any other ended session shows `ended`. Under the state, the time of the last activity as a relative time such as "2 min ago". The exact UTC time shows on hover. For a resumable session, the time it can be resumed until shows on hover.
- **Servers:** the servers that the session has enabled, as small badges, and how many instances are running for it. The count includes ready shared instances, the user's ready per-user instances and the session's own ready per-session instances of those servers.
- **Last hour:** three small badges on one line: the tool decisions in the last hour, how many of them were refusals (`deny` or `refuse`), and the approvals that still wait for a decision. The first two badges link to the audit page, filtered to this session. The third links to the approvals page. A session with no activity shows zeros.

Tool search (on or off) shows on the session page.

Every row of an open session has an "Open" button.

The state filter is a segmented control. The workspace and user filters are menus in the same row. With JavaScript, a change applies at once. Without it, use the Filter button. The filters are in the URL, so you can bookmark a view: `state` is `active` (the default), `recent` (last request in the last 24 hours) or `all` (all sessions inside the resume window); `workspace` is a workspace ID. The default view shows your own active sessions. An admin can also pick one user, or everyone, with `user` (a subject, or `all`). A user who is not an admin always sees only their own sessions.

If there are no sessions, the page says that sessions appear when Claude Code connects through the toolgate plugin, and has a Quick setup button.

"Open" on the row of an open session shows `/ui/sessions/` followed by the session's number. The page has an "All sessions" link above the title, and four parts:

- **Summary row:** the same facts as the list row, as labelled values: who, where (with the verified or unverified badge), the client and its kind, the state, tool search, the short session ID with a small copy button, and when the session started and was last active.
- **Servers:** each server that is enabled in the session, with its status (`ready`, `starting`, `failed`, `blocked`, `stopped`) and the reason, if any. The Instance column says `remote` for a remote server, or the namespace of its template. If the gateway can't show a status to the viewer, for example an admin who views a session of a user whose groups it doesn't know, the status is `unknown`.
- **Effective scope:** what the session may use. It shows the list that applies (the list of the workspace or the matching default), the allow and deny entries of the workspace, and the servers, allow entries and deny entries that are for this session only. It links to the Workspaces and Servers pages, where you change them.
- **Recent tool calls:** the newest 50 events of the session's calls, with time, server, tool, outcome, latency, and the reason for a refusal. Approval requests and approved calls show too. Each row links to its audit event. A link below the table opens the audit log filtered to the session.

Next to each server, the owner of the session sees a "Deny for this session" button. It opens a page that asks to confirm. After the owner confirms, the server is blocked in this session only: its tools are refused there, and the server row shows "blocked for this session". Other sessions and the workspace lists do not change. This is the same change as `toolgate workspace deny --session`, and it writes the same audit event. A deny only narrows access, so it does not need a fresh sign-in. The form is plain HTML and works without JavaScript. An admin who opens another user's session sees no button, and the deny pages give them 404.

A user who asks for another user's session, or for a session that has ended, gets 404. An admin can open any open session. You cannot end a session from the UI.

## The Secrets page

`/ui/secrets` shows the signed-in user's secrets: each name, the servers in the catalog that use it (templates that reference `${secret:NAME}`), and when it was last set, as a relative time with the exact time on hover. The created time shows only when the secret was set again after it was created. Delete asks for confirmation in the browser first. The form is plain HTML, so it also works without JavaScript. It needs the secrets keyset, like `/api/v1/secrets`, and is not offered without it: the page returns 404 and the navigation hides the Secrets link. It uses the same secret broker as the API, so the CLI and the page show the same secrets. It writes the same `secret.set` and `secret.delete` audit events, with attestation `user` and no workspace or session.

The form takes a value in a text box or from a file, not both. A value is 4 MiB at most. Text from the box has its CRLF line breaks changed to LF, so a PEM key matches one set with the CLI. A file's bytes are kept as they are. The form never shows a value, and each action answers with a redirect back to the page.

## The Grants page

A grant lets a user use an `opt-in` server or tool from their own machine for a limited time. `/ui/grants` lists the live grants of the user and the grants that ended in the last 7 days. Each user sees only their own grants. A grant of another user gives 404, also to an admin.

`/ui/grants/new` is the form that creates a grant. It needs a fresh sign-in (see "Fresh sign-in"). The gateway accepts a grant only when all of these conditions are true:

- The grant is for the user who creates it.
- The user has access to the server in the catalog.
- For a grant on the whole server: the environment of the server or of one of its tools is `opt-in`.
- For a grant on one tool: the environment of the server is `any` and the environment of the tool is `opt-in`. On an `opt-in` server, a user needs a grant on the whole server to turn the server on, so a grant on one tool is not useful there.
- The grant lasts from 1 minute to 7 days.
- The user has fewer than 100 live grants.
- If the user picks a session, it is one of their open sessions. Then only that session can use the grant.

When a call or an enable needs a grant, the refusal links to this form, filled with the server, the tool and the session. A user can revoke a grant at any time, without a fresh sign-in. The gateway writes the events `grant.create` and `grant.revoke`. When a grant ends, the tools of the server leave the tool list of the user's sessions within 5 seconds.

## The Audit page

`/ui/audit` lists audit events, newest first, 100 on a page. A user sees only their own events. An admin sees all events.

Each row is one line: when (for example "3 min ago", with the exact UTC time on hover), an outcome badge (ok, refused, error, pending, approved or denied), a short sentence for what happened, who did it, the workspace (with a "verified" badge when it is verified), the session, and the server and tool. People show as on the other pages. Click the event sentence, the user, workspace, session, server or tool in a row to filter by it. The session links to its page when the session is still open.

A filter bar sits above the table. It has the time range, the outcome (refused, errors, approvals), the event types and a "More filters" panel. The panel holds the category (tool calls, access and scope changes, sign-ins, server lifecycle) and, from lists, the user (for admins), workspace, session, server and tool. These lists hold the values found in the chosen time range. The event type control is a list of checkboxes, grouped by category, with a search box. Each type has a readable name, and the raw type shows on hover. With a category and event types, the filter keeps the chosen types that belong to the category. Each filter must match exactly. For a user, the gateway ignores the user filter and shows only that user's events.

Filters apply when you change them. In a panel, they apply when the panel closes. Without JavaScript, press "Filter". Each applied filter also shows as a chip above the table, such as "Server: mail-archiver ×". The × removes that one filter, and "Clear all" removes all of them. The filters are in the URL (`?type=a&type=b`), so you can share a link.

The time presets are the last hour, the last 24 hours (the default), today and the last 7 days. Choose "Custom…" to show "From" and "To" in UTC. A range can be at most 31 days, so a query reads at most two monthly partitions of the audit table. A longer range gives an error.

The count of matching events, the chips and the export link share one line. "Export as JSON Lines" downloads all events of the current filter, newest first, one JSON object per line. It follows the same rules as the page: a user gets only their own events.

The link "Details" on a row shows one event at `/ui/audit/events/` followed by a key: the time in Unix microseconds, a hyphen, and the event ID. The page shows the summary, the fields as a list, the payload (when stored), the stored JSON (collapsed) and the related events of the same request or trace. The event is shown to its principal and to admins. Anyone else gets 404, so the key tells them nothing.

When audit payloads are on, the event page also shows the payload of the event. The gateway reads a payload from S3 itself and sends it to the browser. It makes no presigned URLs, so browsers do not need to reach S3. The page shows the first 1 MiB of the payload as text. Under it, "Download the payload" gives `/ui/audit/events/` followed by the key and `/payload`. That link downloads the whole payload as a JSON file. It has the headers `Content-Disposition: attachment` and `X-Content-Type-Options: nosniff`. The same people can open it as can open the event page.

## The Blocks page

`/ui/blocks` is only for admins. A user who is not an admin gets 403.

The page starts with a "New block" button. Under it are two tables. The first table lists the temporary blocks that are live now. The second table lists the `ToolgateRule` objects that the gateway reads from git. This list is read-only, and each rule shows if it is valid. To change a rule, change it in git. Times show as relative times, such as "in 3 h". Hold the pointer over a time to see the exact UTC time. A block ID shows as its first 8 characters. Hold the pointer over the ID to see all of it, or select the small copy button next to it to copy it. The match of a block or a rule shows as one small label for each item, such as `server github` or `attestation user`.

A temporary block refuses the tools that match it, for every user, until it expires. "New block" opens the form in a dialog. If the browser does not run JavaScript, the button opens the same form on its own page, `/ui/blocks/new`. The form has these fields:

- Principals, groups, workspaces, sessions, servers and tools. Type an item and press Enter to add it. Select × on an item to remove it. An item is a glob, and `*` matches any run of characters. Without JavaScript, write one item on each line.
- Attestation: any, `attested` or `user`.
- A duration from 1 minute to 30 days: a number and a unit (minutes, hours or days).
- A reason of up to 1024 characters. Users see it in the refusal.

You must set at least one match field. Each list can have 64 items of 256 characters. At most 1000 blocks can be live. The gateway writes a `block.create` event to the audit log, and open sessions get the change within 5 seconds.

You cannot end a block early. It ends at its expiry, so choose a short duration.

## Protection of the pages

Each request to the UI that changes something must come from a UI page. The gateway does two checks:

1. It uses `net/http.CrossOriginProtection` from Go. This check refuses a request that a page on another site sends, by the `Sec-Fetch-Site` and `Origin` headers. Your ingress must not remove these headers.
2. It compares a token in the form with the token of the user's session. A form from another site does not know this token.

If one check fails, the gateway answers 403 and changes nothing.

Each UI response has the header `Content-Security-Policy: default-src 'self'; frame-ancestors 'none'`. Thus the pages load scripts and styles only from the gateway, and no other site can show them in a frame. Pages have `Cache-Control: no-store`. A form body can be at most 64 KiB.

## Approvals

A template can mark a tool with `approval: required`. Then each call of the tool needs the approval of the user who made the call. An approval lets exactly one call through.

If the client supports URL elicitation (a request from the server to open a web page), the gateway asks the client to open the approval page under `/ui/approvals/`. The call then waits until the user approves or denies the request, or until the request expires. A user can have at most 32 calls that wait on one gateway replica. If the client does not support URL elicitation, or the user declines to open the page, the gateway refuses the call and gives the link to the approval page. The user approves the request on that page and calls the tool again with the same arguments.

If the web UI is off, nobody can approve a call, so each call of such a tool fails. A service account cannot sign in to the UI, so its calls of such a tool always fail.

Set `config.ui.approvalTimeout` to the time that a request waits for approval, from `1m` to `1h`. The default is `10m`. An approved request also stays usable for this time. The gateway marks a waiting request expired 1 minute after this time. A user can have at most 100 requests waiting.

`/ui/approvals` lists the requests of the signed-in user that wait for approval, with the server, the tool, the session and the workspace. The list updates every 5 seconds. Each request has its own page, where the user approves or denies it. Approving needs a fresh sign-in. Denying does not, because it gives no access. Only the user who made the call can see or decide a request. Other users get 404, and admins also get 404. If a user opens an approval link without a UI session, the gateway sends the browser straight to a fresh sign-in.

If two people or two browser tabs decide the same request, the first decision wins. The other page then shows the real status, for example "This approval expired." Each decision writes an `approval.approve` or `approval.deny` event to the audit log. When audit payloads are on, the approval page shows the arguments of the call, taken from the payload of the `approval.request` event. When audit payloads are off, the approval page says "Arguments aren't shown because audit payloads are off."
