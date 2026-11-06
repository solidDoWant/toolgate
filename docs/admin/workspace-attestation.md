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
