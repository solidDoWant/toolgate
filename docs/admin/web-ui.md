# The web UI

For: admins who turn on the toolgate web UI.

The web UI is a set of pages under `/ui/` on the gateway's main port. Users sign in to it with your identity provider (IdP). The UI is on only when you set `config.ui.clientID` in the chart (`oidcUIClientID` in the [gateway configuration](configuration.md)).

## Make the OIDC client

The web UI uses its own OIDC client. It is not the client of `toolgate login`, because it uses the authorization code grant with a redirect URI.

1. In your IdP, make a client for the authorization code grant with PKCE. PKCE is a protection for the code that the IdP sends back.
2. Set its redirect URI to the public URL of the gateway plus `/ui/auth/callback`, for example `https://toolgate.example.com/ui/auth/callback`.
3. Make the IdP put the email, the email-verified and the groups claims in the ID token. The UI reads the user from the ID token only.
4. Set `config.ui.publicURL` and `config.ui.clientID` in the chart values.
5. If the client is confidential (it has a secret), mount the secret from a Kubernetes Secret with `persistence`, and set `config.ui.clientSecretFile` to the path of the file.

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

Set `config.ui.adminGroup` to the group whose members are admins in the UI. If it is empty, nobody is an admin. The UI reads the groups at sign-in. If you remove a user from the group, the change takes effect at the latest when the user's 8-hour session ends.

## The Servers page

`/ui/servers` is the first page after a sign-in. It shows two lists:

- The catalog. These are the servers that `toolgate_list_servers` shows to the user from their own machine, with no workspace and no session. A blocked server shows the reason.
- The running instances of the user. An admin also sees the shared instances. Each instance shows its status and if it runs an old version of its template.

An instance is outdated when it runs an old version of its template. A ready, outdated instance has a Restart button. The owner of the instance can use it. For a shared instance, only an admin can use it. The gateway then starts a new instance from the current version of the template and stops the old one. Sessions keep the server enabled. The tools of the server leave the tool list while the new instance starts, and come back when it is ready. The audit log gets an `instance.stop` event with the reason `restart` and the user who asked for it.

If the instance stopped or changed before the request arrived, the page tells the user and shows the current list. If the template was deleted, or the owner lost access, or the instance is idle with no session that uses it, the gateway stops the instance and does not start a new one.

The navigation shows the Blocks link only to admins.

## The Secrets page

`/ui/secrets` shows the signed-in user's secrets: each name with the dates it was created and last set. It needs the secrets keyset, like `/api/v1/secrets`, and is not offered without it. It uses the same secret broker as the API, so the CLI and the page show the same secrets. It writes the same `secret.set` and `secret.delete` audit events, with attestation `user` and no workspace or session.

The form takes a value in a text box or from a file, not both. A value is 4 MiB at most. Text from the box has its CRLF line breaks changed to LF, so a PEM key matches one set with the CLI. A file's bytes are kept as they are. The form never shows a value, and each action answers with a redirect back to the page.

## The Audit page

`/ui/audit` lists audit events, newest first, 100 on a page. A user sees only their own events. An admin sees all events.

You can filter by principal, workspace, session, server and tool. Each filter must match exactly. An admin can also filter by principal. For a user, the gateway ignores the principal filter.

You can also set a time range. The default is the last 24 hours. A range can be at most 31 days, so a query reads at most two monthly partitions of the audit table. A longer range gives an error. All times are in UTC.

The link "Open" on a row shows one event at `/ui/audit/events/` followed by a key: the time in Unix microseconds, a hyphen, and the event ID. The event is shown to its principal and to admins. Anyone else gets 404, so the key tells them nothing.

## Protection of the pages

Each request to the UI that changes something must come from a UI page. The gateway does two checks:

1. It uses `net/http.CrossOriginProtection` from Go. This check refuses a request that a page on another site sends, by the `Sec-Fetch-Site` and `Origin` headers. Your ingress must not remove these headers.
2. It compares a token in the form with the token of the user's session. A form from another site does not know this token.

If one check fails, the gateway answers 403 and changes nothing.

Each UI response has the header `Content-Security-Policy: default-src 'self'; frame-ancestors 'none'`. Thus the pages load scripts and styles only from the gateway, and no other site can show them in a frame. Pages have `Cache-Control: no-store`. A form body can be at most 64 KiB.

## Approvals

A template can mark a tool with `approval: required`. Then each call of the tool needs the approval of the user who made the call. The gateway refuses the call and gives a link to the approval page under `/ui/approvals/`. The user approves the request on that page and calls the tool again with the same arguments. An approval lets exactly one call through.

If the web UI is off, nobody can approve a call, so each call of such a tool fails. A service account cannot sign in to the UI, so its calls of such a tool always fail.

Set `config.ui.approvalTimeout` to the time that a request waits for approval, from `1m` to `1h`. The default is `10m`. An approved request also stays usable for this time. The gateway marks a waiting request expired 1 minute after this time. A user can have at most 100 requests waiting.
