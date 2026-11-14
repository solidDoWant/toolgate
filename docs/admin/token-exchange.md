# Token exchange

For: admins who give an MCP server a token for the user that runs it.

Token exchange (RFC 8693) swaps a token of the user for a new token that another service accepts. A template uses `${exchange:NAME}` to get that new token. For example, a Temporal MCP server can call Temporal as the user who enabled it. This page tells you how to configure the exchanges, how to set up Authentik, and what the risks are.

## Configuration

Set the chart value `config.tokenExchanges`, a list. The chart writes it to the configuration file of the gateway as `tokenExchanges`. You cannot set it with an environment variable. The list can have at most 8 items. The gateway reads it at start, so restart the gateway after you change it.

```yaml
config:
  tokenExchanges:
    - name: temporal
      tokenURL: https://authentik.example.com/application/o/token/
      clientID: toolgate-token-exchange
      clientSecretFile: /etc/toolgate/exchanges/temporal/client-secret
      audience: temporal-client-id
      scope: openid temporal-permissions
      requestedTokenType: urn:ietf:params:oauth:token-type:access_token
      caFile: /etc/toolgate/exchanges/ca.pem
      timeout: 10s
```

| Field | Rule |
|---|---|
| `name` | Required. Lowercase letters, digits and `-`, starting with a letter, at most 63 characters. Each name is unique. |
| `tokenURL` | Required. An `https://` URL with a host. |
| `clientID` | Required. The ID of a confidential client, so that only the gateway can exchange. |
| `clientSecretFile` | Required. The gateway reads this file for each exchange and removes white space at the end. A missing or empty file fails the exchange. |
| `audience` | Optional. The gateway sends it as `audience`. |
| `scope` | Optional. Space-separated scopes. The gateway sends them as `scope`. |
| `requestedTokenType` | Optional. A URI that the gateway sends as `requested_token_type`. |
| `caFile` | Optional. A PEM file of the certificate authorities that sign the certificate of the token endpoint. Without it, the system roots are used. The gateway reads it at start. |
| `timeout` | Optional. A Go duration. The default is `10s` and the most is `60s`. It limits one exchange. |

The gateway refuses to start if `tokenExchanges` is set and `secretsKeysetFile` is not. The gateway encrypts each exchanged token on its way to the leader replica. See [User secrets](user-secrets.md) for the keyset. The chart fails the render if an item has no `name`, `tokenURL`, `clientID` or `clientSecretFile`, if a `name` is not valid, or if a `tokenURL` does not start with `https://`.

Mount the client secret and the `caFile` with `resources.persistence` and `advancedMounts.gateway.gateway`, as for resolvers. The [configuration reference](configuration.md) and the [chart values](chart-values.md) list every setting.

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

Only a signed-in user has a token to exchange. If the session that starts the instance belongs to a service account or a SPIFFE workload, the instance fails at once.

## Lifetime

The `access_token_validity` of the target provider sets the lifetime of the token. Use `minutes=10` to `minutes=30`. A shorter value stops idle instances sooner. With less than `minutes=4`, less than 2 minutes remain between the refresh and the expiry, which is close to the delay of the kubelet. With less than 2 minutes, every exchange fails.

## Threat model

The gateway is trusted to act for active users only. The gateway exchanges each token with the token of one request of that user. The new token is no broader than what the target provider gives that user. The gateway never extends a token when no request is behind it.

If someone compromises the gateway, they can exchange the tokens of users who send requests during the compromise. A server can read its own token. Give the target provider only the permissions that the server needs. Agents also hold user tokens, but they cannot exchange them, because the client is confidential.

## Check before you deploy

The design was not tested against a live Authentik. Before you use a template in production, do this check:

1. Exchange one token by hand against your Authentik with the configuration above.
2. Decode the token. Make sure that `iss` is the issuer of the target provider, that the `kid` is in the JWKS of the target provider, and that the claims hold the permission of the user.
3. If one of these fails, change the Authentik configuration. toolgate needs no change, because it never checks the issuer or the key of the exchanged token.
