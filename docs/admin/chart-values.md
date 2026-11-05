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
| config.database.existingSecret | string | `""` | Name of an existing Secret in the release namespace that holds the PostgreSQL connection string. Required. |
| config.database.secretKey | string | `"url"` | Key in `config.database.existingSecret` that holds the connection string. |
| config.extraEnv | list | `[]` | More environment variables for the gateway container, as a list of Kubernetes `EnvVar` objects. |
| config.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| config.image.repository | string | `"ghcr.io/soliddowant/toolgate"` | Gateway image repository. |
| config.image.tag | string | `""` | Image tag. Empty means the chart's `appVersion`. |
| config.imagePullSecrets | list | `[]` | Secrets that hold credentials for pulling the image, as a list of `{name: ...}` objects. |
| config.limits.instances | int | `200` | Maximum number of MCP server instances that the gateway runs at the same time, for all users and shared instances together. At least 1. |
| config.limits.instancesPerUser | int | `20` | Maximum number of MCP server instances that run for one user at the same time. Per-session and per-user instances count for the user who started them. Shared instances do not count. At least 1. |
| config.logging.level | string | `"info"` | Lowest level of the JSON log lines: `debug`, `info`, `warn` or `error`. |
| config.oidc.audience | string | `""` | Audience that users' access tokens must contain, such as `toolgate`. Required. |
| config.oidc.emailClaim | string | `""` | Access token claim that holds the user's email address. Empty means the gateway's default, `email`. |
| config.oidc.emailVerifiedClaim | string | `""` | Access token claim that tells whether the identity provider verified the email address. Empty means the gateway's default, `email_verified`. |
| config.oidc.groupsClaim | string | `""` | Access token claim that holds the user's groups. Empty means the gateway's default, `groups`. |
| config.oidc.issuerURL | string | `""` | Issuer URL of the OIDC identity provider that users sign in with, such as `https://idp.example.com/realms/main`. Required. |
| config.otlpEndpoint | string | `""` | OTLP/HTTP endpoint for the gateway's traces, such as `http://otel-collector:4318`. Empty turns trace export off. |
| config.replicas | int | `2` | Number of gateway pods. Every replica serves requests; one at a time holds the leader Lease and runs the runtime reconciler. |
| config.service.sessionAffinity | string | `"None"` | Session affinity of the gateway's Service: `None` or `ClientIP`. toolgate does not need affinity to be correct. `ClientIP` makes replica switches rare. |
| config.serviceAccount.name | string | `""` | Name of the gateway's service account. Empty means the release's full name. The RBAC rules and the admission policy apply to this account. |
| config.serviceMonitor.enabled | bool | `false` | Create a Prometheus Operator `ServiceMonitor` for the metrics Service. The `ServiceMonitor` CRD must be installed. |
| config.sessionIdleTimeout | string | `"2h"` | How long a session can go without requests before the gateway ends it and stops its per-session MCP servers, as a Go duration such as `2h`. |
| config.sharedNamespace.create | bool | `true` | Create the shared namespace with toolgate's labels. If false, the namespace must exist and carry the labels `toolgate.soliddowant.dev/writable: "true"` and `pod-security.kubernetes.io/enforce: restricted`. |
| config.sharedNamespace.name | string | `""` | Name of the namespace for instances of `namespace: shared` templates. Empty means the chart creates none. It must not start with `tg-`. |
| rbac.extraRules | list | `[]` | More RBAC rules for the gateway's ClusterRole, added after the default rules. Grant each extra kind a template contains `create`, `patch`, `delete`, `get`, `list` and `watch`. The admission policy still confines every write to toolgate's namespaces. |
| resources | object | see `values.yaml` | The gateway's Deployment, Services, ServiceAccount, PodDisruptionBudget and ServiceMonitor, in the values format of the bjw-s `common` library chart (https://bjw-s-labs.github.io/helm-charts/docs/common-library/). Override any part, such as probes, affinity, resources or extra containers. String values can use Helm templates. The RBAC rules (see `rbac.extraRules`) and the admission policy are not here. |
