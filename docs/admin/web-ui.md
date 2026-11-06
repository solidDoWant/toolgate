# The web UI

For: admins who turn on the toolgate web UI.

The web UI is a set of pages under `/ui/` on the gateway's main port. Users sign in to it with your identity provider (IdP). The UI is on only when you set `config.ui.clientID` in the chart (`oidcUIClientID` in the [gateway configuration](configuration.md)).

## Workspace scoping

When the web UI is on, toolgate limits the servers of each session of a signed-in user to an effective list. Each user chooses this list for each verified workspace, and chooses two defaults: one for verified workspaces and one for sessions without a verified workspace. Each default is all servers or none. The effective list only refuses. It never gives a user a server that the template does not give them. Service accounts and SPIFFE workloads do not have an effective list.

Scoping needs the web UI, because only the UI can ask the user to sign in again before access gets wider. If you turn off the UI, scoping is off. The lists of the users stay in the database, but they do nothing, so the users can use more servers after the restart.

Upgrade note: After upgrading with the web UI on, every user sees no servers until they choose their defaults on the Workspaces page.

A user who has no defaults gets none for both defaults. Tell your users before you upgrade.

## Make the OIDC client

The web UI uses its own OIDC client. It is not the client of `toolgate login`, because it uses the authorization code grant with a redirect URI.

1. In your IdP, make a client for the authorization code grant with PKCE. PKCE is a protection for the code that the IdP sends back.
2. Set its redirect URI to the public URL of the gateway plus `/ui/auth/callback`, for example `https://toolgate.example.com/ui/auth/callback`.
3. Make the IdP put the email, the email-verified and the groups claims in the ID token. The UI reads the user from the ID token only.
4. Set `config.ui.publicURL` and `config.ui.clientID` in the chart values.
5. If the client is confidential (it has a secret), mount the secret from a Kubernetes Secret with `resources.persistence` and `advancedMounts.gateway.gateway`, and set `config.ui.clientSecretFile` to the path of the file.

The public URL must start with `https://`. The UI cookies use the `__Host-` prefix, and a browser keeps them only on a secure origin.

Both clients must give a user the same subject (`sub`). Grants, approvals and secrets set in the UI match on that subject. Some IdPs give each client its own subject. Examples are pairwise subject identifiers (OIDC Core §8) and the "Subject mode" setting of Authentik. With such an IdP, grants never match, users get 404 on their own approvals, and their sessions do not see the secrets that they set in the UI.

## Sessions

A sign-in to the UI lasts 8 hours. It ends earlier when the user does nothing for 1 hour, or when the user signs out.

The gateway stores each sign-in in progress in PostgreSQL for 10 minutes, so the IdP can send the browser back to any replica. While more than 10,000 sign-ins are in progress, `/ui/login` refuses new ones.

A failed sign-in writes an `auth.refuse` event to the audit log. The gateway writes at most 10 of these events a minute for each remote address. The metric `toolgate_ui_auth_refusals_dropped_total` counts the events that it did not write.

## Fresh sign-in

Two actions in the UI give more access: creating a grant and approving a tool call. They need a fresh sign-in, which is a sign-in less than 5 minutes old. When the sign-in is older, the UI sends the user back to the IdP with `prompt=login` and `max_age=0`. The IdP then asks the user to sign in again.

The IdP must put `auth_time` in the ID token. `auth_time` is the time when the user signed in. The gateway accepts a fresh sign-in only when `auth_time` meets all of these conditions:

- It is at most 5 minutes old.
- It is at most 60 seconds in the future.
- It is not earlier than 60 seconds before the sign-in started.

If the IdP does not send `auth_time`, the fresh sign-in fails. The other pages of the UI keep working, but nobody can create a grant or approve a call. If the user signs in again as a different user, the gateway refuses the sign-in and signs the user out.

## Admins

Set `config.ui.adminGroup` to the group whose members are admins in the UI. If it is empty, nobody is an admin. The UI reads the groups at sign-in. If you remove a user from the group, the change takes effect at the latest when the user's 8-hour session ends. So the admin group is a snapshot from the last sign-in, and a removed admin can keep admin access for up to 8 hours.

## The Servers page

`/ui/servers` is the first page after a sign-in. It shows two lists:

- The catalog. These are the servers that `toolgate_list_servers` shows to the user from their own machine, with no workspace and no session. A blocked server shows the reason.
- The running instances of the user. An admin also sees the shared instances. Each instance shows its status and if it runs an old version of its template.

An instance is outdated when it runs an old version of its template. A ready, outdated instance has a Restart button. The owner of the instance can use it. For a shared instance, only an admin can use it. The gateway then starts a new instance from the current version of the template and stops the old one. Sessions keep the server enabled. The tools of the server leave the tool list while the new instance starts, and come back when it is ready. The audit log gets an `instance.stop` event with the reason `restart` and the user who asked for it.

If the instance stopped or changed before the request arrived, the page tells the user and shows the current list. If the template was deleted, or the owner lost access, or the instance is idle with no session that uses it, the gateway stops the instance and does not start a new one.

The navigation shows the Blocks link only to admins.

## The Sessions pages

`/ui/sessions` lists the open sessions. A session is open until the gateway ends it. A user sees only their own sessions. An admin sees all of them.

"Open" on a row shows `/ui/sessions/` followed by the session's number. The page shows the session's servers that are enabled and its 20 newest `tool.decision` events. Each event links to its audit page. A user who asks for another user's session, or for a session that has ended, gets 404. An admin can open any open session. You cannot end a session from the UI.

## The Secrets page

`/ui/secrets` shows the signed-in user's secrets: each name with the dates it was created and last set. It needs the secrets keyset, like `/api/v1/secrets`, and is not offered without it. It uses the same secret broker as the API, so the CLI and the page show the same secrets. It writes the same `secret.set` and `secret.delete` audit events, with attestation `user` and no workspace or session.

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

You can filter by principal, workspace, session, server and tool. Each filter must match exactly. An admin can also filter by principal. For a user, the gateway ignores the principal filter.

You can also set a time range. The default is the last 24 hours. A range can be at most 31 days, so a query reads at most two monthly partitions of the audit table. A longer range gives an error. All times are in UTC.

The link "Open" on a row shows one event at `/ui/audit/events/` followed by a key: the time in Unix microseconds, a hyphen, and the event ID. The event is shown to its principal and to admins. Anyone else gets 404, so the key tells them nothing.

When audit payloads are on, the event page also shows the payload of the event. The gateway reads a payload from S3 itself and sends it to the browser. It makes no presigned URLs, so browsers do not need to reach S3. The page shows the first 1 MiB of the payload as text. Under it, "Download the payload" gives `/ui/audit/events/` followed by the key and `/payload`. That link downloads the whole payload as a JSON file. It has the headers `Content-Disposition: attachment` and `X-Content-Type-Options: nosniff`. The same people can open it as can open the event page.

## The Blocks page

`/ui/blocks` is only for admins. A user who is not an admin gets 403.

The page has two parts. The first part lists the temporary blocks that are live now, and a form that adds one. The second part lists the `ToolgateRule` objects that the gateway reads from git. This list is read-only, and each rule shows if it is valid. To change a rule, change it in git.

A temporary block refuses the tools that match it, for every user, until it expires. The form has these fields:

- Principals, groups, workspaces, sessions, servers and tools. Write one item on each line. An item is a glob, and `*` matches any run of characters.
- Attestation: `attested` or `user`.
- A reason of up to 1024 characters. Users see it in the refusal.
- A duration from 1 minute to 30 days, in minutes.

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
