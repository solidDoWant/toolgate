# Token exchange

For: admins who give an MCP server a token for the user that runs it.

Token exchange (RFC 8693) swaps a token of the user for a new token that another service accepts. A template uses `${exchange:NAME}` to get that new token. For example, a Temporal MCP server can call Temporal as the user who enabled it. This page tells you how to define the exchanges, how to set up Authentik, and what the risks are.

## The TokenExchange resource

A `TokenExchange` is a custom resource that describes one token exchange. You put it in a catalog namespace (`config.catalogNamespaces`, or the release namespace when that list is empty), next to the templates that use it. A template that uses `${exchange:temporal}` gets the `TokenExchange` named `temporal` in the namespace of the template. A `TokenExchange` in another namespace does not count. You do not restart the gateway when you add, change or delete a resource.

```yaml
apiVersion: toolgate.soliddowant.dev/v1alpha1
kind: TokenExchange
metadata:
  name: temporal
  namespace: toolgate-catalog
spec:
  tokenURL: https://authentik.example.com/application/o/token/
  clientID: toolgate-token-exchange
  clientSecretRef:
    name: toolgate-token-exchange-client
    key: client-secret
  audience: temporal-client-id               # optional
  scope: openid temporal-permissions         # optional
  requestedTokenType: urn:ietf:params:oauth:token-type:access_token   # optional
  timeout: 10s                               # optional, at most 60s
```

The gateway reads the client secret from the Secret `clientSecretRef.name`, key `clientSecretRef.key`, in the template secrets namespace (`config.templateSecretsNamespace`). See [Template secrets](template-secrets.md).

The chart lets the gateway read `TokenExchange` resources in the catalog namespaces only. The gateway writes only their status.

The gateway reads the resource and the client secret again for each exchange. So a change to the resource or a new client secret applies to the next exchange. A running instance keeps its token until its next refresh. If you delete the resource, the templates that use it become invalid.

The gateway needs `secretsKeysetFile` to encrypt each exchanged token on its way to the leader replica. See [User secrets](user-secrets.md) for the keyset. Without it, every `TokenExchange` is invalid with the reason `NoKeyset`.

### Trusted certificate authorities

The gateway uses the system roots to check the certificate of a token endpoint. If your IdP uses a private certificate authority, set the chart value `config.tokenExchange.caFile` to the path of a PEM file of the certificate authorities. Mount the file with `resources.persistence` and `advancedMounts.gateway.gateway`, as for resolvers. One file applies to every exchange. A `TokenExchange` cannot choose the certificate authorities. The gateway reads the file at start.

### The allowed token endpoints

The gateway sends a user's access token to the `tokenURL` of an exchange. Anyone who can write a `TokenExchange` chooses that URL. For this reason, the gateway uses a `TokenExchange` only if its `tokenURL` is on an allow-list that only the chart sets.

- Set the list with `config.tokenExchange.allowedTokenURLs`, at most 16 `https://` URLs.
- If you do not set the list, it holds the token endpoint of `config.oidc.issuerURL` and of each `config.oidc.additionalIssuers` item. The gateway reads these endpoints from the discovery documents of the issuers.
- If you set the list, it replaces this default. The gateway does not add the issuer endpoints to it.
- The gateway compares each URL exactly. Only the scheme and the host can differ in case. A longer path, another port or a trailing slash is a different URL.

So catalog authors can define exchanges, but they can send tokens only to the token endpoints that you allowed. The gateway also compares the `tokenURL` with the list before each exchange. If the URL is not on the list, the exchange fails and the gateway sends no request.

### Status

The gateway writes a `Valid` condition on each `TokenExchange`. Read it with `kubectl get tokenexchange temporal -o yaml`. If the resource is not valid, the condition has status `False` and one of these reasons:

| Reason | Cause |
|---|---|
| `BadFields` | A field is wrong. For example, `tokenURL` is not an `https://` URL, `clientID` is empty, or `timeout` is more than `60s`. |
| `TooMany` | The namespace has more than 16 `TokenExchange` resources. The first 16 by name are checked. The others are not valid. |
| `NoKeyset` | The gateway has no `secretsKeysetFile`. The gateway needs it to encrypt each exchanged token. |
| `URLNotAllowed` | The `tokenURL` is not on the allow-list. |
| `SecretMissing` | The Secret or its key does not exist, the value is empty, or the gateway has no template secrets namespace. The message names the Secret and the key, never the value. |

