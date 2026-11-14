# Workspace attestation

For: admins who let a user's sessions in a workspace, such as a Coder workspace, run with a verified workspace.

An attested user session uses two tokens. The user token in `Authorization` says who the user is. A workload token in the `X-Toolgate-Attestation` header says where the session runs. The gateway makes sure that the workload belongs to that user. Then the session is still the user's session, with the user's groups, secrets, instances and grants. But its attestation is `attested`, and its workspace is verified, such as `coder/alice/dev`. An attested user can use `attested` and `opt-in` servers without a grant, and rules with `attestation: attested` match the user.

The workload token is one of these:

- A projected service-account token with audience `toolgate`, bound to the workspace pod.
- A SPIFFE JWT-SVID with audience `toolgate` (see [SPIFFE sign-in](spiffe-sign-in.md) for trust domains).
- A signed assertion from a registered workspace key, for a machine outside the cluster (see [Registered workspace keys](#registered-workspace-keys)).

## Turn it on

Set the chart value `config.workspaceAttestation`. The chart writes it to the `workspaceAttestation` setting in the configuration file of the gateway. You cannot set it with an environment variable. If the value is not set, the gateway ignores the header.

```yaml
config:
  workspaceAttestation:
    namespaces: [coder]
    keys:
      userID: com.coder.user.id
      email: com.coder.user.email
      username: com.coder.user.username
      workspace: com.coder.workspace.name
    match:
      by: email
    workspaceID: coder/{username}/{workspace}
```

The fields are:

- `namespaces`: required. The namespaces whose service-account tokens can attest a user.
- `keys`: the pod label keys of the user ID, the email, the user name and the workspace name. The gateway reads a key from the pod's annotations when the labels do not have it. The defaults are the keys that Coder templates set.
- `match.by`: `email` or `claim`. With `email`, the pod's email must be equal to the verified email of the user token. Case does not matter. With `claim`, the user token's claim `match.claim` must be a string equal to the pod's key `match.key` (`userID` or `username`). Use a claim that the user cannot change.
- `workspaceID`: required. The verified workspace, with the placeholders `{userID}`, `{username}` and `{workspace}`. The first segment must be literal text that is not `claimed`, `k8s` or empty.
- `spiffeIDPattern`: optional. A SPIFFE ID with placeholders, such as `spiffe://coder.example.com/coder/{username}/{workspace}`. The trust domain must be in `config.spiffe.trustDomains`. Each placeholder must be one whole path segment. With a pattern, `match.by` must be `claim`.

The [configuration reference](configuration.md) lists every rule. The gateway does not start if the setting breaks one.

Do not list the workspace namespaces in `serviceAccountNamespaces` too. That setting lets a pod sign in as its own service account, which is a different thing. Add them there only if the pods must also sign in as themselves.

The gateway needs no new permissions. It already gets pods for [pod sign-in](pod-sign-in.md).

## Why the gateway can trust the labels

Whoever creates or patches a pod writes its labels and annotations. So the labels are only as good as your cluster's RBAC:

- You list the namespaces that can attest a user.
- In those namespaces, only the Coder provisioner can create or change pods and virtual machines. Users, workspace service accounts and agents must not have `create`, `patch` or `update` on `pods`, `virtualmachines` or `virtualmachineinstances`.
- The provisioner runs only templates that admins write. Coder fills in the owner and the workspace name, so a workspace owner cannot set the email of another user.

toolgate does not check RBAC. Run this check for each workspace service account, and make sure that the answer is `no`:

```sh
kubectl auth can-i --as=system:serviceaccount:coder:<workspace service account> patch pods -n coder
```

Run it for `create` and `update`, and for `virtualmachines` and `virtualmachineinstances` too, if you use KubeVirt.

## Give the client its token

The client reads the workload token from one of four variables. The user documentation describes them: `TOOLGATE_ATTESTATION_TOKEN_FILE`, `TOOLGATE_ATTESTATION_SPIFFE_SOCKET`, `TOOLGATE_ATTESTATION_TOKEN_REQUEST` and `TOOLGATE_ATTESTATION_SIGNER_FILE`. Set at most one of them in the workspace. `TOOLGATE_ATTESTATION_SIGNER_FILE` is the path of a registered workspace key. The client signs a short assertion with the key on each run.

For a pod, mount a projected token with the audience `toolgate` and set `TOOLGATE_ATTESTATION_TOKEN_FILE` to its path:

```yaml
volumes:
  - name: toolgate-attestation
    projected:
      sources:
        - serviceAccountToken: {audience: toolgate, expirationSeconds: 3600, path: token}
```

For a KubeVirt virtual machine, a guest cannot get a projected token with its own audience. So the client asks for one. Mount the `serviceAccount` disk of the virtual machine and set `TOOLGATE_ATTESTATION_TOKEN_REQUEST` to the directory where the guest mounts it. Then:

- Give the guest egress to the Kubernetes API server. Set `TOOLGATE_KUBE_API` if the URL is not `https://kubernetes.default.svc`.
- Create one service account for each workspace. The template can create it.
- Add a Role in the workspace namespace that allows `create` on `serviceaccounts/token` with `resourceNames` set to the service account of the workspace. Bind it to that service account.

The API server binds a token only to a pod that runs as that service account. If several virtual machines share one service account, a guest can bind a token to the pod of another virtual machine. That pod names its own owner, so the guest gets nothing without the user token of that owner. One service account for each workspace removes the case.

### Claude Code plugin helpers

Claude Code runs the `headersHelper` of the toolgate plugin with a reduced environment. It removes each variable whose name looks like a credential, for example a name that contains `_TOKEN_`. The SessionStart hook of the plugin keeps the full environment, but the helper does not.

Claude Code 2.1.318 removes these variables from the helper:

- `TOOLGATE_TOKEN_COMMAND`
- `TOOLGATE_TOKEN_STORE`
- `TOOLGATE_ATTESTATION_TOKEN_FILE`
- `TOOLGATE_ATTESTATION_TOKEN_REQUEST`
- `CODER_AGENT_TOKEN`

It keeps `TOOLGATE_GATEWAY`, `TOOLGATE_SESSION`, `TOOLGATE_WORKSPACE`, `TOOLGATE_TOOL_SEARCH`, `TOOLGATE_SPIFFE_SOCKET`, `TOOLGATE_ATTESTATION_SPIFFE_SOCKET`, `TOOLGATE_ATTESTATION_SIGNER_FILE`, `TOOLGATE_KUBE_API` and `XDG_RUNTIME_DIR`.

So save the token command and the attestation source in the toolgate configuration file. Run `toolgate configure` as the workspace user, for example in the startup script of the workspace:

```sh
toolgate configure \
  --gateway https://toolgate.example.com \
  --token-command 'my-token-command' \
  --attestation-token-file /var/run/secrets/toolgate/token
```

Use `--attestation-token-request DIR` in place of `--attestation-token-file` for a KubeVirt guest. Use `--attestation-key-file PATH` for a registered workspace key. The command loads the key, saves its absolute path and prints its fingerprint. Give only one of the three attestation flags. The command writes `config.json` in the toolgate configuration directory with mode 0600. It changes only the settings that you give. An empty value removes a setting. If a variable is set, the client uses the variable and not the file. The file holds a command line and paths, not a token.

## KubeVirt virtual machines

A token from a KubeVirt guest is bound to the `virt-launcher` pod of the virtual machine. KubeVirt copies the labels of the virtual machine template to that pod. So set the keys in `spec.template.metadata` of the `VirtualMachine`, not in its own `metadata`. The gateway does not read virtual machine objects.

## Registered workspace keys

Use this when the workspace is not a pod in your cluster, for example a laptop or a virtual machine outside the cluster. The user makes a key pair on the machine and registers the public key with toolgate. The private key stays on the machine. The gateway stores only public keys.

The key is a third source of attestation, next to the pod token and the SPIFFE JWT-SVID. Each run, the client signs a short assertion (a signed token that lives 5 minutes) with the private key and sends it in the `X-Toolgate-Attestation` header. The gateway checks the signature against the registered key, and checks that the key belongs to the user of the access token. Then the session is attested, and its workspace is `PREFIX/USERNAME/NAME`, for example `registered/alice/dev`. `USERNAME` is the `preferred_username` claim at the time of registration, and `NAME` is the name that the user gave the key.

The feature needs no Kubernetes access. It does not depend on `workspaceAttestation`. You can turn on either one, both or neither.

### Turn it on

Two settings control it. [Configuration](configuration.md) and [Chart values](chart-values.md) list them with their rules.

| Setting | Variable | Chart value | Default |
|---|---|---|---|
| `workspaceKeys` | `TOOLGATE_WORKSPACE_KEYS` | `config.workspaceKeys.enabled` | `false` |
| `workspaceKeysPrefix` | `TOOLGATE_WORKSPACE_KEYS_PREFIX` | `config.workspaceKeys.workspaceIDPrefix` | `registered` |

- The feature is off by default. With `workspaceKeys: true` you must also set `oidcUIClientID`, because only the web UI can approve a key. The gateway does not start without it.
- The prefix is one path segment of 1 to 63 letters, digits, `.`, `_` or `-`. It cannot be `claimed` or `k8s`. It cannot be the first segment of `workspaceAttestation.workspaceID`, so a key's workspace never looks like a Coder pod's workspace.
- **Changing the prefix moves every key to a new workspace ID.** After a restart, `registered/alice/dev` becomes, for example, `keys/alice/dev`. Workspace lists, scope entries and rules that name the old ID stop matching. MCP sessions that were bound to the old workspace get 404. Choose the prefix before users register keys, and do not change it later.
- **Turn it on only after every replica runs the new version.** An old replica sends a key assertion to TokenReview, if `workspaceAttestation` is on, and the attestation fails with `attestation token invalid`. An old replica also cannot approve a key request. Roll out the new version first, and then set `workspaceKeys`.

### Access tokens must be short

With workspace keys, user access tokens must live at most 5 minutes.

The reason: Claude Code runs the headers helper when it connects, and again only after a 401 or 403 response. An assertion lives 5 minutes. If the access token lives longer, the assertion expires first. The next request is then unverified (`attestation assertion expired`), and the MCP session gets 404. If the access token lives at most 5 minutes, it expires first. The 401 makes Claude Code run the helper again, and the client signs a new assertion. The gateway accepts an expired assertion for 30 more seconds. Authentik's default is 5 minutes. The client warns the user when it registers a key with a longer lifetime.

### How a user adds a key

1. The user runs `toolgate workspace register NAME` on the machine. It makes the key and prints a fingerprint and a link.
2. The user opens the link in a browser, signs in again, compares the fingerprint on the page with the one in the terminal, and approves. A bearer token cannot approve a key.
3. The user runs `toolgate configure --attestation-key-file PATH`.

The [user guide](../user/README.md) describes the commands. A user can list and revoke keys on the Workspaces page, or with `toolgate workspace keys list` and `toolgate workspace keys revoke NAME`. A revoked key stops working on the next request, on every replica. Revoking also denies the pending requests for that name. A user has at most 20 active keys.

### What a key protects against

The key proves one thing: the session runs where the private key is.

It protects against:

- **A stolen user token.** The thief can use the token anywhere, but the sessions are unverified. The thief can register a key, but it does nothing until the user approves it in the browser after a fresh sign-in.
- **A stolen key file alone.** The key works only with the access token of the user who registered it.
- **A key of one user with the token of another.** The attestation fails with `attestation key owner mismatch`.
- **A forged or changed assertion.** Only an EdDSA signature from the registered key passes.
- **A captured header pair.** It works until the assertion expires, at most 5 minutes and 30 seconds.

It does not protect against:

- **Code that runs in the workspace.** The key protects against a stolen user token, not against code running in the workspace. That code can read the key file and the user's sign-in, and it is attested, as intended.
- **A user who approves an attacker's request.** The user must compare the fingerprint.
- **An access token that lives longer than 5 minutes.** Sessions lose their verified workspace after 5 minutes. This stops work, but it gives no access.

### Admin checklist for keys

- Set the access-token lifetime of the identity provider to 5 minutes or less.
- Make sure that the identity provider gives every user a `preferred_username` claim of 1 to 63 letters, digits, `.`, `_` or `-`. Without it, registration fails with 400.
- `preferred_username` is not unique at every identity provider, so two users can get the same workspace ID, such as `registered/alice/dev`. If your provider lets users change it, a user can choose the name in the workspace IDs of their own future keys. A rule for one user's keys must match the principal as well as the workspace. A rule that names only `registered/alice/*` is not safe.
- Workspace lists are per user, so a shared workspace ID gives no access to the lists of another user.
- Keep the prefix fixed (see above).

## When an attestation fails

A failed attestation never refuses the request. The gateway serves it as the user with the claimed workspace and `user` attestation. It writes an `auth.refuse` audit event with the user's principal and one of these reasons:

- `attestation token invalid`
- `attestation namespace not allowed`
- `attestation not bound to a pod`
- `attestation pod lookup failed`
- `attestation owner key missing`
- `attestation owner mismatch`
- `attestation owner claim missing`
- `attestation workspace key invalid`
- `attestation SPIFFE ID doesn't match the pattern`
- `attestation header repeated or too large`
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

If the Kubernetes API server does not answer, or answers with a 5xx status, the gateway answers 503 with `Retry-After: 1` and writes no event. The same is true when the database does not answer during a key check.

`attestation token invalid` also appears when a client sends a pod token or JWT-SVID and `workspaceAttestation` is off.

The gateway keeps the pod labels for a token until the token expires, at most 1 hour. So a label change takes effect when the workload gets a new token.

An MCP session stays bound to its workspace and attestation. If a client starts sending the attestation, or stops sending it, the session gets 404 and then 400. The user then starts a new Claude Code session.
