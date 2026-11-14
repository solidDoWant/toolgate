# Instance credentials design

This document decides how two new kinds of values reach the Secrets of an instance:

- `${exchange:NAME}`: a token for the calling user, which the gateway gets from the IdP by OAuth 2.0 token exchange (RFC 8693). The gateway refreshes it while the user is active.
- `${templateSecret:NAME/KEY}`: a value from a Secret that an admin keeps in one namespace that the chart configures. The gateway keeps the copies current.

It adds to `docs/design/phase-1.md` to `docs/design/phase-4.md`, which still hold unless a section here says otherwise. Where they disagree, stop and ask (`HANDOFF: question`). Workers implement it and never change its rules. The operator and owner change it.

The targets are the acceptance lists of issues #611 and #613, copied in §10.

## 1. Who does what

Phase 1's split stays (phase 1 §1). Request handling runs on every replica and never touches Kubernetes objects. The runtime reconciler runs only on the Lease holder, and it is still the only code that touches Kubernetes.

The split meets a problem in token exchange. Only a request holds the user's access token, which the exchange needs. Only the leader writes the instance Secret. The exchanged token must never be in a PostgreSQL row. So the request and the leader share the work like this:

- The replica that serves the request runs the exchange, with the request's own token. The user's token never leaves that replica.
- That replica sends the exchanged token to the leader on a new notification channel, `toolgate_credentials`. The payload is encrypted with the secrets keyset (§3, "Delivery to the leader"). PostgreSQL only passes ciphertext from one replica to the others, and stores it in no table.
- The leader writes the token into the instance Secret: in the render at start, or with a patch at refresh.

Rows still say what is wanted and when. A request claims an exchange with a row update, and the leader records each applied token's expiry. So every rule below is still "a row changed, then the reconciler or a replica reacted", in commit order, as in phase 1.

**New request-handling work**, on every replica:
- refusing an exchange template for an identity that is not a signed-in user (§3, "Who can start it");
- claiming and running exchanges at start and at half-life, and writing `credential.exchange` (§3);
- sending each exchanged token to the leader (§3, "Delivery to the leader");
- validating both new variables in the catalog, for the `Valid` condition (§3 and §4).

**New reconciler work**, on the leader only:
- receiving delivered tokens, rendering them at start, and patching them in at refresh (§3);
- stopping an instance whose exchanged token expired (§3, "Expiry");
- reading template secrets at render, watching their namespace, and patching changed values into running instances (§4).

**Notifications.** The new channel `toolgate_credentials` carries delivered tokens (§3). Every replica `LISTEN`s on it, and only the leader acts on it. A request that writes `instance_exchanges` notifies `instance:<id>` in the same transaction, as phase 1 does for other rows.

## 2. Rules shared by both variables

**Where they go.** `${exchange:…}` and `${templateSecret:…}` are allowed only in the values directly under `data` or `stringData` of a `Secret`, like `${secret:…}` and `${resolver:…}` (`SPEC.md` §9, rendering rule 4). They are not allowed in `remote.headers`. The existing header check already refuses them there.

**The whole value.** A value that uses one of them must be exactly that one variable, with no other text and no other variable. For example, `token: ${exchange:temporal}` is valid, and `token: "Bearer ${exchange:temporal}"` is not. The reason: toolgate updates the key in place later, without rendering the template again. The instance's template generation can be gone by then, because the gateway keeps only the current generation. A key that holds one value from one source can be updated without the template.

**`data` and `stringData`.** Under `stringData`, the rendered value is the text of the value. Under `data`, the rendered value is the standard base64 encoding of the value, so the key holds exactly the value's bytes. This differs from `${secret:…}` and `${resolver:…}` under `data`, which are put in as they are (§8, choice 4). A value under `stringData` must be valid UTF-8, or the render fails.

**Updates.** toolgate updates a key with a JSON merge patch of the Secret, `{"data": {"<key>": "<base64 of value>"}}`, with field manager `toolgate`. The gateway already has `patch` on Secrets, and the admission policy already confines it to toolgate's namespaces. The gateway never reads a Secret in an instance namespace. It does not need to, because each update writes the full value of one key.

**Apply order.** The render applies every `Secret` object of the render before any other object. The Secrets keep their template order among themselves, and so do the other objects. One type is the exception: a Secret of type `kubernetes.io/service-account-token` keeps its template position among the other objects. It names a ServiceAccount, which must exist before Kubernetes fills the Secret. So no pod of the instance starts before its Secrets exist with their values. This applies to every template, not only to those that use the new variables (§8, choice 5).

## 3. Token exchange

### Configuration

Chart value `config.tokenExchanges`, a list. The chart writes it to the gateway's configuration file as `tokenExchanges`. It is a YAML-only setting, like `resolvers`, and the generated reference documents it.

```yaml
config:
  tokenExchanges:
    - name: temporal
      tokenURL: https://authentik.example.com/application/o/token/
      clientID: toolgate-token-exchange
      clientSecretFile: /etc/toolgate/exchanges/temporal/client-secret
      audience: temporal-client-id               # optional
      scope: openid temporal-permissions         # optional
      requestedTokenType: urn:ietf:params:oauth:token-type:access_token   # optional
      caFile: /etc/toolgate/exchanges/ca.pem     # optional
      timeout: 10s                               # optional
```

| Field | Rule |
|---|---|
| `name` | Required. `^[a-z][a-z0-9-]{0,62}$`, unique in the list. |
| `tokenURL` | Required. An `https://` URL with a host. |
| `clientID` | Required. A confidential client, so only the gateway can exchange. |
| `clientSecretFile` | Required. The gateway reads the file for each exchange, because the kubelet updates a mounted Secret in place. It trims white space at the end. A missing or empty file fails that exchange. |
| `audience` | Optional. Sent as `audience` when set. |
| `scope` | Optional. Space-separated scopes, sent as `scope` when set. |
| `requestedTokenType` | Optional. A URI, sent as `requested_token_type` when set. |
| `caFile` | Optional. A PEM file of the CAs that sign the certificate of the token endpoint. Otherwise the system roots are used. It is read at start, like a resolver's `caFile`. |
| `timeout` | Optional. A Go duration, default `10s`, at most `60s`. It bounds one exchange. |

- The list has at most 8 entries.
- `tokenExchanges` needs `secretsKeysetFile`. Without it, the gateway refuses to start with "tokenExchanges needs secretsKeysetFile: the gateway encrypts each exchanged token on its way to the leader."
- The chart fails the render for a missing `name`, `tokenURL`, `clientID` or `clientSecretFile`, a bad `name`, or a `tokenURL` that isn't `https://`. The gateway checks the same rules at start.
- The admin mounts the client secret and the `caFile` with `resources.persistence` and `advancedMounts.gateway.gateway`, as for resolvers.
- Settings are read at start. A changed list takes effect after a restart of the gateway.

### Template syntax, scope and validation

`${exchange:NAME}`, where `NAME` is the `name` of an entry. It follows the rules of §2.

**Scope.** `templatescope.ForVariable` returns `per-user` for `exchange:`, like `secret:` and `resolver:`. So a template that uses it is `per-user` or narrower, and `namespace: shared` is never allowed with it. `templatescope.Display` shows it as `${exchange:…}`. The scope table in `SPEC.md` §9 lists it in the `per-user` row.

