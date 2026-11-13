# Registered workspace keys

This document is the design for a third source of workspace attestation: a key pair that a workspace holds and that the user registers with toolgate. It adds to `docs/design/phase-3.md` §10 ("Attested user sessions"), which still holds unless a section here says otherwise. Workers implement it and never change its rules. The operator and the owner change it. If this document does not decide a case, post the case on the issue and hand back `HANDOFF: question`.

The target is the Acceptance list at the end of this document. It is the owner's list from the issue, unchanged.

Words used in this document:

- **Workspace key:** an Ed25519 key pair. The private key stays on one machine. The gateway stores only the public key.
- **Key ID:** the RFC 7638 JWK thumbprint of the public key, SHA-256, base64url without padding (43 characters). go-jose computes it.
- **Fingerprint:** `SHA256:` followed by the key ID. People compare it. Code uses the key ID.
- **Assertion:** a short JWT that the client signs with the private key and sends in `X-Toolgate-Attestation`.
- **Key request:** a pending access request of the new kind `key`. It holds a public key that waits for approval.

## 1. Who does what

Phase 1's split stays. Request handling runs on every replica and never touches Kubernetes objects. The reconciler runs only on the Lease holder.

New request-handling work, on every replica:

- The authenticator verifies assertions. It reads one `workspace_keys` row from PostgreSQL for each request that carries an assertion. It keeps no cache.
- `POST /api/v1/workspace-keys`, `GET /api/v1/workspace-keys` and `DELETE /api/v1/workspace-keys/{name}`.
- The approval page approves key requests.
- The Workspaces page lists keys and revokes them.

New reconciler work, on the leader only:

- The 30 s cleanup pass deletes `workspace_keys` rows 7 days after they were revoked or replaced.
- Key requests expire through the existing expiry pass for access requests. That pass needs no change.

New client work: `toolgate workspace register`, `toolgate workspace keys list|revoke`, `toolgate configure --attestation-key-file`, and the new attestation source in every command that calls the gateway.

Kubernetes is not involved. The key path makes no TokenReview call and reads no pod.

## 2. State

One additive migration, the next free number, for example `00015_workspace_keys.sql`.

**Access requests.** `access_requests.kind` accepts `key` as well as `server`, `change` and `scope`. The migration drops `access_requests_kind_check` and `access_requests_shape_check` and adds them back with `(kind = 'key' AND change IS NOT NULL)` added to the shape check. This is the same method as `00007_phase_4.sql`.

The `change` JSON of a key request is:

```json
{
  "name": "dev",
  "username": "alice",
  "keyID": "<43 characters>",
  "publicKey": "<base64url without padding, 32 bytes>"
}
```

The other columns are filled as for a `change` request. `workspace` is the requester's workspace (`identity.Workspace`), and `verified` is `VerifiedWorkspace(identity) != ""`. So the approval page can show where the registration came from.

**Workspace keys.** A new table holds approved keys only. A pending key exists only as a key request.

```sql
CREATE TABLE workspace_keys (
    id            bigserial   PRIMARY KEY,
    principal     text        NOT NULL,
    username      text        NOT NULL CHECK (username ~ '^[A-Za-z0-9._-]{1,63}$'),
    name          text        NOT NULL CHECK (name ~ '^[A-Za-z0-9._-]{1,63}$'),
    key_id        text        NOT NULL CHECK (key_id ~ '^[A-Za-z0-9_-]{43}$'),
    public_key    bytea       NOT NULL CHECK (octet_length(public_key) = 32),
    status        text        NOT NULL CHECK (status IN ('active', 'replaced', 'revoked')),
    request_id    text        NOT NULL,
    registered_at timestamptz NOT NULL,
    approved_at   timestamptz NOT NULL,
    ended_at      timestamptz,
    last_used_at  timestamptz,
    CHECK ((status = 'active') = (ended_at IS NULL))
);

CREATE UNIQUE INDEX workspace_keys_active_name_key ON workspace_keys (principal, name) WHERE status = 'active';
CREATE UNIQUE INDEX workspace_keys_active_key_id_key ON workspace_keys (key_id) WHERE status = 'active';
CREATE INDEX workspace_keys_key_id_idx ON workspace_keys (key_id);
CREATE INDEX workspace_keys_ended_idx ON workspace_keys (ended_at) WHERE status <> 'active';
```

- One active key per (user, name). The first unique index enforces it.
- One active row per key ID, for all users together. The second unique index enforces it. So the lookup by key ID finds at most one active row.
- `request_id` is the ID of the key request that the user approved. It is not a foreign key, because the leader deletes access requests 7 days after they stop being live.
- `registered_at` is the key request's `created_at`. `approved_at` is the time of the approval.
- `ended_at` is set when the key is revoked or replaced.
- `last_used_at` is written at most once a minute per key (section 8).
- A user has at most 20 active keys. Replacing a key does not count as a new key.

**Cleanup.** On its 30 s pass, the leader deletes `workspace_keys` rows whose `ended_at` is more than 7 days old, the same delay as for access requests. After that, an assertion of that key gets `attestation key unknown` instead of `attestation key revoked`.

**Queries** go in a new sqlc file, `internal/store/queries/workspacekeys.sql`:

- `GetWorkspaceKeyByKeyID`: `WHERE key_id = $1 ORDER BY (status = 'active') DESC, ended_at DESC NULLS FIRST LIMIT 1`. This gives the active row, or else the newest ended row.
- `HasPendingKeyRequest`: whether `principal` has a key request with `status = 'pending'`, `expires_at > now()` and `change->>'keyID' = $2`. The partial index `access_requests_principal_pending_idx` bounds it to 100 rows.
- `TouchWorkspaceKey`: `UPDATE workspace_keys SET last_used_at = now() WHERE id = $1 AND status = 'active' AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`.
- The list, insert, replace and revoke queries that sections 4 to 6 use.

`InsertChangeRequestWithinLimit` takes the kind as a parameter, or a sibling query inserts kind `key`. Both use the existing pending limit of 100 access requests per principal, under `LockPrincipalAccessRequests`.

**Notifications.** No new kinds. Approving and denying a key request notify `approval:<id>`, as for every access request. The authenticator keeps no cache, so a key change needs no notification to take effect.

## 3. Configuration

Two new gateway settings. Both are documented by their doc comments in `internal/config`, and `make generate-docs` writes `docs/admin/configuration.md`.

| Setting | Variable | Chart value | Default |
|---|---|---|---|
| `workspaceKeys` (bool) | `TOOLGATE_WORKSPACE_KEYS` | `config.workspaceKeys.enabled` | `false` |
| `workspaceKeysPrefix` (string) | `TOOLGATE_WORKSPACE_KEYS_PREFIX` | `config.workspaceKeys.workspaceIDPrefix` | `registered` |

