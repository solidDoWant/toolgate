# claude.ai connectors

For: admins who want users to connect to toolgate from a claude.ai custom connector.

A custom connector is a remote MCP server that a user adds in claude.ai. claude.ai signs the user in with OAuth 2.0 and sends the user's access token with each MCP request. toolgate does not issue tokens. Your identity provider (IdP) signs the user in, and the gateway checks the access token as it checks every token.

The gateway tells the connector where to sign in. An MCP request without a valid token gets HTTP 401 with this header:

```
WWW-Authenticate: Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/mcp"
```

That URL serves the protected resource metadata (RFC 9728). This is a public JSON document. It names the MCP endpoint, your IdP as the authorization server, and the scopes of `oidcCLIScopes`. The gateway serves the same document at `/.well-known/oauth-protected-resource` too.

This page uses Authentik as the IdP. The names below come from the Authentik documentation (version 2026.8) and the claude.ai connector documentation. Check them against the versions that you run.

## 1. Authentik

Create an OAuth2/OpenID provider for the connector with these settings:

- Client type: confidential.
- Redirect URIs: `https://claude.ai/api/mcp/auth_callback`.
- Scope mappings: `openid`, `email`, `profile` and `offline_access`.
- Signing Key: select one. Access tokens must be JWTs that the JWKS of the provider can verify.
- Subject mode: the same as the provider of the toolgate CLI.
- Issuer mode: "Same identifier is used for all providers". The gateway accepts tokens from one issuer only, `oidcIssuerURL`. So the connector's provider must have the same issuer as the CLI's provider.

Bind the provider to an application. Give the application the same access policy as the application of toolgate.

Authentik has no dynamic client registration. So claude.ai cannot register a client itself, and you give it the client ID and secret of this provider.

## 2. The gateway

Add the client ID of the new provider to the audiences. The first item is the audience that the CLI already uses. Then set the URL of the MCP endpoint as clients on the internet reach it:

```yaml
config:
  oidc:
    audience: [toolgate, claude-connector]
  mcpPublicURL: https://mcp.example.com/mcp
```

`mcpPublicURL` must be `https://`. If it is empty, the gateway uses `config.ui.publicURL` + `/mcp`. If both are empty, the gateway serves no metadata, and its 401 header has no `resource_metadata`. See the [configuration reference](configuration.md).

A connector token is checked as every user token: its signature against the JWKS of the IdP, its issuer, its expiry, and its audience. If its audience is not in `oidcAudience`, the request gets 401 and an `auth.refuse` event with the reason "audience not allowed".

## 3. claude.ai

An organization owner adds the connector. In a Team or Enterprise plan, go to Organization settings, then Connectors. Click Add, then Custom, then Web. In a Pro or Max plan, go to Customize, then Connectors, then Add custom connector.

1. Set the URL to the value of `mcpPublicURL`.
2. Under the OAuth client setting, select "Use your own OAuth client".
3. Enter the client ID and the client secret of the Authentik provider.

Each user then signs in through Authentik when they connect.

## 4. Expose the endpoint

Route only three paths from the internet to the gateway:

- `/mcp`.
- `/.well-known/oauth-protected-resource/mcp`.
- `/.well-known/oauth-protected-resource`.

Keep the web UI (`/ui/`), the API (`/api/v1/`), the OTLP routes and the metrics port internal. The chart has no value for this. Add the route yourself, as you add any ingress. This example `HTTPRoute` matches the three paths and nothing else:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: toolgate-mcp
  namespace: toolgate-system
spec:
  parentRefs:
    - name: internet-gateway
      namespace: gateway-system
  hostnames:
    - mcp.example.com
  rules:
    - matches:
        - path:
            type: Exact
            value: /mcp
        - path:
            type: Exact
            value: /.well-known/oauth-protected-resource/mcp
        - path:
            type: Exact
            value: /.well-known/oauth-protected-resource
      backendRefs:
        - name: toolgate
          port: 80
```

Change `parentRefs` and `hostnames` to the names of your cluster. The Service has the full name of the release, `toolgate` for a release named `toolgate`.

## 5. Sessions and session affinity

A connector does not send the `X-Toolgate-Session` header of the toolgate plugin. So the gateway ties the toolgate session of a connector to its MCP session (the `Mcp-Session-Id` header). Each connection has its own session, with its own enabled servers. The session has the claimed workspace `claimed/default`, so it is not verified. The settings for unverified sessions decide what it can use. In the audit log, its session ID is `mcp.` followed by the `Mcp-Session-Id`.

The session ends when the connector deletes its MCP session, or when the MCP session has no requests for 30 minutes. The gateway then writes `session.end` and stops the per-session instances of the session. The session never resumes.

The gateway stores each MCP session in PostgreSQL when the connector sends `initialize`. A replica that gets a request for a session it does not hold reads the session and serves the request. So you do not need session affinity. A request can reach any replica. A replica can stop or restart, and the connector keeps its session and its enabled servers. Only the requests that were in progress on the stopped replica fail.

A GET stream of the connector stays open on one replica. When that replica stops, the connector opens the stream again on another replica. The new stream gets a `list_changed` notification at once, so the connector does not miss a change that happened in between.

Do not configure affinity on the `Mcp-Session-Id` header. Affinity does not make sessions more correct. It only means that fewer requests change the replica. The chart value `config.service.sessionAffinity` is `None` by default.

## What users need to know

- Approval and step-up links point at `config.ui.publicURL`, which stays internal. A connector user opens these links in a browser that can reach the internal URL.
- The sign-in pages and the token endpoint of the IdP must be reachable from the browser of the user and from the servers of claude.ai.

See [the security model](security-model.md#connectors-on-the-internet) for what changes when the MCP endpoint is on the internet.