**Validation** (the catalog's `Validator`, so every replica agrees). The `Validator` gets a new field `Exchanges []string`, the configured names. Each problem goes in the `Valid` condition, sorted, like today's variable problems:

| Problem | Reason |
|---|---|
| Not a Secret value | `<location>: ${exchange:temporal} is allowed only in the data or stringData values of a Secret` (the existing message, with `exchange:` added to `isSecretVariable`) |
| Not the whole value | `` <location>: ${exchange:temporal} must be the whole value, with no other text `` |
| Name not configured | `` <location>: token exchange `temporal` isn't configured `` |
| Scope too broad | The existing scope reason, for example `` `scope: shared`, but `resources[0].stringData.token` uses `${exchange:…}`, which needs `per-user` or narrower `` |

An invalid template is handled as today: it isn't offered, and running instances keep their generation.

### Who can start it

Only identities of kind `user` (`auth.KindUser`) have a token to exchange. Service accounts (pod sign-in) and SPIFFE workloads have none.

- **Enable.** Phase 1's enable step 2 gets a second check, next to the secret check. If the current template generation uses `${exchange:…}` and the identity's kind isn't `user`, the enable is refused with reason `no-user-token`. It writes `server.enable` (refuse, with that reason) and returns: "`<server>` needs a token exchanged from your sign-in. Service accounts, SPIFFE workloads and pod sign-in have none, so they can't use it."
- **A call that enables, or starts an instance.** The same check runs before the transaction. The refusal is a `tool.decision` with reason `no-user-token` and the same message.
- **Resume.** A server that the check refuses is dropped with `server.disable` and reason `no-user-token`, as resume drops a refused server today.
- **Leader.** If the render session's snapshot (`sessions.kind`) isn't `user`, a render that needs an exchange fails the instance at once with the same message. This covers a template that adds an exchange while a non-user identity has the server enabled.

Attested user sessions, connectors and `TOOLGATE_TOKEN_COMMAND` tokens are user identities. Their bearer token is the subject token. The IdP decides whether it accepts that token for exchange (§7, the Authentik setup).

### Where the user's token comes from

`auth.Identity` holds no token, and it must never get one, because the `sessions` row stores a JSON snapshot of it. The raw bearer token travels like this:

- `auth.Middleware` puts the token that it verified in the request's context with `auth.WithSubjectToken(ctx, token)`, only for an identity of kind `user`. `auth.SubjectTokenFrom(ctx)` reads it. The reader can't be named `SubjectToken`, because Go doesn't allow a function and a type of the same name in one package.
- The value has a new type, `auth.SubjectToken`, a string type. Its `String`, `GoString` and `LogValue` methods return `[redacted]`, and its `MarshalJSON` returns an error. So a format verb, a `slog` attribute or a JSON encoding never prints it. Only `internal/credentials` calls its `Reveal()` method, to build the exchange request.
- Tool handlers don't read the request's context. Per-request data reaches them in `requestContext`, which travels in the go-sdk's `TokenInfo.Extra` (`internal/gateway/mcp.go`). So `mcpEndpoint.ServeHTTP` copies the value into a new field, `requestContext.subjectToken`. Code that has the HTTP request itself, such as resume, reads `auth.SubjectTokenFrom(request.Context())`.
- An exchange uses only the token of the request that it serves. `sessionTools.setCurrent` and `acquireStateless` keep a `requestContext` after its request ends, so they store it with `subjectToken` cleared.
- The background refresh (§3, "Refresh") copies only the `auth.SubjectToken` value into its goroutine. The goroutine ends after the exchange's `timeout`.

The token is never a field of `Identity`, never a `slog` attribute, and never in a row, an audit event, a span or a notification.

### The exchange request

A new package, `internal/credentials`, makes the request with `golang.org/x/oauth2/clientcredentials`, which is already in `go.mod`. No other code builds an OAuth request.

- For each exchange, the code builds a new `clientcredentials.Config`:
  - `ClientID`, the client secret from the file, and `TokenURL`;
  - `Scopes: strings.Fields(scope)`;
  - `AuthStyle: oauth2.AuthStyleInHeader` (`client_secret_basic`). The style is fixed, so the subject token is never sent twice by auto-detection;
  - `EndpointParams`: `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, `subject_token=<the request's bearer token>`, `subject_token_type=urn:ietf:params:oauth:token-type:access_token`, and `audience` and `requested_token_type` when configured.

  `clientcredentials` lets `EndpointParams` replace `grant_type`, and says so in its source. The request never has `resource` or `actor_token`. Authentik refuses `resource` with `invalid_target`.
- It calls `Config.Token(ctx)` once. `Token` makes a new token source for each call, so nothing is cached between exchanges.
- `ctx` carries `oauth2.HTTPClient`: an `http.Client` with the entry's `timeout`, its `caFile` roots, TLS 1.2 or later, and `CheckRedirect` returning `http.ErrUseLastResponse`. Redirects aren't followed, because the request carries two credentials. The library reads at most 1 MiB of the answer.

**The answer.**
- `access_token` must not be empty, and must be at most 4 KiB, so it fits a notification (§3, "Delivery to the leader").
- If `requestedTokenType` is set, `issued_token_type` (`Token.Extra`) must equal it.
- **Expiry.** It is the earlier of two times: the time from `expires_in` (`Token.Expiry`), and the `exp` claim when the token is a JWT. The claim is read without verification with `github.com/go-jose/go-jose/v4/jwt` (`ParseSigned`, then `UnsafeClaimsWithoutVerification`), which is already in `go.mod`. If there is neither, the exchange fails. The expiry must be at least 2 minutes after the answer, like a resolver's `expiresAt`. toolgate never checks the token's signature. It passes the token through to the instance, and only uses `exp` to plan the refresh and the stop.
- **Issued at** is the gateway's clock when the answer arrived.

**Errors.** An error names the exchange and a cause, and never a token, a client secret or the answer's body. The code never logs or returns the `Error()` of an `oauth2.RetrieveError`, because it holds the body.

| Cause | Reason text |
|---|---|
| OAuth error answer | `` Token exchange `temporal` failed: the token endpoint answered `invalid_grant`. `` The code is `RetrieveError.ErrorCode` when it matches `^[a-z_]{1,64}$`. Otherwise the reason gives the HTTP status, as in the next row. |
| Other HTTP status | `` Token exchange `temporal` failed: the token endpoint answered HTTP 502. `` |
| Timeout | `` Token exchange `temporal` failed: the token endpoint didn't answer within 10s. `` |
| Network error | `` Token exchange `temporal` failed: the token endpoint couldn't be reached. `` |
| Bad answer | `` Token exchange `temporal` failed: the answer has no access token `` (or: no expiry, an expiry less than 2 minutes away, a token larger than 4 KiB, a different `issued_token_type`). |
| Client secret | `` Token exchange `temporal` failed: its client secret file can't be read. `` |

The `error_description` goes only to the gateway's log, at level warning, with control characters removed and cut to 256 bytes. It never goes into a reason, a row or an audit event.

### Delivery to the leader

The replica that ran an exchange sends the token with `pg_notify('toolgate_credentials', <json>)`:

```json
{"v": 1, "instance": "<instance id>", "exchange": "temporal", "principal": "<owner principal>",
 "issuedAt": "<RFC 3339 nano>", "expiresAt": "<RFC 3339 nano>", "ciphertext": "<base64>"}
```

- `ciphertext` is `secrets.Cipher.Encrypt(principal, name, token)` with `name = "credential/<instance>/<exchange>/<issuedAt>/<expiresAt>"`. A user secret name can't contain `/`, so this name can never equal a user secret's. The associated data binds the token to the instance, the exchange, the principal and both times. A payload with any of them changed doesn't decrypt.
- Every replica listens. A replica that doesn't run the reconciler ignores the payload and never decrypts it.
- The leader decrypts it and checks that the instance is live, that its `owner_principal` is `principal`, and that it has an `instance_exchanges` row for `exchange`. Otherwise it drops the payload and logs the instance and exchange at level info.
- The leader keeps the token in memory, keyed by (instance, exchange), only until it applies it, and at most 2 minutes. It keeps only the newest `issuedAt` for a key. Then it enqueues a pass.
- A payload over 7 KiB is never sent. The 4 KiB token limit keeps every payload under it.
- A delivery can be lost: there is no leader, the leader changes, or the leader dies. Nothing retries the send. The claim expires (§3, "Claims"), and the user's next request exchanges again.

Any role that can connect to the database can `LISTEN` on the channel. So the token is encrypted, and only the gateway has the key. The payload never enters a table or a log, and PostgreSQL drops it once every listener has read it.

### State

One additive migration, `00017_instance_credentials.sql`, holds the tables of §3 and §4. The five 0.4.12 designs take fixed numbers, in the recommended build order: `00015_upstream_sessions`, `00016_workspace_keys`, `00017_instance_credentials`, `00018_tool_result_cache` and `00019_persistent_volumes`. If master already has this number when the PR rebases, take the next free number and say so in the PR.

`instance_exchanges`, one row for each exchange that an instance uses:

| Column | Meaning |
|---|---|
| `instance_id` | References `instances.id`, `ON DELETE CASCADE`. Primary key with `exchange`. |
| `exchange` | The configured name. |
| `targets` | `jsonb`, a list of `{"secret", "key"}`: the rendered Secret name and key of each value that uses this exchange. Null until the render. |
| `issued_at`, `expires_at` | The times of the token in the Secret now. Null until the first apply. |
| `refresh_after` | When a request must exchange again. |
| `wanted_since` | Set while the instance waits for a first token to render. Null otherwise. |
| `claimed_at` | When a request claimed the exchange. Null when nobody holds a claim. |
| `last_error`, `last_error_at` | The reason of the latest failed exchange (§3, "Errors"), never a token. |

`instances` gains two columns:
- `uses_exchange boolean NOT NULL DEFAULT false`: the instance's generation uses `${exchange:…}`. The request that inserts the row sets it from the template. The leader sets it again from the generation it renders, in the `pending` to `starting` write.
- `credential_expired boolean NOT NULL DEFAULT false`: the instance stopped because an exchanged token expired (§3, "Expiry").

The token itself is never in PostgreSQL, a log, an audit event, a span, a metric or a `Valid` condition.

### Claims

A claim makes sure that one exchange runs at a time for each (instance, exchange). A request claims with one statement:

```sql
UPDATE instance_exchanges AS e SET claimed_at = now()
FROM unnest(@exchanges::text[], @claim_timeouts::interval[]) AS t (exchange, claim_timeout)
WHERE e.instance_id = @instance_id AND e.exchange = t.exchange
  AND e.refresh_after <= now()
  AND (e.claimed_at IS NULL OR e.claimed_at < now() - t.claim_timeout)
RETURNING e.exchange
```

The two arrays list each configured exchange and its own claim timeout: the exchange's `timeout` plus 30 seconds. So each row expires after the timeout of its own exchange. A claim of a replica that died, or whose delivery was lost, expires after this time. A row of an exchange that is no longer configured is never claimed (X16). The request runs one exchange for each row returned.

A request claims only when all of these hold:
- the instance is live and `uses_exchange` is true;
- the caller's principal is the instance's `owner_principal`. Scope keys already make this true for every request that reaches the instance. The check is there so that a mistake can never send one user's token to another user's instance;
- the caller is of kind `user`, and its token's `exp` is in the future.

### Start

1. The request that inserts a `pending` row (enable, a call that enables or starts, resume) does so in its existing transaction. If the current generation uses `${exchange:…}`, the transaction also inserts one `instance_exchanges` row for each exchange, with `refresh_after = now()`, `wanted_since = now()` and `claimed_at = now()`. So the inserting request holds the claim. If the generation also has `persistentVolumes`, the rows are inserted as "Start order with both features" says instead, and the request holds no claim.
2. After the commit, that request runs the exchanges and delivers the tokens, if it holds the claim. It does this before it waits. A resume, which doesn't wait, also does this.
3. The leader's `startPending` for a row with exchanges, at the place that "Start order with both features" gives it:
   - It makes the rows match the generation it renders. It inserts a row with `refresh_after = now()` and `wanted_since = now()` for each exchange the generation uses and that has no row. It deletes the rows of exchanges it doesn't use. It sets `wanted_since = now()` and `refresh_after = now()` on each row that has no token in memory and no `wanted_since`. If this changed a row, it notifies `instance:<id>` in the same transaction.
   - If a row has `last_error_at` at or after its `wanted_since`, it fails the instance (`pending` to `failed`) with that row's `last_error`.
   - If the leader holds a token in memory for every row, it renders with them, applies (Secrets first, §2), and in the `pending` to `starting` write sets each row's `targets`, `issued_at`, `expires_at`, `refresh_after` (the half-life, below), `wanted_since = null` and `claimed_at = null`. Then it forgets the tokens.
   - Otherwise the row stays `pending`. If `startTimeout` has passed since `attempt_started_at`, it fails with "No exchanged token for `temporal` arrived within 5m0s. Enable `<server>` again to retry."
4. Each request that waits on the instance (an enable, a call waiting for a `pending` or `starting` instance) runs the claim at each wake, including the 10 s tick and each `instance:<id>` notification. So a lost delivery is retried after the claim expires, while someone waits.

The exchange happens at most once for each claim. A failure writes `last_error` and `last_error_at`, sets `claimed_at = null`, writes `credential.exchange` (failure) and notifies `instance:<id>`, all in one transaction. The leader then fails the row (step 3). The waiting enable returns the reason, as for any failed start.

### Start order with both features

This section is the same in `instance-credentials.md` and `persistent-volumes.md`. A change to one copy must change the other.

`Starter.startPending` runs these steps in this order for an in-cluster `pending` row. A template that uses only one of the two features skips the steps of the other. A step that waits leaves the row `pending` and returns, and the next pass starts again at step 1. Each step is safe to repeat.

1. **Volumes.** `persistent-volumes.md` §4, steps 1 to 3: claim the volumes, check the StorageClasses of new volumes, and check the recorded PVs. These steps read only PostgreSQL and the API server, and they render nothing.
2. **Exchanges.** `instance-credentials.md` §3, "Start", step 3. It runs only after step 1 claimed every volume and found no wait.
3. **Render.** `Render`, with the delivered tokens.
4. **Volumes.** `persistent-volumes.md` §4, steps 4 to 6: the namespace, the pre-bind and the PVCs.
5. **Apply.** `applyAll`, Secrets first, then the `pending` to `starting` write (`MarkStarting`), which also writes the exchange rows.

**Exchange rows of a template with volumes.** If the generation has both `${exchange:…}` and `persistentVolumes`, the request that inserts the `pending` row inserts its exchange rows with `refresh_after = 'infinity'`, and with `wanted_since` and `claimed_at` null. It runs no exchange after the commit. So no request exchanges while the row waits for a volume, and no delivered token waits in the leader's memory for a volume. At step 2, the leader sets `wanted_since = now()` and `refresh_after = now()` on each row that has no `wanted_since`, and notifies `instance:<id>` in the same transaction. Each waiting request wakes on that notification, claims and exchanges (`instance-credentials.md` §3, "Start", step 4).

**One timeout.** A row that waits at step 1 or step 2 fails when `startTimeout` has passed since `attempt_started_at`. The insert sets `attempt_started_at`, and `SetInstancePending` resets it, so a row that the sweep sends back to `pending` gets a full `startTimeout` again. The reason names the wait that the row is in when the time passes:
- at step 1: "Timed out waiting for volume `<name>`: `<wait reason>`";
- at step 2: "No exchanged token for `<exchange>` arrived within `<startTimeout>`. Enable `<server>` again to retry."

### Refresh

**Half-life.** After each apply, `refresh_after = issued_at + (expires_at − issued_at) / 2`.

**Trigger.** A call forwarded to a `ready` instance with `uses_exchange` runs the claim after its `tool.decision` commits. A call that the tool-result cache serves runs the claim too, after its `tool.decision` commits (`tool-result-cache.md` §5, step 3). A cache hit is a call by an active user, so it keeps the token fresh. If the claim returns rows, the replica runs those exchanges in the background, with a copy of this request's token and a context that keeps no cancellation from the request and ends after the `timeout`. The call is forwarded at once with the token already in the Secret, so a refresh never delays a call. Waits run the claim too (§3, "Start", step 4). No other request refreshes: a request must reach the instance.

**Apply.** When the leader receives a token for a `starting` or `ready` instance, on its next pass it:
1. drops the token if its `issuedAt` isn't later than the row's `issued_at`, which drops an old or repeated delivery;
2. patches each of the row's `targets` with the token (§2, "Updates"). If a patch fails, it logs the instance, Secret and key, keeps the row as it is, and forgets the token. The claim expires and a later call exchanges again;
3. in one transaction, updates the row with a compare-and-set on the old `issued_at`: new `issued_at`, `expires_at` and `refresh_after`, and `claimed_at`, `last_error` and `last_error_at` set to null. Then it forgets the token.

The instance is not replaced and no pod restarts. A server that reads the file again on each use, such as temporal-mcp, gets the new token. The kubelet updates a mounted Secret volume within about a minute. Refreshing at half-life leaves time for that.

**Failed refresh.** The request writes `last_error` and `last_error_at`, sets `claimed_at = null` and `refresh_after = least(now() + 60 s, expires_at)`, and writes `credential.exchange` (failure), in one transaction. The token in the Secret stays until it expires. The next call after 60 seconds tries again. A failed refresh never fails or stops the instance by itself.

### Expiry

`StopDecider.stopReason` gets one more condition. It runs after the access check and before the check of enabling sessions, so it applies even while a session has the server enabled. The condition holds when a row of the instance has `expires_at <= now()`. Its reason is "its exchanged token `temporal` expired". The stop write also sets `instances.credential_expired = true`.

So an instance whose token nobody refreshed stops within one 30 s pass of the expiry. toolgate never extends a token without a user's token behind it.

**The next request.** The session keeps the server enabled. Phase 1's call step 2 and phase 4's lazy start insert a new instance only in some cases. One case is added: a call to an enabled server with no live instance, whose newest row has `credential_expired`, inserts a new `pending` row as a lazy start does, and waits for it. This also applies to `start: onEnable` servers. The new row exchanges at once (§3, "Start"). An enable starts a new instance as it does today.

### Replacements and repairs

A restart from the web UI, a resolver-expiry replacement, a `replace` repair, and the sweep that sends an instance with missing objects back to `pending` all render again. Each needs a fresh token, and the leader holds none, because it can't read the Secret and keeps no token.

- A new or re-pending row waits for a token, as in §3, "Start", step 3. The sweep's write sets `wanted_since = now()`, `refresh_after = now()` and `claimed_at = null` on the instance's rows. For a template with `persistentVolumes`, it sets `wanted_since` and `claimed_at` to null and `refresh_after = 'infinity'` instead, and the leader opens the rows after the volume steps, as "Start order with both features" says.
- A `reapply` repair of an instance with `uses_exchange` replaces the instance instead, with reason `health-check`, as `reapply` does today for an outdated instance.
- The owner's next request delivers the token. If none arrives within `startTimeout`, the row fails with the reason in step 3.

### Audit

The replica that ran an exchange writes one `credential.exchange` event for each exchange, in the same transaction as its row update. The event has the usual identity fields and the server, and these details:

| Detail | Value |
|---|---|
| `instance` | The instance ID. |
| `exchange` | The configured name. |
| `kind` | `start` or `refresh`. A start is an exchange for a row with `wanted_since` set. |
| `outcome` | `success` or `failure`. |
| `error` | On failure: the OAuth error code, or one of `http-status`, `timeout`, `unreachable`, `bad-answer`, `client-secret`. |
| `expiresAt` | On success: the expiry. |

It never holds a token, a client secret or an `error_description`. The web UI's audit summary labels it "Credential exchanged".

### Metrics

`toolgate_token_exchanges_total{exchange, kind, outcome}`, a counter on every replica. `exchange` is a configured name, at most 8. `kind` is `start` or `refresh`. `outcome` is `success`, `refused` (an OAuth error answer) or `error` (anything else). That is at most 48 series for each replica. `internal/metricsbudget` lists the labels.

### What Authentik puts in the exchanged token

Issue #611 asked which issuer and signing key Authentik uses when `audience` names another provider. The target (Temporal) trusts only the Temporal provider's JWKS. Authentik's documentation for token exchange (`docs.goauthentik.io/add-secure-apps/providers/oauth2/token_exchange/`) says:

- "authentik signs the token using the target provider's signing key."
- "The token uses the target provider's issuer as `iss` and its `client_id` as `aud`."
- `audience` must be the target provider's `client_id`, and only one value is accepted.
- "If `scope` is omitted, the issued token has no scopes."
- The user must pass the target application's policy bindings. Otherwise the request fails with `invalid_grant`.
- The answer has `access_token`, `token_type` (`Bearer`), `expires_in`, `issued_token_type` and `scope`.

toolgate doesn't depend on this answer. It never verifies the exchanged token and never reads its issuer or key. It only passes the token to the instance and reads `exp` to plan. So if Authentik signs differently, only the target refuses the token, and toolgate needs no change.

**Deploy-time check for the owner.** This is not a gate for the design or the build. The design was not tested against a live Authentik. Before the homelab uses the Temporal template:
1. Exchange one token by hand against the live Authentik with the configuration in §7.
2. Decode the token. Make sure that `iss` is the Temporal provider's issuer, that the `kid` is in the Temporal provider's JWKS, and that `permissions` holds the user's permission.
3. If any of these fail, change the Authentik configuration. toolgate needs no change.

## 4. Template secrets

### Configuration and RBAC

Chart value `config.templateSecretsNamespace`, a string, default empty. The chart sets the gateway setting `templateSecretsNamespace` (`TOOLGATE_TEMPLATE_SECRETS_NAMESPACE`) from it. Empty means the feature is off.

Rules, checked by the chart and again by the gateway at start:
- It is a DNS label.
- It doesn't start with `tg-`.
- It isn't the gateway's own namespace (`leaseNamespace`), which holds the gateway's keyset, database credentials and client secrets.
- It isn't `config.sharedNamespace.name`, where instances run.
- It isn't `kube-system`.
- It isn't one of `catalogNamespaces`. Template authors write there, and the Secrets there are not meant for every template.

The admin creates the namespace and its Secrets, for example with Flux and SOPS. The chart doesn't create it. The namespace must not have the label `toolgate.soliddowant.dev/writable`, so the admission policy refuses every gateway write in it. The admin docs say so.

When it is set, the chart adds:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: <fullname>-template-secrets
  namespace: <templateSecretsNamespace>
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "watch"]
---
# RoleBinding <fullname>-template-secrets in the same namespace, to the gateway's service account.
```

The ClusterRole doesn't change: it still has only `create`, `patch` and `delete` on Secrets. So the gateway can read Secrets in this namespace only. Kubernetes RBAC enforces this, not the gateway. The comment "The gateway never reads Secrets" in `rbac.yaml` changes to say that it reads Secrets only in the template secrets namespace. `list` is in the Role because the watch in §4, "Keeping copies current", lists before it watches (§8, choice 15).

### Template syntax, scope and validation

`${templateSecret:NAME/KEY}`. `NAME` is the Secret's name in the template secrets namespace and `KEY` is a key of its `data`. There is no way to name another namespace. The text splits at the first `/`. It follows the rules of §2.

**Scope.** It doesn't narrow the scope: `templatescope.ForVariable` returns `shared` for it. A `shared` template can use it, also with `namespace: shared`.

**Validation.** The `Validator` gets a new field, `TemplateSecretsNamespace`. Each problem goes in the `Valid` condition:

| Problem | Reason |
|---|---|
| Not a Secret value | `<location>: ${templateSecret:keys/api-key} is allowed only in the data or stringData values of a Secret` |
| Not the whole value | `` <location>: ${templateSecret:keys/api-key} must be the whole value, with no other text `` |
| No namespace configured | `` <location>: ${templateSecret:…} needs a template secrets namespace, and the gateway has none (`config.templateSecretsNamespace`) `` |
| Bad form | `` <location>: ${templateSecret:…} must be NAME/KEY, with a Secret name and a key `` (a name that isn't a DNS-1123 subdomain, or a key that doesn't match `^[-._a-zA-Z0-9]{1,253}$`) |

Validation never reads the Secret. A Secret can be created after the template.

### Render at start

The leader reads each named Secret once for each render. It uses an uncached `get` through the API (controller-runtime's `APIReader`), not the watch cache, so a render never uses an old value. Rendering finishes before anything is applied, and Secrets are applied first (§2). So a pod never starts without the value.

**Failures** fail the render (`pending` to `failed`) with the usual "The template didn't render:" reason. The `VariableError` names the location, the variable, the Secret and the key, never the value:

| Cause | Reason ends with |
|---|---|
| Secret missing | `` could not be read: Secret `keys` doesn't exist in `toolgate-template-secrets` `` |
| Key missing | `` could not be read: Secret `keys` has no key `api-key` `` |
| Not UTF-8 under `stringData` | `` could not be read: the value of key `api-key` in Secret `keys` isn't UTF-8 text; use it under `data` `` |
| API error, such as RBAC | `` could not be read: the API server refused to read Secret `keys`: <API server message> `` |

### State

The same additive migration adds `instance_template_secrets`, one row for each rendered value:

| Column | Meaning |
|---|---|
| `instance_id` | References `instances.id`, `ON DELETE CASCADE`. Primary key with `target_secret` and `target_key`. |
| `target_secret`, `target_key`, `target_field` | The rendered Secret name, its key, and `data` or `stringData`. |
| `source_namespace` | The template secrets namespace that the render read the value from. |
| `source_name`, `source_key` | The Secret and key in the template secrets namespace. Indexed on `source_name`. |
| `source_version` | The `resourceVersion` of the source Secret that was last written into the target. |
| `last_error` | Why the latest update failed, or null. Names only, never a value. |

The leader writes the rows in the `pending` to `starting` write, and again after a `reapply`. This is how toolgate knows which instances used which Secret. Values are never stored.

### Keeping copies current

**The watch.** The leader runs one controller-runtime cache for `Secret` objects in the template secrets namespace only. It is added to the manager as a runnable that needs leader election, so other replicas never hold these values. Its transform removes `managedFields`. Each event on a Secret enqueues a pass of the runtime reconciler.

**The update step.** A new step of the full pass runs for each live instance in `starting` or `ready` that has rows. When a namespace is configured, it runs after the cache has synced. When none is configured, it runs with no cache, and only step 1 applies:
1. If no namespace is configured, or `source_namespace` isn't the configured namespace, keep the target as it is. Set `last_error` to "the template secrets namespace changed", log a warning when `last_error` changes, count `missing`, and go to the next row. A Secret of the same name in another namespace is never copied into an instance that was rendered from the old one.
2. For each other row, read the source from the cache.
3. If the Secret or the key is missing, keep the target as it is. Set `last_error` (for example "Secret `keys` was deleted"), log a warning when `last_error` changes, and count `missing`.
4. If `resourceVersion` equals `source_version`, do nothing.
5. Otherwise, check UTF-8 for a `stringData` target, as at render. Patch the target key (§2, "Updates"), one patch for each target Secret with all its changed keys. Then set `source_version` to the new version and `last_error` to null, with a compare-and-set on the old `source_version`.
6. If the patch fails, keep the row, log the instance and the target, and count `error`. The next pass tries again.

The step is level-based. It compares versions on every pass, so a missed event, a leader change, or a change during a render is caught by the next pass. The full pass runs the steps of one instance one after another, so an update never runs at the same time as that instance's render, reapply or teardown.

Nothing is restarted. Restarting workloads is left to the cluster's tools. The admin docs say how (§7).

### Metrics

`toolgate_template_secret_updates_total{outcome}`, with `outcome` one of `updated`, `missing` and `error`. Only the leader counts. That is 3 series.

## 5. Interaction tables

Phase 1 to phase 4 rows still hold. Each row is one event combined with one operation already in flight, in the order PostgreSQL commits them. Each row's covering test proves its rule. Tests marked `-count=50` pass with `-count=50 -race`.

### Token exchange

| # | Event | In flight | Rule | Test |
|---|---|---|---|---|
| X1 | Exchange succeeds at start | enable waiting | The leader renders with the token, applies Secrets first, and the instance moves to `starting`. The enable returns when it is `ready`. | `internal/gateway: TestEnableRendersExchangedTokenBeforeOtherObjects` |
| X2 | Exchange fails at start | enable waiting | The request writes `last_error` and `credential.exchange` (failure). The leader fails the row with the reason. The enable returns it. | `internal/gateway: TestEnableFailsWithExchangeErrorCode` |
| X3 | Two sessions of one user start a per-user server | each other | The advisory lock gives one row. The inserting request holds the claim, and the other waits on the same row. One exchange. | `internal/gateway: TestConcurrentStartsExchangeOnce` (`-count=50`) |
| X4 | Delivery lost (no leader, leader change, leader died) | instance `pending`, enable waiting | The claim expires after `timeout` + 30 s. The waiting enable's next tick claims and exchanges again. | `internal/gateway: TestLostDeliveryIsRetriedByWaitingRequest` (`-count=50`) |
| X5 | Delivery lost | instance `pending`, nobody waiting (resume) | The row waits. A later call waits on it and claims. With no request, the row fails after `startTimeout` with the "No exchanged token" reason. | `internal/runtime: TestPendingExchangeTimesOut` |
| X6 | Subject token expired | waiting request ticks | A request never claims with an expired token. Another request of the user claims. | `internal/credentials: TestClaimSkipsExpiredSubjectToken` |
| X7 | Token passes half-life | call in flight | The call is forwarded at once. The replica claims and exchanges in the background. The leader patches the Secret. The MCP session and the instance survive. | `internal/gateway: TestRefreshAtHalfLifeKeepsSessionAndInstance` |
| X8 | Many calls after half-life | each other | One claim statement returns the row. The others forward without exchanging. | `internal/gateway: TestOneRefreshPerHalfLife` (`-count=50`) |
| X9 | Refresh fails | later calls | The Secret keeps the current token. `last_error`, `credential.exchange` (failure), and a retry 60 s later, at most until the expiry. | `internal/gateway: TestFailedRefreshKeepsTokenUntilExpiry` |
| X10 | Token expires with no refresh | reconcile pass | The stop condition holds even with sessions enabling the server. The instance stops with `credential_expired`. | `internal/runtime: TestExpiredExchangeStopsEnabledInstance` |
| X11 | Token expires | call in flight | The call gets the upstream result or error. The stop goes on. The next call inserts a new row, waits, and the new row exchanges. | `internal/gateway: TestCallAfterExpiryStopStartsNewInstance` |
| X12 | Delivery for an instance that is not live, or not the owner's | leader receives it | Dropped, and logged at level info with the instance and exchange. | `internal/runtime: TestDeliveryDroppedForStoppedOrForeignInstance` |
| X13 | Older delivery arrives after a newer apply | leader | `issuedAt` isn't later than `issued_at`, so it is dropped. | `internal/runtime: TestRefreshDropsOlderDelivery` |
| X14 | Template changed | instance running with an exchange | It keeps its generation. Refresh patches the `targets` from its render and never reads the template. | `internal/runtime: TestRefreshUsesRenderedTargets` |
| X15 | Template changed | instance `pending`, waiting for a token | The leader renders the current generation and makes the rows match it. | `internal/runtime: TestPendingExchangeRowsFollowRenderedGeneration` |
| X16 | Exchange removed from the gateway configuration (restart) | instance running | The template becomes invalid. Running instances keep their generation. No refresh can run, so the instance stops at the expiry. | `internal/runtime: TestUnconfiguredExchangeStopsAtExpiry` |
| X17 | Restart, resolver-expiry or `replace` repair | instance with an exchange | The new row waits for a token from the owner's next request, or fails after `startTimeout`. | `internal/runtime: TestReplacementWaitsForExchangedToken` |
| X18 | `reapply` repair due | instance with an exchange | It replaces the instance instead. | `internal/runtime: TestReapplyReplacesInstanceWithExchange` |
| X19 | Objects missing after a leader change | instance `ready` with an exchange | The sweep sends it to `pending` and marks its rows wanted. It continues as X17. | `internal/runtime: TestSweepMarksExchangeWanted` |
| X20 | Instance fails, stops or is replaced | refresh in flight | The delivery is dropped (X12). A patch to a deleted namespace fails and is logged. | `internal/runtime: TestDeliveryDroppedForStoppedOrForeignInstance` |
| X21 | Leader change | token applied, refresh not due | Nothing is lost. The token is in the Secret, and the new leader reads `expires_at` from the row for the stop. | `internal/runtime: TestNewLeaderStopsOnRowExpiry` |
| X22 | Replica dies during an exchange | claim held | The claim expires. The next call claims. The dead request writes no audit event. | `internal/credentials: TestExpiredClaimCanBeTakenAgain` |
| X23 | User leaves the group the IdP checks | instance running | The next refresh fails with `invalid_grant` (X9). The instance stops at the expiry. A new start fails with that reason. | `internal/gateway: TestRefusedRefreshThenRefusedStart` |
| X24 | Service account, SPIFFE workload or pod sign-in enables or calls | — | Refused with `no-user-token` before any row is written. | `internal/gateway: TestExchangeTemplateRefusesNonUserIdentities` |
| X25 | Template gains an exchange | non-user identity has it enabled | Its next start is refused (X24). A leader render for a non-user snapshot fails at once. | `internal/runtime: TestNonUserSnapshotFailsExchangeRender` |
| X26 | Two users enable the server | each other | Two rows, two claims, two tokens. The leader checks `owner_principal` against the payload, and the associated data binds the principal. | `internal/gateway: TestTwoUsersGetTheirOwnTokens` |
| X27 | Session ends or disable | refresh in flight | Phase 1's stop rules run. A late delivery is dropped (X12). | `internal/runtime: TestDeliveryDroppedForStoppedOrForeignInstance` |
| X28 | Secret patch fails at refresh | — | The row keeps its times and the claim expires. A later call exchanges again. | `internal/runtime: TestRefreshPatchFailureKeepsRow` |
| X29 | Client secret rotated | exchanges | The file is read for each exchange, so the next exchange uses the new secret. | `internal/credentials: TestClientSecretReadForEachExchange` |
| X30 | Database role other than the gateway `LISTEN`s | deliveries | It gets only ciphertext. | `internal/credentials: TestDeliveryPayloadHoldsNoToken` |
| X31 | Token passes half-life | a call that the tool-result cache serves | The hit runs the claim after its `tool.decision` commits, as a forwarded call does. The replica refreshes in the background, and the MCP session survives. | `internal/gateway: TestCacheHitRefreshesExchangedToken` |
| X32 | Template has `${exchange:…}` and `persistentVolumes` | restart while the old instance drains its volume | The new row's exchange rows stay closed (`refresh_after = 'infinity'`), so nobody exchanges during the volume wait. When the volume is free, the leader opens them and notifies. A waiting request exchanges, and the leader renders within the token's 2 minutes in memory. With no waiting request, the row fails after `startTimeout` from `attempt_started_at`, with the reason of the wait it is in. | `internal/runtime: TestStartOrderWithVolumesAndExchanges` (`-count=50`) |
| X33 | Two exchanges with different `timeout` values | a dead claim on each | Each row's claim expires after its own exchange's `timeout` plus 30 s. | `internal/credentials: TestClaimUsesEachExchangesTimeout` |

### Template secrets

| # | Event | In flight | Rule | Test |
|---|---|---|---|---|
| T1 | Source Secret missing | start | The render fails with a reason that names the Secret. | `internal/runtime: TestTemplateSecretMissingFailsStart` |
| T2 | Key missing | start | The render fails with a reason that names the Secret and the key. | `internal/runtime: TestTemplateSecretMissingFailsStart` |
| T3 | Source created after a failed start | next enable | The render reads through the API, not the cache, so the new start succeeds at once. | `internal/runtime: TestTemplateSecretReadThroughAPIAtRender` |
| T4 | Source changed | instance `ready` | The next pass patches the target and records the version. Nothing restarts. | `internal/runtime: TestTemplateSecretChangeUpdatesRunningInstances` |
| T5 | Source changed | render in flight | The render reads either version and records it. The next pass compares with the cache and patches if it differs. | `internal/runtime: TestTemplateSecretUpdateIsLevelBased` (`-count=50`) |
| T6 | Source changed | instance `starting` | Patched like a `ready` one. | `internal/runtime: TestTemplateSecretChangeUpdatesRunningInstances` |
| T7 | Source changed twice before a pass | — | The pass writes the latest value only. | `internal/runtime: TestTemplateSecretUpdateIsLevelBased` |
| T8 | Source Secret deleted, or key removed | instances running | The copies stay. `last_error` is set, a warning is logged once, and `missing` is counted. New starts fail (T1). | `internal/runtime: TestTemplateSecretDeletedKeepsCopies` |
| T9 | Source created again | instances with `last_error` | The version differs, so the next pass patches and clears `last_error`. | `internal/runtime: TestTemplateSecretDeletedKeepsCopies` |
| T10 | Template changed | instance running | It keeps its generation and its rows, so it keeps getting updates. | `internal/runtime: TestTemplateSecretUpdateIgnoresTemplateChanges` |
| T11 | Instance stops, fails or is replaced | update in flight | The step runs only for live rows. A patch into a deleted namespace fails and is logged. The rows go with the instance row. | `internal/runtime: TestTemplateSecretUpdateSkipsStoppedInstances` |
| T12 | Leader change | events missed | The new leader's first pass after its cache syncs compares every row. | `internal/runtime: TestNewLeaderCatchesUpTemplateSecrets` |
| T13 | Cache not synced yet | pass | The update step waits for the sync. Renders read through the API and don't wait. | `internal/runtime: TestNewLeaderCatchesUpTemplateSecrets` |
| T14 | Role missing or wrong | start | The render fails with the API server's message. The watch logs its errors and counts `error`. | `internal/runtime: TestTemplateSecretMissingFailsStart` |
| T15 | `reapply` repair | instance using a template secret | The render reads the current value, applies it, and writes the rows again. | `internal/runtime: TestReapplyRendersCurrentTemplateSecret` |
| T16 | Two instances use one source | change | Each instance gets its own patch. | `internal/runtime: TestTemplateSecretChangeUpdatesRunningInstances` |
| T17 | Instance in the shared namespace | change | The patch goes to its Secret in the shared namespace. | `test/e2e: TestTemplateSecretSharedInstance` |
| T18 | Value isn't UTF-8 | update of a `stringData` target | The target is kept and `last_error` says why, as at render. | `internal/runtime: TestTemplateSecretUpdateChecksUTF8` |
| T19 | `templateSecretsNamespace` changed (restart) | instances rendered from the old namespace | Their targets are kept. `last_error` is "the template secrets namespace changed", and `missing` is counted. A Secret of the same name in the new namespace is never copied into them. New starts render from the new namespace. | `internal/runtime: TestTemplateSecretNamespaceChangeKeepsCopies` |
| T20 | `templateSecretsNamespace` removed (restart) | instances with rows | The update step runs with no cache. Targets are kept, and each row gets the same `last_error`. Templates that use the variable become invalid, and running instances keep their generation. | `internal/runtime: TestTemplateSecretNamespaceChangeKeepsCopies` (subtest: namespace removed) |

## 6. Tests

Unit and integration tests use a fake token endpoint: an `httptest.Server` in tests, and a small binary in `test/tokenexchange/` for e2e, like `test/resolver/`. It answers per subject with a token that holds the subject's `sub` and a counter, with a lifetime and error codes that the test sets. No test needs a live Authentik. The test MCP server reads a file from its Secret volume on each call, so a test can see the value it gets.

- `internal/config`: `TestTokenExchangesConfig` (each field rule, at most 8, `secretsKeysetFile` needed), `TestTemplateSecretsNamespaceConfig` (DNS label, `tg-`, the gateway's own, the shared, `kube-system` and each catalog namespace refused).
- `internal/auth`: `TestSubjectTokenIsNeverPrinted`: `fmt` with `%v`, `%+v` and `%#v`, a `slog` attribute and `json.Marshal`, of the value alone and of a struct that holds it, never contain the token.
- `internal/catalog`: `TestValidateExchangeVariable` and `TestValidateTemplateSecretVariable`, tables over every reason in §3 and §4. `internal/templatescope`: `TestExchangeNarrowsAndTemplateSecretDoesNot`.
- `internal/credentials`: `TestExchangeSendsTokenExchangeGrant` (form fields, Basic auth, no `resource`, no redirect followed), `TestExchangeExpiryFromExpiresInOrJWT`, `TestExchangeRefusesBadAnswers` (no token, no expiry, under 2 minutes, over 4 KiB, wrong `issued_token_type`), `TestExchangeErrorsNameCodeOnly` (no body, description or token in any error), `TestClientSecretReadForEachExchange`, `TestClaimSkipsExpiredSubjectToken`, `TestExpiredClaimCanBeTakenAgain`, `TestDeliveryPayloadHoldsNoToken`, `TestDeliveryRejectsChangedPayload` (each bound field changed).
- `internal/runtime`: the runtime rows of §5, plus `TestRenderAppliesSecretsFirst` (with a `kubernetes.io/service-account-token` Secret that keeps its position) and `TestRenderTemplateSecretEncoding` (`data` base64, `stringData` text).
- `internal/gateway` (integration): the gateway rows of §5, plus `TestNoTokenInDatabaseLogsOrConditions`. It runs a start, refreshes and failures, then searches every table (the identity snapshot of the `sessions` row included), the captured log and audit lines, and the template's conditions for the subject token, the exchanged tokens and the client secret. It also checks that the `requestContext` that `setCurrent` stores holds no subject token.
- `test/chart`: `TestChartTokenExchangesConfigFile` (written to the file, chart failures) and `TestChartTemplateSecretsRole` (a Role and RoleBinding in that namespace only, no Secret read in the ClusterRole, the namespace rules, `kube-system` and the catalog namespaces included).
- `test/e2e`: `TestTokenExchangeEndToEnd` (two users, refresh with the session kept, idle stop and start again, a refused user) and `TestTemplateSecretSharedInstance` (the value at first start, an update with no restart). `TestTemplateSecretsRBACConfinedToNamespace` runs a `SubjectAccessReview` for the gateway's service account: `get secrets` is allowed in the template secrets namespace and refused in the gateway's namespace, the catalog namespace and an instance namespace.