The gateway does not start if a rule below is broken. Each problem goes in the start error, as `ValidateAttestation` does today.

- `workspaceKeys: true` needs the web UI (`oidcUIClientID` set). The message is "workspaceKeys: needs the web UI (oidcUIClientID), because only the web UI can approve a key."
- `workspaceKeysPrefix` must match `^[A-Za-z0-9._-]{1,63}$`. So it is one path segment and never empty.
- `workspaceKeysPrefix` must not be `claimed` or `k8s`.
- `workspaceKeysPrefix` must not equal the first segment of `workspaceAttestation.workspaceID`, when that setting is present. So a key's workspace never looks like a Coder pod's workspace.
- The prefix rules apply only when `workspaceKeys` is `true`.

`workspaceKeys` does not depend on `workspaceAttestation`. Either one, both or neither can be on. The `workspaceAttestation` setting does not change.

The chart writes both values to the configuration file. `config.workspaceKeys.enabled` defaults to `false` and `config.workspaceKeys.workspaceIDPrefix` defaults to `registered` in `values.yaml`. `docs/admin/chart-values.md` lists them.

## 4. Registering a key

### The API

`POST /api/v1/workspace-keys`, with the bearer token, as every `/api/v1/` route. The body is at most 4 KiB:

```json
{"name": "dev", "publicKey": "<base64url without padding, 32 bytes>"}
```

The handler checks, in this order:

1. The identity's `Kind` is not `user`: 403 "Only users can register workspace keys."
2. `workspaceKeys` is off: 409 "Workspace keys are off on this gateway."
3. `name` does not match `^[A-Za-z0-9._-]{1,63}$`: 400 "A key name has 1 to 63 letters, digits, `.`, `_` or `-`."
4. `publicKey` does not decode to 32 bytes, or go-jose's `Thumbprint(crypto.SHA256)` refuses it: 400 "publicKey must be an Ed25519 public key of 32 bytes, base64url without padding."
5. `identity.Username` (the `preferred_username` claim) does not match `^[A-Za-z0-9._-]{1,63}$`: 400 "Your sign-in has no user name that fits in a workspace ID. toolgate needs the preferred_username claim, with 1 to 63 letters, digits, `.`, `_` or `-`."

Then one transaction:

1. Lock the principal's `user_scopes` row with the existing `lockUserScope`.
2. If an active row has this key ID:
   - If the row is this principal's with the same name, answer 200 `{"status": "active", "name", "workspace", "fingerprint"}`. Nothing is written.
   - Otherwise, answer 409 "This key is already registered. Make a new key with a new --key-file."
3. If this principal has a pending, unexpired key request with the same name and key ID, answer 202 with that request. Nothing is written.
4. If the principal has 20 active keys and none of them has this name, answer 409 "You already have 20 workspace keys. Revoke one first."
5. Insert the key request within the pending limit, as `requestChange` does. At the limit, answer the existing 409 "You already have 100 pending access requests. Approve or deny some first."
6. Write `access.request` and `workspace.key.register` (section 12). Notify `approval:<id>`. Commit.

The 202 answer is:

```json
{
  "request": "<id>",
  "url": "<publicURL>/ui/approvals/<id>",
  "name": "dev",
  "workspace": "registered/alice/dev",
  "fingerprint": "SHA256:<key ID>",
  "replaces": "SHA256:<key ID>"
}
```

`replaces` is the fingerprint of the current active key with that name, or `null`. `workspace` uses the prefix that is configured now.

Registering never makes a key usable. Only an approval in the browser does (section 5). Two pending key requests for one name can exist, with different keys. Each approval makes its own key active and replaces the key that was active before.

### The CLI

`toolgate workspace register NAME [--key-file PATH]`.

1. The key file is `PATH`, or `<configuration directory>/workspace-keys/NAME.pem` by default. The configuration directory is the one that holds `config.json`.
2. If the file exists, the command loads it (section 10, "The key file"). This lets a user register again after a request expired, with the same key.
3. If the file does not exist, the command makes a key with `ed25519.GenerateKey(rand.Reader)` from Go's standard library. It writes the key as a PKCS#8 PEM block (`x509.MarshalPKCS8PrivateKey`, type `PRIVATE KEY`) with `internal/client/atomicfile`, mode 0600. It creates a missing parent directory with mode 0700. The private key never leaves the machine.
4. It computes the key ID and sends `POST /api/v1/workspace-keys`. The request carries the same headers as every other command, including the attestation of the source that is configured now.
5. It prints the result:
   - 200: "Key `dev` is already registered and active. Its workspace is `registered/alice/dev`." Exit 0.
   - 202:
     ```
     Registered key `dev` with fingerprint SHA256:<key ID>.
     It replaces the key with fingerprint SHA256:<key ID>.       (only when replaces is set)
     Approve it at <url>. Make sure that the page shows the same fingerprint.
     Waiting… (Ctrl-C stops waiting; the request stays open until it expires.)
     ```
     It polls `GET /api/v1/access-requests/{id}` every 2 s, as `workspace allow` does today (`awaitDecision`).
   - When the request is approved, it prints "Key `dev` is active. Sessions that use it run in workspace `registered/alice/dev`. To use it, run: toolgate configure --attestation-key-file PATH" with the absolute path. Exit 0.
   - When it is denied or expires, it prints the existing messages of `awaitDecision` and exits 1.
   - Errors print the gateway's message and exit 1. Usage errors exit 2.
6. Before it sends the request, it reads the `iat` and `exp` claims of the user token that it sends, without verification, with go-jose's `UnsafeClaimsWithoutVerification`. If `exp - iat` is more than 330 s, it prints to standard error: "Warning: your access tokens live N minutes. A session that uses a workspace key loses its verified workspace after 5 minutes, unless access tokens live at most 5 minutes. Ask your admin to make the access-token lifetime 5 minutes or less." It goes on. Section 9 gives the reason.

`register` does not change `config.json`. The user runs `toolgate configure` after the approval. To replace a key, the user registers the same name with a new `--key-file` path, approves it, and runs `toolgate configure --attestation-key-file` with the new path (section 6).

## 5. Approving a key

A key request appears on the Approvals page with the other access requests. Its page is `/ui/approvals/<id>`, as for every access request.

**How the code tells the browser from the CLI.** The approval routes are in `internal/ui`. Every `User` route goes through `requireSession`, which accepts only the `SessionCookie` UI session that the browser gets from the authorization-code sign-in of the UI client. It never reads `Authorization`. `requireCSRFToken` and Go's `CrossOriginProtection` (`protect`) also run. `POST /ui/approvals/{id}/approve` is wrapped in `RequireStepUp`, which needs a session whose `AuthTime` is at most `StepUpMaxAge` (5 minutes) old. `AuthTime` comes from the ID token's `auth_time` of a step-up sign-in, checked by `checkStepUpAuthTime`. The `/api/v1/` routes use `auth.Middleware` with bearer tokens only, and none of them approves anything. So a CLI token can never approve a key. Getting a fresh UI session needs an interactive sign-in at the IdP. The key approval reuses this route and adds no new path.

