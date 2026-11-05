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

## Admins

Set `config.ui.adminGroup` to the group whose members are admins in the UI. If it is empty, nobody is an admin. The UI reads the groups at sign-in. If you remove a user from the group, the change takes effect at the latest when the user's 8-hour session ends.