Tests named with `-count=50` in §5 are timing-sensitive and pass with `-count=50 -race`. They wait on conditions with deadlines, never fixed sleeps. Time-based rules (half-life, claim expiry, stop at expiry, the 60 s retry) take their clock from a `Now` field, as resolvers do.

## 7. Documentation work

- **New page `docs/admin/token-exchange.md`** (admins):
  - The configuration (§3) and an example template.
  - The Authentik setup. Make a confidential provider (for example `toolgate-token-exchange`) with the `Token exchange` grant type, and bind it to an application. List every provider whose user tokens reach the gateway (`toolgate`, `coder-toolgate`, and a connector's client) as its federated providers. List `toolgate-token-exchange` as a federated provider of the target provider (Temporal). Set `scope`, because Authentik gives no scopes without it.
  - The lifetime. The target provider's `access_token_validity` sets the token's lifetime. Recommend `minutes=10` to `minutes=30`. A shorter value stops idle instances sooner. Under `minutes=4`, less than 2 minutes remain between the refresh and the expiry, which is close to the kubelet's delay. Under 2 minutes, every exchange fails.
  - The threat model. The gateway is trusted to act for active users only. Each token is exchanged with the token of one of the user's own requests, and is no broader than the target provider gives that user. A token is never extended without a request behind it. A gateway compromise can exchange the tokens of users who send requests while it is compromised. A server can read its own token, so give the target provider only what the server needs. Agents hold user tokens too, but can't exchange them, because the client is confidential.
  - The deploy-time check of §3.
- **New page `docs/admin/template-secrets.md`** (admins):
  - How to make the namespace, set `config.templateSecretsNamespace`, and keep the `writable` label off it.
  - An example source Secret and a `shared` template that uses it.
  - The threat model. Anyone who can write a template in a catalog namespace can read every Secret in this namespace. So keep only Secrets for templates in it.
  - Moving from a Kyverno copy policy.
  - The reloader note. toolgate updates the Secret, and the kubelet updates mounted files within about a minute. Environment variables and `subPath` mounts don't change until the pod restarts. To restart on a change, use the cluster's reloader (for example an annotation that a reloader controller reads) on a Deployment. Use a Deployment rather than a bare Pod for templates that need a restart on a change.
- **`docs/admin/templates-and-rules.md`**: add `${exchange:NAME}` and `${templateSecret:NAME/KEY}` to the variables table, `${exchange:…}` to the `per-user` row of the scope table, the whole-value rule, and the `data` encoding. Link both new pages.
- **`docs/admin/security-model.md`**: one short section for each feature that links its threat model.
- **`docs/admin/metrics.md`**: the two counters.
- **Generated**: `docs/admin/configuration.md` (`tokenExchanges`, `templateSecretsNamespace`) and `docs/admin/chart-values.md` (`config.tokenExchanges`, `config.templateSecretsNamespace`), from `make generate`.
- **`docs/README.md`**: list the two new pages.

## 8. Choices made

1. **Delivery through an encrypted notification.** The issues need a token that a request gets to reach a Secret that only the leader writes, and never a PostgreSQL row. Three ways were weighed. A request that patches the Secret itself breaks phase 1's rule that only the reconciler touches Kubernetes. A direct call from a replica to the leader needs a new listener, new authentication between replicas, and a way to find the leader. A notification reuses the channel that already carries client answers between replicas (phase 1 §3). Encrypting it with the existing keyset keeps the token from any database role. The cost is a 4 KiB limit on the token.
2. **The exchange runs on the request's replica,** not the leader. So the user's token never leaves that replica, and only the exchanged token, which is meant for the instance, travels.
3. **Whole-value rule** for both variables (§2). It makes updates in place possible without the old template generation. It is stricter than the issues ask: a value like `Bearer ${exchange:x}` is not allowed.
4. **`data` holds the base64 of the value** for both new variables (§2), unlike `${secret:…}`. A template secret can be binary, and copying bytes is the clear meaning.
5. **Secrets are applied first in every render** (§2), not only for the new variables. It is harmless for other templates, and it closes the same gap for `${secret:…}`. A `kubernetes.io/service-account-token` Secret keeps its position, because its ServiceAccount must exist first.
6. **`reapply` replaces an instance with an exchange.** The leader can't read the Secret back and keeps no token.
7. **A start waits for a token up to `startTimeout`** from `attempt_started_at`, then fails. Replacements and the sweep wait the same way, because only a request can bring a token. The volume wait of `persistent-volumes.md` counts toward the same timeout ("Start order with both features").
8. **After an expiry stop, the next call starts a new instance** even for `start: onEnable` servers (§3, "Expiry"). Without it, a user of such a server must enable it again.
9. **Refresh only on forwarded calls, cache hits and waits,** in the background. Policy-refused calls, `tools/list` and other routes don't refresh. A failed refresh is retried after 60 seconds.
10. **The expiry stop applies while sessions have the server enabled.** The issue asks for a stop when nobody refreshes, and an enabled set doesn't mean a user is present.
11. **Expiry is the earlier of `expires_in` and the JWT's unverified `exp`,** at least 2 minutes away, like resolvers.
12. **Reasons give only the OAuth error code.** `error_description` comes from the IdP with no bound on its content, so it goes only to the log, cut short.
13. **`client_secret_basic` only.** Authentik supports it. A configurable style can come later.
14. **`tokenExchanges` needs `secretsKeysetFile`,** for the delivery encryption.
15. **The Role has `list` as well as `get` and `watch`.** The issue says `get`/`watch`. A controller-runtime watch lists first. `list` in the same namespace doesn't widen what the gateway can read.
16. **Namespace rules.** The template secrets namespace can't be the gateway's own namespace, the shared namespace, `kube-system` or a catalog namespace, and can't start with `tg-`. The gateway's own namespace holds the keyset and its database credentials.
17. **Only the leader watches template secrets.** Other replicas never hold their values.
18. **A deleted source keeps the copies** in running instances. Deleting values from running servers is a surprise, and new starts already fail.
19. **`SPEC.md` already matches this design.** §9, "Resources", says that the gateway reads Secrets only in the template secrets namespace. The connector paragraph of §6 says that the only user credential an upstream server can get is one exchanged for it with `${exchange:…}`. No `SPEC.md` change is left for the worker.
20. **At most 8 exchanges,** to bound the metric series and the configuration.
21. **A restart from the web UI waits for the owner's next MCP request** to get a token. The web UI's token is for another client, which the exchange provider does not have to accept.
22. **One claim statement with a timeout for each exchange.** The arrays hold the configured values, so a changed `timeout` applies after the gateway restarts, with no column to update.
23. **Changing the template secrets namespace never moves values between namespaces.** A row records where its value came from. Copying a Secret of the same name from the new namespace would put a value into an instance that its template never chose.

## 9. Not in scope

- An allow list on a source Secret that names the templates that can read it.
- `${templateSecret:…}` or `${exchange:…}` in `remote.headers`.
- Restarting workloads after a template secret or token changes.
- Showing a failed refresh or a missing source in `toolgate_server_status` or the web UI. The audit event, the row and the log hold them.
- An audit event for template secret updates.
- Actor tokens (delegation), more than one `audience`, and `resource`.
- Keeping exchanged tokens in memory beyond their apply, or refreshing with the web UI's token.
- Text around the new variables in a value (choice 3).

## 10. Acceptance

The issues' lists, unchanged. Scenarios that need the live homelab (Temporal and Authentik) are checked by the owner at deploy time. The tests named for them prove the same rule against the fake token endpoint and the test MCP server.

### Token exchange (issue #611)

- A user in "Temporal users" enables the server. temporal-mcp reads workflows with a token whose `sub` is that user's (in the Temporal provider's subject mode) and whose `permissions` is `temporal-system:read`. Temporal refuses that user's `start_workflow`.
  Owner's deploy-time check. Tests: `test/e2e: TestTokenExchangeEndToEnd`, `internal/gateway: TestEnableRendersExchangedTokenBeforeOtherObjects`.
