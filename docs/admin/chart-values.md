# Chart values reference

For: admins who install the toolgate Helm chart.

This page is generated from `deploy/charts/toolgate/values.yaml` by `make generate-docs`. Do not edit it by hand.

[The chart page](chart.md) tells you what the chart installs and why. [The quick start](quick-start.md) shows an install.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| config.crds.enabled | bool | `true` | Install toolgate's CRDs with the chart. `helm upgrade` updates them. If false, install them yourself. |
| config.crds.keep | bool | `true` | Keep the CRDs, and so every template, rule and settings profile, when the release is uninstalled (`helm.sh/resource-policy: keep`). |
| config.database.existingSecret | string | `""` | Name of an existing Secret in the release namespace that holds the PostgreSQL connection string. Required. |
| config.database.secretKey | string | `"url"` | Key in `config.database.existingSecret` that holds the connection string. |
| config.extraEnv | list | `[]` | More environment variables for the gateway container, as a list of Kubernetes `EnvVar` objects. |
| config.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| config.image.repository | string | `"ghcr.io/soliddowant/toolgate"` | Gateway image repository. |
| config.image.tag | string | `""` | Image tag. Empty means the chart's `appVersion`. |
| config.imagePullSecrets | list | `[]` | Secrets that hold credentials for pulling the image, as a list of `{name: ...}` objects. |
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
| config.sharedNamespace.create | bool | `true` | Create the shared namespace with toolgate's label. If false, the namespace must exist and carry the label `app.kubernetes.io/managed-by: toolgate`. |
| config.sharedNamespace.name | string | `""` | Name of the namespace for instances of `namespace: shared` templates. Empty means the chart creates none. It must not start with `tg-`. |
| resources | object | see `values.yaml` | The gateway's Deployment, Services, ServiceAccount, PodDisruptionBudget and ServiceMonitor, in the values format of the bjw-s `common` library chart (https://bjw-s-labs.github.io/helm-charts/docs/common-library/). Override any part, such as probes, affinity, resources or extra containers. String values can use Helm templates. The RBAC rules and the admission policy are not here: values cannot change them. |