**The page** for a `key` request (`access_request.html`) shows:

- the title "Register workspace key `dev`";
- the workspace it gives: `registered/alice/dev`, rendered with the prefix that is configured now;
- the fingerprint, in full, in a monospace font;
- "It replaces the key with fingerprint `SHA256:…`, registered `<date>`." or "No key has this name yet.";
- where it was asked from: the request's workspace, with the existing workspace label, and "verified" or "unverified";
- the expiry;
- the warning: "Approve only if you ran `toolgate workspace register dev` yourself and it printed the same fingerprint. A process that holds this key and your sign-in gets your verified sessions.";
- Approve (step-up) and Deny. A request that is no longer pending shows its status and offers nothing, as today.

`describeAccessRequest` gets a `key` case for the list: "Register workspace key `dev` (SHA256:<first 12 characters of the key ID>…)".

**The approval transaction.** `approveAccessRequest` in `internal/ui/workspaces.go` gets a `key` case that calls the new `Workspaces.ApproveKeyRequest(ctx, tx, identity, id)` through `decideAccessRequest`. `gateway.Workspaces` gets the two settings of section 3. `ApproveKeyRequest`:

1. If `workspaceKeys` is off, returns `*WorkspaceError` 409 "Workspace keys are off on this gateway." The request stays pending and expires later.
2. Locks the principal's `user_scopes` row with `lockUserScope`. This comes before the compare-and-set, so it has the same lock order as revoke (section 6). Revoke takes the same two locks in the same order, so the two never deadlock.
3. Runs the existing `ApproveAccessRequest` compare-and-set. No row: `ErrAccessRequestUndecidable`. A kind other than `key`: `ErrAccessRequestUndecidable`.
4. Decodes the `change` JSON.
5. If an active row has the key ID:
   - If it is this principal's with the same name, writes `access.approve` only and returns. The key is already active.
   - Otherwise, returns `*WorkspaceError` 409 "This key is already registered. Make a new key with a new --key-file."
6. If the principal has 20 active keys and none of them has this name, returns `*WorkspaceError` 409 "You already have 20 workspace keys. Revoke one, then approve again."
7. If an active row has this principal and name, sets it to `replaced` with `ended_at = now()`, and writes `workspace.key.replace`.
8. Inserts the new row with `status = 'active'`, `registered_at` from the request and `approved_at = now()`.
9. Writes `workspace.key.approve` and `access.approve`. Notifies `approval:<id>`.

Any error rolls back the transaction, so the request stays pending. The page shows the `*WorkspaceError` message, as for other access requests.

**Deny** uses the existing `DenyAccessRequest` with no change. It needs no step-up. No key row is written.

**Expiry** uses the existing leader pass and writes `access.expire`. No key row is written.

## 6. Replacing and revoking

**Replace.** Registering a name that has an active key, with a different key, makes a key request. Its approval (section 5, step 7) sets the old row to `replaced` and inserts the new one in one transaction. The workspace ID stays `registered/<username>/<name>`, so the workspace's list and scope entries keep applying.

**Revoke.** `DELETE /api/v1/workspace-keys/{name}` with the bearer token. The Workspaces page posts `POST /ui/workspaces/keys/revoke` with the field `name`, with the CSRF token and no step-up. Both call `Workspaces.RevokeKey(ctx, identity, name, via)`:

1. The identity's `Kind` is not `user`: 403 "Only users have workspace keys."
2. One transaction. Lock the principal's `user_scopes` row.
3. Set every pending key request of this principal whose `change->>'name'` is `name` to `denied`, with `decided_by` set to the principal. Write `access.deny` and notify `approval:<id>` for each.
4. Set the active row of (principal, name) to `revoked` with `ended_at = now()`. Write `workspace.key.revoke`.
5. If steps 3 and 4 changed nothing: 404 "You have no key named `dev`."
6. Otherwise: 200 `{"revoked": true|false, "deniedRequests": N}`.

Revoking works when `workspaceKeys` is off. It only narrows, so a bearer token can do it, like `workspace deny`. A stolen token can revoke the user's key. That only takes away access.

Revoking denies the pending requests for the name too. So a request that an attacker made for that name cannot be approved by mistake later.

**The CLI.**

- `toolgate workspace keys list` prints a table: `NAME`, `WORKSPACE`, `FINGERPRINT`, `STATUS` (`active` or `pending`), `REGISTERED` and `LAST USED` (`never` when null). It reads `GET /api/v1/workspace-keys`. With `workspaceKeys` off, it first prints "Workspace keys are off on this gateway. These keys don't attest."
- `toolgate workspace keys revoke NAME` prints "Revoked key `dev`." and, when requests were denied, "Denied N pending requests for `dev`." Exit 0. A 404 prints the message and exits 1.

**`GET /api/v1/workspace-keys`** returns:

```json
{
  "enabled": true,
  "keys": [
    {
      "name": "dev",
      "workspace": "registered/alice/dev",
      "fingerprint": "SHA256:…",
      "status": "active",
      "registeredAt": "…",
      "approvedAt": "…",
      "lastUsedAt": null,
      "request": null
    }
  ]
}
```

It lists the principal's active keys and pending, unexpired key requests (`status: "pending"`, `approvedAt` null, `request` set), sorted by name and then status. A non-user gets 403. All three routes go in `internal/workspacesapi` and in the OpenAPI document in `docs/dev/`.

## 7. The assertion

The client makes one assertion each run, with go-jose (`github.com/go-jose/go-jose/v4`, already in `go.mod`). The signer is `jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: privateKey}, options)`, and the token is built with `jwt.Signed(signer).Claims(…).Serialize()`. Nothing in toolgate signs or verifies by hand.

Protected header:

| Field | Value |
|---|---|
| `alg` | `EdDSA` |
| `typ` | `toolgate-workspace-key+jwt` |
| `kid` | the key ID |

Claims:

| Claim | Value |
|---|---|
| `aud` | `toolgate` |
| `iat` | the time now, in seconds |
| `exp` | `iat` + 300 |
| `jti` | 16 bytes from `crypto/rand`, base64url without padding |

No other claims are sent. The gateway ignores any other claims.

