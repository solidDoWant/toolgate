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
| `databaseURL` | `TOOLGATE_DATABASE_URL` | none | The PostgreSQL connection string, as a URL or as key/value pairs. Required. When it starts, the gateway creates and updates its tables in this database. If it cannot, it stops with an error. `/readyz` answers 503 while the gateway cannot connect to this database. |
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
| `leaseNamespace` | `TOOLGATE_LEASE_NAMESPACE` | none | The namespace of the Lease that picks the one replica that runs the runtime reconciler, normally the gateway's own namespace. The chart sets it. If it is empty, the gateway runs without Kubernetes: it serves requests but never starts or stops MCP servers. |
| `catalogNamespaces` | `TOOLGATE_CATALOG_NAMESPACES` | none | The namespaces whose `MCPServerTemplate` and `ToolgateRule` objects the gateway reads, such as `[toolgate-catalog]`. In the environment variable, separate the names with commas. Objects in other namespaces are ignored. If the list is empty, the gateway reads only `leaseNamespace`. The gateway reads no catalog without `leaseNamespace`. |
| `gatewayPodLabels` | `TOOLGATE_GATEWAY_POD_LABELS` | none | The labels of the gateway's own pods, as `key=value` items, such as `[app.kubernetes.io/name=toolgate]`. In the environment variable, separate the items with commas. Each MCP server's network policy accepts connections only from pods in `leaseNamespace` with these labels. Required with `leaseNamespace`. The chart sets it. |
| `sharedNamespace` | `TOOLGATE_SHARED_NAMESPACE` | none | The namespace where the MCP servers of `namespace: shared` templates run, all together. The chart sets it from `config.sharedNamespace.name`. The namespace must exist and carry the label `app.kubernetes.io/managed-by: toolgate`. The gateway never creates, labels or deletes it. It must not start with `tg-`, the prefix of the namespaces the gateway makes for single servers. If it is empty, every `namespace: shared` template is invalid. |
| `instanceLimitPerUser` | `TOOLGATE_INSTANCE_LIMIT_PER_USER` | `20` | The maximum number of MCP server instances that run for one user at the same time. Per-session and per-user instances count for the user who started them. Shared instances do not count. When a user is at the limit, an enable that must start a new instance fails. An enable of a server that already runs for the user still works. At least 1. The chart sets it from `config.limits.instancesPerUser`. |
| `instanceLimit` | `TOOLGATE_INSTANCE_LIMIT` | `200` | The maximum number of MCP server instances that the gateway runs at the same time, for all users and shared instances together. At the limit, an enable that must start a new instance fails. At least 1. The chart sets it from `config.limits.instances`. |
| `sessionIdleTimeout` | `TOOLGATE_SESSION_IDLE_TIMEOUT` | `2h0m0s` | How long a session can go without requests before the gateway ends it. Ending a session disables all of its servers, so its per-session MCP servers stop. A later request with the same session ID starts the session again with no servers enabled. The gateway checks every 30 seconds. Write it as a Go duration, such as `2h`. It must be more than zero. The chart sets it from `config.sessionIdleTimeout`. |
