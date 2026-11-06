# Coder workspaces

For: admins who run Coder workspaces and want each user's workspace to sign in to toolgate without `toolgate login`.

Coder can hold the user's consent to an identity provider (IdP) and give the workspace an access token. This is Coder's external auth. toolgate reads that token with the `TOOLGATE_TOKEN_COMMAND` variable. The sessions in the workspace then act as the user. If you also bind the workspace with an attestation, the sessions are the user's, attested, in a workspace such as `coder/alice/dev`.

This page uses Authentik as the IdP. The names below come from the Coder and Authentik documentation. Check them against the versions that you run.

## Security note

The gateway accepts the access tokens of every client in `oidcAudience`. Make a provider for toolgate in Authentik. Never use the provider that Coder uses for its own sign-in.

The IdP must give every client the same `sub` for one person. Without it, the same person has two identities in toolgate. See [Web UI](web-ui.md) for the same rule for the web UI.

## 1. Authentik

Create an OAuth2/OpenID provider for Coder with these settings:

- Client type: confidential.
- Redirect URI: `https://coder.example.com/external-auth/toolgate/callback`.
- Scope mappings: `openid`, `email`, `profile` and `offline_access`.
- Signing key: select one. Access tokens must be JWTs that the JWKS of the provider can verify.
- Subject mode: the same as the provider of the toolgate CLI.
- Access token validity: at least 5 minutes.

Bind the provider to an application. Give the application the same access policy as the application of toolgate.

## 2. The gateway

Add the client ID of the new provider to the audiences. The first item is the audience that the CLI already uses.

```yaml
config:
  oidc:
    audience: [toolgate, coder-toolgate]
```

## 3. Coder

Set these variables on the Coder server. Take the client secret from a Kubernetes Secret.

```sh
CODER_EXTERNAL_AUTH_0_ID=toolgate
CODER_EXTERNAL_AUTH_0_TYPE=toolgate
CODER_EXTERNAL_AUTH_0_DISPLAY_NAME=toolgate
CODER_EXTERNAL_AUTH_0_CLIENT_ID=coder-toolgate
CODER_EXTERNAL_AUTH_0_CLIENT_SECRET=...
CODER_EXTERNAL_AUTH_0_AUTH_URL=https://auth.example.com/application/o/authorize/
CODER_EXTERNAL_AUTH_0_TOKEN_URL=https://auth.example.com/application/o/token/
CODER_EXTERNAL_AUTH_0_VALIDATE_URL=https://auth.example.com/application/o/userinfo/
CODER_EXTERNAL_AUTH_0_SCOPES="openid email profile offline_access"
```

Coder refreshes the token with the refresh token that it holds.

## 4. The template

Add this block to the Terraform of the workspace template. Coder then asks the user to link the account when the user creates the workspace.

```hcl
data "coder_external_auth" "toolgate" {
  id = "toolgate"
}
```

Set these variables in the `env` of the `coder_agent` resource:

```hcl
env = {
  TOOLGATE_TOKEN_COMMAND = "coder external-auth access-token toolgate"
  TOOLGATE_GATEWAY       = "https://toolgate.example.com"
}
```

To attest the workspace as well, set the attestation variables from the user documentation next to these two.

## What the user sees

If the user did not link the account, the command prints its own message, for example a link to the Coder page. toolgate shows that message and the error. See the user documentation for the errors.
