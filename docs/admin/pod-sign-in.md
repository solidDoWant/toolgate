# Pod sign-in

For: admins who let workloads in Kubernetes use toolgate.

A pod can sign in to the gateway with a Kubernetes service-account token instead of a user token. The gateway then treats the pod as attested. Attested means that Kubernetes, not the client, vouches for where the request comes from.

## What the pod needs

1. Add the namespace of the pod to the `serviceAccountNamespaces` setting of the gateway. The [configuration reference](configuration.md) describes the setting. The gateway refuses tokens from all other namespaces with 401.
2. Mount a projected service-account token with the audience `toolgate` in the pod:

```yaml
volumes:
  - name: toolgate-token
    projected:
      sources:
        - serviceAccountToken:
            audience: toolgate
            expirationSeconds: 3600
            path: token
```

3. Send the token in the `Authorization: Bearer` header. The kubelet replaces the token before it expires, so read the file again for each request.

The gateway refuses a token with another audience with 401.

## The identity of a pod

The principal is the service account: `system:serviceaccount:<namespace>:<name>`.

The workspace is `k8s/<namespace>/<owner kind>/<owner name>`. The owner is the controller of the pod, for example a Job or a StatefulSet. If the controller is a ReplicaSet that a Deployment owns, the owner is the Deployment. If the pod has no controller, the owner is the pod itself, for example `k8s/ci/Pod/debug`. The `X-Toolgate-Workspace` header does not change this workspace.

The gateway gets the pod and makes sure that its UID is the UID in the token. So a token from a deleted pod cannot sign in as a new pod with the same name.

The gateway keeps the result of each token check until the token expires, and for one hour at most. So a pod that you delete can still sign in with its old token until then.

## Kubernetes permissions

The service account of the gateway needs these permissions:

- `create` on `tokenreviews` (API group `authentication.k8s.io`).
- `get` on `pods` in each namespace in `serviceAccountNamespaces`.
- `get` on `replicasets` (API group `apps`) in each namespace in `serviceAccountNamespaces`.
