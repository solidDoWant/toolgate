# Chart values reference

For: admins who install the toolgate Helm chart.

This page is generated from `deploy/charts/toolgate/values.yaml` by `make generate-docs`. Do not edit it by hand.

[The chart page](chart.md) tells you what the chart installs and why. [The quick start](quick-start.md) shows an install.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity for the gateway pods. Empty means a preferred anti-affinity that spreads the replicas over nodes. |
| database.existingSecret | string | `""` | Name of an existing Secret in the release namespace that holds the PostgreSQL connection string. Required. |
| database.secretKey | string | `"url"` | Key in `database.existingSecret` that holds the connection string. |
| extraEnv | list | `[]` | More environment variables for the gateway container, as a list of Kubernetes `EnvVar` objects. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/soliddowant/toolgate"` | Gateway image repository. |
| image.tag | string | `""` | Image tag. Empty means the chart's `appVersion`. |
| imagePullSecrets | list | `[]` | Secrets that hold credentials for pulling the image. |
| logLevel | string | `"info"` | Lowest level of the JSON log lines: `debug`, `info`, `warn` or `error`. |
| nodeSelector | object | `{}` | Node selector for the gateway pods. |
| oidc.audience | string | `""` | Audience that users' access tokens must contain, such as `toolgate`. Required. |
| oidc.emailClaim | string | `""` | Access token claim that holds the user's email address. Empty means the gateway's default, `email`. |
| oidc.emailVerifiedClaim | string | `""` | Access token claim that tells whether the identity provider verified the email address. Empty means the gateway's default, `email_verified`. |
| oidc.groupsClaim | string | `""` | Access token claim that holds the user's groups. Empty means the gateway's default, `groups`. |
| oidc.issuerURL | string | `""` | Issuer URL of the OIDC identity provider that users sign in with, such as `https://idp.example.com/realms/main`. Required. |
| otlpEndpoint | string | `""` | OTLP/HTTP endpoint for the gateway's traces, such as `http://otel-collector:4318`. Empty turns trace export off. |
| podAnnotations | object | `{}` | Extra annotations for the gateway pods. |
| podLabels | object | `{}` | Extra labels for the gateway pods. |
| replicaCount | int | `2` | Number of gateway pods. Every replica serves requests; one at a time holds the leader Lease and runs the runtime reconciler. |
| resources | object | `{"limits":{"memory":"512Mi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | Resource requests and limits of the gateway container. |
| service.metricsPort | int | `9090` | Port of the metrics listener on the Service. |
| service.port | int | `80` | Port of the MCP and API listener on the Service. |
| service.sessionAffinity | string | `"None"` | Session affinity of the Service: `None` or `ClientIP`. toolgate does not need affinity to be correct. `ClientIP` makes replica switches rare. |
| service.type | string | `"ClusterIP"` | Service type. |
| serviceAccount.annotations | object | `{}` | Annotations for the service account. |
| serviceAccount.name | string | `""` | Name of the gateway's service account. Empty means the release's full name. The RBAC rules and the admission policy apply to this account. |
| sharedNamespace.create | bool | `true` | Create the shared namespace with toolgate's label. If false, the namespace must exist and carry the label `app.kubernetes.io/managed-by: toolgate`. |
| sharedNamespace.name | string | `""` | Name of the namespace for instances of `namespace: shared` templates. Empty means the chart creates none. It must not start with `tg-`. |
| tolerations | list | `[]` | Tolerations for the gateway pods. |