The gateway checks each resource again every minute. So a Secret that you create later makes the resource valid after a short time.

A template that uses an exchange is valid only when the `TokenExchange` exists in the namespace of the template and its `Valid` condition is `True` for its current spec. Just after you change a resource, its templates are invalid until the gateway checks it again. This takes a few seconds. The `Valid` condition of the template names the cause.

## An example template

A template uses the variable as the whole value of a field. Text around it, such as `Bearer ${exchange:temporal}`, is not allowed. See [Templates and rules](templates-and-rules.md) for where the variable can go.

```yaml
env:
  - name: TEMPORAL_API_KEY
    value: ${exchange:temporal}
```

Only users who sign in through the IdP can use a template with `${exchange:…}`. Service accounts, SPIFFE workloads and pod sign-in have no user token to exchange. The gateway refuses their enables and their calls with the reason `no-user-token`. If such an identity had the server enabled before the template got the variable, the gateway removes the server from its session when the session resumes.

## Authentik setup

1. Make a confidential OAuth2 provider, for example `toolgate-token-exchange`, with the `Token exchange` grant type. Bind it to an application.
2. List every provider whose user tokens reach the gateway as a federated provider of `toolgate-token-exchange`. These are `toolgate`, `coder-toolgate` and the client of each connector.
3. Add `toolgate-token-exchange` to the federated providers of the target provider, for example the Temporal provider.
4. Set `audience` to the `client_id` of the target provider. Authentik accepts one value.
5. Set `scope`. Authentik gives the new token no scopes when `scope` is not set.

The user must pass the policy bindings of the target application. If the user does not, Authentik refuses the exchange with `invalid_grant`, and the instance does not start.

## Start

An instance that uses `${exchange:…}` starts only when the leader replica holds an exchanged token for each exchange in the template. The request of the user sends the token to the leader. If no token arrives within the `startTimeout` of the template, the instance fails with this reason: "No exchanged token for `temporal` arrived within 5m0s. Enable `<server>` again to retry." If an exchange fails, the instance fails with the reason of the exchange.

If the template also has `persistentVolumes`, the exchange runs only after toolgate has the volumes. The volume wait counts toward the same `startTimeout`. See [Start order with volumes and token exchange](instances.md#start-order-with-volumes-and-token-exchange).

The request that starts the instance runs the exchange. Each exchange writes a `credential.exchange` audit event, with the instance, the name of the exchange, `start` or `refresh`, and `success` or `failure`. A failure also has the error code, for example `invalid_grant`. The event never holds a token. The web UI shows it as "Credential exchanged".

If a token does not get to the leader, for example because the leader changed, another request of the user that waits for the instance runs the exchange again. This happens after the `timeout` of the exchange plus 30 seconds.

If the `TokenExchange` is missing or is not valid when the exchange runs, the exchange fails with the error code `unavailable`, and the gateway sends no request.

Only a signed-in user has a token to exchange. If the session that starts the instance belongs to a service account or a SPIFFE workload, the instance fails at once.

## Lifetime

The `access_token_validity` of the target provider sets the lifetime of the token. Use `minutes=10` to `minutes=30`. A shorter value stops idle instances sooner. With less than `minutes=4`, less than 2 minutes remain between the refresh and the expiry, which is close to the delay of the kubelet. With less than 2 minutes, every exchange fails.

## Threat model

The gateway is trusted to act for active users only. The gateway exchanges each token with the token of one request of that user. The new token is no broader than what the target provider gives that user. The gateway never extends a token when no request is behind it.

If someone compromises the gateway, they can exchange the tokens of users who send requests during the compromise. A server can read its own token. Give the target provider only the permissions that the server needs. Agents also hold user tokens, but they cannot exchange them, because the client is confidential.

## Check before you deploy

The design was not tested against a live Authentik. Before you use a template in production, do this check:

1. Exchange one token by hand against your Authentik with the `TokenExchange` above.
2. Decode the token. Make sure that `iss` is the issuer of the target provider, that the `kid` is in the JWKS of the target provider, and that the claims hold the permission of the user.
3. If one of these fails, change the Authentik configuration. toolgate needs no change, because it never checks the issuer or the key of the exchanged token.
