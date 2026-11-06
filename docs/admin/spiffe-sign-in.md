# SPIFFE sign-in

For: admins who let workloads with a SPIFFE identity use toolgate, such as Coder workspaces with a SPIRE agent.

A workload can sign in to the gateway with a SPIFFE JWT-SVID instead of a user token. A JWT-SVID is a signed token that names the workload with a SPIFFE ID, such as `spiffe://coder.example.com/owner/alice/workspace/dev`. A SPIFFE trust domain is the part after `spiffe://` and before the path, here `coder.example.com`. The gateway treats the workload as attested. Attested means that SPIRE, not the client, vouches for where the request comes from.

## Allow a trust domain

Add the trust domain to the chart value `config.spiffe.trustDomains`. The chart writes it to the `spiffeTrustDomains` setting in the configuration file of the gateway. You cannot set it with an environment variable.

```yaml
config:
  spiffe:
    trustDomains:
      - name: coder.example.com
        bundleEndpointURL: https://spire.example.com:8443
        caFile: /etc/toolgate/spire-ca.pem
```

Each item has these fields:

- `name`: the trust domain.
- `bundleEndpointURL`: the SPIFFE bundle endpoint of the trust domain, with the `https_web` profile. The SPIRE server serves it when you turn on its federation bundle endpoint.
- `jwksURL`: a JWKS document, such as the one that the SPIRE OIDC discovery provider serves. Set it instead of `bundleEndpointURL`.
- `caFile`: optional. A PEM file of private certificate authorities that the gateway adds to the system ones. Mount the file with `resources.persistence`.

Set exactly one of `bundleEndpointURL` and `jwksURL`. Both must be `https://` URLs.

The list of trust domains is the only allow-list in the gateway. The gateway refuses an SVID from any other trust domain with 401. The access lists of templates and the rules decide what each SPIFFE ID can use.

## What the workload sends

The workload sends a JWT-SVID with the audience `toolgate` in the `Authorization: Bearer` header. The gateway refuses with 401 an SVID that has another audience, that expired, whose SPIFFE ID is longer than 2048 bytes, or whose SPIFFE ID has no path. Each refusal writes an `auth.refuse` audit event with the reason and the trust domain. The event never holds the token.

## The identity of a workload

The principal is the SPIFFE ID, such as `spiffe://coder.example.com/owner/alice/workspace/dev`.

The workspace is the path of the SPIFFE ID, with its leading `/`: `/owner/alice/workspace/dev`. The `X-Toolgate-Workspace` header does not change this workspace. Two trust domains can give the same path. That only matters to rules, and rules only refuse.

A workload has no email address and no groups. A template that uses `${principal.email}` cannot start for it.

## Give a workload access

List SPIFFE IDs in `access.spiffeIDs` of a template. Each item is a glob pattern, as in rules. A template can have 64 patterns, and each can be 2048 characters long. A pattern that does not compile makes the template invalid.

```yaml
access:
  spiffeIDs:
    - spiffe://coder.example.com/owner/*/workspace/*
```

A SPIFFE identity matches only `access.spiffeIDs`. `access.users`, `access.groups` and `access.serviceAccounts` never match it. `access.spiffeIDs` never matches a user, because the identity provider chooses the subject of a user, and that subject can start with `spiffe://`.

A workload is attested, so `environments: attested` and `opt-in` pass without a grant.

## Rules for workloads

Rules match a workload as any other identity: `principals` against the SPIFFE ID, `workspaces` against the path, and `attestation: attested`. This rule refuses a server in scratch workspaces:

```yaml
apiVersion: toolgate.soliddowant.dev/v1alpha1
kind: ToolgateRule
metadata:
  name: no-scratch-github
spec:
  match:
    servers: ["github"]
    workspaces: ["/owner/*/workspace/scratch-*"]
    attestation: attested
  reason: Scratch workspaces cannot use GitHub.
```

## Bundles

Each gateway replica fetches the bundle of each trust domain when it starts. Then it fetches the bundle every 5 minutes, or sooner if the bundle has a shorter `spiffe_refresh_hint`, but not more often than every 30 seconds. Each fetch stops after 10 seconds, and a bundle can be 1 MiB at most.

If an SVID has a key ID that the bundle does not have, the gateway fetches the bundle again one time and checks the SVID again. If the gateway fetched that bundle less than 30 seconds before, it refuses the SVID at once with the reason "unknown SPIFFE key ID".

If a fetch fails, the gateway logs the error and tries again after 1 second, and then after longer times up to 30 seconds. The last good bundle stays valid for 24 hours after its last good fetch. After that, the gateway refuses the SVIDs of that trust domain until a fetch succeeds. Until the first fetch succeeds, it refuses them too. The readiness of the gateway does not wait for bundles.

The gateway reads the trust domains when it starts. If you remove a trust domain, its SVIDs get 401 after the gateway restarts.

The metric `toolgate_spiffe_bundle_age_seconds{trust_domain}` gives the time in seconds since the last good fetch of each bundle.
