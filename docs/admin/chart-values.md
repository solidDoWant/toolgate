# Chart values reference

For: admins who install the toolgate Helm chart.

This page is generated from `deploy/charts/toolgate/values.yaml` by `make generate-docs`. Do not edit it by hand.

[The chart page](chart.md) tells you what the chart installs and why. [The quick start](quick-start.md) shows an install.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| config.audit.payloads | bool | `false` | Store audit payloads: the arguments of each tool call and approval request, and each tool result. Payloads under 64 KiB go in PostgreSQL, larger ones in the S3 bucket. If true, `config.audit.s3.bucket` is required. |
| config.audit.s3.accessKeyIDKey | string | `"accessKeyID"` | Key in `config.audit.s3.existingSecret` that holds the access key ID. |
| config.audit.s3.bucket | string | `""` | S3 bucket for large audit payloads. Required when `config.audit.payloads` is true. |
| config.audit.s3.endpoint | string | `""` | URL of the S3 API, such as `https://s3.example.com`. Empty means the AWS S3 endpoint of `config.audit.s3.region`. |
| config.audit.s3.existingSecret | string | `""` | Name of an existing Secret in the release namespace with the S3 access key. Empty means the gateway uses the other credential sources of the AWS SDK, such as a web identity token. |
| config.audit.s3.pathStyle | bool | `false` | Put the bucket name in the URL path instead of the host name. Many S3-compatible servers need true. |
| config.audit.s3.prefix | string | `""` | Prefix of the payload object keys, such as `toolgate/`. |
| config.audit.s3.region | string | `""` | S3 region of the bucket, such as `us-east-1`. |
| config.audit.s3.secretAccessKeyKey | string | `"secretAccessKey"` | Key in `config.audit.s3.existingSecret` that holds the secret access key. |
| config.catalogNamespaces | list | `[]` | Namespaces whose `MCPServerTemplate` and `ToolgateRule` objects the gateway reads, such as `[toolgate-catalog]`. Empty means the release namespace. |
| config.crds.enabled | bool | `true` | Install toolgate's CRDs with the chart. `helm upgrade` updates them. If false, install them yourself. |
| config.crds.keep | bool | `true` | Keep the CRDs, and so every template, rule and settings profile, when the release is uninstalled (`helm.sh/resource-policy: keep`). |
| config.database.existingSecret | string | `""` | Name of an existing Secret in the release namespace that holds the PostgreSQL connection string. Use it for any string with a password. Set exactly one of this and `config.database.url`. |
| config.database.migrateOnStart | bool | `true` | Whether the gateway creates and updates its tables when it starts. Set false when the gateway's database user has data rights only. Then run the migrations with `config.database.migrationSecret`, or run `toolgate migrate` yourself. |
| config.database.migrationSecret | string | `""` | Name of an existing Secret in the release namespace that holds the connection string of the database role that owns the tables. When it is set and `config.database.migrateOnStart` is false, a Helm hook Job runs `toolgate migrate` with it before every install and upgrade. Hooks need Helm or Flux: `helm template | kubectl apply` skips them. |
| config.database.migrationSecretKey | string | `"url"` | Key in `config.database.migrationSecret` that holds the connection string. |
| config.database.migrationUrl | string | `""` | The connection string of the database role that owns the tables, inline. Use it only for a string with no password. The value ends up in the Job spec and in the Helm release. Set at most one of this and `config.database.migrationSecret`; the Job renders as for the Secret. |
| config.database.secretKey | string | `"url"` | Key in `config.database.existingSecret` that holds the connection string. |
| config.database.url | string | `""` | The PostgreSQL connection string, inline. Use it only for a string with no password, such as client-certificate or token authentication. The value ends up in the Deployment spec and in the Helm release, where anyone who can read them sees it. Set exactly one of this and `config.database.existingSecret`. |
| config.extraEnv | list | `[]` | More environment variables for the gateway container, as a list of Kubernetes `EnvVar` objects. |
| config.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| config.image.repository | string | `"ghcr.io/soliddowant/toolgate"` | Gateway image repository. |
| config.image.tag | string | `""` | Image tag. Empty means the chart's `appVersion`. |
| config.imagePullSecrets | list | `[]` | Secrets that hold credentials for pulling the image, as a list of `{name: ...}` objects. |
| config.instanceOwnerDetails | bool | `true` | Put the owner's verified email and name on the annotations of instance namespaces and objects. Set it to `false` to keep personal data off cluster objects. The owner's subject is annotated either way. |
| config.limits.instances | int | `200` | Maximum number of MCP server instances that the gateway runs at the same time, for all users and shared instances together. At least 1. |
| config.limits.instancesPerUser | int | `20` | Maximum number of MCP server instances that run for one user at the same time. Per-session and per-user instances count for the user who started them. Shared instances do not count. At least 1. |
| config.logging.level | string | `"info"` | Lowest level of the JSON log lines: `debug`, `info`, `warn` or `error`. |
| config.mcpPublicURL | string | `""` | URL of the MCP endpoint as MCP clients on the internet reach it, such as `https://mcp.example.com/mcp`. The gateway names it in its OAuth protected resource metadata, so that claude.ai connectors can sign in. It must be `https://`. Empty means `config.ui.publicURL` + `/mcp`; with both empty, the gateway serves no metadata. See the connectors page of the admin docs. |
| config.metaTools.enableDisable | string | `"auto"` | Whether sessions list the `toolgate_enable_server` and `toolgate_disable_server` tools. `auto` lists them when tool search mode is off and hides them when it is on, because a call to a tool of an allowed server turns the server on. `always` lists them in every mode. `never` lists them in none. |
| config.oidc.additionalIssuers | list | `[]` | More issuers whose access tokens the gateway accepts, for an identity provider that gives each client its own issuer (Authentik does). At most 8. Each item has an `issuerURL`, exactly as in its tokens' `iss` claim, and an `audience`, one value or a list of 1 to 8. Every issuer must give one person the same `sub` claim. The CLI and the web UI still use `config.oidc.issuerURL`. |
| config.oidc.audience | string or list | `""` | Audiences that users' access tokens may be for: one value such as `toolgate`, or a list of 1 to 8 such as `[toolgate, coder-toolgate]`. Each is the client ID of a client at `config.oidc.issuerURL`. Required. The gateway accepts access tokens of every listed client, so list only clients made for toolgate. |
| config.oidc.cliClientID | string | `""` | OIDC client ID that `toolgate login` signs in with, such as `toolgate-cli`. It must be a public client with the device authorization grant. The gateway publishes it, so users need no `--client-id`. It is usually also in `config.oidc.audience`. It may be the same client as `config.ui.clientID`. |
| config.oidc.cliScopes | list | `[]` | Scopes that `toolgate login` asks for. Empty means the gateway's default, `openid email profile offline_access`. |
| config.oidc.emailClaim | string | `""` | Access token claim that holds the user's email address. Empty means the gateway's default, `email`. |
| config.oidc.emailVerifiedClaim | string | `""` | Access token claim that tells whether the identity provider verified the email address. Empty means the gateway's default, `email_verified`. |
| config.oidc.groupsClaim | string | `""` | Access token claim that holds the user's groups. Empty means the gateway's default, `groups`. |
| config.oidc.issuerURL | string | `""` | Issuer URL of the OIDC identity provider that users sign in with, such as `https://idp.example.com/realms/main`. Required. |
| config.otlpEndpoint | string | `""` | OTLP/HTTP endpoint for the gateway's traces, such as `http://otel-collector:4318`. Empty turns trace export off. |
| config.replicas | int | `2` | Number of gateway pods. Every replica serves requests; one at a time holds the leader Lease and runs the runtime reconciler. |
| config.resolvers | list | `[]` | Resolvers: HTTP services a template's `${resolver:<name>}` gets a value from. Each item has a `name`, an `https://` `url`, an `audience`, an optional `timeout` (default `10s`, at most `60s`) and an optional `caFile`. The chart writes them to the gateway's configuration file and mounts a service-account token with each resolver's audience at `/var/run/secrets/toolgate/resolvers/<name>/token`. Mount a `caFile` with `resources.persistence`. |
| config.service.sessionAffinity | string | `"None"` | Session affinity of the gateway's Service: `None` or `ClientIP`. toolgate does not need affinity to be correct. `ClientIP` makes replica switches rare. |
| config.serviceAccount.name | string | `""` | Name of the gateway's service account. Empty means the release's full name. The RBAC rules and the admission policy apply to this account. |
| config.serviceMonitor.enabled | bool | `false` | Create a Prometheus Operator `ServiceMonitor` for the metrics Service. The `ServiceMonitor` CRD must be installed. |
| config.sessionIdleTimeout | string | `"2h"` | How long a session can go without requests before the gateway ends it and stops its per-session MCP servers, as a Go duration such as `2h`. |
| config.sessionResumeWindow | string | `"24h"` | How long the gateway keeps the list of enabled servers of an ended session, as a Go duration from `1h` to `168h` (7 days). |
| config.sharedNamespace.create | bool | `true` | Create the shared namespace with toolgate's labels. If false, the namespace must exist and carry the labels `toolgate.soliddowant.dev/writable: "true"` and `pod-security.kubernetes.io/enforce: restricted`. |
| config.sharedNamespace.name | string | `""` | Name of the namespace for instances of `namespace: shared` templates. Empty means the chart creates none. It must not start with `tg-`. |
| config.spiffe.trustDomains | list | `[]` | SPIFFE trust domains whose JWT-SVIDs may sign in, attested as the workload. Each item has a `name` (the trust domain), exactly one of `bundleEndpointURL` (a SPIFFE bundle endpoint with the `https_web` profile) and `jwksURL` (a JWKS document, such as that of SPIRE's OIDC discovery provider), both `https://`, and an optional `caFile`. The chart writes them to the gateway's configuration file. Mount a `caFile` with `resources.persistence`. |
| config.toolSearch | bool | `false` | Turn on tool search mode for sessions whose client sends no `X-Toolgate-Tool-Search` header. A client can still turn it on or off with that header. It only changes which tools a session lists. |
| config.toolSearchEmbeddings.apiKeyFile | string | `""` | Path of a file that holds an API key for the endpoint, mounted from a Secret with `persistence`. Empty means no key. |
| config.toolSearchEmbeddings.model | string | `""` | Model name sent to the endpoint, such as `BAAI/bge-small-en-v1.5`. Required when `url` is set. Changing it embeds all tools again. |
| config.toolSearchEmbeddings.timeout | string | `""` | How long one request to the endpoint may take, as a Go duration. Empty means 2s. |
| config.toolSearchEmbeddings.url | string | `""` | Base URL of an endpoint that speaks the OpenAI embeddings API (`POST <url>/v1/embeddings`), such as `http://tei.tools.svc:80`. Empty means search uses words only. |
| config.ui.adminGroup | string | `""` | Group whose members are web UI admins. Empty means nobody is an admin. |
| config.ui.approvalTimeout | string | `"10m"` | How long a call that needs approval waits for a person, and how long an approved request stays usable, as a Go duration from `1m` to `1h`. |
| config.ui.clientID | string | `""` | OIDC client ID of the web UI. The web UI is on only when this is set. The identity provider must allow the redirect URI `config.ui.publicURL` + `/ui/auth/callback`. |
| config.ui.clientSecretFile | string | `""` | Path of a file that holds the web UI client's secret, mounted from a Secret with `persistence`. Empty means a public client. |
| config.ui.publicURL | string | `""` | URL where users open the gateway in a browser, such as `https://toolgate.example.com`. Required when `config.ui.clientID` is set. It must be `https://`. |
| config.ui.scopes | list | `[]` | Scopes the web UI asks for at sign-in. Empty means the gateway's default, `openid email profile`. |
| config.workspaceAttestation | string | `nil` | Attested user sessions: a user's token plus a workload token from a workspace pod (or a SPIFFE workload) bound to that user, so the session's workspace is verified. Off when null. It has `namespaces` (required), `keys` (`userID`, `email`, `username` and `workspace`: pod label keys, Coder's by default), `match` (`by: email`, or `by: claim` with `claim` and `key`), `workspaceID` (such as `coder/{username}/{workspace}`) and an optional `spiffeIDPattern`. The chart writes it to the gateway's configuration file. See docs/admin/workspace-attestation.md. |
| dashboards.annotations | object | `{}` | Annotations of the dashboard ConfigMaps, such as `grafana_folder: toolgate` for the sidecar's folder annotation. |
| dashboards.enabled | bool | `false` | Create one ConfigMap for each dashboard, for the Grafana dashboard sidecar. Ignored when `dashboards.grafanaOperator.enabled` is true. |
| dashboards.grafanaOperator.allowCrossNamespaceImport | bool | `false` | Namespaces whose Grafana instances may load the dashboards: `allowCrossNamespaceImport`. |
| dashboards.grafanaOperator.enabled | bool | `false` | Create a grafana-operator v5 `GrafanaDashboard` for each dashboard instead of a ConfigMap. |
| dashboards.grafanaOperator.folder | string | `""` | Grafana folder for the dashboards. Empty means the General folder. |
| dashboards.grafanaOperator.instanceSelector | object | `{"matchLabels":{}}` | Which Grafana instances load the dashboards: the `matchLabels` of the `GrafanaDashboard` `instanceSelector`. Required with `dashboards.grafanaOperator.enabled`. |
| dashboards.labelKey | string | `"grafana_dashboard"` | Key of the label the sidecar selects ConfigMaps by. |
| dashboards.labelValue | string | `"1"` | Value of that label. |
| embeddings.downloader.hubVersion | string | `"1.0.0"` | Version of huggingface_hub that `uvx` installs. |
| embeddings.downloader.image.digest | string | `"sha256:531f855bda2c73cd6ef67d56b733b357cea384185b3022bd09f05e002cd144ca"` |  |
| embeddings.downloader.image.repository | string | `"ghcr.io/astral-sh/uv"` | Image that runs the model download Job: a pinned `uv`, which runs the `hf` command of huggingface_hub. |
| embeddings.downloader.image.tag | string | `"0.9-python3.13-bookworm-slim"` |  |
| embeddings.downloader.ttlSecondsAfterFinished | int | `3600` | Seconds a finished Job stays before Kubernetes removes it. |
| embeddings.server.cache.accessModes | list | `["ReadWriteMany"]` | Access modes of the claim the chart creates. |
| embeddings.server.cache.existingClaim | string | `""` | Name of an existing PersistentVolumeClaim for the model files. Recommended: an RWX claim you create, so it survives uninstall and the pre-upgrade Job can mount it while the server runs. If empty, the chart creates a claim `<release>-toolgate-models` that Helm keeps. |
| embeddings.server.cache.size | string | `"5Gi"` | Size of the claim the chart creates. |
| embeddings.server.cache.storageClassName | string | `""` | Storage class of the claim the chart creates. Empty means the cluster default. |
| embeddings.server.enabled | bool | `false` | Run the embeddings server. |
| embeddings.server.image.digest | string | `"sha256:2538ea1c9640d3763b15af668039d24172d063b42337b0c27796fc2be180c78d"` | Digest of the image. Pins it. |
| embeddings.server.image.repository | string | `"ghcr.io/huggingface/text-embeddings-inference"` | Image of text-embeddings-inference. The CPU image is x86-64 only; set another image for ARM64. |
| embeddings.server.image.tag | string | `"cpu-1.9"` |  |
| embeddings.server.model | string | `"BAAI/bge-small-en-v1.5"` | Hugging Face model the server loads. |
| embeddings.server.modelRevision | string | `"5c38ec7c405ec4b44b94cc5a9bb96e735b38267a"` | Commit of the model repository, a full 40-character SHA. The hook Job downloads exactly this revision. |
| embeddings.server.resources | object | `{"limits":{"memory":"768Mi"},"requests":{"cpu":"500m","memory":"768Mi"}}` | Memory request equals the limit; there is no CPU limit. About 320 MiB are in use after the model is loaded and has served requests. |
| embeddings.server.scaleToZero.enabled | bool | `false` | Let the gateway scale the embeddings server to zero replicas when tool search is idle, and back to one when it is needed. The Deployment then has no `replicas` field. Can't be used with `config.toolSearchEmbeddings.url`. See `docs/admin/chart.md`. |
| embeddings.server.scaleToZero.idleTimeout | string | `"15m"` | How long tool search must be idle before the gateway scales the server to zero. A duration in hours, minutes and seconds, such as `15m` or `1h30m`, from `1m` to `24h`. |
| rbac.extraRules | list | `[]` | More RBAC rules for the gateway's ClusterRole, added after the default rules. Grant each extra kind a template contains `create`, `patch`, `delete`, `get`, `list` and `watch`. The admission policy still confines every write to toolgate's namespaces. |
| resources | object | see `values.yaml` | The gateway's Deployment, Services, ServiceAccount, PodDisruptionBudget and ServiceMonitor, in the values format of the bjw-s `common` library chart (https://bjw-s-labs.github.io/helm-charts/docs/common-library/). Override any part, such as probes, affinity, resources or extra containers. String values can use Helm templates. The RBAC rules (see `rbac.extraRules`) and the admission policy are not here. |