- A user in "Temporal admins" can call `start_workflow`.
  Owner's deploy-time check. Tests: `test/e2e: TestTokenExchangeEndToEnd`.
- A user in neither group can't start the instance. The exchange fails with Authentik's error, and the reason names the exchange.
  Tests: `internal/gateway: TestEnableFailsWithExchangeErrorCode`, `test/e2e: TestTokenExchangeEndToEnd`.
- Two users get two instances with different tokens. One user's token never reaches the other's instance.
  Tests: `internal/gateway: TestTwoUsersGetTheirOwnTokens`, `internal/credentials: TestDeliveryRejectsChangedPayload`.
- Under continuous use, the exchanged token is refreshed in place before it expires, and the MCP session survives.
  Tests: `internal/gateway: TestRefreshAtHalfLifeKeepsSessionAndInstance`, `test/e2e: TestTokenExchangeEndToEnd`.
- After an idle period longer than the token's lifetime, the instance stops. The next request starts it again with a new exchange.
  Tests: `internal/runtime: TestExpiredExchangeStopsEnabledInstance`, `internal/gateway: TestCallAfterExpiryStopStartsNewInstance`.
- A service account, SPIFFE workload or pod sign-in can't start a template that uses `${exchange:…}`.
  Tests: `internal/gateway: TestExchangeTemplateRefusesNonUserIdentities`.