- **Lifetime.** `exp - iat` must be at most 300 s. The client always uses exactly 300 s.
- **Clock skew.** The gateway allows 30 s both ways: `exp` up to 30 s in the past, and `iat` or `nbf` up to 30 s in the future. The lifetime check has no skew.
- **`jti` is required but not tracked.** Claude Code runs the headers helper when it connects, and then sends the same headers on every request of that connection. So one assertion is sent many times in normal use, and a replay check refuses the user's own requests. A tracked `jti` also needs a PostgreSQL write on every request, shared by all replicas. Without tracking, a captured header pair works until the assertion expires, at most 5 minutes and 30 s. A captured user token alone works for its own lifetime anyway. No state lives longer than one assertion, as the acceptance list asks. The gateway checks that `jti` is present and at most 128 characters.

## 8. Verification

### Routing

`auth.Middleware` runs the attestation step for `Kind == user` when `workspaceAttestation` or `workspaceKeys` is on. When both are off, the header is ignored with no event, as today. The step:

1. Applies the existing header checks: repeated, or over 16 KiB, gives `attestation header repeated or too large`.
2. Parses the value with `jose.ParseSignedCompact(value, tokenAlgorithms)` and reads the protected header's `typ` (`jose.HeaderType`).
3. If `typ` is `toolgate-workspace-key+jwt`, it goes to the key verifier below. If `workspaceKeys` is off, the verdict is `attestation workspace keys off`.
4. If the parse fails, or `typ` has any other value, the value goes to the existing verifier of phase 3 §10 (TokenReview or SPIFFE). If `workspaceAttestation` is off, the verdict is `attestation token invalid`.

So a key assertion never reaches TokenReview, and a pod token or a JWT-SVID never reaches the key verifier. The pod path, with `match.by: email`, does not change.

The code goes in a new file, `internal/auth/workspacekeys.go`. `auth.Options` gets `WorkspaceKeys *WorkspaceKeyOptions`:

```go
type WorkspaceKeyOptions struct {
    Store             WorkspaceKeyStore
    WorkspaceIDPrefix string
    Now               func() time.Time // nil uses time.Now
}

type WorkspaceKey struct {
    ID        int64
    Principal string
    Username  string
    Name      string
    Status    string // active, replaced or revoked
    PublicKey ed25519.PublicKey
}

type WorkspaceKeyStore interface {
    WorkspaceKey(ctx context.Context, keyID string) (WorkspaceKey, bool, error)
    HasPendingKeyRequest(ctx context.Context, principal, keyID string) (bool, error)
    TouchWorkspaceKey(ctx context.Context, id int64) error
}
```

`internal/store` implements the interface with the queries of section 2. `cmd/toolgate/serve_auth.go` wires it.

### Order of checks

The key verifier runs these steps in this order. The first that fails gives the verdict.

1. `jwt.ParseSigned(value, []jose.SignatureAlgorithm{jose.EdDSA})` fails, or `kid` does not match `^[A-Za-z0-9_-]{43}$`: `attestation assertion invalid`. Only EdDSA is accepted, so `none`, HMAC and other algorithms fail here.
2. `Store.WorkspaceKey(kid)`. An error gives the unavailable result below.
3. No row:
   - If `Store.HasPendingKeyRequest(principal, kid)` is true: `attestation key not approved`.
   - Otherwise: `attestation key unknown`.
4. `parsed.Claims(row.PublicKey, &claims)` fails: `attestation assertion signature invalid`. From here on, the claims are signed.
5. `row.Principal` differs from the identity's principal: `attestation key owner mismatch`. Because step 4 passed, this event means that someone holds the key of another user and uses it with their own token.
6. `row.Status` is not `active`: `attestation key revoked`. This covers `revoked` and `replaced`.
7. `iat`, `exp` or `aud` is missing, or `jti` is empty or over 128 characters: `attestation assertion invalid`.
8. `exp - iat` is more than 300 s: `attestation assertion lifetime too long`.
9. `claims.ValidateWithLeeway(jwt.Expected{AnyAudience: jwt.Audience{"toolgate"}, Time: now}, 30*time.Second)`:
   - `jwt.ErrInvalidAudience`: `attestation assertion audience wrong`.
   - `jwt.ErrExpired`: `attestation assertion expired`.
   - `jwt.ErrNotValidYet` or `jwt.ErrIssuedInTheFuture`: `attestation assertion issued in the future`.
10. Success. The identity gets `Attestation = attested` and `Workspace = <prefix>/<row.Username>/<row.Name>`. Then the verifier calls `Store.TouchWorkspaceKey(row.ID)`. If that write fails, the verifier logs it at warning level and goes on. The request is still attested.

The signature check comes before the owner check and the status check. So `attestation key owner mismatch` and `attestation key revoked` are written only for a client that holds the private key. Anyone with the user's token can make the other verdicts with a forged header. They are written to that user's own audit log only.

The order of steps 7 to 9 follows go-jose's `ValidateWithLeeway`, which checks the audience before the times.

### Fail open

Every verdict above is a failed attestation, as in phase 3 §10 step 5. The request is served as the user, with `user` attestation and the claimed workspace. The gateway writes `auth.refuse` with the reason and the user's principal, under the existing rate limit. It never answers 401 because of the attestation.

**Unavailable.** If a store call in step 2 or 3 fails with a database error, the gateway answers 503 with `Retry-After: 1` and writes no `auth.refuse`. This is the same rule as a TokenReview outage today. A fallback moves a live attested session to another workspace.

**The reasons**, all new except the first two, which exist today:

- `attestation header repeated or too large`
- `attestation token invalid` (a non-key token while `workspaceAttestation` is off)
- `attestation workspace keys off`
- `attestation assertion invalid`
- `attestation key unknown`
- `attestation key not approved`
- `attestation assertion signature invalid`
- `attestation key owner mismatch`
- `attestation key revoked`
- `attestation assertion lifetime too long`
- `attestation assertion audience wrong`
- `attestation assertion expired`
- `attestation assertion issued in the future`

### No cache

The verifier reads the row on every request that carries an assertion. It keeps nothing in memory. So an approval, a replacement or a revocation takes effect on the next request on every replica. The read is one index lookup on `key_id`. The `last_used_at` write happens at most once a minute per key.

## 9. Sessions

**Identity.** A key-attested request has the user's principal, email, groups, secrets, instances and grants, as every attested user session. Only the attestation and the workspace change. `Kind` stays `user`.

**Workspace ID.** `<workspaceKeysPrefix>/<username>/<name>`, for example `registered/alice/dev`.

- `username` is the user's `preferred_username` when they registered the key. It is stored on the row and never read from later tokens. So the workspace does not change when a later token has another user name.
- `name` is the key name.
- The prefix is read from the configuration on each request. If the admin changes it, existing keys get the new prefix after the restart.
- The longest ID is 63 + 1 + 63 + 1 + 63 = 191 characters, under the limit of 256.
- The prefix rules (section 3) mean that the ID never starts with `claimed/`, `k8s/` or `/`, or with the first segment of the Coder workspace ID.

