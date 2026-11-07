# Coder workspaces

For: admins who run Coder workspaces and want each user's workspace to sign in to toolgate without `toolgate login`.

Coder can hold the user's consent to an identity provider (IdP) and give the workspace an access token. This is Coder's external auth. toolgate reads that token with a token command, which `toolgate configure --token-command` saves. The sessions in the workspace then act as the user. If you also bind the workspace with an attestation, the sessions are the user's, attested, in a workspace such as `coder/alice/dev`.

This page uses Authentik as the IdP. The names below come from the Coder and Authentik documentation. Check them against the versions that you run.

## Security note

The gateway accepts the access tokens of every client in its audiences. Make a provider for toolgate in Authentik. Never use the provider that Coder uses for its own sign-in.

The IdP must give every client the same `sub` for one person. Without it, the same person has two identities in toolgate. This rule also applies when each client has its own issuer. In Authentik, set the subject mode of every provider to `Based on the User's hashed ID` (`hashed_user_id`). That mode gives one user the same `sub` in every provider. See [Web UI](web-ui.md) for the same rule for the web UI.

## 1. Authentik

Create an OAuth2/OpenID provider for Coder with these settings:

- Client type: confidential.
- Redirect URI: `https://coder.example.com/external-auth/toolgate/callback`.
- Scope mappings: `openid`, `email`, `profile` and `offline_access`.
- Signing key: select one. Access tokens must be JWTs that the JWKS of the provider can verify.
- Subject mode: the same as the provider of the toolgate CLI, for example `Based on the User's hashed ID`.
- Access token validity: at least 5 minutes.

Bind the provider to an application. Give the application the same access policy as the application of toolgate.

## 2. The gateway

Authentik gives every provider its own issuer (the URL that the IdP writes in the `iss` claim of its tokens). The issuer of a provider is `https://auth.example.com/application/o/<application slug>/`. For example, the provider of the CLI has the issuer `https://auth.example.com/application/o/toolgate/`, and the provider for Coder has the issuer `https://auth.example.com/application/o/coder-toolgate/`.

Add the issuer of the new provider to `config.oidc.additionalIssuers`, with its client ID as the audience. Keep `config.oidc.issuerURL` on the provider of the CLI and the web UI. The gateway gives that issuer to `toolgate login`.

```yaml
config:
  oidc:
    issuerURL: https://auth.example.com/application/o/toolgate/
    audience: toolgate
    additionalIssuers:
      - issuerURL: https://auth.example.com/application/o/coder-toolgate/
        audience: coder-toolgate
```

The gateway selects the issuer by the `iss` claim of the token. Then it checks the token with the signing keys and the audiences of that issuer only. The gateway refuses a token from an issuer that is not in this configuration. If one issuer is down, the gateway still accepts the tokens of the other issuers. The gateway must reach `config.oidc.issuerURL` when it starts.

If your IdP gives all clients one issuer (Keycloak and Okta do), do not add an issuer. Add the client ID to the audiences instead: `audience: [toolgate, coder-toolgate]`.

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

Claude Code removes environment variables with credential-like names from the toolgate plugin's helpers. It removes `TOOLGATE_TOKEN_COMMAND` and `CODER_AGENT_TOKEN`, for example. For the list, see [Claude Code plugin helpers](workspace-attestation.md#claude-code-plugin-helpers). Because of this, the template saves the settings in the toolgate configuration file with `toolgate configure`, and the command reads the agent token from a file.

Add these lines to the `startup_script` of the `coder_agent` resource. Put the toolgate CLI on the `PATH` before these lines.

```hcl
startup_script = <<-EOT
  install -d -m 700 "$HOME/.config/coder"
  (umask 077 && printf '%s' "$CODER_AGENT_TOKEN" > "$HOME/.config/coder/agent-token")
  toolgate configure \
    --gateway https://toolgate.example.com \
    --token-command "coder external-auth access-token toolgate --agent-token-file $HOME/.config/coder/agent-token"
EOT
```

The agent token file has mode 0600. The command does not need the `CODER_AGENT_TOKEN` variable. `CODER_AGENT_URL` stays in the environment of the helpers, so the command can use it.

To attest the workspace as well, add `--attestation-token-file` or `--attestation-token-request` to the `toolgate configure` command. See [workspace attestation](workspace-attestation.md).

## What the user sees

If the user did not link the account, the command prints its own message, for example a link to the Coder page. toolgate shows that message and the error. See the user documentation for the errors.