- No exchanged or user token appears in logs, audit events, PostgreSQL or `Valid` conditions.
  Tests: `internal/gateway: TestNoTokenInDatabaseLogsOrConditions`, `internal/credentials: TestExchangeErrorsNameCodeOnly`, `internal/credentials: TestDeliveryPayloadHoldsNoToken`.
- Docs:
  - The admin docs cover the config, the authentik setup and the threat model. In that model the gateway is trusted to act for active users only, with credentials no broader than the target provider gives that user.
  - `templates-and-rules.md` lists `${exchange:…}` with its scope rule.

  Manual: the owner reviews the pages in §7.

### Template secrets (issue #613)

- A `shared` template can render `${templateSecret:NAME/KEY}` into a Secret, and its pods see the value on their first start.
  Tests: `test/e2e: TestTemplateSecretSharedInstance`, `internal/runtime: TestRenderAppliesSecretsFirst`.
- A template naming a missing Secret or key fails to start with a reason that names the Secret and key, never the value.
  Tests: `internal/runtime: TestTemplateSecretMissingFailsStart`.
- A template can't read Secrets outside the configured namespace; the chart's RBAC enforces this, not only the gateway.
  Tests: `test/e2e: TestTemplateSecretsRBACConfinedToNamespace`, `test/chart: TestChartTemplateSecretsRole`.
- Changing the source Secret updates the rendered copy in running instances; nothing is restarted.
  Tests: `internal/runtime: TestTemplateSecretChangeUpdatesRunningInstances`, `test/e2e: TestTemplateSecretSharedInstance`.
- Docs: admin guide (how to set up the namespace, and an example), and a note on pairing with a reloader.
  Manual: the owner reviews `docs/admin/template-secrets.md`.