**Policy.** Nothing changes. `VerifiedWorkspace` returns the workspace, because the identity is `Kind == user` and `attested`. Workspace lists, defaults, session additions and scope entries apply as for any verified workspace. `attested` and `opt-in` environments pass without a grant. Rules match the workspace and `attestation: attested`.

**MCP session binding.** It does not change. Phase 3 §10 binds an MCP session to the identity's final workspace and attestation. A new assertion on each helper run, with a new `jti`, gives the same workspace and attestation, so the binding holds. A request whose attestation fails, or that uses a key with another name, has another workspace or attestation. It gets 404, and then 400 on the same toolgate session, exactly as with the pod and SPIFFE sources. A request that passed authentication before a revocation finishes. That includes a call that waits for an approval, which re-decides policy but does not authenticate again.

**Resume.** Phase 3 §11 resumes an ended session only from the same workspace and attestation. A replaced key keeps the workspace, so a session resumes after a replacement. After a plain revocation the session comes back unverified, so its set is cleared.

**Access-token lifetime.** Claude Code runs the headers helper when it connects, and again only after a 401 or 403 (Claude Code's MCP documentation, "Use dynamic headers for custom authentication"). An assertion lives 5 minutes. If the user token lives longer, the assertion expires first. The next request is then unverified, with `attestation assertion expired`, and the MCP session gets 404. If the user token lives at most 5 minutes, it expires first. The 401 makes Claude Code run the helper again, which signs a new assertion, and the gateway accepts an expired `exp` for 30 s more. So the admin docs must state the rule: "With workspace keys, user access tokens must live at most 5 minutes." Authentik's default access-token lifetime is 5 minutes. The CLI warns at registration (section 4).

## 10. Client

### The source

`internal/client/attestation.Source` gets a fourth field, `KeyFile`.

- **Variable:** `TOOLGATE_ATTESTATION_SIGNER_FILE`. The name avoids `TOKEN`, `KEY`, `SECRET`, `PASSWORD`, `AUTH` and `CREDENTIAL`, so it is meant to pass Claude Code's environment filter for the headers helper. The worker must check this with the same method as `docs/dev/claude-code-integration.md`, on the Claude Code version that CI pins, and record the result in the "Claude Code plugin helpers" list of `docs/admin/workspace-attestation.md`. If the filter removes it, the worker stops and hands back `HANDOFF: question` with the filter's rule. The `config.json` setting works either way.
- **`config.json`:** `attestationKeyFile`, next to `attestationTokenFile` and `attestationTokenRequest`.
- **Which wins:** the existing rule. If any attestation variable is set, the client uses the variables only. Otherwise it uses `config.json`.
- **At most one source.** `ConflictMessage` becomes "Set only one of TOOLGATE_ATTESTATION_TOKEN_FILE, TOOLGATE_ATTESTATION_SPIFFE_SOCKET, TOOLGATE_ATTESTATION_TOKEN_REQUEST and TOOLGATE_ATTESTATION_SIGNER_FILE." `TOOLGATE_SPIFFE_SOCKET` still cannot be combined with any of them.
- **`Token(ctx)`** loads the key file, makes the assertion of section 7 with the time now, and returns it. Every command that sends an attestation today sends it: `headers`, `otel-headers`, `hook session-start`, `servers`, `secret`, `sessions`, `audit tail` and `workspace`.
- **Failure.** If the key file cannot be read or parsed, the command prints why and exits non-zero, with no headers. It never sends the request without the attestation. This is the existing rule.

### The key file

- It is read up to 16 KiB.
- On Unix, if its mode gives any access to the group or to other users (`mode & 0o077 != 0`), the client refuses it: "The key file PATH can be read by other users. Run chmod 600 PATH." This check does not run on Windows.
- It must hold one PEM block of type `PRIVATE KEY` that `x509.ParsePKCS8PrivateKey` parses to an `ed25519.PrivateKey`. Otherwise: "The key file PATH doesn't hold an Ed25519 private key."
- The key ID comes from `(&jose.JSONWebKey{Key: privateKey.Public()}).Thumbprint(crypto.SHA256)`, encoded base64url without padding.

### `toolgate configure --attestation-key-file PATH`

- It loads the file as above and fails with that message if it cannot load it. On success it saves the absolute path as `attestationKeyFile` and prints "Saved the settings in DIR. The key's fingerprint is SHA256:…".
- An empty value removes the setting, as for the other flags.
- `validateConfigured` allows at most one of `attestationTokenFile`, `attestationTokenRequest` and `attestationKeyFile`: "set only one of --attestation-token-file, --attestation-token-request and --attestation-key-file". The user removes another source with an empty value in the same command.

## 11. Web UI

**Workspaces page** (`GET /ui/workspaces`). A new card "Workspace keys", after "Your workspaces", with the summary "Keys that let a machine outside the cluster prove where its sessions run."

- One row per active key and per pending key request, from the same data as `GET /api/v1/workspace-keys`.
- Each row shows the name, the workspace, the full fingerprint (monospace, can wrap), a status badge ("Active", or "Waiting for approval" linked to `/ui/approvals/<id>`), "Registered" and "Last used" as relative times ("Never" when null).
- An active key has a "Revoke" button with the existing `row-action-confirm` component. The question is "Revoke key `dev`? Sessions that use it lose their verified workspace on their next request." It posts `POST /ui/workspaces/keys/revoke`. Revoke needs no step-up.
- With no rows and `workspaceKeys` on, the card shows the empty state "You have no workspace keys. To add one, run `toolgate workspace register NAME` on the machine."
- With `workspaceKeys` off and no rows, the card is not shown. With `workspaceKeys` off and active keys, the card shows "Workspace keys are off on this gateway. These keys don't attest." above the rows, and Revoke still works.
- Each user sees only their own keys.

**Approvals page and approval page:** section 5.

**Screenshots.** The seeded data of `make ui-screenshots` gets one active key and one pending key request, so CI renders the card and the approval page.

## 12. Audit events and metrics

New event types, written in the transaction of their change, with `Principal` set to the key's owner and `Workspace` set to the key's workspace:

| Type | When | Details |
|---|---|---|
| `workspace.key.register` | Section 4, step 6 | `name`, `fingerprint`, `request`, `replaces` (a fingerprint or null) |
| `workspace.key.approve` | Section 5, step 9 | `name`, `fingerprint`, `request` |
| `workspace.key.replace` | Section 5, step 7 | `name`, `oldFingerprint`, `newFingerprint`, `request` |
| `workspace.key.revoke` | Section 6, step 4 | `name`, `fingerprint`, `via` (`ui` or `cli`) |

The access-request events (`access.request`, `access.approve`, `access.deny`, `access.expire`) are written as for every access request, with `kind: key`.

The Audit page gets a summary line and a label for each new type in `internal/ui/auditsummary.go`, in the same category as `workspace.change`.

`auth.refuse` gets the reasons of section 8. A refusal never holds the assertion. The existing `Refusal` fields stay. Issuer, audience and trust domain stay empty for a key assertion.

Metrics:

- `opsmetrics` gets `KindKey = "key"` for the pending access-request gauge and the approval outcome counter.
- A key-attested request counts `ResultAttested`, as today.
- `docs/admin/metrics.md` names the new label value.

## 13. Interaction table

Each row is one state change combined with the work in flight at that time. Each row's test proves its rule. Rows are numbered for this document only.

| # | Change | In flight | Rule | Test |
|---|---|---|---|---|
| 1 | Register (CLI, user token) | requests that send the new key's assertion | Only a key request is inserted. No key row exists, so each request gets `attestation key not approved` and is unverified. | `TestWorkspaceKeyVerdicts/pending key is unverified` (unit), `TestWorkspaceKeyRegistration/user token alone makes no usable key` (integration) |
| 2 | Register the same name and key again | its pending request | The same request is returned. No new row and no new event. | `TestWorkspaceKeyRegistration/same key twice returns the pending request` |
| 3 | Register a key that is active for another name or user | nothing | 409. Nothing is written. | `TestWorkspaceKeyRegistration/active key ID is refused` |
| 4 | Approve (browser, step-up) | requests on other replicas that send the key's assertion | The commit makes the row active. The next request on any replica reads it and is attested. No cache. | `TestWorkspaceKeyApproval/approved key attests on another replica` |
| 5 | Approve | an MCP session that this client opened while the key was pending | That session is bound to the claimed workspace. Its next request is attested, so it gets 404 (phase 3 §10 binding). A new session is attested. | `TestWorkspaceKeySessionBinding/approval during an unverified session gives 404` |
| 6 | Approve with a CLI token | nothing | Impossible. `/ui/approvals/{id}/approve` reads only the UI session cookie and gets 401 without one. With a cookie whose sign-in is older than 5 minutes, `RequireStepUp` redirects to a step-up and the request stays pending. | `TestWorkspaceKeyApproval/bearer token cannot approve`, `TestWorkspaceKeyApproval/stale sign-in cannot approve` |
| 7 | Approve | another approval of the same request on another replica | The compare-and-set lets one win. The other page shows the status. | `TestWorkspaceKeyApproval/two approvals at once` |
| 8 | Approve | a revoke of the same name | Both take the `user_scopes` lock first. If revoke wins, the request is denied and the approval finds nothing to approve. If the approval wins, the revoke then revokes the new key. | `TestWorkspaceKeyApproval/approval and revoke at once` (integration, `-count=50 -race`) |
| 9 | Approve | the key ID became active for another user | Rolled back with 409. The request stays pending. | `TestWorkspaceKeyApproval/key ID taken meanwhile` |
| 10 | Approve the 21st key | nothing | Rolled back with 409. The request stays pending. | `TestWorkspaceKeyApproval/limit of 20 keys` |
| 11 | Deny | CLI waiting | `access.deny`. No key row. The CLI exits 1. An active key with the same name stays active. | `TestWorkspaceKeyApproval/deny keeps the active key` |
| 12 | Expire | CLI waiting | The leader's pass writes `access.expire`. No key row. The CLI exits 1. | `TestWorkspaceKeyApproval/expired request makes no key` |
| 13 | Replace (approval of a new key for an active name) | sessions that use the old key | The old row is `replaced` in the same transaction. The next request with the old key gets `attestation key revoked`, is unverified, and its MCP session gets 404. A session with the new key gets the same workspace, so lists still apply and an ended session resumes. | `TestWorkspaceKeyApproval/replace keeps the workspace` |
| 14 | Revoke | requests on other replicas | A request that read the row before the commit finishes as attested. The next request reads `revoked`, gets `attestation key revoked` and is unverified. An open GET stream stays open, but it carries only notifications, and every call is a new request. | `TestWorkspaceKeyRevocation/revoke applies on the next request` |
| 15 | Revoke | pending requests for that name | Each is denied in the same transaction, with `access.deny`. | `TestWorkspaceKeyRevocation/revoke denies pending requests` |
| 16 | User deleted or disabled at the IdP | sessions with the user's keys | The user token fails, so every request gets 401 before the attestation runs. The key rows stay. They are of no use without that user's token. None new. The existing tests of a refused user token cover the 401. |
| 17 | `workspaceKeys` turned off (restart) | sessions bound to `registered/…` workspaces | Assertions get `attestation workspace keys off` when `workspaceAttestation` is on. Otherwise the header is ignored with no event. Requests are unverified and bound MCP sessions get 404. Registration and approval answer 409. List and revoke work. Rows stay, and turning the setting on again makes them work again. | `TestWorkspaceKeysOff` (integration) |
| 18 | `workspaceKeysPrefix` changed (restart) | bound sessions, workspace lists | Keys give the new workspace ID. Bound MCP sessions get 404. Lists under the old ID no longer match. The admin docs say so. | `TestWorkspaceKeyVerdicts/prefix is read from the configuration` (unit) |
| 19 | Assertion expires | an MCP session whose headers were not refreshed | `attestation assertion expired`, unverified, 404. A user token of at most 5 minutes gets a 401 first, and Claude Code runs the helper again (section 9). | `TestWorkspaceKeyVerdicts/expired assertion` (unit). The helper behaviour is documented, not tested. |
| 20 | `workspaceAttestation` with `match.by: email` is on too | a Coder pod's request | Routed by `typ`. The pod token never reaches the key verifier. The pod path does not change. | `TestAttestedUserSession/pod with email match while workspace keys are on` (e2e) |
| 21 | Database unavailable | a request with an assertion | 503 with `Retry-After: 1`. No `auth.refuse`. | `TestWorkspaceKeyVerdicts/store error gives unavailable` (unit) |
| 22 | `last_used_at` write fails | the request | Logged at warning level. The request stays attested. | `TestWorkspaceKeyVerdicts/touch failure keeps the attestation` (unit) |
| 23 | Rolling upgrade from 0.4.11 | key requests and assertions | Turn `workspaceKeys` on only after every replica runs the new version, and the admin docs and the CHANGELOG say so. An old replica with `workspaceAttestation` on sends a key assertion to TokenReview, which gives `attestation token invalid`. An old replica does not approve a `key` request: its approve path runs `ApproveChangeRequest`, which rolls back for a kind other than `change`. | `TestWorkspaceKeysUpgrade/change approval rolls back a key request` (integration: it calls `ApproveChangeRequest` on a `key` request, as a 0.4.11 replica does) |
| 24 | Ended key rows deleted after 7 days | assertions of that key | `attestation key unknown` instead of `attestation key revoked`. Still unverified. | `TestWorkspaceKeyCleanup` (integration) |

## 14. Threat model

The key proves one thing: the session runs where the private key is. With the user token, that tells this workspace apart from every other place where the user's token is used.

**What it protects against:**

- **A stolen user token.** The thief can use the token anywhere, but its sessions are unverified. They get only `unverified_default` and the unverified allow list, and no `attested` or `opt-in` server without a grant. The thief can register a key, but the key does nothing until someone approves it in the browser after a fresh sign-in at the IdP (section 5). The thief cannot do that with the token.
- **A stolen key file alone.** The gateway checks that the key's owner is the user of the token (section 8, step 5). Without that user's token, the key gives nothing.
- **A key of one user used with the token of another.** `attestation key owner mismatch`, unverified.
- **A forged or altered assertion.** Only EdDSA with the registered public key passes. go-jose does the parsing and the signature check.
- **A captured header pair.** It works until the assertion expires, at most 5 minutes and 30 s. TLS protects the headers in transit.

**What it does not protect against:**

- **Code that runs in the workspace.** It can read the key file and the user's sign-in. It is in the workspace, so it is attested, as intended. The key does not prove that the machine was not changed. The issue's non-goals say so.
- **A user who approves an attacker's request.** The approval page shows the fingerprint and where the request came from, and the CLI prints the fingerprint. The user must compare them. The user can revoke a key at once, and revoking also denies pending requests for the name.
- **The user's preferred_username.** It forms the middle of the workspace ID. If the IdP lets users change it, a user can choose the name in their own future workspace IDs. Workspace lists are per user, so this gives no access to another user's lists. Rules only refuse, so a rule that names `registered/alice/*` can stop matching Alice if she changes her user name before she registers. An admin who writes rules for one user's keys must match on the principal, or use an IdP that does not let users change the name.
- **A user token that lives longer than 5 minutes.** Sessions lose their verified workspace after 5 minutes (section 9). This is an availability problem, not an access problem.

**What a stolen user token can still do:** revoke the user's keys and deny pending requests. Both only take access away.

**The gateway stores only public keys.** A read of the database gives nothing that signs.

## 15. Choices made

The issue leaves these open, or they needed a decision against the code. Each is the smallest option that meets the acceptance list.

1. **A pending key is an access request of a new kind, `key`.** The key row is written only on approval. This reuses the Approvals page, the pending limit, the expiry pass, the cleanup, `GET /api/v1/access-requests/{id}` and the CLI's wait. The other option, a `pending` status on the key row, needs the expiry pass and deny to update a second table.
2. **The key ID is the RFC 7638 thumbprint**, computed by go-jose. The fingerprint is `SHA256:` plus the key ID. A standard value needs no format of our own.
3. **Assertions are routed by the `typ` header**, so the existing TokenReview and SPIFFE paths see no key assertions, and the reverse.
4. **`jti` is required but not tracked** (section 7).
5. **The admin docs require user access tokens of at most 5 minutes** when keys are on (section 9). The issue fixes the assertion at 5 minutes and fails open, and Claude Code runs the helper again only on connect, 401 and 403.
6. **The user name is `preferred_username` at registration**, stored on the row. A token without a usable value cannot register a key. The workspace ID then stays the same for the life of the key.
7. **More refusal reasons than the issue lists:** `attestation key not approved`, `attestation assertion invalid`, `attestation assertion signature invalid`, `attestation assertion lifetime too long`, `attestation assertion audience wrong`, `attestation assertion issued in the future` and `attestation workspace keys off`. The acceptance list needs a distinct reason for each failure it names.
8. **The signature is checked before the owner and the status**, so owner-mismatch and revoked events mean that the client holds the key.
9. **No cache at all.** One indexed read per request with an assertion. `last_used_at` is written at most once a minute per key.
10. **A bearer token can revoke**, as it can `deny`, because revoking only narrows. Revoking also denies pending requests for that name.
11. **`workspaceKeys` needs the web UI.** Without the UI nobody can approve a key, so the gateway does not start with the setting on and no UI.
12. **The prefix must not equal the first segment of `workspaceAttestation.workspaceID`**, in addition to the issue's `claimed`, `k8s` and empty.
13. **At most 20 active keys per user.** Inputs must be bounded, and the issue gives no number.
14. **One key ID is active only once, for all users together.** So the lookup finds one row, and a user cannot take over another user's public key.
15. **Ended key rows are deleted after 7 days**, as access requests are.
16. **The approval takes the `user_scopes` lock before the compare-and-set.** This is the reverse of `ApproveChangeRequest`, and it matches revoke's order, so the two cannot deadlock.
17. **The variable is `TOOLGATE_ATTESTATION_SIGNER_FILE`**, and the worker checks it against Claude Code's filter (section 10). The flag and the `config.json` field keep the issue's name, `attestation-key-file` and `attestationKeyFile`.
18. **The key file is PKCS#8 PEM**, and the client refuses a file that other users can read, as OpenSSH does.
19. **`register` does not write `config.json`.** The issue names `configure` as the way to save the source, and a key that is not approved yet must not become the source.
20. **Two settings**, `workspaceKeys` (bool) and `workspaceKeysPrefix`, so the switch can be an environment variable like `otlpIngest`.
21. **Turn the setting on only after the whole rollout** (row 23), the same advice as for earlier features that old replicas do not know.

## 16. Work items

These are the pieces for the issue's decomposition. Each piece updates its own docs in the same change, as `CLAUDE.md` asks.

1. **State and gateway service.** The migration, the sqlc queries, `Workspaces` methods for register, approve, revoke and list, the three `/api/v1/workspace-keys` routes, the OpenAPI document, the audit events, `KindKey`, and the leader's cleanup. Sections 2, 4, 5, 6 and 12.
2. **Verifier and configuration.** `internal/auth/workspacekeys.go`, the routing in `auth.Middleware`, the two settings and their start checks, the chart values, `docs/admin/configuration.md` (generated) and `docs/admin/chart-values.md`. Sections 3 and 8.
3. **Approval page and Workspaces page.** The `key` case on the approval page and in the list, the Workspaces card, revoke from the UI, the screenshot seed, and the audit summary lines. Sections 5 and 11.
4. **CLI.** `workspace register`, `workspace keys list|revoke`, `configure --attestation-key-file`, the new source in `internal/client/attestation`, and the check of the variable against Claude Code's filter. Sections 4, 6 and 10. `SPEC.md` §13 lists the new commands and the new `configure` flag.
5. **Docs.** `docs/admin/workspace-attestation.md` describes the new source: the setting, the prefix, the 5-minute rule for access tokens, the new reasons, and the threat model of section 14, including "the key protects against a stolen user token, not against code running in the workspace". The user docs (`docs/user/README.md`) describe `register`, the approval, `configure --attestation-key-file`, `keys list` and `keys revoke`, and the same threat model in user words. `docs/admin/security-model.md` names the third source. The CHANGELOG notes that the feature is off by default and must be turned on only after the rollout.
6. **Related doc fix: KubeVirt with `containerPath`.** `docs/admin/workspace-attestation.md` still describes the KubeVirt setup as a TokenRequest exchange. Rewrite that part. Do not change any code for it.
   - The preferred setup, for KubeVirt 1.8 and later, with the feature gate `ContainerPathVolumes`:
     - A MutatingAdmissionPolicy adds a projected service-account token with audience `toolgate` to the `compute` container of the `virt-launcher` pod. The policy matches the launcher pods of the workspace namespace and service account. It must use a JSONPatch mutation, because an ApplyConfiguration mutation cannot change the atomic list `projected.sources`.
     - The `VirtualMachine` keeps a `serviceAccount` volume only so that the launcher pod runs as that service account. KubeVirt's `serviceAccount` volume has no audience field, so it cannot give the `toolgate` audience itself.
     - A `containerPath` volume shares the token's directory with the guest, which mounts it over virtiofs. If the workspace user cannot read the file as it is mounted, the image maps it, for example with bindfs, to a path that the workspace user owns with mode 0400.
     - The guest runs `toolgate configure --attestation-token-request "" --attestation-token-file PATH`.
     - No Role, no RoleBinding and no egress to the API server are needed.
   - Keep the TokenRequest setup, with its RBAC and egress, as the fallback for KubeVirt before 1.8.
   - The worker checks the field names against the KubeVirt and Kubernetes documentation of the versions that the docs name.

## 17. Tests

Test names describe behaviour. They carry no tracking IDs.

**Unit, `internal/auth`:** `TestWorkspaceKeyVerdicts`, table-driven, with a fake `WorkspaceKeyStore` and assertions signed in the test with go-jose and `ed25519.GenerateKey`. Subtests:

- approved key attests with the registered workspace
- pending key is unverified
- unknown key is unverified
- revoked key is unverified
- replaced key is unverified
- owner mismatch
- bad signature
- expired assertion
- lifetime over five minutes
- wrong audience
- issued in the future
- missing jti
- algorithm other than EdDSA
- workspace keys off
- prefix is read from the configuration
- store error gives unavailable
- touch failure keeps the attestation
- pod token while only workspace keys are on

Each subtest checks the identity's attestation and workspace, and the exact reason on the recorded `auth.refuse`.

**Unit, `internal/config`:** `TestWorkspaceKeysSetting`: off by default, the default prefix, each forbidden prefix, a prefix equal to the Coder workspace ID's first segment, and the setting without the web UI.

**Unit, client:** `TestWorkspaceKeySource`: the assertion's header and claims (decoded with go-jose), the conflict with each other source, a group-readable key file, a file that is not an Ed25519 key, and the environment variable winning over `config.json`. `TestConfigureAttestationKeyFile`: it saves the absolute path, refuses a second source, and removes the setting with an empty value. `TestWorkspaceRegister`: it makes a key with mode 0600 and a parent directory with mode 0700, reuses an existing file, prints the fingerprint, waits for approval, and prints the warning for a long-lived access token.

**Integration, `internal/gateway` and `internal/ui`** (PostgreSQL in Docker):

- `TestWorkspaceKeyRegistration`: user token alone makes no usable key; same key twice returns the pending request; active key ID is refused; a non-user identity gets 403; the pending limit.
- `TestWorkspaceKeyApproval`: approved key attests on another replica; bearer token cannot approve; stale sign-in cannot approve; two approvals at once; approval and revoke at once (`-count=50 -race`); key ID taken meanwhile; limit of 20 keys; deny keeps the active key; expired request makes no key; replace keeps the workspace.
- `TestWorkspaceKeyRevocation`: revoke applies on the next request; revoke denies pending requests; revoke works while the setting is off; revoke from the Workspaces page.
- `TestWorkspaceKeySessionBinding`: an MCP session keeps its workspace across new assertions; a key with another name gets 404; approval during an unverified session gives 404.
- `TestWorkspaceKeysOff`, `TestWorkspaceKeyCleanup`, `TestWorkspaceKeysUpgrade`.

**e2e, `test/e2e`:**

- `TestWorkspaceKeyAttestation`: the test registers a key through the CLI, approves it through the browser steps with a fresh sign-in, and runs `toolgate configure --attestation-key-file`. Then an MCP session through `toolgate headers` is `attested` in `registered/<user>/<name>`, and an `attested` server and an `opt-in` server work with no unverified grant and no unverified allow list. A second user's token with the first user's key gives an unverified session and `attestation key owner mismatch`.
- `TestAttestedUserSession/pod with email match while workspace keys are on`: the existing pod test, run with `workspaceKeys` on.

`docs/acceptance/` is the owner's. The owner decides whether these tests go on a checklist `Tests:` line.

## Acceptance

- A registered, approved key gives an attested session with workspace `registered/<user>/<name>`. `attested` and `opt-in` servers work without unverified grants.
- A key that is registered but not yet approved gives an unverified session.
- The user token alone can't create a usable key.
- A key registered by user A, used with user B's token, gives an unverified session and an `attestation key owner mismatch` event.
- Expired assertions, assertions with a lifetime over 5 minutes, the wrong audience, a bad signature and a revoked key each give unverified, with a distinct reason.
- Revoking a key takes effect on the next request; there is no cache beyond one assertion's lifetime.
- Coder pod attestation with `match.by: email` keeps working while this feature is on.
- An MCP session keeps its bound workspace and attestation, as with the existing sources.
- `docs/admin/workspace-attestation.md` and the user docs describe the new source. They state the threat model: the key protects against a stolen user token, not against code running in the workspace.

## Not in scope

- Proving that the machine was not changed (the issue's non-goal).
- Keys shared between users, and keys that an admin registers for another user (the issue's non-goal).
- An admin view or admin revocation of other users' keys. An admin who must remove a key of a disabled user does it in the database.
- Key types other than Ed25519, and keys in hardware tokens or the OS keychain.
- Tracking `jti` against replay (section 7).
- Automatic key rotation, and a `--rotate` flag. A replacement is a new registration with a new key file.
- Saving the source in `config.json` from `register`.
- A label for `registered/…` workspaces in `principal.Workspace`. The UI shows the ID as it is.
- Any change to the pod or SPIFFE sources, or to `workspaceAttestation`.
