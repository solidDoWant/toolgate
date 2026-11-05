# Gateway configuration reference

For: admins who deploy the toolgate gateway.

This page is generated from the code by `make generate-docs`. Do not edit it by hand.

The gateway reads its configuration when `toolgate serve` starts, in this order:

1. The default value of each setting.
2. The YAML file named by the `--config` flag, or else by the `TOOLGATE_CONFIG` environment variable. The file is optional. An unknown key in the file is an error.
3. The environment variables. A variable that is set and not empty replaces the value from the file.

If a setting is missing or not valid, the gateway does not start. It prints one line for each problem and exits with code 1.

| YAML key | Environment variable | Default | Description |
|---|---|---|---|
| `listenAddress` | `TOOLGATE_LISTEN_ADDRESS` | `:8080` | The address where the gateway listens for MCP and API requests and for the `/healthz` and `/readyz` probes. |
| `metricsListenAddress` | `TOOLGATE_METRICS_LISTEN_ADDRESS` | `:9090` | The address of the separate listener that serves Prometheus metrics at `/metrics`. It must differ from `listenAddress`. |
| `databaseURL` | `TOOLGATE_DATABASE_URL` | none | The PostgreSQL connection string, as a URL or as key/value pairs. Required. `/readyz` answers 503 while the gateway cannot connect to this database. |
| `shutdownTimeout` | `TOOLGATE_SHUTDOWN_TIMEOUT` | `30s` | After SIGTERM, the gateway stops accepting requests and waits this long for open requests to finish. Then it stops. Write it as a Go duration, such as `30s`. |
| `logLevel` | `TOOLGATE_LOG_LEVEL` | `info` | The lowest level of the JSON log lines that the gateway writes to stdout: `debug`, `info`, `warn` or `error`. |
| `otlpEndpoint` | `TOOLGATE_OTLP_ENDPOINT` | none | The OTLP/HTTP endpoint that receives the traces of the gateway, such as `http://otel-collector:4318`. The gateway sends traces to `/v1/traces` under this URL. If it is empty, the gateway does not export traces. |
| `oidcIssuerURL` | `TOOLGATE_OIDC_ISSUER_URL` | none | The issuer URL of the OIDC identity provider (IdP) that users sign in with, such as `https://idp.example.com/realms/main`. Required. The gateway reads the IdP's discovery document and signing keys from it, and accepts only access tokens this issuer signed. |
| `oidcAudience` | `TOOLGATE_OIDC_AUDIENCE` | none | The audience that users' access tokens must contain, such as `toolgate`. Required. The gateway refuses a token for another audience. |
| `oidcEmailClaim` | `TOOLGATE_OIDC_EMAIL_CLAIM` | `email` | The access token claim that holds the user's email address. |
| `oidcEmailVerifiedClaim` | `TOOLGATE_OIDC_EMAIL_VERIFIED_CLAIM` | `email_verified` | The access token claim that tells whether the IdP verified the email address. The gateway uses the email only when this claim is `true`. |
| `oidcGroupsClaim` | `TOOLGATE_OIDC_GROUPS_CLAIM` | `groups` | The access token claim that holds the user's groups, as a list of strings. The gateway refuses a token if this claim has another type. |
| `oidcCLIClientID` | `TOOLGATE_OIDC_CLI_CLIENT_ID` | none | The client ID that `toolgate login` signs in with, such as `toolgate-cli`. It must be a public client (one without a secret) with the OAuth 2.0 device authorization grant. The gateway publishes it, with `oidcIssuerURL`, `oidcCLIScopes` and `oidcEmailClaim`, at `/.well-known/toolgate-client` without authentication. If it is empty, users must pass `--client-id` to `toolgate login`. |
| `oidcCLIScopes` | `TOOLGATE_OIDC_CLI_SCOPES` | `["openid","email","profile","offline_access"]` | The scopes that `toolgate login` asks for. In the environment variable, separate the scopes with commas. Keep `offline_access`: without it, the IdP gives no refresh token and users must sign in again when their access token expires. |
| `serviceAccountNamespaces` | `TOOLGATE_SERVICE_ACCOUNT_NAMESPACES` | none | The namespaces whose pods may sign in with a Kubernetes service-account token, such as `[ci, agents]`. In the environment variable, separate the names with commas. A pod signs in with a projected token for the audience `toolgate`, and its workspace is the pod's owner. If the list is empty, only users can sign in. |
| `secretsKeysetFile` | `TOOLGATE_SECRETS_KEYSET_FILE` | none | The path of the Tink keyset that encrypts user secrets, in the JSON keyset format of Tink. Mount it from a Kubernetes Secret. It is never stored in PostgreSQL. The gateway serves the secrets API (`/api/v1/secrets`) only when this is set. `toolgate admin rewrap` needs it too. See [User secrets](user-secrets.md). |
