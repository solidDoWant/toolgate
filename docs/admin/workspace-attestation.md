# Workspace attestation

For: admins who let a user's sessions in a workspace, such as a Coder workspace, run with a verified workspace.

An attested user session uses two tokens. The user token in `Authorization` says who the user is. A workload token in the `X-Toolgate-Attestation` header says where the session runs. The gateway makes sure that the workload belongs to that user. Then the session is still the user's session, with the user's groups, secrets, instances and grants. But its attestation is `attested`, and its workspace is verified, such as `coder/alice/dev`. An attested user can use `attested` and `opt-in` servers without a grant, and rules with `attestation: attested` match the user.

The workload token is one of these:

- A projected service-account token with audience `toolgate`, bound to the workspace pod.
- A SPIFFE JWT-SVID with audience `toolgate` (see [SPIFFE sign-in](spiffe-sign-in.md) for trust domains).

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

If the Kubernetes API server does not answer, or answers with a 5xx status, the gateway answers 503 with `Retry-After: 1` and writes no event.

The gateway keeps the pod labels for a token until the token expires, at most 1 hour. So a label change takes effect when the workload gets a new token.

An MCP session stays bound to its workspace and attestation. If a client starts sending the attestation, or stops sending it, the session gets 404 and then 400. The user then starts a new Claude Code session.
